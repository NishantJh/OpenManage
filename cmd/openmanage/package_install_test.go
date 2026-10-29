package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func checkStatus(checks []upgradeCheck, name string) string {
	for _, c := range checks {
		if c.Name == name {
			return c.Status
		}
	}
	return "missing"
}

func TestPackagePreflight(t *testing.T) {
	m := firmwareManifest{
		PackageManager: "apk", Arch: "aarch64_cortex-a53", OverlayFreeKB: 10000, MemAvailableKB: 100000,
		Packages: []firmwarePackage{{Name: "libc", Version: "1"}, {Name: "luci-theme-x", Version: "1.0"}},
	}
	good := packageInfo{Format: "apk", Name: "luci-theme-fluent", Version: "2.0", Arch: "noarch", InstalledSize: 100 << 10}
	for _, c := range packagePreflight(good, 50<<10, m) {
		if c.Status != "ok" {
			t.Errorf("%s should pass: %s", c.Name, c.Detail)
		}
	}
	cases := map[string]struct {
		mutate func(*packageInfo, *firmwareManifest)
		check  string
	}{
		"wrong manager": {func(i *packageInfo, m *firmwareManifest) { i.Format = "ipk" }, "package format"},
		"wrong arch":    {func(i *packageInfo, m *firmwareManifest) { i.Arch = "mipsel_24kc" }, "architecture"},
		"no flash":      {func(i *packageInfo, m *firmwareManifest) { m.OverlayFreeKB = 100 }, "free flash"},
		"flash unknown": {func(i *packageInfo, m *firmwareManifest) { m.OverlayFreeKB = 0 }, "free flash"},
		"no memory":     {func(i *packageInfo, m *firmwareManifest) { m.MemAvailableKB = 1000 }, "free memory"},
		"same version":  {func(i *packageInfo, m *firmwareManifest) { i.Name, i.Version = "luci-theme-x", "1.0" }, "already installed"},
		"missing ipk dep": {func(i *packageInfo, m *firmwareManifest) {
			i.Format, m.PackageManager, i.Depends = "ipk", "opkg", []string{"libc", "luci-base (>= 1)"}
		}, "dependencies"},
	}
	for name, tc := range cases {
		info, man := good, m
		tc.mutate(&info, &man)
		if got := checkStatus(packagePreflight(info, 50<<10, man), tc.check); got != "failed" {
			t.Errorf("%s: %s = %s, want failed", name, tc.check, got)
		}
	}
	// A different installed version is an upgrade, not a failure.
	info := good
	info.Name, info.Version = "luci-theme-x", "2.0"
	if got := checkStatus(packagePreflight(info, 1, m), "already installed"); got != "ok" {
		t.Errorf("an upgrade must pass, got %s", got)
	}
}

func TestPackageDependencyName(t *testing.T) {
	for in, want := range map[string]string{"libc": "libc", "libubox (>= 2020)": "libubox", "a (= 1)": "a"} {
		if got := packageDependencyName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestInstallCommandsAreFixed(t *testing.T) {
	for _, f := range installFormats {
		for _, op := range []operation{f.stage, f.hash, f.install} {
			if op.Class != operationWrite || len(op.Roles) != 1 || op.Roles[0] != "admin" {
				t.Errorf("%s must be an admin-only write operation", op.Name)
			}
		}
	}
	if !strings.Contains(installFormats["apk"].install.command, "--allow-untrusted "+packageStagePath("apk")) {
		t.Error("apk install must name the one staged file")
	}
	if strings.Contains(installFormats["ipk"].install.command, "--allow-untrusted") {
		t.Error("opkg install must not be given --allow-untrusted")
	}
}

func TestInstallPackageSourceRequests(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	var sourceID, pendingID int64
	for status, dst := range map[string]*int64{"confirmed": &sourceID, "pending": &pendingID} {
		if err := db.QueryRow(ctx, `INSERT INTO package_sources (name, version, arch, format, sha256, size, file, info, status)
			VALUES ('t','1','all','ipk',$1,1,'x','{"format":"ipk","name":"t","version":"1","arch":"all","depends":[],"scripts":[]}',$2) RETURNING id`,
			strings.Repeat(map[string]string{"confirmed": "a", "pending": "b"}[status], 64), status).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	post := func(role string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", deviceID)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: role}))
		rec := httptest.NewRecorder()
		a.installPackageSource(rec, req)
		return rec
	}
	body := func(id int64, name string, ack bool) string {
		b, _ := json.Marshal(packageInstallRequest{SourceID: id, ConfirmName: name, AcknowledgeScripts: ack})
		return string(b)
	}
	for _, role := range []string{"operator", "readonly"} {
		if c := post(role, body(sourceID, "test-router", true)).Code; c != http.StatusForbidden {
			t.Fatalf("%s install: %d", role, c)
		}
	}
	if c := post("admin", body(sourceID, "wrong", true)).Code; c != http.StatusBadRequest {
		t.Fatalf("wrong device name: %d", c)
	}
	if c := post("admin", body(sourceID, "test-router", false)).Code; c != http.StatusBadRequest {
		t.Fatalf("scripts not acknowledged: %d", c)
	}
	if c := post("admin", body(pendingID, "test-router", true)).Code; c != http.StatusNotFound {
		t.Fatalf("a pending source must not install: %d", c)
	}
	rec := post("admin", body(sourceID, "test-router", true))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("install: %d %s", rec.Code, rec.Body)
	}
	// The device is not enrolled, so the job ends failed without touching anything.
	var job packageInstall
	json.Unmarshal(rec.Body.Bytes(), &job)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status string
		_ = db.QueryRow(ctx, `SELECT status FROM package_installs WHERE id=$1`, job.ID).Scan(&status)
		if status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d status %q, want failed", job.ID, status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var audited int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='device.package_install'`).Scan(&audited)
	if audited != 1 {
		t.Fatalf("audit events = %d, want 1", audited)
	}

	// A running install blocks a second one and other long jobs.
	if _, err := db.Exec(ctx, `INSERT INTO package_installs (device_id, source_id, name, version, sha256) VALUES ($1,$2,'t','1','x')`, deviceID, sourceID); err != nil {
		t.Fatal(err)
	}
	if c := post("admin", body(sourceID, "test-router", true)).Code; c != http.StatusConflict {
		t.Fatalf("second install: %d", c)
	}
	recov := a.recoverPackageInstalls(ctx, time.Now().Add(time.Second))
	if recov != nil {
		t.Fatal(recov)
	}
	var running int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM package_installs WHERE status='running'`).Scan(&running)
	if running != 0 {
		t.Fatal(fmt.Sprint("recovery must end running installs, ", running, " left"))
	}
}
