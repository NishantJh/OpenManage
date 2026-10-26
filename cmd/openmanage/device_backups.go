package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Device configuration backups. A backup is a `sysupgrade -b` archive of any enrolled
// OpenWrt device, router or access point. The archive is root-equivalent for the
// device (Wi-Fi keys, RADIUS secrets, the SSH host key, /etc/shadow), so:
//   - the command is fixed and takes no parameters;
//   - the archive is held in memory only, validated by backup_archive.go, sealed with
//     AES-256-GCM bound to the device and backup id, and never logged or audited;
//   - roles: create admin/operator, list any role (metadata), download and delete admin.
// See docs/firmware-upgrades.md, "Device config backup (Phase 0)".

const (
	backupOutputLimit = 4 << 20
	backupTimeout     = 15 * time.Second
	// backupsPerDevice is how many unpinned backups are kept per device.
	backupsPerDevice = 5
	backupBodyLimit  = 2 * 1024
	// backupJobTimeout covers connecting plus the three reads of one capture.
	backupJobTimeout = 60 * time.Second
)

// backupOperation streams the archive on stdout. umask keeps any file sysupgrade
// creates private, and stderr is dropped because it can echo file names.
var backupOperation = operation{
	Name: "firmware_backup", Label: "Configuration backup", Class: operationRead,
	Roles: []string{"admin", "operator"}, Timeout: backupTimeout, OutputLimit: backupOutputLimit,
	Audit: auditTransactional, command: "umask 077; sysupgrade -b - 2>/dev/null",
}

// backupHardwareOperation reads one stable network address as a board identity.
var backupHardwareOperation = readOperation("hardware_id", "Hardware address",
	`for i in br-lan eth0 lan; do [ -r /sys/class/net/$i/address ] && { cat /sys/class/net/$i/address; break; }; done`)

var backupOperations = []operation{backupOperation}

var hardwareAddressPattern = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// deviceJobs allows one backup or restore job per device at a time.
type deviceJobs struct {
	mu   sync.Mutex
	busy map[string]bool
}

var deviceJobGuard deviceJobs

func (j *deviceJobs) tryStart(deviceID string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.busy[deviceID] {
		return false
	}
	if j.busy == nil {
		j.busy = map[string]bool{}
	}
	j.busy[deviceID] = true
	return true
}

func (j *deviceJobs) finish(deviceID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.busy, deviceID)
}

func backupAAD(deviceID, backupID string) string {
	return "openmanage:firmware-backup:v1:" + deviceID + ":" + backupID
}

func backupDigest(key []byte, contentDigest string) string {
	return keyedDigest(key, "openmanage:backup-digest:v1", contentDigest)
}

const backupKeyVersion = 1

type deviceBackup struct {
	ID                 string    `json:"id"`
	DeviceID           string    `json:"device_id"`
	CapturedAt         time.Time `json:"captured_at"`
	LastVerifiedAt     time.Time `json:"last_verified_at"`
	Scheduled          bool      `json:"scheduled"`
	Pinned             bool      `json:"pinned"`
	SizeBytes          int       `json:"size_bytes"`
	FileCount          int       `json:"file_count"`
	BoardName          string    `json:"board_name"`
	Target             string    `json:"target"`
	Release            string    `json:"release"`
	HardwareID         string    `json:"hardware_id"`
	HostKeyFingerprint string    `json:"host_key_fingerprint"`
}

const deviceBackupColumns = `id::text, device_id::text, captured_at, last_verified_at, scheduled, pinned, size_bytes, file_count,
	board_name, target, release, hardware_id, hostkey_fingerprint`

func scanDeviceBackup(row pgx.Row, b *deviceBackup) error {
	return row.Scan(&b.ID, &b.DeviceID, &b.CapturedAt, &b.LastVerifiedAt, &b.Scheduled, &b.Pinned, &b.SizeBytes, &b.FileCount,
		&b.BoardName, &b.Target, &b.Release, &b.HardwareID, &b.HostKeyFingerprint)
}

type backupResult struct {
	Backup deviceBackup `json:"backup"`
	// Unchanged is true when the device matched its newest backup, so no copy was
	// stored and the newest backup's last_verified_at moved forward instead.
	Unchanged bool `json:"unchanged"`
}

// backupBoard is the device identity recorded with a backup, from `ubus call system board`.
type backupBoard struct{ Name, Target, Release string }

func parseBackupBoard(output string) backupBoard {
	var in struct {
		BoardName string `json:"board_name"`
		Release   struct {
			Version string `json:"version"`
			Target  string `json:"target"`
		} `json:"release"`
	}
	if json.Unmarshal([]byte(output), &in) != nil {
		return backupBoard{}
	}
	return backupBoard{Name: cleanInventoryField(in.BoardName), Target: cleanInventoryField(in.Release.Target), Release: cleanInventoryField(in.Release.Version)}
}

