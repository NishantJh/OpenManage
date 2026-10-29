package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Installing a confirmed package source on a device (docs/firmware-upgrades.md,
// "Installing a package source"). An administrator asks for it from the app; nothing runs
// on its own. The safety rules:
//   - only a confirmed source can be installed, and only the stored copy is used: its
//     SHA-256 is checked on the device before the install command runs;
//   - the administrator types the device name and acknowledges that the package's install
//     scripts run as root on the device;
//   - every device command is fixed text. The file goes in on stdin to a fresh mode-700
//     directory, and --allow-untrusted (apk) applies only to that one file;
//   - the device is checked first: package manager matches the file, architecture,
//     free flash and memory, dependencies (ipk), and not already installed;
//   - one install per device, and none while an upgrade, restore or configuration change
//     is open on it.

const (
	packageStageDir      = "/tmp/.om-pkg"
	packageInstallTTL    = 5 * time.Minute
	packageJobTimeout    = 10 * time.Minute
	packageUploadTimeout = 5 * time.Minute
	packageBodyLimit     = 512
	packageDetailLimit   = 600
	packageListLimit     = 20
	packageRAMMargin     = 16 << 20
	packageFlashMarginKB = 1024
)

func packageStagePath(format string) string { return packageStageDir + "/pkg." + format }

func packageOperation(name, label, command string, timeout time.Duration) operation {
	op := upgradeOperation(name, label, command, timeout)
	op.Roles = []string{"admin"}
	return op
}

// installFormats maps a package format to its device-side manager and fixed commands.
type installFormat struct {
	manager string
	stage   operation
	hash    operation
	install operation
}

var installFormats = map[string]installFormat{
	"ipk": {
		manager: "opkg",
		stage: packageOperation("package_stage_ipk", "Stage package file",
			"umask 077; mkdir -m 700 "+packageStageDir+" && cat > "+packageStagePath("ipk"), packageUploadTimeout),
		hash: packageOperation("package_hash_ipk", "Check staged package file",
			"sha256sum "+packageStagePath("ipk"), healthCheckTimeout),
		install: packageOperation("package_install_ipk", "Install package",
			"opkg install "+packageStagePath("ipk")+" </dev/null 2>&1", packageInstallTTL),
	},
	"apk": {
		manager: "apk",
		stage: packageOperation("package_stage_apk", "Stage package file",
			"umask 077; mkdir -m 700 "+packageStageDir+" && cat > "+packageStagePath("apk"), packageUploadTimeout),
		hash: packageOperation("package_hash_apk", "Check staged package file",
			"sha256sum "+packageStagePath("apk"), healthCheckTimeout),
		// --allow-untrusted is needed because the package is not signed by a key the device
		// knows. apk applies it to the whole transaction, so --no-network keeps that
		// transaction to this one stored, hash-checked file and packages already installed:
		// nothing is fetched from a feed while signatures are not checked.
		install: packageOperation("package_install_apk", "Install package",
			"apk add --no-network --allow-untrusted "+packageStagePath("apk")+" </dev/null 2>&1", packageInstallTTL),
	},
}

var opPackageCleanup = packageOperation("package_cleanup", "Remove staged package file", "rm -rf "+packageStageDir, healthCheckTimeout)

var packageInstallOperations = []operation{
	installFormats["ipk"].stage, installFormats["ipk"].hash, installFormats["ipk"].install,
	installFormats["apk"].stage, installFormats["apk"].hash, installFormats["apk"].install,
	opPackageCleanup,
}

// packageInstallRunningSQL is true while a package install is running on the device. Other
// long jobs (upgrade, restore, configuration change) refuse to start while it is.
const packageInstallRunningSQL = `EXISTS(SELECT 1 FROM package_installs WHERE device_id=$1 AND status='running')`

const packageInstallBusyMessage = "a package is being installed on this device; wait for it to finish"

type packageInstall struct {
	ID         int64          `json:"id"`
	DeviceID   string         `json:"device_id"`
	SourceID   *int64         `json:"source_id"`
	Name       string         `json:"name"`
	Version    string         `json:"version"`
	SHA256     string         `json:"sha256"`
	Status     string         `json:"status"`
	Step       string         `json:"step"`
	Detail     string         `json:"detail"`
	Checks     []upgradeCheck `json:"checks"`
	CreatedAt  time.Time      `json:"created_at"`
	FinishedAt *time.Time     `json:"finished_at"`
}

const packageInstallColumns = `id, device_id::text, source_id, name, version, sha256, status, step, detail, checks, created_at, finished_at`

func scanPackageInstall(row pgx.Row, p *packageInstall) error {
	var checks []byte
	if err := row.Scan(&p.ID, &p.DeviceID, &p.SourceID, &p.Name, &p.Version, &p.SHA256, &p.Status, &p.Step, &p.Detail, &checks, &p.CreatedAt, &p.FinishedAt); err != nil {
		return err
	}
	p.Checks = []upgradeCheck{}
	return json.Unmarshal(checks, &p.Checks)
}

type packageInstallRequest struct {
	SourceID           int64  `json:"source_id"`
	ConfirmName        string `json:"confirm_name"`
	AcknowledgeScripts bool   `json:"acknowledge_scripts"`
}

// installPackageSource claims the device and starts the install. It returns at once.
func (a *app) installPackageSource(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, packageInstallOperations) {
		return
	}
	var in packageInstallRequest
	if err := decodeJSONBody(w, r, packageBodyLimit, &in); err != nil || in.SourceID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid install request")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)

	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the install")
		return
	}
	defer tx.Rollback(r.Context())
	var deviceName string
	err = tx.QueryRow(r.Context(), `SELECT name FROM devices WHERE id=$1 FOR NO KEY UPDATE`, deviceID).Scan(&deviceName)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the install")
		return
	}
	if in.ConfirmName != deviceName {
		writeError(w, http.StatusBadRequest, "type the device name exactly to confirm the install")
		return
	}
	if !in.AcknowledgeScripts {
		writeError(w, http.StatusBadRequest, "acknowledge that the package's install scripts run as root on the device")
		return
	}
	var name, version, hash string
	err = tx.QueryRow(r.Context(), `SELECT name, version, sha256 FROM package_sources WHERE id=$1 AND status='confirmed'`, in.SourceID).Scan(&name, &version, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "package source not found or not confirmed")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the install")
		return
	}
	var busy string
	err = tx.QueryRow(r.Context(), `SELECT CASE
		WHEN EXISTS(SELECT 1 FROM device_restores WHERE device_id=$1 AND status='running') THEN 'a backup restore is running on this device'
		WHEN EXISTS(SELECT 1 FROM config_changes WHERE device_id=$1 AND status='applying') THEN 'a configuration change is being applied to this device'
		WHEN EXISTS(SELECT 1 FROM firmware_upgrades WHERE device_id=$1 AND status IN ('preparing','ready','flashing','verifying')) THEN 'a firmware upgrade is open on this device; cancel or finish it first'
		WHEN `+packageInstallRunningSQL+` THEN 'a package is already being installed on this device'
		ELSE '' END`, deviceID).Scan(&busy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the install")
		return
	}
	if busy != "" {
		writeError(w, http.StatusConflict, busy)
		return
	}
	var job packageInstall
	err = scanPackageInstall(tx.QueryRow(r.Context(), `INSERT INTO package_installs (device_id, source_id, name, version, sha256, step, created_by)
		VALUES ($1,$2,$3,$4,$5,'checking the device',$6) RETURNING `+packageInstallColumns, deviceID, in.SourceID, name, version, hash, user.ID), &job)
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "device.package_install", "device", deviceID,
			map[string]any{"install_id": job.ID, "source_id": in.SourceID, "name": name, "version": version, "sha256": hash})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the install")
		return
	}
	go a.runPackageInstall(job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

func (a *app) listPackageInstalls(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT `+packageInstallColumns+` FROM package_installs WHERE device_id=$1
		ORDER BY created_at DESC, id DESC LIMIT $2`, deviceID, packageListLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list package installs")
		return
	}
	defer rows.Close()
	out := []packageInstall{}
	for rows.Next() {
		var p packageInstall
		if err := scanPackageInstall(rows, &p); err != nil {
			writeError(w, http.StatusInternalServerError, "could not list package installs")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// packageDependencyName is the package name at the start of an ipk dependency such as
// "libubox (>= 2020)".
func packageDependencyName(dep string) string {
	if i := strings.IndexAny(dep, " ("); i > 0 {
		dep = dep[:i]
	}
	return dep
}

// packagePreflight returns the checks that decide whether a source may be installed on a
// device with this inventory. The install proceeds only if none failed.
func packagePreflight(info packageInfo, size int64, m firmwareManifest) []upgradeCheck {
	var checks []upgradeCheck
	add := func(name, status, detail string) {
		checks = append(checks, upgradeCheck{Name: name, Status: status, Detail: detail})
	}
	format, known := installFormats[info.Format]
	switch {
	case !known:
		add("package format", "failed", "the package format is not supported")
	case format.manager != m.PackageManager:
		add("package format", "failed", fmt.Sprintf("this is an .%s file but the device uses %s", info.Format, m.PackageManager))
	default:
		add("package format", "ok", fmt.Sprintf(".%s matches the device's %s", info.Format, m.PackageManager))
	}
	switch {
	case info.Arch == "all" || info.Arch == "noarch" || info.Arch == m.Arch:
		add("architecture", "ok", info.Arch+" fits "+m.Arch)
	default:
		add("architecture", "failed", fmt.Sprintf("the package is for %s but the device is %s", info.Arch, m.Arch))
	}
	// The package's own installed-size claim can be missing or too low, so for an .ipk the
	// data archive was measured when it was registered and the larger number is used.
	need := info.InstalledSize
	for _, n := range []int64{size, info.UnpackedSize} {
		if n > need {
			need = n
		}
	}
	needKB := need*2/1024 + packageFlashMarginKB
	switch {
	case info.Format == "ipk" && info.UnpackedSize <= 0:
		add("free flash", "failed", "the size of the unpacked files could not be measured; register the file again")
	case m.OverlayFreeKB <= 0:
		add("free flash", "failed", "could not read the device's free flash space")
	case m.OverlayFreeKB < needKB:
		add("free flash", "failed", fmt.Sprintf("needs about %d KB free, the device has %d KB", needKB, m.OverlayFreeKB))
	case info.Format == "apk":
		// An .apk's unpacked size is only what the package claims.
		add("free flash", "warning", fmt.Sprintf("%d KB free, about %d KB needed by the package's own size claim", m.OverlayFreeKB, needKB))
	default:
		add("free flash", "ok", fmt.Sprintf("%d KB free, about %d KB needed", m.OverlayFreeKB, needKB))
	}
	if m.MemAvailableKB*1024 < size+packageRAMMargin {
		add("free memory", "failed", "not enough free memory to stage the file")
	} else {
		add("free memory", "ok", fmt.Sprintf("%d KB available", m.MemAvailableKB))
	}
	installed := map[string]string{}
	for _, p := range m.Packages {
		installed[p.Name] = p.Version
	}
	userInstalled := map[string]bool{}
	for _, n := range m.UserInstalled {
		userInstalled[n] = true
	}
	v, ok := installed[info.Name]
	switch {
	case ok && v == info.Version:
		add("already installed", "failed", fmt.Sprintf("%s %s is already installed", info.Name, v))
	case ok && !userInstalled[info.Name]:
		// Replacing a package that came with the firmware (dropbear, netifd, ...) with a
		// third-party file could cut the device off; that is not what a package source is for.
		add("already installed", "failed", fmt.Sprintf("%s came with the firmware or as a dependency; a package source does not replace it", info.Name))
	case ok:
		add("already installed", "ok", fmt.Sprintf("version %s is installed; this replaces it", v))
	default:
		add("already installed", "ok", "not installed")
	}
	// apk dependencies are shared-library names the package manager resolves itself.
	if info.Format == "ipk" {
		var missing []string
		for _, dep := range info.Depends {
			if n := packageDependencyName(dep); n != "" {
				if _, ok := installed[n]; !ok {
					missing = append(missing, n)
				}
			}
		}
		if len(missing) > 0 {
			add("dependencies", "failed", "not installed on the device: "+strings.Join(missing, ", "))
		} else {
			add("dependencies", "ok", "all dependencies are installed")
		}
	}
	return checks
}

func checksFailed(checks []upgradeCheck) bool {
	for _, c := range checks {
		if c.Status == "failed" {
			return true
		}
	}
	return false
}

func (a *app) setPackageInstallStep(ctx context.Context, id int64, step string) {
	if _, err := a.db.Exec(ctx, `UPDATE package_installs SET step=$2 WHERE id=$1 AND status='running'`, id, step); err != nil {
		log.Printf("package install %d: could not record step", id)
	}
}

func (a *app) finishPackageInstall(id int64, status, detail string, checks []upgradeCheck) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if checks == nil {
		checks = []upgradeCheck{}
	}
	encoded, _ := json.Marshal(checks)
	if _, err := a.db.Exec(ctx, `UPDATE package_installs SET status=$2, step='', detail=$3, checks=$4, finished_at=NOW()
		WHERE id=$1 AND status='running'`, id, status, cleanPackageText(detail, packageDetailLimit), encoded); err != nil {
		log.Printf("package install %d: could not record the result", id)
	}
}

func (a *app) runPackageInstall(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), packageJobTimeout)
	defer cancel()
	fail := func(detail string, checks []upgradeCheck) { a.finishPackageInstall(id, "failed", detail, checks) }

	var deviceID string
	var sourceID *int64
	var wantHash string
	err := a.db.QueryRow(ctx, `SELECT device_id::text, source_id, sha256 FROM package_installs WHERE id=$1`, id).Scan(&deviceID, &sourceID, &wantHash)
	// The hash is compared with the device's output by prefix, so it must be a full one.
	if err != nil || sourceID == nil || !imageHashPattern.MatchString(wantHash) {
		fail("could not load the install", nil)
		return
	}
	var info packageInfo
	var infoJSON, file []byte
	err = a.db.QueryRow(ctx, `SELECT info, file FROM package_sources WHERE id=$1 AND status='confirmed' AND sha256=$2`, *sourceID, wantHash).Scan(&infoJSON, &file)
	if err != nil || json.Unmarshal(infoJSON, &info) != nil {
		fail("the package source was removed or changed; nothing was installed", nil)
		return
	}
	format, ok := installFormats[info.Format]
	if !ok {
		fail("unsupported package format", nil)
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

	inv := session.run(firmwareInventoryOperation)
	manifest, perr := parseFirmwareInventory(inv.Output)
	if inv.Status != "ok" || perr != nil {
		fail("could not read the device's packages and free space", nil)
		return
	}
	checks := packagePreflight(info, int64(len(file)), manifest)
	if checksFailed(checks) {
		fail("not installed: a check failed", checks)
		return
	}

	// A leftover directory from an interrupted run would make the stage command fail.
	session.runWriteRaw(opPackageCleanup, nil)
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if s, err := openDeviceSession(cctx, target, healthCheckTimeout); err == nil {
			s.runWriteRaw(opPackageCleanup, nil)
			s.Close()
		}
	}()

	a.setPackageInstallStep(ctx, id, "sending the package file")
	if res := session.runWriteRaw(format.stage, file); res.Status != "ok" {
		fail("could not stage the package file on the device: "+res.Error, checks)
		return
	}
	res := session.runWriteRaw(format.hash, nil)
	if res.Status != "ok" || !strings.HasPrefix(res.Output, wantHash) {
		fail("the staged file did not match its hash; nothing was installed", checks)
		return
	}
	checks = append(checks, upgradeCheck{Name: "file hash on the device", Status: "ok", Detail: wantHash})

	a.setPackageInstallStep(ctx, id, "installing")
	res = session.runWriteRaw(format.install, nil)
	if res.Status != "ok" {
		// The package manager's output is not stored: a root-running install script can
		// print anything, including secrets, and operators can read this record.
		fail("the install command failed; nothing is stored from its output", append(checks, upgradeCheck{Name: "install", Status: "failed", Detail: res.Error}))
		return
	}
	checks = append(checks, upgradeCheck{Name: "install", Status: "ok", Detail: "the package manager reported success"})

	// Confirm from the device, not from the command's exit status.
	a.setPackageInstallStep(ctx, id, "checking the result")
	after := session.run(firmwareInventoryOperation)
	m2, perr := parseFirmwareInventory(after.Output)
	found := false
	if after.Status == "ok" && perr == nil {
		for _, p := range m2.Packages {
			if p.Name == info.Name && p.Version == info.Version {
				found = true
			}
		}
	}
	if !found {
		checks = append(checks, upgradeCheck{Name: "installed version", Status: "failed", Detail: "the device does not list " + info.Name + " " + info.Version})
		fail("the install command succeeded but the package is not listed on the device; check it", checks)
		return
	}
	checks = append(checks, upgradeCheck{Name: "installed version", Status: "ok", Detail: info.Name + " " + info.Version + " is listed on the device"})
	a.finishPackageInstall(id, "done", "installed", checks)
}

// recoverPackageInstalls ends installs left running by a server restart. The device may
// have installed the package, so the result says to check rather than assume.
func (a *app) recoverPackageInstalls(ctx context.Context, startedAt time.Time) error {
	_, err := a.db.Exec(ctx, `UPDATE package_installs SET status='failed', step='',
		detail='the server restarted during this install; check whether the package is installed on the device',
		finished_at=NOW() WHERE status='running' AND created_at < $1`, startedAt)
	return err
}

// recoverPackageInstallsAtStartup retries while the database is unavailable, since a
// running install blocks the device's other jobs until it is ended.
func (a *app) recoverPackageInstallsAtStartup(ctx context.Context) {
	startedAt := time.Now()
	for attempt := 0; attempt < 20; attempt++ {
		if err := a.recoverPackageInstalls(ctx, startedAt); err == nil {
			return
		}
		log.Printf("package installs: could not look for interrupted installs; retrying")
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}
