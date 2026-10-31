package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func validFirmwareHardwareQualificationRequest() firmwareHardwareQualificationRequest {
	return firmwareHardwareQualificationRequest{
		BoardName: "generic", Model: "OpenWrt x86",
		SourceRelease: "24.10.4", TargetRelease: "25.12.2",
		TestedOn:         time.Now().UTC().Format("2006-01-02"),
		PhysicalHardware: true,
		UpgradeChecks: []string{
			"image_hash_verified", "pre_flash_backup_verified", "sysupgrade_test_passed",
			"post_reboot_reachable", "post_reboot_inventory_verified", "management_access_verified",
		},
		UpgradeOutcome: "passed", RecoveryMethod: "failsafe", RecoveryOutcome: "passed",
		EvidenceReference: "lab-run-2026-10-31", Notes: "Physical device upgrade and recovery drill completed.",
	}
}

func TestNormalizeFirmwareHardwareQualification(t *testing.T) {
	valid := validFirmwareHardwareQualificationRequest()
	got, err := normalizeFirmwareHardwareQualification(valid)
	if err != nil {
		t.Fatal(err)
	}
	record := firmwareHardwareQualification{
		BoardName: got.BoardName, Model: got.Model, SourceRelease: got.SourceRelease,
		TargetRelease: got.TargetRelease, TestedOn: got.TestedOn,
		PhysicalHardware: got.PhysicalHardware, UpgradeChecks: got.UpgradeChecks,
		UpgradeOutcome: got.UpgradeOutcome, RecoveryMethod: got.RecoveryMethod,
		RecoveryOutcome: got.RecoveryOutcome, EvidenceReference: got.EvidenceReference,
	}
	if !firmwareHardwareQualificationQualifies(record) {
		t.Fatal("complete current physical hardware drill should qualify")
	}

	virtual := validFirmwareHardwareQualificationRequest()
	virtual.PhysicalHardware = false
	virtualRecord, err := normalizeFirmwareHardwareQualification(virtual)
	if err != nil {
		t.Fatal(err)
	}
	if firmwareHardwareQualificationQualifies(firmwareHardwareQualification{
		PhysicalHardware:  virtualRecord.PhysicalHardware,
		UpgradeOutcome:    virtualRecord.UpgradeOutcome,
		RecoveryMethod:    virtualRecord.RecoveryMethod,
		RecoveryOutcome:   virtualRecord.RecoveryOutcome,
		TestedOn:          virtualRecord.TestedOn,
		UpgradeChecks:     virtualRecord.UpgradeChecks,
		EvidenceReference: virtualRecord.EvidenceReference,
	}) {
		t.Fatal("VM/container evidence must never qualify")
	}

	tests := []struct {
		name string
		edit func(*firmwareHardwareQualificationRequest)
	}{
		{"same release", func(q *firmwareHardwareQualificationRequest) { q.TargetRelease = q.SourceRelease }},
		{"snapshot target", func(q *firmwareHardwareQualificationRequest) { q.TargetRelease = "SNAPSHOT" }},
		{"future date", func(q *firmwareHardwareQualificationRequest) {
			q.TestedOn = time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
		}},
		{"unknown check", func(q *firmwareHardwareQualificationRequest) { q.UpgradeChecks = append(q.UpgradeChecks, "run_shell") }},
		{"duplicate check", func(q *firmwareHardwareQualificationRequest) {
			q.UpgradeChecks = append(q.UpgradeChecks, q.UpgradeChecks[0])
		}},
		{"unknown recovery method", func(q *firmwareHardwareQualificationRequest) { q.RecoveryMethod = "ssh" }},
		{"missing evidence for success", func(q *firmwareHardwareQualificationRequest) { q.EvidenceReference = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validFirmwareHardwareQualificationRequest()
			tc.edit(&in)
			if _, err := normalizeFirmwareHardwareQualification(in); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestCreateAndListFirmwareHardwareQualification(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	_, userID := seedDeviceAndUser(t, db)
	in, err := json.Marshal(validFirmwareHardwareQualificationRequest())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/firmware/hardware-qualifications", strings.NewReader(string(in)))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	rec := httptest.NewRecorder()
	a.createFirmwareHardwareQualification(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created firmwareHardwareQualification
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || !created.Qualifies || created.BoardName != "generic" || created.Model != "OpenWrt x86" {
		t.Fatalf("created qualification=%+v", created)
	}
	var auditCount int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='firmware.hardware_qualification_record'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("qualification audit count=%d", auditCount)
	}

	list := httptest.NewRequest(http.MethodGet, "/api/firmware/hardware-qualifications?board_name=generic&model=OpenWrt+x86", nil)
	list = list.WithContext(context.WithValue(list.Context(), userContextKey, authUser{ID: userID, Role: "operator"}))
	listRec := httptest.NewRecorder()
	a.listFirmwareHardwareQualifications(listRec, list)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", listRec.Code, listRec.Body)
	}
	var result struct {
		Qualifications []firmwareHardwareQualification `json:"qualifications"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Qualifications) != 1 || result.Qualifications[0].ID != created.ID || !result.Qualifications[0].Qualifies {
		t.Fatalf("listed qualifications=%+v", result.Qualifications)
	}
}

func TestFirmwareRolloutQualificationStatusRequiresExactHardwareAndReleaseMatch(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	_, userID := seedDeviceAndUser(t, db)
	in := validFirmwareHardwareQualificationRequest()
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/firmware/hardware-qualifications", strings.NewReader(string(encoded)))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	rec := httptest.NewRecorder()
	a.createFirmwareHardwareQualification(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create qualification: %d %s", rec.Code, rec.Body)
	}

	rollout := firmwareRollout{
		ID: 42, Version: "25.12.2",
		Devices: []firmwareRolloutMember{
			{DeviceID: "device-1", BoardName: "generic", Model: "OpenWrt x86", CurrentRelease: "24.10.4"},
			{DeviceID: "device-2", BoardName: "generic", Model: "OpenWrt x86", CurrentRelease: "24.10.4"},
		},
	}
	status, err := a.firmwareRolloutQualificationStatus(context.Background(), rollout)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Qualified || status.RequiredHardware != 1 || len(status.Matched) != 1 || len(status.Missing) != 0 {
		t.Fatalf("matching qualification status=%+v", status)
	}

	rollout.Devices[1].Model = "different-model"
	status, err = a.firmwareRolloutQualificationStatus(context.Background(), rollout)
	if err != nil {
		t.Fatal(err)
	}
	if status.Qualified || status.RequiredHardware != 2 || len(status.Missing) != 1 {
		t.Fatalf("mismatched model should remain unqualified: %+v", status)
	}
}
