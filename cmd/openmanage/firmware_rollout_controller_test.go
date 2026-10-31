package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestNormalizeFirmwareRolloutRequestFlashConcurrency(t *testing.T) {
	base := firmwareRolloutRequest{
		Version:        "25.12.2",
		CanaryDeviceID: "550e8400-e29b-41d4-a716-446655440000",
	}
	got, err := normalizeFirmwareRolloutRequest(base)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxConcurrentFlashes != 2 {
		t.Fatalf("default max concurrency=%d, want 2", got.MaxConcurrentFlashes)
	}
	base.MaxConcurrentFlashes = 1
	got, err = normalizeFirmwareRolloutRequest(base)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxConcurrentFlashes != 1 {
		t.Fatalf("configured max concurrency=%d, want 1", got.MaxConcurrentFlashes)
	}
	for _, invalid := range []int{-1, 3, 100} {
		base.MaxConcurrentFlashes = invalid
		if _, err := normalizeFirmwareRolloutRequest(base); err == nil {
			t.Errorf("max concurrent flashes %d should be rejected", invalid)
		}
	}
}

func TestFirmwareRolloutMemberPlanRequiresExactDigestAndCleanPlan(t *testing.T) {
	member := firmwareRolloutMember{
		DeviceID:                  "550e8400-e29b-41d4-a716-446655440000",
		DeviceName:                "router-1",
		Model:                     "OpenWrt x86",
		CurrentRelease:            "24.10.4",
		BoardName:                 "generic",
		Target:                    "x86/64",
		InventoryID:               17,
		InventoryDigest:           "inventory-digest",
		DesiredPackageSetRevision: 3,
	}
	plan := firmwarePlan{Version: "25.12.2"}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	member.Plan = encoded
	member.PlanDigest, err = firmwareRolloutPlanDigest(member, plan.Version, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firmwareRolloutMemberPlan(member, plan.Version); err != nil {
		t.Fatalf("valid immutable plan rejected: %v", err)
	}

	tampered := member
	tampered.Plan = append(json.RawMessage(nil), member.Plan...)
	tampered.Plan = json.RawMessage(`{"version":"25.12.3"}`)
	if _, err := firmwareRolloutMemberPlan(tampered, plan.Version); err == nil {
		t.Fatal("tampered plan should fail its digest check")
	}

	warned := firmwarePlan{Version: "25.12.2", Warnings: []string{"custom source changed"}}
	warnedJSON, err := json.Marshal(warned)
	if err != nil {
		t.Fatal(err)
	}
	warnedMember := member
	warnedMember.Plan = warnedJSON
	warnedMember.PlanDigest, err = firmwareRolloutPlanDigest(warnedMember, warned.Version, warned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firmwareRolloutMemberPlan(warnedMember, warned.Version); err == nil {
		t.Fatal("plans with warnings must not be executable")
	}
}

func TestAdvanceFirmwareRolloutRequiresAcknowledgedSuccessfulBatch(t *testing.T) {
	db := newEnrollmentTestDB(t)
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	var nextDeviceID string
	if err := db.QueryRow(ctx, `INSERT INTO devices(name,address,model,status)
		VALUES('batch-router','192.0.2.2','OpenWrt x86','online') RETURNING id::text`).Scan(&nextDeviceID); err != nil {
		t.Fatal(err)
	}

	var rolloutID int64
	if err := db.QueryRow(ctx, `INSERT INTO firmware_rollouts(version,status,canary_device_id,created_by)
		VALUES('25.12.2','canary_running',$1,$2) RETURNING id`, deviceID, userID).Scan(&rolloutID); err != nil {
		t.Fatal(err)
	}
	var canaryUpgradeID int64
	if err := db.QueryRow(ctx, `INSERT INTO firmware_upgrades(device_id,version,status,created_by)
		VALUES($1,'25.12.2','done',$2) RETURNING id`, deviceID, userID).Scan(&canaryUpgradeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO firmware_rollout_devices(
		rollout_id,device_id,device_id_snapshot,device_name,model,current_release,board_name,target,
		inventory_id,inventory_digest,inventory_manifest,inventory_verified_at,desired_package_set_revision,
		batch_number,position,status,upgrade_id)
		VALUES($1,$2,$2,'canary-router','OpenWrt x86','24.10.4','generic','x86/64',
		1,'canary-inventory','{}'::jsonb,NOW(),0,0,0,'done',$3)`,
		rolloutID, deviceID, canaryUpgradeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO firmware_rollout_devices(
		rollout_id,device_id,device_id_snapshot,device_name,model,current_release,board_name,target,
		inventory_id,inventory_digest,inventory_manifest,inventory_verified_at,desired_package_set_revision,
		batch_number,position,status)
		VALUES($1,$2,$2,'batch-router','OpenWrt x86','24.10.4','generic','x86/64',
		2,'batch-inventory','{}'::jsonb,NOW(),0,1,0,'queued')`,
		rolloutID, nextDeviceID); err != nil {
		t.Fatal(err)
	}

	a := &app{db: db}
	advance := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/firmware/rollouts/"+strconv.FormatInt(rolloutID, 10)+"/advance", nil)
		req.SetPathValue("rollout", strconv.FormatInt(rolloutID, 10))
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
		rec := httptest.NewRecorder()
		a.advanceFirmwareRollout(rec, req)
		return rec
	}
	if rec := advance(); rec.Code != http.StatusConflict {
		t.Fatalf("unacknowledged canary must not release the next batch: %d %s", rec.Code, rec.Body)
	}
	if _, err := db.Exec(ctx, `UPDATE firmware_upgrades SET acknowledged_at=NOW(),acknowledged_by=$2 WHERE id=$1`, canaryUpgradeID, userID); err != nil {
		t.Fatal(err)
	}
	if rec := advance(); rec.Code != http.StatusOK {
		t.Fatalf("acknowledged successful canary should release batch 1: %d %s", rec.Code, rec.Body)
	}
	var status string
	var currentBatch int
	if err := db.QueryRow(ctx, `SELECT status,current_batch FROM firmware_rollouts WHERE id=$1`, rolloutID).Scan(&status, &currentBatch); err != nil {
		t.Fatal(err)
	}
	if status != "batch_running" || currentBatch != 1 {
		t.Fatalf("after canary: status=%q current_batch=%d", status, currentBatch)
	}

	var batchUpgradeID int64
	if err := db.QueryRow(ctx, `INSERT INTO firmware_upgrades(device_id,version,status,created_by,acknowledged_at,acknowledged_by)
		VALUES($1,'25.12.2','done',$2,NOW(),$2) RETURNING id`, nextDeviceID, userID).Scan(&batchUpgradeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE firmware_rollout_devices SET status='done',upgrade_id=$3
		WHERE rollout_id=$1 AND device_id_snapshot=$2`, rolloutID, nextDeviceID, batchUpgradeID); err != nil {
		t.Fatal(err)
	}
	if rec := advance(); rec.Code != http.StatusOK {
		t.Fatalf("acknowledged successful final batch should complete the rollout: %d %s", rec.Code, rec.Body)
	}
	if err := db.QueryRow(ctx, `SELECT status,current_batch FROM firmware_rollouts WHERE id=$1`, rolloutID).Scan(&status, &currentBatch); err != nil {
		t.Fatal(err)
	}
	if status != "complete" || currentBatch != 1 {
		t.Fatalf("after final batch: status=%q current_batch=%d", status, currentBatch)
	}

	var warnedRolloutID int64
	if err := db.QueryRow(ctx, `INSERT INTO firmware_rollouts(version,status,canary_device_id,created_by)
		VALUES('25.12.2','canary_running',$1,$2) RETURNING id`, deviceID, userID).Scan(&warnedRolloutID); err != nil {
		t.Fatal(err)
	}
	var warnedUpgradeID int64
	if err := db.QueryRow(ctx, `INSERT INTO firmware_upgrades(device_id,version,status,created_by,acknowledged_at,acknowledged_by)
		VALUES($1,'25.12.2','done_with_warnings',$2,NOW(),$2) RETURNING id`, deviceID, userID).Scan(&warnedUpgradeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO firmware_rollout_devices(
		rollout_id,device_id,device_id_snapshot,device_name,model,current_release,board_name,target,
		inventory_id,inventory_digest,inventory_manifest,inventory_verified_at,desired_package_set_revision,
		batch_number,position,status,upgrade_id)
		VALUES($1,$2,$2,'warned-router','OpenWrt x86','24.10.4','generic','x86/64',
		3,'warned-inventory','{}'::jsonb,NOW(),0,0,0,'done_with_warnings',$3)`,
		warnedRolloutID, deviceID, warnedUpgradeID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/firmware/rollouts/"+strconv.FormatInt(warnedRolloutID, 10)+"/advance", nil)
	req.SetPathValue("rollout", strconv.FormatInt(warnedRolloutID, 10))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	rec := httptest.NewRecorder()
	a.advanceFirmwareRollout(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("acknowledged done_with_warnings must not pass the rollout gate: %d %s", rec.Code, rec.Body)
	}
}
