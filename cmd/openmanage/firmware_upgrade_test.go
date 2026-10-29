package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestUpgradePathProblem(t *testing.T) {
	branches := []string{"23.05", "24.10", "25.12"}
	for name, tc := range map[string]struct {
		from, to string
		problem  bool
	}{
		"same release reinstall": {"24.10.4", "24.10.4", false},
		"same family newer":      {"24.10.0", "24.10.4", false},
		"next family":            {"24.10.4", "25.12.2", false},
		"skips a family":         {"23.05.5", "25.12.2", true},
		"downgrade":              {"25.12.2", "24.10.4", true},
		"downgrade in family":    {"24.10.4", "24.10.0", true},
		"snapshot target":        {"24.10.4", "SNAPSHOT", true},
		"branch snapshot target": {"24.10.4", "25.12-SNAPSHOT", true},
		"snapshot device":        {"SNAPSHOT", "25.12.2", true},
		"device family unlisted": {"22.03.7", "23.05.5", true},
		"garbage":                {"x", "25.12.2", true},
	} {
		if got := upgradePathProblem(tc.from, tc.to, branches); (got != "") != tc.problem {
			t.Errorf("%s: problem = %q, want problem=%v", name, got, tc.problem)
		}
	}
}

func TestPickSysupgradeImage(t *testing.T) {
	h := strings.Repeat("a", 64)
	good := asuImage{Name: "x-squashfs-sysupgrade.bin", Type: "sysupgrade", Filesystem: "squashfs", SHA256: h}
	for name, tc := range map[string]struct {
		in   []asuImage
		want string
		fail bool
	}{
		"single": {[]asuImage{{Name: "f.itb", Type: "factory", SHA256: h}, good}, good.Name, false},
		"prefers squashfs": {[]asuImage{
			{Name: "x-ext4-sysupgrade.img.gz", Type: "sysupgrade", Filesystem: "ext4", SHA256: h}, good}, good.Name, false},
		"two ext4":      {[]asuImage{{Name: "a.img", Type: "sysupgrade", Filesystem: "ext4", SHA256: h}, {Name: "b.img", Type: "sysupgrade", Filesystem: "btrfs", SHA256: h}}, "", true},
		"no hash":       {[]asuImage{{Name: "a.bin", Type: "sysupgrade", Filesystem: "squashfs"}}, "", true},
		"bad name":      {[]asuImage{{Name: "../a.bin", Type: "sysupgrade", Filesystem: "squashfs", SHA256: h}}, "", true},
		"no sysupgrade": {[]asuImage{{Name: "a.bin", Type: "factory", SHA256: h}}, "", true},
	} {
		got, err := pickSysupgradeImage(tc.in)
		if (err != nil) != tc.fail || got.Name != tc.want {
			t.Errorf("%s: got %q, %v", name, got.Name, err)
		}
	}
}

func TestUpgradeOperationsAreFixedWriteOperations(t *testing.T) {
	for _, op := range upgradeOperations {
		if op.Class != operationWrite || op.allows("readonly") || !op.allows("operator") || !op.allows("admin") {
			t.Errorf("%s must be a write operation for admins and operators", op.Name)
		}
		if strings.Contains(op.command, "sysupgrade ") && !strings.Contains(op.command, " -T ") {
			t.Errorf("%s must only test the image, never flash it: %s", op.Name, op.command)
		}
	}
	if !strings.Contains(opUpgradeUpload.command, "mkdir -m 700") {
		t.Error("the staging directory must be created fresh with mode 700")
	}
}

