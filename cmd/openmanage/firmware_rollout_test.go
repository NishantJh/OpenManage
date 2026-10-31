package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestNormalizeFirmwareRolloutRequest(t *testing.T) {
	canary := "550e8400-e29b-41d4-a716-446655440000"
	first := "550e8400-e29b-41d4-a716-446655440001"
	second := "550e8400-e29b-41d4-a716-446655440002"
	tests := []struct {
		name    string
		in      firmwareRolloutRequest
		wantErr bool
	}{
		{"valid", firmwareRolloutRequest{Version: "25.12.2", CanaryDeviceID: canary, Batches: [][]string{{first}, {second}}}, false},
		{"canary only", firmwareRolloutRequest{Version: "25.12.2", CanaryDeviceID: canary}, false},
		{"snapshot", firmwareRolloutRequest{Version: "SNAPSHOT", CanaryDeviceID: canary}, true},
		{"release candidate", firmwareRolloutRequest{Version: "25.12.2-rc1", CanaryDeviceID: canary}, true},
		{"invalid canary", firmwareRolloutRequest{Version: "25.12.2", CanaryDeviceID: "bad"}, true},
		{"duplicate across batches", firmwareRolloutRequest{Version: "25.12.2", CanaryDeviceID: canary, Batches: [][]string{{first}, {first}}}, true},
		{"canary duplicated in batch", firmwareRolloutRequest{Version: "25.12.2", CanaryDeviceID: canary, Batches: [][]string{{canary}}}, true},
		{"empty batch", firmwareRolloutRequest{Version: "25.12.2", CanaryDeviceID: canary, Batches: [][]string{{}}}, true},
		{"oversized batch", firmwareRolloutRequest{Version: "25.12.2", CanaryDeviceID: canary, Batches: [][]string{{first, second, "550e8400-e29b-41d4-a716-446655440003", "550e8400-e29b-41d4-a716-446655440004", "550e8400-e29b-41d4-a716-446655440005", "550e8400-e29b-41d4-a716-446655440006"}}}, true},
		{"too many batches", firmwareRolloutRequest{Version: "25.12.2", CanaryDeviceID: canary, Batches: [][]string{{first}, {second}, {"550e8400-e29b-41d4-a716-446655440003"}, {"550e8400-e29b-41d4-a716-446655440004"}, {"550e8400-e29b-41d4-a716-446655440005"}}}, true},
		{"too many devices", firmwareRolloutRequest{Version: "25.12.2", CanaryDeviceID: canary, Batches: [][]string{{first, second, "550e8400-e29b-41d4-a716-446655440003", "550e8400-e29b-41d4-a716-446655440004", "550e8400-e29b-41d4-a716-446655440005"}, {"550e8400-e29b-41d4-a716-446655440006", "550e8400-e29b-41d4-a716-446655440007", "550e8400-e29b-41d4-a716-446655440008", "550e8400-e29b-41d4-a716-446655440009", "550e8400-e29b-41d4-a716-446655440010"}, {"550e8400-e29b-41d4-a716-446655440011", "550e8400-e29b-41d4-a716-446655440012", "550e8400-e29b-41d4-a716-446655440013", "550e8400-e29b-41d4-a716-446655440014"}}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeFirmwareRolloutRequest(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestCreateFirmwareRolloutRequiresAdminAndValidBody(t *testing.T) {
	id := "550e8400-e29b-41d4-a716-446655440000"
	cases := []struct {
		role string
		body string
		want int
	}{
		{"operator", `{"version":"25.12.2","canary_device_id":"` + id + `"}`, http.StatusForbidden},
		{"readonly", `{"version":"25.12.2","canary_device_id":"` + id + `"}`, http.StatusForbidden},
		{"admin", `{"version":"SNAPSHOT","canary_device_id":"` + id + `"}`, http.StatusBadRequest},
		{"admin", `{"version":"25.12.2","canary_device_id":"bad"}`, http.StatusBadRequest},
		{"", `{}`, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/firmware/rollouts", strings.NewReader(tc.body))
		if tc.role != "" {
			req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: tc.role}))
		}
		rec := httptest.NewRecorder()
		(&app{}).createFirmwareRollout(rec, req)
		if rec.Code != tc.want {
			t.Errorf("role=%q body=%s status=%d want=%d: %s", tc.role, tc.body, rec.Code, tc.want, rec.Body)
		}
	}
}

func TestFirmwareRolloutDraftSnapshotsInventoryAndPackageRevision(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	manifest := firmwareManifest{Release: "24.10.4", Revision: "r1", Target: "x86/64", Arch: "x86_64", BoardName: "generic", Model: "OpenWrt x86", PackageManager: "opkg", Packages: []firmwarePackage{}, UserInstalled: []string{}, RemovedDefaults: []string{}}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var inventoryID int64
	if err = db.QueryRow(ctx, `INSERT INTO firmware_inventories(device_id,digest,manifest) VALUES($1,$2,$3) RETURNING id`, deviceID, strings.Repeat("a", 64), encoded).Scan(&inventoryID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO device_package_sets(device_id) VALUES($1)`, deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO device_package_set_revisions(package_set_id,revision,official_packages,removed_defaults,created_by)
		SELECT id,1,ARRAY['curl']::text[],'{}'::text[],$2 FROM device_package_sets WHERE device_id=$1`, deviceID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `UPDATE device_package_sets SET current_revision_id=(SELECT id FROM device_package_set_revisions WHERE package_set_id=device_package_sets.id AND revision=1) WHERE device_id=$1`, deviceID); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"version":"25.12.2","canary_device_id":"` + deviceID + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/firmware/rollouts", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	rec := httptest.NewRecorder()
	a.createFirmwareRollout(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var got firmwareRollout
	if err = json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "draft" || got.Version != "25.12.2" || got.CanaryDeviceID != deviceID || len(got.Devices) != 1 {
		t.Fatalf("rollout=%+v", got)
	}
	member := got.Devices[0]
	if member.InventoryID != inventoryID || member.InventoryDigest != strings.Repeat("a", 64) || member.DesiredPackageSetRevision != 1 ||
		member.CurrentRelease != "24.10.4" || member.BoardName != "generic" || member.BatchNumber != 0 || member.Status != "queued" {
		t.Fatalf("member snapshot=%+v", member)
	}
	var upgrades, audits int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM firmware_upgrades WHERE device_id=$1`, deviceID).Scan(&upgrades); err != nil {
		t.Fatal(err)
	}
	if upgrades != 0 {
		t.Fatalf("draft creation must not prepare firmware; jobs=%d", upgrades)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='firmware.rollout_draft_create'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("draft audit count=%d", audits)
	}
	read := httptest.NewRequest(http.MethodGet, "/api/firmware/rollouts/"+strconv.FormatInt(got.ID, 10), nil)
	read.SetPathValue("rollout", strconv.FormatInt(got.ID, 10))
	read = read.WithContext(context.WithValue(read.Context(), userContextKey, authUser{ID: userID, Role: "operator"}))
	readRec := httptest.NewRecorder()
	a.getFirmwareRollout(readRec, read)
	if readRec.Code != http.StatusOK {
		t.Fatalf("read: %d %s", readRec.Code, readRec.Body)
	}
	var loaded firmwareRollout
	if err = json.Unmarshal(readRec.Body.Bytes(), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.ID != got.ID || len(loaded.Devices) != 1 || loaded.Devices[0].InventoryID != inventoryID {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestFirmwareRolloutDraftRequiresStoredInventory(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	deviceID, userID := seedDeviceAndUser(t, db)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"version":"25.12.2","canary_device_id":"`+deviceID+`"}`))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	rec := httptest.NewRecorder()
	a.createFirmwareRollout(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("missing inventory: %d %s", rec.Code, rec.Body)
	}
	var count int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM firmware_rollouts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed draft left %d campaign rows", count)
	}
}