func newBackupID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func (a *app) createDeviceBackup(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, backupOperations) {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	ctx, cancel := context.WithTimeout(r.Context(), backupJobTimeout)
	defer cancel()
	out, fail := a.captureBackup(ctx, deviceID, user.ID)
	if fail != nil {
		if fail.reason != "" {
			a.recordHealthCheckOutcome(r, "device.backup_create.failed", deviceID, fail.reason)
		}
		writeError(w, fail.status, fail.msg)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// captureBackup takes a backup and stores it unless the device is unchanged. An empty
// userID means the scheduler took it; unchanged scheduled runs are not audited.
func (a *app) captureBackup(ctx context.Context, deviceID, userID string) (backupResult, *captureFailure) {
	var out backupResult
	if !deviceJobGuard.tryStart(deviceID) {
		return out, &captureFailure{status: http.StatusConflict, msg: "a backup or restore is already running for this device"}
	}
	defer deviceJobGuard.finish(deviceID)

	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		status, msg := targetError(err)
		return out, &captureFailure{status: status, msg: msg}
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		return out, &captureFailure{http.StatusBadGateway, "could not open a verified SSH session to the device", "ssh_failed"}
	}
	boardResult := session.run(readOperation("system_board", "System information", "ubus call system board"))
	hardwareResult := session.run(backupHardwareOperation)
	archive, result := session.runBinary(backupOperation)
	session.Close()
	if result.Status != "ok" {
		return out, &captureFailure{http.StatusBadGateway, "configuration backup failed: " + result.Error, "backup_failed"}
	}
	info, err := inspectBackupArchive(archive)
	if err != nil {
		return out, &captureFailure{http.StatusBadGateway, err.Error(), "bad_archive"}
	}
	var board backupBoard
	if boardResult.Status == "ok" {
		board = parseBackupBoard(boardResult.Output)
	}
	hardwareID := ""
	if hardwareResult.Status == "ok" && hardwareAddressPattern.MatchString(hardwareResult.Output) {
		hardwareID = hardwareResult.Output
	}
	key, err := encryptionKey()
	if err != nil {
		return out, &captureFailure{status: http.StatusServiceUnavailable, msg: "SSH operations are disabled: server encryption key is not configured"}
	}
	digest := backupDigest(key, info.ContentDigest)
	storeFailed := &captureFailure{status: http.StatusInternalServerError, msg: "could not store the configuration backup"}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return out, storeFailed
	}
	defer tx.Rollback(ctx)
	var latestID, latestDigest string
	err = tx.QueryRow(ctx, `SELECT id::text, digest FROM device_backups WHERE device_id=$1
		ORDER BY captured_at DESC, id DESC LIMIT 1 FOR UPDATE`, deviceID).Scan(&latestID, &latestDigest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, storeFailed
	}
	if err == nil && latestDigest == digest {
		out.Unchanged = true
		err = scanDeviceBackup(tx.QueryRow(ctx, `UPDATE device_backups SET last_verified_at=NOW() WHERE id=$1
			RETURNING `+deviceBackupColumns, latestID), &out.Backup)
	} else {
		var backupID string
		var ciphertext []byte
		if backupID, err = newBackupID(); err == nil {
			ciphertext, err = sealSecret(key, archive, backupAAD(deviceID, backupID))
		}
		if err == nil {
			err = scanDeviceBackup(tx.QueryRow(ctx, `INSERT INTO device_backups(id,device_id,captured_by,scheduled,digest,size_bytes,file_count,
					board_name,target,release,hardware_id,hostkey_fingerprint,key_version,ciphertext)
				VALUES($1,$2,NULLIF($3,'')::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
				RETURNING `+deviceBackupColumns,
				backupID, deviceID, userID, userID == "", digest, len(archive), info.Files,
				board.Name, board.Target, board.Release, hardwareID, info.HostKeyFingerprint, backupKeyVersion, ciphertext), &out.Backup)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM device_backups WHERE device_id=$1 AND NOT pinned AND id NOT IN (
				SELECT id FROM device_backups WHERE device_id=$1 AND NOT pinned ORDER BY captured_at DESC, id DESC LIMIT $2)`,
				deviceID, backupsPerDevice)
		}
	}
	if err == nil && (userID != "" || !out.Unchanged) {
		err = insertAuditEvent(ctx, tx, userID, "device.backup_create", "device", deviceID, backupAuditDetails(out.Backup, digest, userID == "", out.Unchanged))
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return out, storeFailed
	}
	return out, nil
}

// backupAuditDetails is everything an audit event may say about a backup: ids, size,
// keyed digest and who. Never file names or content.
func backupAuditDetails(b deviceBackup, digest string, scheduled, unchanged bool) map[string]any {
	return map[string]any{"backup_id": b.ID, "device_id": b.DeviceID, "size_bytes": b.SizeBytes, "digest": digest,
		"scheduled": scheduled, "unchanged": unchanged}
}

func (a *app) listDeviceBackups(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin", "operator", "readonly") {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT `+deviceBackupColumns+` FROM device_backups
		WHERE device_id=$1 ORDER BY captured_at DESC, id DESC LIMIT 50`, deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load configuration backups")
		return
	}
	defer rows.Close()
	out := []deviceBackup{}
	for rows.Next() {
		var b deviceBackup
		if err := scanDeviceBackup(rows, &b); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read configuration backups")
			return
		}
		out = append(out, b)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read configuration backups")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// downloadDeviceBackup returns the decrypted archive to an administrator who has just
// re-entered their password. The audit event commits before any byte is sent. It is a
// POST so the password travels in the body, never in a URL or log line.
func (a *app) downloadDeviceBackup(w http.ResponseWriter, r *http.Request) {
	deviceID, backupID := r.PathValue("id"), r.PathValue("backup")
	if !validUUID(deviceID) || !validUUID(backupID) {
		writeError(w, http.StatusBadRequest, "invalid device or backup ID")
		return
	}
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decodeJSONBody(w, r, backupBodyLimit, &in); err != nil || in.Password == "" {
		writeError(w, http.StatusBadRequest, "password is required to download a backup")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	if !a.verifyUserPassword(w, r, user.ID, in.Password, "password is incorrect") {
		return
	}
	key, err := encryptionKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server encryption key is not configured")
		return
	}
	var b deviceBackup
	var digest string
	var ciphertext []byte
	err = a.db.QueryRow(r.Context(), `SELECT `+deviceBackupColumns+`, digest, ciphertext FROM device_backups
		WHERE id=$1 AND device_id=$2`, backupID, deviceID).Scan(&b.ID, &b.DeviceID, &b.CapturedAt, &b.LastVerifiedAt, &b.Scheduled,
		&b.Pinned, &b.SizeBytes, &b.FileCount, &b.BoardName, &b.Target, &b.Release, &b.HardwareID, &b.HostKeyFingerprint, &digest, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the configuration backup")
		return
	}
	archive, err := openSecret(key, ciphertext, backupAAD(deviceID, backupID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stored backup could not be decrypted; verify the configured encryption key")
		return
	}
	if err := a.auditBackup(r.Context(), user.ID, "device.backup_download", b, digest); err != nil {
		writeError(w, http.StatusInternalServerError, "could not record the download in the audit log")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Disposition", `attachment; filename="openmanage-backup-`+backupID+`.tar.gz"`)
	h.Set("Content-Length", strconv.Itoa(len(archive)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(archive)
}

func (a *app) auditBackup(ctx context.Context, userID, action string, b deviceBackup, digest string) error {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = insertAuditEvent(ctx, tx, userID, action, "device", b.DeviceID, backupAuditDetails(b, digest, b.Scheduled, false)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (a *app) deleteDeviceBackup(w http.ResponseWriter, r *http.Request) {
	deviceID, backupID := r.PathValue("id"), r.PathValue("backup")
	if !validUUID(deviceID) || !validUUID(backupID) {
		writeError(w, http.StatusBadRequest, "invalid device or backup ID")
		return
	}
	if !requireAnyRole(w, r, "admin") {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete the configuration backup")
		return
	}
	defer tx.Rollback(r.Context())
	var b deviceBackup
	var digest string
	err = tx.QueryRow(r.Context(), `SELECT `+deviceBackupColumns+`, digest FROM device_backups WHERE id=$1 AND device_id=$2 FOR UPDATE`,
		backupID, deviceID).Scan(&b.ID, &b.DeviceID, &b.CapturedAt, &b.LastVerifiedAt, &b.Scheduled,
		&b.Pinned, &b.SizeBytes, &b.FileCount, &b.BoardName, &b.Target, &b.Release, &b.HardwareID, &b.HostKeyFingerprint, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	if err == nil && b.Pinned {
		writeError(w, http.StatusConflict, "this backup is pinned for a firmware upgrade and cannot be deleted yet")
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `DELETE FROM device_backups WHERE id=$1`, backupID)
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "device.backup_delete", "device", deviceID, backupAuditDetails(b, digest, b.Scheduled, false))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete the configuration backup")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
