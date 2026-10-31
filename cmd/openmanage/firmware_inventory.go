package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Firmware inventory is the read-only first step of firmware upgrades
// (docs/firmware-upgrades.md, Phase 1): what release a device runs, which package
// manager it uses, which packages the owner added or removed compared with the
// firmware image, custom feeds, the sysupgrade keep list and free space. Nothing here
// changes a device. The output is not secret, but it is untrusted: every field is
// validated and capped, and feed URLs lose any credentials before they are stored.

const (
	firmwareInventoryLimit        = 512 * 1024
	firmwareInventoriesPerDevice  = 20
	firmwareInventoryMaxPackages  = 4000
	firmwareInventoryMaxLines     = 200
	firmwareInventoryMaxLineBytes = 512
)

// firmwareInventoryCommand prints fixed sections, each introduced by an "@@name" line.
// It reads the package database directly (opkg status file, or the apk installed
// database and world file) so the output does not depend on package-manager versions.
// opkg keeps records of removed packages (state "not-installed" or "config-files");
// only entries whose state is "installed" are listed.
// /rom holds the original firmware image on squashfs devices; when it is missing the
// device was not flashed from a squashfs image and ROM comparisons are unknown.
const firmwareInventoryCommand = `exec 2>/dev/null
echo @@board; ubus call system board
echo @@arch; sed -n "s/^DISTRIB_ARCH='\(.*\)'$/\1/p" /etc/openwrt_release
echo @@manager; if command -v apk >/dev/null; then echo apk; elif command -v opkg >/dev/null; then echo opkg; fi
pkgs() { if [ -f "$1/lib/apk/db/installed" ]; then awk '/^P:/{p=substr($0,3)} /^V:/{v=substr($0,3)} /^$/{if(p!="")print p"\t"v"\t-";p=v=""} END{if(p!="")print p"\t"v"\t-"}' "$1/lib/apk/db/installed"; elif [ -f "$1/usr/lib/opkg/status" ]; then awk '/^Package:/{p=$2} /^Version:/{v=$2} /^Status:/{s=($4=="installed")?$3:""} /^$/{if(p!=""&&s!="")print p"\t"v"\t"s;p=v=s=""} END{if(p!=""&&s!="")print p"\t"v"\t"s}' "$1/usr/lib/opkg/status"; fi; }
echo @@installed; pkgs ""
echo @@world; cat /etc/apk/world
if [ -d /rom/etc ]; then echo @@rom; echo @@rom_installed; pkgs /rom; echo @@rom_world; cat /rom/etc/apk/world; fi
echo @@feeds; grep -hv '^[[:space:]]*#' /etc/opkg/customfeeds.conf /etc/apk/repositories.d/customfeeds.list
echo @@keep; grep -v '^[[:space:]]*#' /etc/sysupgrade.conf
echo @@fstab; uci -q show fstab
echo @@df; df -k /overlay || df -k /
echo @@mem; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo
true`

// firmwareInventoryOperation is read-only. Administrators and operators may capture an
// inventory; every role may view the stored result.
var firmwareInventoryOperation = operation{
	Name: "firmware_inventory", Label: "Firmware and package inventory", Class: operationRead,
	Roles: []string{"admin", "operator"}, Timeout: healthCheckTimeout, OutputLimit: firmwareInventoryLimit,
	Audit: auditBestEffort, command: firmwareInventoryCommand,
}

type firmwarePackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// UserInstalled means the owner asked for this package rather than it coming with
	// the firmware image or as a dependency.
	UserInstalled bool `json:"user_installed"`
	Hold          bool `json:"hold,omitempty"`
}

type firmwareManifest struct {
	Release        string            `json:"release"`
	Revision       string            `json:"revision"`
	Target         string            `json:"target"`
	Arch           string            `json:"arch"`
	BoardName      string            `json:"board_name"`
	Model          string            `json:"model"`
	PackageManager string            `json:"package_manager"`
	Packages       []firmwarePackage `json:"packages"`
	// ROMKnown is false when the device has no /rom (no squashfs image), so packages
	// cannot be compared with the firmware image and RemovedDefaults is empty.
	ROMKnown        bool     `json:"rom_known"`
	UserInstalled   []string `json:"user_installed"`
	RemovedDefaults []string `json:"removed_defaults"`
	CustomFeeds     []string `json:"custom_feeds"`
	KeepList        []string `json:"keep_list"`
	// Extroot means an enabled fstab mount targets /overlay. Upgrades refuse such devices.
	Extroot        bool  `json:"extroot"`
	OverlayTotalKB int64 `json:"overlay_total_kb"`
	OverlayFreeKB  int64 `json:"overlay_free_kb"`
	MemTotalKB     int64 `json:"mem_total_kb"`
	MemAvailableKB int64 `json:"mem_available_kb"`
}

