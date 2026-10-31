package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	packageRemoveBodyLimit = 512
	packageRemoveListLimit = 20
	packageRemoveTimeout   = 5 * time.Minute
)

var protectedPackageNames = map[string]struct{}{
	"apk": {}, "opkg": {}, "dropbear": {}, "openssh-server": {},
	"wpad": {}, "wpad-basic-mbedtls": {}, "wpad-basic-wolfssl": {}, "wpad-openssl": {},
	"hostapd": {}, "hostapd-common": {}, "firewall": {}, "firewall4": {},
	"nftables": {}, "iptables": {}, "netifd": {}, "procd": {}, "busybox": {},
	"base-files": {}, "libc": {}, "musl": {}, "ubus": {}, "uci": {},
	"dnsmasq": {}, "odhcpd": {}, "odhcpd-ipv6only": {},
}

func validPackageName(name string) bool {
	return packageNamePattern.MatchString(name)
}

func protectedPackage(name string) bool {
	if _, ok := protectedPackageNames[name]; ok {
		return true
	}
	for _, prefix := range []string{"kernel", "kmod-", "wpad", "hostapd", "firewall", "nftables", "iptables", "openssh", "dropbear", "netifd", "procd", "busybox", "base-files", "libc", "musl", "ubus", "uci", "dnsmasq", "odhcpd", "uhttpd", "rpcd", "luci-base", "openmanage"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// packageRemoveOperation constructs one of two fixed commands only after the package
// name has passed the strict grammar above. No command text or flags come from the API.
func packageRemoveOperation(manager, name string) (operation, bool) {
	if !validPackageName(name) {
		return operation{}, false
	}
	switch manager {
	case "opkg":
		return packageOperation("package_remove_opkg", "Remove package",
			"opkg remove "+name+" </dev/null >/dev/null 2>&1", packageRemoveTimeout), true
	case "apk":
		return packageOperation("package_remove_apk", "Remove package",
			"apk del --no-network "+name+" </dev/null >/dev/null 2>&1", packageRemoveTimeout), true
	default:
		return operation{}, false
	}
}

type packageRemoval struct {
	ID         int64          `json:"id"`
	DeviceID   string         `json:"device_id"`
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	Step       string         `json:"step"`
	Detail     string         `json:"detail"`
	Checks     []upgradeCheck `json:"checks"`
	CreatedAt  time.Time      `json:"created_at"`
	FinishedAt *time.Time     `json:"finished_at"`
}

const packageRemovalColumns = `id, device_id::text, name, status, step, detail, checks, created_at, finished_at`

func scanPackageRemoval(row pgx.Row, p *packageRemoval) error {
	var checks []byte
	if err := row.Scan(&p.ID, &p.DeviceID, &p.Name, &p.Status, &p.Step, &p.Detail, &checks, &p.CreatedAt, &p.FinishedAt); err != nil {
		return err
	}
	p.Checks = []upgradeCheck{}
	return json.Unmarshal(checks, &p.Checks)
}

type packageRemovalRequest struct {
	PackageName        string `json:"package_name"`
	ConfirmPackageName string `json:"confirm_package_name"`
	ConfirmName        string `json:"confirm_name"`
	ApplyNow           bool   `json:"apply_now"`
	AcknowledgeScripts bool   `json:"acknowledge_scripts"`
}

func (a *app) removeDevicePackage(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in packageRemovalRequest
	if err := decodeJSONBody(w, r, packageRemoveBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid removal request")
		return
	}
	if in.PackageName != strings.TrimSpace(in.PackageName) || !validPackageName(in.PackageName) || in.ConfirmPackageName != in.PackageName {
		writeError(w, http.StatusBadRequest, "enter a valid package name and repeat it exactly to confirm removal")
		return
	}
	if !in.ApplyNow {
		writeError(w, http.StatusBadRequest, "explicitly set apply_now to true to authorize immediate removal")
		return
	}
	if !in.AcknowledgeScripts {
		writeError(w, http.StatusBadRequest, "acknowledge that package removal scripts may run as root on the device")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start package removal")
		return
	}
	defer tx.Rollback(r.Context())
	var deviceName string
	err = tx.QueryRow(r.Context(), "SELECT name FROM devices WHERE id=$1 FOR NO KEY UPDATE", deviceID).Scan(&deviceName)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start package removal")
		return
	}
	if in.ConfirmName != deviceName {
		writeError(w, http.StatusBadRequest, "type the device name exactly to confirm package removal")
		return
	}
	busy := ""
	err = tx.QueryRow(r.Context(), `SELECT CASE
		WHEN EXISTS(SELECT 1 FROM device_restores WHERE device_id=$1 AND status='running') THEN 'a backup restore is running on this device'
		WHEN EXISTS(SELECT 1 FROM config_changes WHERE device_id=$1 AND status='applying') THEN 'a configuration change is being applied to this device'
		WHEN EXISTS(SELECT 1 FROM firmware_upgrades WHERE device_id=$1 AND status IN ('preparing','ready','flashing','verifying')) THEN 'a firmware upgrade is open on this device; cancel or finish it first'
		WHEN `+packageInstallRunningSQL+` THEN 'a package operation is already running on this device'
		ELSE '' END`, deviceID).Scan(&busy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start package removal")
		return
	}
	if busy != "" {
		writeError(w, http.StatusConflict, busy)
		return
	}
	var job packageRemoval
	err = scanPackageRemoval(tx.QueryRow(r.Context(), `INSERT INTO package_removals (device_id, name, step, created_by)
		VALUES ($1,$2,'checking the device',$3) RETURNING `+packageRemovalColumns, deviceID, in.PackageName, user.ID), &job)
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "device.package_remove", "device", deviceID,
			map[string]any{"removal_id": job.ID, "package_name": in.PackageName, "apply_now": true})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start package removal and record audit event")
		return
	}
	go a.runPackageRemoval(job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

func (a *app) listPackageRemovals(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT `+packageRemovalColumns+` FROM package_removals
		WHERE device_id=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, deviceID, packageRemoveListLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list package removals")
		return
	}
	defer rows.Close()
	out := []packageRemoval{}
	for rows.Next() {
		var p packageRemoval
		if err := scanPackageRemoval(rows, &p); err != nil {
			writeError(w, http.StatusInternalServerError, "could not list package removals")
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not list package removals")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func packageRemovePreflight(name string, m firmwareManifest) []upgradeCheck {
	checks := []upgradeCheck{}
	add := func(label, status, detail string) {
		checks = append(checks, upgradeCheck{Name: label, Status: status, Detail: detail})
	}
	if !validPackageName(name) {
		add("package name", "failed", "the package name is invalid")
		return checks
	}
	if protectedPackage(name) {
		add("protected package", "failed", "this package is protected because removing it could break device access, networking, security or package management")
		return checks
	}
	if m.PackageManager != "apk" && m.PackageManager != "opkg" {
		add("package manager", "failed", "the device package manager is not supported")
		return checks
	}
	installed := false
	for _, p := range m.Packages {
		if p.Name == name {
			installed = true
			break
		}
	}
	if !installed {
		add("installed package", "failed", "the package is not installed on the device")
		return checks
	}
	userInstalled := false
	for _, p := range m.UserInstalled {
		if p == name {
			userInstalled = true
			break
		}
	}
	if !userInstalled {
		add("user-installed package", "failed", "only packages marked as user-installed may be removed; firmware defaults and dependency packages are protected")
		return checks
	}
	add("package removal", "ok", name+" is installed and marked user-installed")
	return checks
}

func (a *app) setPackageRemovalStep(ctx context.Context, id int64, step string) {
	if _, err := a.db.Exec(ctx, "UPDATE package_removals SET step=$2 WHERE id=$1 AND status='running'", id, step); err != nil {
		log.Printf("package removal %d: could not record step", id)
	}
}

func (a *app) finishPackageRemoval(id int64, status, detail string, checks []upgradeCheck) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if checks == nil {
		checks = []upgradeCheck{}
	}
	encoded, _ := json.Marshal(checks)
	if _, err := a.db.Exec(ctx, `UPDATE package_removals SET status=$2, step='', detail=$3, checks=$4, finished_at=NOW()
		WHERE id=$1 AND status='running'`, id, status, cleanPackageText(detail, packageDetailLimit), encoded); err != nil {
		log.Printf("package removal %d: could not record the result", id)
	}
}

func (a *app) runPackageRemoval(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), packageJobTimeout)
	defer cancel()
	fail := func(detail string, checks []upgradeCheck) { a.finishPackageRemoval(id, "failed", detail, checks) }
	var deviceID, name string
	if err := a.db.QueryRow(ctx, "SELECT device_id::text, name FROM package_removals WHERE id=$1", id).Scan(&deviceID, &name); err != nil {
		fail("could not load the package removal", nil)
		return
	}
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		_, msg := targetError(err)
		fail(msg, nil)
		return
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		fail("could not open a verified SSH session to the device", nil)
		return
	}
	defer session.Close()
	before := session.run(firmwareInventoryOperation)
	manifest, perr := parseFirmwareInventory(before.Output)
	if before.Status != "ok" || perr != nil {
		fail("could not read the device's package inventory", nil)
		return
	}
	checks := packageRemovePreflight(name, manifest)
	if checksFailed(checks) {
		fail("not removed: a package removal check failed", checks)
		return
	}
	op, ok := packageRemoveOperation(manifest.PackageManager, name)
	if !ok {
		fail("the package manager or package name is not supported", checks)
		return
	}
	a.setPackageRemovalStep(ctx, id, "removing "+name)
	res := session.runWriteRaw(op, nil)
	if res.Status != "ok" {
		fail("the package removal command failed; package-manager output was discarded", append(checks, upgradeCheck{Name: "remove", Status: "failed", Detail: res.Error}))
		return
	}
	checks = append(checks, upgradeCheck{Name: "remove", Status: "ok", Detail: "the package manager reported success"})
	a.setPackageRemovalStep(ctx, id, "verifying the result")
	after := session.run(firmwareInventoryOperation)
	m2, err := parseFirmwareInventory(after.Output)
	present := false
	if after.Status == "ok" && err == nil {
		for _, p := range m2.Packages {
			if p.Name == name {
				present = true
				break
			}
		}
	}
	if present || after.Status != "ok" || err != nil {
		checks = append(checks, upgradeCheck{Name: "removed package", Status: "failed", Detail: "the device inventory did not verify that "+name+" is absent"})
		fail("the removal command returned, but the package's absence could not be verified; check the device", checks)
		return
	}
	checks = append(checks, upgradeCheck{Name: "removed package", Status: "ok", Detail: name+" is absent from the device inventory"})
	a.finishPackageRemoval(id, "done", "removed", checks)
}

func (a *app) recoverPackageRemovals(ctx context.Context, startedAt time.Time) error {
	_, err := a.db.Exec(ctx, `UPDATE package_removals SET status='failed', step='',
		detail='the server restarted during this removal; check whether the package is still installed on the device',
		finished_at=NOW() WHERE status='running' AND created_at < $1`, startedAt)
	return err
}

func (a *app) recoverPackageRemovalsAtStartup(ctx context.Context) {
	startedAt := time.Now()
	for attempt := 0; attempt < 20; attempt++ {
		if err := a.recoverPackageRemovals(ctx, startedAt); err == nil {
			return
		}
		log.Printf("package removals: could not look for interrupted removals; retrying")
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}
