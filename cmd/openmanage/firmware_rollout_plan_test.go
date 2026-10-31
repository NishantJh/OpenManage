package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestLoadDevicePackageSetRevisionUsesCapturedRevision(t *testing.T) {
	db := newEnrollmentTestDB(t)
	deviceID, userID := seedDeviceAndUser(t, db)
	ctx := context.Background()

	var setID int64
	if err := db.QueryRow(ctx, `INSERT INTO device_package_sets(device_id) VALUES($1) RETURNING id`, deviceID).Scan(&setID); err != nil {
		t.Fatal(err)
	}
	var firstID, secondID int64
	if err := db.QueryRow(ctx, `INSERT INTO device_package_set_revisions(package_set_id,revision,official_packages,removed_defaults,created_by)
		VALUES($1,1,ARRAY['curl']::text[],ARRAY['ppp']::text[],$2) RETURNING id`, setID, userID).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO device_package_set_revisions(package_set_id,revision,official_packages,removed_defaults,created_by)
		VALUES($1,2,ARRAY['wget']::text[],ARRAY['dnsmasq']::text[],$2) RETURNING id`, setID, userID).Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE device_package_sets SET current_revision_id=$2 WHERE id=$1`, setID, secondID); err != nil {
		t.Fatal(err)
	}

	got, err := loadDevicePackageSetRevision(ctx, db, deviceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || len(got.OfficialPackages) != 1 || got.OfficialPackages[0] != "curl" ||
		len(got.RemovedDefaults) != 1 || got.RemovedDefaults[0] != "ppp" {
		t.Fatalf("revision 1 = %+v", got)
	}
	current, err := loadDevicePackageSetRevision(ctx, db, deviceID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 2 || current.OfficialPackages[0] != "wget" || current.RemovedDefaults[0] != "dnsmasq" {
		t.Fatalf("revision 2 = %+v", current)
	}
	empty, err := loadDevicePackageSetRevision(ctx, db, deviceID, 0)
	if err != nil || empty.Revision != 0 || len(empty.OfficialPackages) != 0 {
		t.Fatalf("empty revision = %+v, err=%v", empty, err)
	}
}

func TestFirmwareRolloutMemberCurrentDetectsNewInventory(t *testing.T) {
	db := newEnrollmentTestDB(t)
	deviceID, _ := seedDeviceAndUser(t, db)
	ctx := context.Background()
	var name, model string
	if err := db.QueryRow(ctx, `SELECT name,model FROM devices WHERE id=$1`, deviceID).Scan(&name, &model); err != nil {
		t.Fatal(err)
	}
	manifest := firmwareManifest{Release: "24.10.4", BoardName: "generic", Target: "x86/64"}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var inventoryID int64
	digest := strings.Repeat("a", 64)
	if err = db.QueryRow(ctx, `INSERT INTO firmware_inventories(device_id,digest,manifest) VALUES($1,$2,$3) RETURNING id`, deviceID, digest, encoded).Scan(&inventoryID); err != nil {
		t.Fatal(err)
	}
	member := firmwareRolloutMember{
		DeviceID: deviceID, DeviceName: name, Model: model, InventoryID: inventoryID,
		InventoryDigest: digest, DesiredPackageSetRevision: 0,
	}
	current, err := firmwareRolloutMemberCurrent(ctx, db, member)
	if err != nil || !current {
		t.Fatalf("unchanged snapshot current=%v err=%v", current, err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO firmware_inventories(device_id,digest,manifest) VALUES($1,$2,$3)`, deviceID, strings.Repeat("b", 64), encoded); err != nil {
		t.Fatal(err)
	}
	current, err = firmwareRolloutMemberCurrent(ctx, db, member)
	if err != nil {
		t.Fatal(err)
	}
	if current {
		t.Fatal("a newer inventory must invalidate the rollout snapshot")
	}
}

func TestFirmwareRolloutPlanDigestIncludesSnapshotAndPlan(t *testing.T) {
	member := firmwareRolloutMember{
		DeviceID: "550e8400-e29b-41d4-a716-446655440000", DeviceName: "ap-1", Model: "router",
		CurrentRelease: "24.10.4", BoardName: "generic", Target: "x86/64",
		InventoryID: 12, InventoryDigest: strings.Repeat("a", 64), DesiredPackageSetRevision: 4,
	}
	plan := firmwarePlan{Version: "25.12.2", Packages: []string{"base-files", "curl"}}
	first, err := firmwareRolloutPlanDigest(member, "25.12.2", plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := firmwareRolloutPlanDigest(member, "25.12.2", plan)
	if err != nil || first != second {
		t.Fatalf("digest must be stable: %q %q err=%v", first, second, err)
	}
	member.InventoryDigest = strings.Repeat("b", 64)
	changedSnapshot, err := firmwareRolloutPlanDigest(member, "25.12.2", plan)
	if err != nil || changedSnapshot == first {
		t.Fatalf("snapshot change did not change digest: %q %q err=%v", first, changedSnapshot, err)
	}
	plan.Packages = append(plan.Packages, "luci-app-demo")
	changedPlan, err := firmwareRolloutPlanDigest(member, "25.12.2", plan)
	if err != nil || changedPlan == changedSnapshot {
		t.Fatalf("plan change did not change digest: %q %q err=%v", changedSnapshot, changedPlan, err)
	}
}

func TestPreviewFirmwareRolloutRequiresAdminAndValidID(t *testing.T) {
	for _, tc := range []struct {
		role string
		id   string
		want int
	}{
		{"operator", "1", http.StatusForbidden},
		{"admin", "not-an-id", http.StatusBadRequest},
		{"", "1", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(http.MethodPost, "/preview", nil)
		req.SetPathValue("rollout", tc.id)
		if tc.role != "" {
			req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: tc.role}))
		}
		rec := httptest.NewRecorder()
		(&app{}).previewFirmwareRollout(rec, req)
		if rec.Code != tc.want {
			t.Errorf("role=%q id=%q status=%d want=%d", tc.role, tc.id, rec.Code, tc.want)
		}
	}
}

func TestPreviewFirmwareRolloutRejectsStaleInventoryBeforeBuildServer(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	manifest := firmwareManifest{Release: "24.10.4", Revision: "r1", Target: "x86/64", BoardName: "generic", PackageManager: "opkg"}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO firmware_inventories(device_id,digest,manifest) VALUES($1,$2,$3)`, deviceID, strings.Repeat("a", 64), encoded); err != nil {
		t.Fatal(err)
	}
	body := `{"version":"25.12.2","canary_device_id":"` + deviceID + `"}`
	create := httptest.NewRequest(http.MethodPost, "/api/firmware/rollouts", strings.NewReader(body))
	create = create.WithContext(context.WithValue(create.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	createRec := httptest.NewRecorder()
	a.createFirmwareRollout(createRec, create)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create draft: %d %s", createRec.Code, createRec.Body)
	}
	var rollout firmwareRollout
	if err = json.Unmarshal(createRec.Body.Bytes(), &rollout); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO firmware_inventories(device_id,digest,manifest) VALUES($1,$2,$3)`, deviceID, strings.Repeat("b", 64), encoded); err != nil {
		t.Fatal(err)
	}

	preview := httptest.NewRequest(http.MethodPost, "/preview", nil)
	preview.SetPathValue("rollout", strconv.FormatInt(rollout.ID, 10))
	preview = preview.WithContext(context.WithValue(preview.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	previewRec := httptest.NewRecorder()
	a.previewFirmwareRollout(previewRec, preview)
	if previewRec.Code != http.StatusConflict {
		t.Fatalf("stale preview: %d %s", previewRec.Code, previewRec.Body)
	}
	var status string
	if err = db.QueryRow(ctx, `SELECT status FROM firmware_rollouts WHERE id=$1`, rollout.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "draft" {
		t.Fatalf("stale preview changed campaign status to %q", status)
	}
	var plans int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM firmware_rollout_devices WHERE rollout_id=$1 AND plan IS NOT NULL`, rollout.ID).Scan(&plans); err != nil {
		t.Fatal(err)
	}
	if plans != 0 {
		t.Fatalf("stale preview persisted %d plans", plans)
	}
}