var (
	errFirmwareInventoryBoard   = errors.New("the device did not report its OpenWrt release")
	errFirmwareInventoryManager = errors.New("the device has neither opkg nor apk")
	packageNamePattern          = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]{0,127}$`)
	packageVersionPattern       = regexp.MustCompile(`^[A-Za-z0-9+._~:-]{1,128}$`)
)

// splitInventorySections splits command output into its "@@name" sections. Unknown
// sections are kept and ignored by the caller; repeated sections are appended.
func splitInventorySections(output string) map[string][]string {
	sections := map[string][]string{}
	current := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if name, ok := strings.CutPrefix(line, "@@"); ok && name != "" && !strings.ContainsAny(name, " \t") {
			current = name
			if _, seen := sections[current]; !seen {
				sections[current] = []string{}
			}
			continue
		}
		if current != "" {
			sections[current] = append(sections[current], line)
		}
	}
	return sections
}

// parsePackageLines reads "name<TAB>version<TAB>flags" lines. flags is the opkg
// Status flag field (for example "user" or "ok,hold"), or "-" for apk. Invalid lines
// are dropped, so a misbehaving device cannot inject odd names.
func parsePackageLines(lines []string) map[string]firmwarePackage {
	out := map[string]firmwarePackage{}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || !packageNamePattern.MatchString(fields[0]) || len(out) >= firmwareInventoryMaxPackages {
			continue
		}
		pkg := firmwarePackage{Name: fields[0]}
		if packageVersionPattern.MatchString(fields[1]) {
			pkg.Version = fields[1]
		}
		for _, flag := range strings.Split(fields[2], ",") {
			switch flag {
			case "user":
				pkg.UserInstalled = true
			case "hold":
				pkg.Hold = true
			}
		}
		out[pkg.Name] = pkg
	}
	return out
}

// worldNames reads /etc/apk/world entries, dropping version constraints and tags
// ("base-files=1699~f505", "foo@custom", "bar>1.0").
func worldNames(lines []string) map[string]bool {
	out := map[string]bool{}
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if i := strings.IndexAny(name, "=<>~@"); i >= 0 {
			name = name[:i]
		}
		if packageNamePattern.MatchString(name) && len(out) < firmwareInventoryMaxPackages {
			out[name] = true
		}
	}
	return out
}

// cleanInventoryLines keeps non-empty lines without control characters, capped in
// count and length.
func cleanInventoryLines(lines []string) []string {
	out := []string{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || len(line) > firmwareInventoryMaxLineBytes || strings.IndexFunc(line, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			continue
		}
		if len(out) == firmwareInventoryMaxLines {
			break
		}
		out = append(out, line)
	}
	return out
}

// redactFeedLine removes credentials and query strings from any URL in a feed line,
// since custom feeds sometimes embed an access token.
func redactFeedLine(line string) string {
	words := strings.Fields(line)
	for i, word := range words {
		if !strings.Contains(word, "://") {
			continue
		}
		u, err := url.Parse(word)
		if err != nil || u.Host == "" {
			words[i] = "[unreadable URL]"
			continue
		}
		if u.User != nil {
			u.User = url.User("redacted")
		}
		if u.RawQuery != "" {
			u.RawQuery = "redacted"
		}
		u.Fragment = ""
		words[i] = u.String()
	}
	return strings.Join(words, " ")
}

// fstabHasExtroot reads `uci show fstab` and reports whether an enabled mount
// section targets /overlay.
func fstabHasExtroot(lines []string) bool {
	targets, disabled := map[string]bool{}, map[string]bool{}
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, "'")
		parts := strings.Split(key, ".")
		if len(parts) != 3 || parts[0] != "fstab" {
			continue
		}
		switch parts[2] {
		case "target":
			if strings.TrimRight(value, "/") == "/overlay" {
				targets[parts[1]] = true
			}
		case "enabled":
			disabled[parts[1]] = value == "0"
		}
	}
	for section := range targets {
		if !disabled[section] {
			return true
		}
	}
	return false
}

// parseDF reads busybox `df -k` output: the last line ends with
// 1K-blocks, Used, Available, Use%, Mounted on.
func parseDF(lines []string) (total, free int64) {
	for i := len(lines) - 1; i >= 0; i-- {
		fields := strings.Fields(lines[i])
		if len(fields) < 5 {
			continue
		}
		n := len(fields)
		t, err1 := strconv.ParseInt(fields[n-5], 10, 64)
		f, err2 := strconv.ParseInt(fields[n-3], 10, 64)
		if err1 == nil && err2 == nil && t >= 0 && f >= 0 {
			return t, f
		}
	}
	return 0, 0
}

func parseMeminfo(lines []string) (total, available int64) {
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || v < 0 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			available = v
		}
	}
	return total, available
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseFirmwareInventory turns the command output into a manifest. It fails only
// when the release or package manager is missing, since nothing useful can be planned
// without them; other missing sections leave their fields empty.
func parseFirmwareInventory(output string) (firmwareManifest, error) {
	s := splitInventorySections(output)
	var m firmwareManifest
	var board struct {
		BoardName string `json:"board_name"`
		Model     string `json:"model"`
		Release   struct {
			Version  string `json:"version"`
			Revision string `json:"revision"`
			Target   string `json:"target"`
		} `json:"release"`
	}
	if json.Unmarshal([]byte(strings.Join(s["board"], "\n")), &board) != nil || board.Release.Version == "" {
		return m, errFirmwareInventoryBoard
	}
	m.BoardName, m.Model = cleanInventoryField(board.BoardName), cleanInventoryField(board.Model)
	m.Release, m.Revision, m.Target = cleanInventoryField(board.Release.Version), cleanInventoryField(board.Release.Revision), cleanInventoryField(board.Release.Target)
	if arch := cleanInventoryLines(s["arch"]); len(arch) > 0 && packageNamePattern.MatchString(arch[0]) {
		m.Arch = arch[0]
	}
	if manager := cleanInventoryLines(s["manager"]); len(manager) > 0 && (manager[0] == "opkg" || manager[0] == "apk") {
		m.PackageManager = manager[0]
	} else {
		return m, errFirmwareInventoryManager
	}

	installed := parsePackageLines(s["installed"])
	_, m.ROMKnown = s["rom"]
	romInstalled := parsePackageLines(s["rom_installed"])
	user := map[string]bool{}
	if m.PackageManager == "apk" {
		// apk images list every image package in world, so what the owner added is
		// world minus the image's world. Without /rom the two cannot be told apart.
		world, romWorld := worldNames(s["world"]), worldNames(s["rom_world"])
		for name := range world {
			if _, ok := installed[name]; ok && (!m.ROMKnown || !romWorld[name]) {
				user[name] = true
			}
		}
	} else {
		for name, pkg := range installed {
			if pkg.UserInstalled && (!m.ROMKnown || romInstalled[name].Name == "") {
				user[name] = true
			}
		}
	}
	removed := map[string]bool{}
	if m.ROMKnown {
		for name := range romInstalled {
			if _, ok := installed[name]; !ok {
				removed[name] = true
			}
		}
	}
	names := make([]string, 0, len(installed))
	for name := range installed {
		names = append(names, name)
	}
	sort.Strings(names)
	m.Packages = []firmwarePackage{}
	for _, name := range names {
		pkg := installed[name]
		pkg.UserInstalled = user[name]
		m.Packages = append(m.Packages, pkg)
	}
	m.UserInstalled, m.RemovedDefaults = sortedKeys(user), sortedKeys(removed)

	m.CustomFeeds = []string{}
	for _, line := range cleanInventoryLines(s["feeds"]) {
		m.CustomFeeds = append(m.CustomFeeds, redactFeedLine(line))
	}
	m.KeepList = cleanInventoryLines(s["keep"])
	m.Extroot = fstabHasExtroot(s["fstab"])
	m.OverlayTotalKB, m.OverlayFreeKB = parseDF(s["df"])
	m.MemTotalKB, m.MemAvailableKB = parseMeminfo(s["mem"])
	return m, nil
}

// firmwareManifestDigest covers everything except free space and memory, which move
// on every read. An inventory that differs only in those is updated in place.
func firmwareManifestDigest(m firmwareManifest) (string, error) {
	m.OverlayFreeKB, m.MemAvailableKB = 0, 0
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

type firmwareInventory struct {
	ID             int64            `json:"id"`
	DeviceID       string           `json:"device_id"`
	CapturedAt     time.Time        `json:"captured_at"`
	LastVerifiedAt time.Time        `json:"last_verified_at"`
	Manifest       firmwareManifest `json:"manifest"`
}

const firmwareInventoryColumns = `id, device_id::text, captured_at, last_verified_at, manifest`

func scanFirmwareInventory(row pgx.Row, inv *firmwareInventory) error {
	return row.Scan(&inv.ID, &inv.DeviceID, &inv.CapturedAt, &inv.LastVerifiedAt, &inv.Manifest)
}

type firmwareInventoryResult struct {
	Inventory firmwareInventory `json:"inventory"`
	First     bool              `json:"first"`
	Changed   bool              `json:"changed"`
}

func (a *app) captureFirmwareInventory(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, []operation{firmwareInventoryOperation}) {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()
	out, fail := a.takeFirmwareInventory(ctx, deviceID, user.ID)
	if fail != nil {
		if fail.reason != "" {
			a.recordHealthCheckOutcome(r, "device.firmware_inventory.failed", deviceID, fail.reason)
		}
		writeError(w, fail.status, fail.msg)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// takeFirmwareInventory reads a device's inventory and stores it if it changed; an
// unchanged inventory refreshes last_verified_at, free space and memory instead.
func (a *app) takeFirmwareInventory(ctx context.Context, deviceID, userID string) (firmwareInventoryResult, *captureFailure) {
	var out firmwareInventoryResult
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		status, msg := targetError(err)
		return out, &captureFailure{status: status, msg: msg}
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		return out, &captureFailure{http.StatusBadGateway, "could not open a verified SSH session to the device", "ssh_failed"}
	}
	result := session.run(firmwareInventoryOperation)
	session.Close()
	if result.Status != "ok" {
		return out, &captureFailure{http.StatusBadGateway, "firmware inventory failed: " + result.Error, "inventory_failed"}
	}
	manifest, err := parseFirmwareInventory(result.Output)
	if err != nil {
		return out, &captureFailure{http.StatusBadGateway, err.Error(), "not_openwrt"}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return out, &captureFailure{status: http.StatusInternalServerError, msg: "could not store the firmware inventory"}
	}
	digest, err := firmwareManifestDigest(manifest)
	if err != nil {
		return out, &captureFailure{status: http.StatusInternalServerError, msg: "could not store the firmware inventory"}
	}
	storeFailed := &captureFailure{status: http.StatusInternalServerError, msg: "could not store the firmware inventory"}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return out, storeFailed
	}
	defer tx.Rollback(ctx)
	// Serialize inventory snapshots with rollout previews and desired-package-set
	// edits, all of which lock the device row before reading or changing its state.
	var lockedDeviceID string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM devices WHERE id=$1 FOR UPDATE`, deviceID).Scan(&lockedDeviceID); err != nil {
		return out, storeFailed
	}
	var flashActive bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM firmware_rollout_devices
		WHERE device_id_snapshot=$1 AND status='flashing')`, deviceID).Scan(&flashActive); err != nil {
		return out, storeFailed
	}
	if flashActive {
		return out, &captureFailure{status: http.StatusConflict, msg: "a staged campaign flash is active on this device"}
	}
	var prevID int64
	var prevDigest string
	err = tx.QueryRow(ctx, `SELECT id, digest FROM firmware_inventories WHERE device_id=$1
		ORDER BY captured_at DESC, id DESC LIMIT 1 FOR UPDATE`, deviceID).Scan(&prevID, &prevDigest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		out.First = true
	case err != nil:
		return out, storeFailed
	}
	if !out.First && prevDigest == digest {
		err = scanFirmwareInventory(tx.QueryRow(ctx, `UPDATE firmware_inventories SET last_verified_at=NOW(), manifest=$2 WHERE id=$1
			RETURNING `+firmwareInventoryColumns, prevID, encoded), &out.Inventory)
	} else {
		out.Changed = !out.First
		err = scanFirmwareInventory(tx.QueryRow(ctx, `INSERT INTO firmware_inventories(device_id,captured_by,digest,manifest)
			VALUES($1,NULLIF($2,'')::uuid,$3,$4) RETURNING `+firmwareInventoryColumns,
			deviceID, userID, digest, encoded), &out.Inventory)
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM firmware_inventories WHERE device_id=$1 AND id NOT IN (
				SELECT id FROM firmware_inventories WHERE device_id=$1 ORDER BY captured_at DESC, id DESC LIMIT $2)`,
				deviceID, firmwareInventoriesPerDevice)
		}
	}
	if err == nil && (userID != "" || out.First || out.Changed) {
		err = insertAuditEvent(ctx, tx, userID, "device.firmware_inventory", "device", deviceID, map[string]any{
			"inventory_id": out.Inventory.ID, "first": out.First, "changed": out.Changed,
			"release": manifest.Release, "package_count": len(manifest.Packages), "user_installed_count": len(manifest.UserInstalled),
		})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return out, storeFailed
	}
	return out, nil
}

// getFirmwareInventory returns the newest stored inventory, or null when none exists.
// Any signed-in role may read it; it holds no secrets.
func (a *app) getFirmwareInventory(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, readOnlyRoles...) {
		return
	}
	var inv firmwareInventory
	err := scanFirmwareInventory(a.db.QueryRow(r.Context(), `SELECT `+firmwareInventoryColumns+`
		FROM firmware_inventories WHERE device_id=$1 ORDER BY captured_at DESC, id DESC LIMIT 1`, deviceID), &inv)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeJSON(w, http.StatusOK, map[string]any{"inventory": nil})
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not load the firmware inventory")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"inventory": inv})
	}
}