// fakeBuildServer answers ASU's build API: a POST that queues, GETs that stay pending
// until pendingPolls have passed, then a finished build with one image.
func fakeBuildServer(t *testing.T, image []byte, pendingPolls int32, imageHash string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/build":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":202,"detail":"queued","request_hash":"abcdef0123456789","queue_position":2}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/build/abcdef0123456789":
			if polls.Add(1) <= pendingPolls {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":202,"detail":"started","request_hash":"abcdef0123456789"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":200,"detail":"done","bin_dir":"abcdef0123456789","images":[{"name":"img-sysupgrade.bin","type":"sysupgrade","filesystem":"squashfs","sha256":"` + imageHash + `"}]}`))
		case r.URL.Path == "/store/abcdef0123456789/img-sysupgrade.bin":
			_, _ = w.Write(image)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old, oldImages, oldPoll := buildServer, imageClient, upgradeBuildPoll
	buildServer = newBuildServerClient()
	buildServer.http.Transport = srv.Client().Transport
	imageClient = &http.Client{Transport: srv.Client().Transport, CheckRedirect: refuseCrossHostRedirect}
	upgradeBuildPoll = time.Millisecond
	t.Cleanup(func() { buildServer, imageClient, upgradeBuildPoll = old, oldImages, oldPoll })
	return srv, &polls
}

func TestBuildAndDownloadAgainstFakeBuildServer(t *testing.T) {
	image := []byte("firmware image bytes")
	sum := sha256.Sum256(image)
	srv, polls := fakeBuildServer(t, image, 2, hex.EncodeToString(sum[:]))
	var steps []string
	res, err := buildFirmwareImage(context.Background(), srv.URL, firmwarePlan{Version: "25.12.2", Target: "x86/64", Profile: "generic", Packages: []string{"luci"}},
		func(s string) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 3 || len(steps) != 3 || !strings.Contains(steps[0], "queue position 2") {
		t.Fatalf("polls=%d steps=%v", polls.Load(), steps)
	}
	img, err := pickSysupgradeImage(res.Images)
	if err != nil {
		t.Fatal(err)
	}
	got, err := downloadFirmwareImage(context.Background(), srv.URL, res.BinDir, img.Name)
	if err != nil || string(got) != string(image) {
		t.Fatalf("download = %q, %v", got, err)
	}
	if _, err := downloadFirmwareImage(context.Background(), srv.URL, "../x", img.Name); err == nil {
		t.Fatal("an unusable build directory must be refused")
	}
	if _, err := downloadFirmwareImage(context.Background(), srv.URL, res.BinDir, "missing.bin"); err == nil {
		t.Fatal("a missing image must fail")
	}
}

func TestBuildFirmwareImageSurfacesRefusalAndTimeout(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Unsupported profile"}`))
	}))
	defer srv.Close()
	old := buildServer
	buildServer = newBuildServerClient()
	buildServer.http.Transport = srv.Client().Transport
	defer func() { buildServer = old }()
	_, err := buildFirmwareImage(context.Background(), srv.URL, firmwarePlan{}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "Unsupported profile") {
		t.Fatalf("err = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildFirmwareImage(ctx, srv.URL, firmwarePlan{}, func(string) {}); err == nil {
		t.Fatal("a cancelled context must stop the build wait")
	}
}

func TestDownloadFirmwareImageEnforcesLimit(t *testing.T) {
	big := make([]byte, upgradeImageLimit+1)
	srv, _ := fakeBuildServer(t, big, 0, strings.Repeat("0", 64))
	if _, err := downloadFirmwareImage(context.Background(), srv.URL, "abcdef0123456789", "img-sysupgrade.bin"); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartFirmwareUpgradeRoles(t *testing.T) {
	dev := "550e8400-e29b-41d4-a716-446655440000"
	for _, role := range []string{"readonly", ""} {
		req := httptest.NewRequest(http.MethodPost, "/api/devices/"+dev+"/firmware-upgrades", strings.NewReader(`{"version":"25.12.2"}`))
		req.SetPathValue("id", dev)
		if role != "" {
			req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: role}))
		}
		rec := httptest.NewRecorder()
		(&app{}).startFirmwareUpgrade(rec, req)
		want := http.StatusForbidden
		if role == "" {
			want = http.StatusUnauthorized
		}
		if rec.Code != want {
			t.Fatalf("role %q: status = %d, want %d", role, rec.Code, want)
		}
	}
}

