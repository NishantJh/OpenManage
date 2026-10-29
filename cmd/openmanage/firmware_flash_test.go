package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpgradeOutcome(t *testing.T) {
	ok, warn, bad := upgradeCheck{"a", "ok", ""}, upgradeCheck{"b", "warning", ""}, upgradeCheck{"c", "failed", ""}
	for name, tc := range map[string]struct {
		checks []upgradeCheck
		want   string
	}{
		"all ok":        {[]upgradeCheck{ok, ok}, "done"},
		"one warning":   {[]upgradeCheck{ok, warn}, "done_with_warnings"},
		"failure wins":  {[]upgradeCheck{warn, bad, ok}, "needs_attention"},
		"failure first": {[]upgradeCheck{bad, warn}, "needs_attention"},
		"no checks yet": {nil, "done"},
	} {
		if got := upgradeOutcome(tc.checks); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func TestMissingPackages(t *testing.T) {
	plan := firmwarePlan{Packages: []string{"-wpad-basic-mbedtls", "luci-theme-fluent", "libubus", "tcpdump"}}
	exp := expectedPackages(plan)
	if len(exp) != 3 || exp[0] != "luci-theme-fluent" {
		t.Fatalf("expected = %v", exp)
	}
	installed := []firmwarePackage{{Name: "luci-theme-fluent"}, {Name: "libubus20250102"}}
	got := missingPackages(exp, installed)
	if len(got) != 1 || got[0] != "tcpdump" {
		t.Fatalf("missing = %v (an ABI-suffixed library must satisfy its plain name)", got)
	}
}

func TestRestartVerdict(t *testing.T) {
	long := flashDownWait + time.Second
	for name, tc := range map[string]struct {
		now, before     string
		reachable, down bool
		since           time.Duration
		want            string
	}{
		"unreachable":                {"", "b1", false, true, time.Minute, "waiting"},
		"new boot id":                {"b2", "b1", true, false, time.Second, "restarted"},
		"same boot soon":             {"b1", "b1", true, false, time.Minute, "waiting"},
		"same boot long after":       {"b1", "b1", true, false, long, "never_restarted"},
		"same boot after a blip":     {"b1", "b1", true, true, long, "never_restarted"},
		"unknown boot, seen down":    {"", "", true, true, time.Minute, "restarted"},
		"unknown boot, never down":   {"", "", true, false, long, "never_restarted"},
		"unknown before, boot given": {"b2", "", true, true, time.Minute, "restarted"},
	} {
		if got := restartVerdict(tc.now, tc.before, tc.reachable, tc.down, tc.since); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func TestFlashOperationIsFixedAndAdminOnly(t *testing.T) {
	op := opUpgradeFlash
	if op.Class != operationWrite || op.allows("operator") || op.allows("readonly") || !op.allows("admin") {
		t.Fatal("flashing is a write operation for admins only")
	}
	for _, bad := range []string{" -n", " -F", " -f", " -R", "$"} {
		// "$want" is the one allowed expansion: the hash arrives on stdin.
		if bad == "$" {
			if strings.Count(op.command, "$") != 1 || !strings.Contains(op.command, `"$want  `) {
				t.Errorf("only $want may be expanded: %s", op.command)
			}
			continue
		}
		if strings.Contains(op.command, bad) {
			t.Errorf("the flash command must never carry %q: %s", bad, op.command)
		}
	}
	if !strings.Contains(op.command, "sha256sum -c -s") || !strings.Contains(op.command, "read -r want") {
		t.Errorf("the flash command must re-check the hash from stdin before flashing: %s", op.command)
	}
	// Preparation operations may only test an image, never flash it.
	for _, prep := range upgradeOperations {
		if strings.Contains(prep.command, "sysupgrade") && !strings.Contains(prep.command, " -T ") {
			t.Errorf("%s must not flash: %s", prep.Name, prep.command)
		}
	}
}

func TestFlashEndpointRefusesNonAdmins(t *testing.T) {
	dev := "550e8400-e29b-41d4-a716-446655440000"
	for _, role := range []string{"operator", "readonly"} {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"confirm_name":"x"}`))
		req.SetPathValue("id", dev)
		req.SetPathValue("job", "1")
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: role}))
		rec := httptest.NewRecorder()
		(&app{}).flashFirmwareUpgrade(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", role, rec.Code)
		}
	}
}

func TestFlashClaimChecksAndAcknowledge(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	var backupID string
	if err := db.QueryRow(ctx, `INSERT INTO device_backups(id,device_id,digest,size_bytes,file_count,board_name,target,release,hardware_id,hostkey_fingerprint,key_version,ciphertext,pinned)
		VALUES(gen_random_uuid(),$1,'d',1,1,'b','t','24.10.4','','fp',1,'\x00',TRUE) RETURNING id::text`, deviceID).Scan(&backupID); err != nil {
		t.Fatal(err)
	}
	var id int64
	plan := `{"warnings":["This moves to a new release branch."],"packages":[]}`
	if err := db.QueryRow(ctx, `INSERT INTO firmware_upgrades(device_id,version,status,plan,image_sha256,backup_id,backup_pinned_by_job,created_by,ready_at,expires_at)
		VALUES($1,'25.12.2','ready',$2,$3,$4,TRUE,$5,NOW(),NOW()+INTERVAL '10 minutes') RETURNING id`,
		deviceID, plan, strings.Repeat("a", 64), backupID, userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	flash := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		req.SetPathValue("id", deviceID)
		req.SetPathValue("job", fmt.Sprint(id))
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
		rec := httptest.NewRecorder()
		a.flashFirmwareUpgrade(rec, req)
		return rec.Code
	}
	status := func() (s, detail string) {
		_ = db.QueryRow(ctx, `SELECT status, detail FROM firmware_upgrades WHERE id=$1`, id).Scan(&s, &detail)
		return
	}
	if c := flash(`{"confirm_name":"wrong-name","acknowledge_warnings":true}`); c != http.StatusBadRequest {
		t.Fatalf("wrong device name: %d", c)
	}
	if c := flash(`{"confirm_name":"test-router"}`); c != http.StatusBadRequest {
		t.Fatalf("unacknowledged warnings: %d", c)
	}
	if s, _ := status(); s != "ready" {
		t.Fatalf("a refused flash must leave the job ready, status = %q", s)
	}
	_, _ = db.Exec(ctx, `UPDATE firmware_upgrades SET expires_at=NOW()-INTERVAL '1 minute' WHERE id=$1`, id)
	if c := flash(`{"confirm_name":"test-router","acknowledge_warnings":true}`); c != http.StatusConflict {
		t.Fatalf("expired image: %d", c)
	}
	_, _ = db.Exec(ctx, `UPDATE firmware_upgrades SET expires_at=NOW()+INTERVAL '10 minutes' WHERE id=$1`, id)

	// The claim succeeds; the device has no credentials here, so the job ends as stopped
	// before any command is sent, and releases the backup it pinned.
	if c := flash(`{"confirm_name":"test-router","acknowledge_warnings":true}`); c != http.StatusAccepted {
		t.Fatalf("valid flash: %d", c)
	}
	if c := flash(`{"confirm_name":"test-router","acknowledge_warnings":true}`); c != http.StatusConflict {
		t.Fatalf("a second flash of the same job: %d", c)
	}
	var s, detail string
	for range 40 {
		if s, detail = status(); s == "stopped" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s != "stopped" || !strings.HasPrefix(detail, "not flashed:") {
		t.Fatalf("status = %q, detail = %q", s, detail)
	}
	var pinned bool
	_ = db.QueryRow(ctx, `SELECT pinned FROM device_backups WHERE id=$1`, backupID).Scan(&pinned)
	if pinned {
		t.Fatal("a job that never flashed must release its backup")
	}

	// Acknowledging a finished result releases the backup the job pinned, once.
	_, _ = db.Exec(ctx, `UPDATE device_backups SET pinned=TRUE WHERE id=$1`, backupID)
	_, _ = db.Exec(ctx, `UPDATE firmware_upgrades SET status='needs_attention' WHERE id=$1`, id)
	ack := func() int {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.SetPathValue("id", deviceID)
		req.SetPathValue("job", fmt.Sprint(id))
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "operator"}))
		rec := httptest.NewRecorder()
		a.acknowledgeFirmwareUpgrade(rec, req)
		return rec.Code
	}
	if c := ack(); c != http.StatusOK {
		t.Fatalf("acknowledge: %d", c)
	}
	_ = db.QueryRow(ctx, `SELECT pinned FROM device_backups WHERE id=$1`, backupID).Scan(&pinned)
	if pinned {
		t.Fatal("acknowledging must release the pinned backup")
	}
	if c := ack(); c != http.StatusConflict {
		t.Fatalf("a second acknowledge: %d", c)
	}
}
