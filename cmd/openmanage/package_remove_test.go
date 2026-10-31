package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPackageRemoveNamesAndCommands(t *testing.T) {
	for _, name := range []string{"luci-theme-example", "libfoo+bar", "pkg_name", "pkg.name", "pkg-1"} {
		if !validPackageName(name) {
			t.Errorf("valid package name rejected: %q", name)
		}
	}
	for _, name := range []string{"", "-rf", "pkg;reboot", "pkg name", "pkg/other", "pkg$(id)", "pkg\nreboot", strings.Repeat("a", 129)} {
		if validPackageName(name) {
			t.Errorf("invalid package name accepted: %q", name)
		}
	}
	for manager, want := range map[string]string{
		"opkg": "opkg remove luci-theme-example </dev/null >/dev/null 2>&1",
		"apk":  "apk del --no-network luci-theme-example </dev/null >/dev/null 2>&1",
	} {
		op, ok := packageRemoveOperation(manager, "luci-theme-example")
		if !ok || op.Class != operationWrite || len(op.Roles) != 1 || op.Roles[0] != "admin" || op.command != want {
			t.Errorf("%s operation = %+v, ok=%v", manager, op, ok)
		}
	}
	if _, ok := packageRemoveOperation("rpm", "safe"); ok {
		t.Fatal("unsupported manager must be refused")
	}
}

func TestPackageRemovePreflight(t *testing.T) {
	manifest := firmwareManifest{
		PackageManager: "apk",
		Packages:       []firmwarePackage{{Name: "luci-theme-example"}, {Name: "dropbear"}, {Name: "libfoo"}},
		UserInstalled:  []string{"luci-theme-example"},
	}
	if got := packageRemovePreflight("luci-theme-example", manifest); checksFailed(got) {
		t.Fatalf("user-installed package should pass: %+v", got)
	}
	for name, want := range map[string]string{
		"missing-package": "installed package",
		"dropbear":        "protected package",
		"libfoo":          "user-installed package",
		"kernel-module":   "protected package",
	} {
		checks := packageRemovePreflight(name, manifest)
		if !checksFailed(checks) {
			t.Errorf("%s should be refused: %+v", name, checks)
			continue
		}
		if checks[0].Name != want {
			t.Errorf("%s first check = %q, want %q", name, checks[0].Name, want)
		}
	}
}

func TestRemoveDevicePackageRequest(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	post := func(role string, req packageRemovalRequest) *httptest.ResponseRecorder {
		body, _ := json.Marshal(req)
		r := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
		r.SetPathValue("id", deviceID)
		r = r.WithContext(context.WithValue(r.Context(), userContextKey, authUser{ID: userID, Role: role}))
		w := httptest.NewRecorder()
		a.removeDevicePackage(w, r)
		return w
	}
	valid := packageRemovalRequest{
		PackageName: "luci-theme-example", ConfirmPackageName: "luci-theme-example",
		ConfirmName: "test-router", ApplyNow: true, AcknowledgeScripts: true,
	}
	if got := post("operator", valid).Code; got != http.StatusForbidden {
		t.Fatalf("operator status=%d, want forbidden", got)
	}
	bad := valid
	bad.ConfirmName = "wrong"
	if got := post("admin", bad).Code; got != http.StatusBadRequest {
		t.Fatalf("wrong device confirmation status=%d", got)
	}
	bad = valid
	bad.ConfirmPackageName = "other"
	if got := post("admin", bad).Code; got != http.StatusBadRequest {
		t.Fatalf("wrong package confirmation status=%d", got)
	}
	bad = valid
	bad.ApplyNow = false
	if got := post("admin", bad).Code; got != http.StatusBadRequest {
		t.Fatalf("missing apply-now acknowledgement status=%d", got)
	}
	bad = valid
	bad.AcknowledgeScripts = false
	if got := post("admin", bad).Code; got != http.StatusBadRequest {
		t.Fatalf("missing script acknowledgement status=%d", got)
	}
	bad = valid
	bad.PackageName = "luci-theme-example;reboot"
	bad.ConfirmPackageName = bad.PackageName
	if got := post("admin", bad).Code; got != http.StatusBadRequest {
		t.Fatalf("invalid package name status=%d", got)
	}
	w := post("admin", valid)
	if w.Code != http.StatusAccepted {
		t.Fatalf("valid request status=%d body=%s", w.Code, w.Body)
	}
	var job packageRemoval
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status string
		_ = db.QueryRow(ctx, "SELECT status FROM package_removals WHERE id=$1", job.ID).Scan(&status)
		if status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d status=%q, want failed for unenrolled test device", job.ID, status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var audited int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='device.package_remove'").Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("package removal audit count=%d err=%v", audited, err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO package_removals (device_id, name) VALUES ($1,'luci-theme-example')", deviceID); err != nil {
		t.Fatal(err)
	}
	if got := post("admin", valid).Code; got != http.StatusConflict {
		t.Fatalf("second removal status=%d, want conflict", got)
	}

	// A running removal also blocks the existing package-install endpoint.
	var sourceID int64
	if err := db.QueryRow(ctx, `INSERT INTO package_sources (name, version, arch, format, sha256, size, file, info, status)
		VALUES ('luci-theme-install','1','all','ipk',$1,1,'x','{"format":"ipk","name":"luci-theme-install","version":"1","arch":"all","depends":[],"scripts":[]}','confirmed') RETURNING id`,
		strings.Repeat("a", 64)).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	installBody, _ := json.Marshal(packageInstallRequest{SourceID: sourceID, ConfirmName: "test-router", AcknowledgeScripts: true})
	installReq := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(installBody))
	installReq.SetPathValue("id", deviceID)
	installReq = installReq.WithContext(context.WithValue(installReq.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	installRec := httptest.NewRecorder()
	a.installPackageSource(installRec, installReq)
	if installRec.Code != http.StatusConflict {
		t.Fatalf("install while removal is running status=%d, want conflict", installRec.Code)
	}
	if err := a.recoverPackageRemovals(ctx, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var running int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM package_removals WHERE status='running'").Scan(&running); err != nil || running != 0 {
		t.Fatalf("running removals=%d err=%v", running, err)
	}
}