func TestFirmwareUpgradeJobsInDatabase(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)

	var backupID string
	if err := db.QueryRow(ctx, `INSERT INTO device_backups(id,device_id,digest,size_bytes,file_count,board_name,target,release,hardware_id,hostkey_fingerprint,key_version,ciphertext)
		VALUES(gen_random_uuid(),$1,'d',1,1,'b','t','24.10.4','','fp',1,'\x00') RETURNING id::text`, deviceID).Scan(&backupID); err != nil {
		t.Fatal(err)
	}
	insert := func(status string) (int64, error) {
		var id int64
		err := db.QueryRow(ctx, `INSERT INTO firmware_upgrades(device_id,version,status,created_by) VALUES($1,'25.12.2',$2,$3) RETURNING id`,
			deviceID, status, userID).Scan(&id)
		return id, err
	}

	id, err := insert("preparing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insert("ready"); err == nil {
		t.Fatal("a second open job on the same device must be refused")
	} else if pg, ok := err.(*pgconn.PgError); !ok || pg.Code != "23505" {
		t.Fatalf("err = %v", err)
	}
	var open bool
	if err := db.QueryRow(ctx, openUpgradeSQL, deviceID).Scan(&open); err != nil || !open {
		t.Fatalf("open = %v, %v", open, err)
	}

	// The job pinned the backup; stopping it unpins and audits in one step, once.
	if _, err := db.Exec(ctx, `UPDATE device_backups SET pinned=TRUE WHERE id=$1`, backupID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE firmware_upgrades SET backup_id=$2, backup_pinned_by_job=TRUE WHERE id=$1`, id, backupID); err != nil {
		t.Fatal(err)
	}
	if stopped, dev := a.markUpgradeStopped(ctx, id, "ready", "x", ""); stopped || dev != "" {
		t.Fatal("a job in another status must not be stopped")
	}
	if stopped, dev := a.markUpgradeStopped(ctx, id, "preparing", "cancelled", userID); !stopped || dev != deviceID {
		t.Fatalf("stopped = %v, device = %q", stopped, dev)
	}
	if stopped, _ := a.markUpgradeStopped(ctx, id, "preparing", "cancelled", userID); stopped {
		t.Fatal("a job can only be stopped once")
	}
	var pinned bool
	var status string
	var audits int
	_ = db.QueryRow(ctx, `SELECT pinned FROM device_backups WHERE id=$1`, backupID).Scan(&pinned)
	_ = db.QueryRow(ctx, `SELECT status FROM firmware_upgrades WHERE id=$1`, id).Scan(&status)
	_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE action='device.firmware_upgrade_stopped'`).Scan(&audits)
	if pinned || status != "stopped" || audits != 1 {
		t.Fatalf("pinned=%v status=%q audits=%d", pinned, status, audits)
	}

	// A backup that was already pinned before the job is left pinned.
	id, err = insert("ready")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(ctx, `UPDATE device_backups SET pinned=TRUE WHERE id=$1`, backupID)
	_, _ = db.Exec(ctx, `UPDATE firmware_upgrades SET backup_id=$2, backup_pinned_by_job=FALSE WHERE id=$1`, id, backupID)
	if stopped, _ := a.markUpgradeStopped(ctx, id, "ready", "expired", ""); !stopped {
		t.Fatal("ready job must stop")
	}
	_ = db.QueryRow(ctx, `SELECT pinned FROM device_backups WHERE id=$1`, backupID).Scan(&pinned)
	if !pinned {
		t.Fatal("a backup pinned by someone else must stay pinned")
	}
	if err := db.QueryRow(ctx, openUpgradeSQL, deviceID).Scan(&open); err != nil || open {
		t.Fatalf("after stopping, open = %v, %v", open, err)
	}
}

func TestDeviceClaimsAreSerialisedByTheDeviceLock(t *testing.T) {
	db := newEnrollmentTestDB(t)
	ctx := context.Background()
	deviceID, _ := seedDeviceAndUser(t, db)
	first, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	if _, err := first.Exec(ctx, lockDeviceSQL, deviceID); err != nil {
		t.Fatal(err)
	}
	second, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(ctx)
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := second.Exec(short, lockDeviceSQL, deviceID); err == nil {
		t.Fatal("a second claim must wait while the first holds the device lock")
	}
}

func TestRecoverFirmwareUpgradesOnlyEndsJobsFromBeforeStartup(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	var id int64
	if err := db.QueryRow(ctx, `INSERT INTO firmware_upgrades(device_id,version,status,created_by) VALUES($1,'25.12.2','preparing',$2) RETURNING id`,
		deviceID, userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverFirmwareUpgrades(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = db.QueryRow(ctx, `SELECT status FROM firmware_upgrades WHERE id=$1`, id).Scan(&status)
	if status != "preparing" {
		t.Fatalf("a job started after startup must not be touched, status = %q", status)
	}
	if err := a.recoverFirmwareUpgrades(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(ctx, `SELECT status FROM firmware_upgrades WHERE id=$1`, id).Scan(&status)
	if status != "stopped" {
		t.Fatalf("a job from before startup must be ended, status = %q", status)
	}
}
