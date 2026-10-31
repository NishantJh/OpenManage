package main

import (
	"encoding/json"
	"testing"
)

func TestNormalizeFirmwareRolloutRequestFlashConcurrency(t *testing.T) {
	base := firmwareRolloutRequest{
		Version: "25.12.2",
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
		DeviceID: "550e8400-e29b-41d4-a716-446655440000",
		DeviceName: "router-1",
		Model: "OpenWrt x86",
		CurrentRelease: "24.10.4",
		BoardName: "generic",
		Target: "x86/64",
		InventoryID: 17,
		InventoryDigest: "inventory-digest",
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
