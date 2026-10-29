package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/ssh"
)

// Restore of a stored device backup (docs/firmware-upgrades.md, "Restore"). The archive
// can rewrite rc.local, hotplug scripts, authorized_keys and /etc/shadow, so:
//   - only archives OpenManage captured, by backup id, onto the same device;
//   - admin only, with the administrator's password re-entered and the device name
//     typed as confirmation;
//   - the board must match and a release difference needs a second confirmation;
//   - lockout preflight: the archive must still authorize OpenManage's key and carry
//     the host key OpenManage pinned, or the device would become unmanageable;
//   - every command is a fixed string, the archive goes in on stdin to a fixed path
//     with mode 600, is hash-checked on the device, and is deleted afterwards.

const (
	// The archive lives in a fresh directory that only root can have created: mkdir
	// fails if the name already exists, so a local process cannot pre-create the path
	// and swap the file between the hash check and the restore.
	restoreDir           = "/tmp/.om-restore"
	restoreArchivePath   = restoreDir + "/backup.tar.gz"
	restoreApplyTimeout  = 90 * time.Second
	restoreDownWait      = 90 * time.Second
	restoreStepTimeout   = 30 * time.Second
	restoreJobTimeout    = 6 * time.Minute
	restoreRebootGrace   = 15 * time.Second
	restoreReturnTimeout = 4 * time.Minute
	restorePollInterval  = 5 * time.Second
	restoreBodyLimit     = 1024
	staleRestoreAfter    = 15 * time.Minute
)

func restoreOperation(name, label, command string) operation {
	return operation{
		Name: name, Label: label, Class: operationWrite, Roles: []string{"admin"},
		Timeout: restoreStepTimeout, OutputLimit: healthOutputLimit, Audit: auditTransactional,
		command: command,
	}
}

var (
	opRestoreUpload = restoreOperation("firmware_restore_upload", "Upload backup",
		"umask 077; mkdir -m 700 "+restoreDir+" && cat > "+restoreArchivePath)
	opRestoreHash = restoreOperation("firmware_restore_hash", "Check uploaded backup",
		"sha256sum "+restoreArchivePath)
	// The reboot is detached so this command's exit status reaches us before the
	// connection drops. A failed restore removes the file and does not reboot.
	opRestoreApply = restoreOperation("firmware_restore", "Restore backup",
		"if sysupgrade -r "+restoreArchivePath+" >/dev/null 2>&1; then rm -rf "+restoreDir+
			"; (sleep 2; reboot) >/dev/null 2>&1 & else rm -rf "+restoreDir+"; exit 1; fi")
	opRestoreCleanup = restoreOperation("firmware_restore_cleanup", "Remove uploaded backup",
		"rm -rf "+restoreDir)
	opBootID = readOperation("boot_id", "Boot identity", "cat /proc/sys/kernel/random/boot_id")
)

func init() { opRestoreApply.Timeout = restoreApplyTimeout }

var restoreOperations = []operation{opRestoreUpload, opRestoreHash, opRestoreApply, opRestoreCleanup}

const staleRestoreSQL = `UPDATE device_restores SET status='interrupted', finished_at=NOW(),
	detail='the server stopped before this restore finished'
	WHERE device_id=$1 AND status='running' AND created_at < NOW() - ($2 * INTERVAL '1 second')`

type deviceRestore struct {
	ID         int64      `json:"id"`
	DeviceID   string     `json:"device_id"`
	BackupID   string     `json:"backup_id"`
	Status     string     `json:"status"`
	Step       string     `json:"step"`
	Detail     string     `json:"detail"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

const deviceRestoreColumns = `id, device_id::text, backup_id::text, status, step, detail, created_at, finished_at`

func scanDeviceRestore(row pgx.Row, d *deviceRestore) error {
	return row.Scan(&d.ID, &d.DeviceID, &d.BackupID, &d.Status, &d.Step, &d.Detail, &d.CreatedAt, &d.FinishedAt)
}

// restorePreflightError is a refusal with a stable code and a plain message.
type restorePreflightError struct {
	status int
	code   string
	msg    string
}

func (e *restorePreflightError) Error() string { return e.msg }

func refuse(status int, code, msg string) *restorePreflightError {
	return &restorePreflightError{status: status, code: code, msg: msg}
}

// archiveAuthorizesKey reports whether an archived authorized_keys file lists pub.
func archiveAuthorizesKey(authorizedKeys []byte, pub ssh.PublicKey) bool {
	want := pub.Marshal()
	rest := authorizedKeys
	for len(rest) > 0 {
		key, _, options, next, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			return false
		}
		// An entry with options (command=, from=, restrict) would let the key in but
		// not run OpenManage's commands, so it does not count.
		if len(options) == 0 && bytes.Equal(key.Marshal(), want) {
			return true
		}
		rest = next
	}
	return false
}

// restorePreflight compares the live device with what the backup recorded and checks
// that restoring cannot lock OpenManage out. It only reads from the device.
func restorePreflight(b deviceBackup, info archiveInfo, target deviceTarget, liveBoard backupBoard, liveHardware string, acceptRelease bool) *restorePreflightError {
	if liveBoard.Name != b.BoardName {
		return refuse(http.StatusConflict, "board_mismatch", "this backup was taken on a different board; a backup can only be restored to the device and board it came from")
	}
	if liveHardware != "" && b.HardwareID != "" && liveHardware != b.HardwareID {
		return refuse(http.StatusConflict, "hardware_mismatch", "this backup was taken on different hardware; a backup can only be restored to the device it came from")
	}
	if liveBoard.Release != b.Release && !acceptRelease {
		return refuse(http.StatusConflict, "release_mismatch", "the device runs OpenWrt "+liveBoard.Release+" but the backup was taken on "+b.Release+
			"; restoring across releases does not migrate settings. Confirm again to continue")
	}
	if !archiveAuthorizesKey(info.AuthorizedKeys, target.signer.PublicKey()) {
		return refuse(http.StatusConflict, "lockout_key", "restoring this backup would remove OpenManage's SSH key (as a plain, unrestricted entry) from the device and lock OpenManage out")
	}
	if info.HostKeyFingerprint == "" || info.HostKeyFingerprint != target.Fingerprint {
		return refuse(http.StatusConflict, "lockout_host_key", "this backup does not carry the SSH host key OpenManage has pinned for the device; restoring it would make the device fail host-key verification")
	}
	return nil
}

func (a *app) listDeviceRestores(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin", "operator", "readonly") {
		return
	}
	// A restore whose server stopped midway has no worker left to finish its row.
	_, _ = a.db.Exec(r.Context(), staleRestoreSQL, deviceID, int(staleRestoreAfter.Seconds()))
	rows, err := a.db.Query(r.Context(), `SELECT `+deviceRestoreColumns+` FROM device_restores
		WHERE device_id=$1 ORDER BY created_at DESC, id DESC LIMIT 20`, deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load restores")
		return
	}
	defer rows.Close()
	out := []deviceRestore{}
	for rows.Next() {
		var d deviceRestore
		if err := scanDeviceRestore(rows, &d); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read restores")
			return
		}
		out = append(out, d)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read restores")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// restoreDeviceBackup validates everything it can while the request is open, records
// the start in the audit log, and then continues in the background: the device reboots,
// which takes longer than an API request may.
func (a *app) restoreDeviceBackup(w http.ResponseWriter, r *http.Request) {
	deviceID, backupID := strings.ToLower(r.PathValue("id")), strings.ToLower(r.PathValue("backup"))
	if !validUUID(deviceID) || !validUUID(backupID) {
		writeError(w, http.StatusBadRequest, "invalid device or backup ID")
		return
	}
	if !requireOperationRoles(w, r, restoreOperations) {
		return
	}
	var in struct {
		ConfirmName   string `json:"confirm_name"`
		Password      string `json:"password"`
		AcceptRelease bool   `json:"accept_release_mismatch"`
	}
	if err := decodeJSONBody(w, r, restoreBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid restore request")
		return
	}
	if in.Password == "" {
		writeError(w, http.StatusBadRequest, "password is required to restore a backup")
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
	var deviceName string
	if err := a.db.QueryRow(r.Context(), `SELECT name FROM devices WHERE id=$1`, deviceID).Scan(&deviceName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "device not found")
		} else {
			writeError(w, http.StatusInternalServerError, "could not load device")
		}
		return
	}
	if in.ConfirmName != deviceName {
		writeError(w, http.StatusBadRequest, "type the device name exactly to confirm the restore")
		return
	}
	if !deviceJobGuard.tryStart(deviceID) {
		writeError(w, http.StatusConflict, "a backup or restore is already running for this device")
		return
	}
	started := false
	defer func() {
		if !started {
			deviceJobGuard.finish(deviceID)
		}
	}()

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
	info, err := inspectBackupArchive(archive)
	if err != nil || backupDigest(key, info.ContentDigest) != digest {
		writeError(w, http.StatusInternalServerError, "stored backup failed its integrity check")
		return
	}

	target, err := a.loadDeviceTarget(r.Context(), deviceID)
	if err != nil {
		status, msg := targetError(err)
		writeError(w, status, msg)
		return
	}
	session, err := openDeviceSession(r.Context(), target, healthCheckTimeout)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not open a verified SSH session to the device")
		return
	}
	boardResult := session.run(readOperation("system_board", "System information", "ubus call system board"))
	hardwareResult := session.run(backupHardwareOperation)
	session.Close()
	if boardResult.Status != "ok" {
		writeError(w, http.StatusBadGateway, "could not read the device's board before restoring")
		return
	}
	liveHardware := ""
	if hardwareResult.Status == "ok" && hardwareAddressPattern.MatchString(hardwareResult.Output) {
		liveHardware = hardwareResult.Output
	}
	if refusal := restorePreflight(b, info, target, parseBackupBoard(boardResult.Output), liveHardware, in.AcceptRelease); refusal != nil {
		writeJSON(w, refusal.status, map[string]string{"error": refusal.msg, "code": refusal.code})
		return
	}

	// Claim the device and write the audit event in one transaction, before anything
	// on the device changes.
	var rec deviceRestore
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the restore")
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), staleRestoreSQL, deviceID, int(staleRestoreAfter.Seconds()))
	var changing bool
	if err == nil {
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM config_changes WHERE device_id=$1 AND status='applying')`, deviceID).Scan(&changing)
	}
	if err == nil && changing {
		writeError(w, http.StatusConflict, "a configuration change is being applied to this device; wait for it to finish")
		return
	}
	var upgrading bool
	if err == nil {
		err = tx.QueryRow(r.Context(), openUpgradeSQL, deviceID).Scan(&upgrading)
	}
	if err == nil && upgrading {
		writeError(w, http.StatusConflict, openUpgradeMessage)
		return
	}
	if err == nil {
		err = scanDeviceRestore(tx.QueryRow(r.Context(), `INSERT INTO device_restores(device_id,backup_id,status,step,release_mismatch_accepted,created_by)
			VALUES($1,$2,'running','uploading',$3,$4) RETURNING `+deviceRestoreColumns, deviceID, backupID, in.AcceptRelease, user.ID), &rec)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, "a restore is already running for this device")
		return
	}
	if err == nil {
		details := backupAuditDetails(b, digest, false, false)
		details["restore_id"], details["release_mismatch_accepted"] = rec.ID, in.AcceptRelease
		err = insertAuditEvent(r.Context(), tx, user.ID, "device.backup_restore", "device", deviceID, details)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the restore")
		return
	}

	started = true
	go a.runRestore(rec, b, digest, archive, user.ID)
	writeJSON(w, http.StatusAccepted, rec)
}

// runRestore performs the restore and records the outcome. It owns the device job
// guard until it finishes.
func (a *app) runRestore(rec deviceRestore, b deviceBackup, digest string, archive []byte, userID string) {
	defer deviceJobGuard.finish(rec.DeviceID)
	ctx, cancel := context.WithTimeout(context.Background(), restoreJobTimeout)
	defer cancel()
	step := func(name string) {
		sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer scancel()
		_, _ = a.db.Exec(sctx, `UPDATE device_restores SET step=$2 WHERE id=$1`, rec.ID, name)
	}
	status, detail := "done", "restored and verified over a fresh SSH connection"
	if msg := a.restoreOnDevice(ctx, rec.DeviceID, archive, step); msg != "" {
		status, detail = "failed", msg
	}
	// Fresh context: ctx may be spent.
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	tx, err := a.db.Begin(sctx)
	if err == nil {
		defer tx.Rollback(sctx)
		_, err = tx.Exec(sctx, `UPDATE device_restores SET status=$2, detail=$3, step='', finished_at=NOW() WHERE id=$1`, rec.ID, status, detail)
	}
	if err == nil {
		details := backupAuditDetails(b, digest, false, false)
		details["restore_id"], details["detail"] = rec.ID, detail
		err = insertAuditEvent(sctx, tx, userID, "device.backup_restore."+status, "device", rec.DeviceID, details)
	}
	if err == nil {
		err = tx.Commit(sctx)
	}
	if err != nil {
		log.Printf("restore %d finished as %s but could not be recorded", rec.ID, status)
	}
	if status == "done" {
		// A snapshot taken right after is the new baseline, not drift.
		snapCtx, snapCancel := context.WithTimeout(context.Background(), healthCheckTimeout+3*time.Second)
		defer snapCancel()
		_, _ = a.captureSnapshot(snapCtx, rec.DeviceID, userID, true)
	}
}

// restoreOnDevice returns "" on success or a fixed, secret-free failure message.
func (a *app) restoreOnDevice(ctx context.Context, deviceID string, archive []byte, step func(string)) string {
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		_, msg := targetError(err)
		return msg
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		return "could not open a verified SSH session to the device; nothing was changed"
	}
	cleanup := func() { _ = session.runWriteRaw(opRestoreCleanup, nil) }
	// Clear anything left by an earlier attempt, or by a local process squatting on the
	// name; the upload then creates the directory itself or fails.
	cleanup()
	if res := session.runWriteRaw(opRestoreUpload, archive); res.Status != "ok" {
		cleanup()
		session.Close()
		return "uploading the backup failed: " + res.Error + "; nothing was changed"
	}
	sum := sha256.Sum256(archive)
	res := session.runWriteRaw(opRestoreHash, nil)
	if res.Status != "ok" || !strings.HasPrefix(res.Output, hex.EncodeToString(sum[:])) {
		cleanup()
		session.Close()
		return "the uploaded backup did not match what was sent; nothing was changed"
	}
	bootBefore := ""
	if res := session.run(opBootID); res.Status == "ok" {
		bootBefore = res.Output
	}
	step("restoring")
	// A timed-out apply says nothing about the device: the remote shell may still
	// finish and reboot, so only a definite failure stops here.
	if res := session.runWriteRaw(opRestoreApply, nil); res.Status != "ok" && !strings.Contains(res.Error, "timed out") {
		cleanup()
		session.Close()
		return "the device refused to restore the backup; it was not rebooted and may be partly restored. Check it before changing anything else"
	}
	session.Close()

	step("rebooting")
	select {
	case <-time.After(restoreRebootGrace):
	case <-ctx.Done():
		return "stopped waiting for the device to restart"
	}
	deadline := time.Now().Add(restoreReturnTimeout)
	step("verifying")
	// Success needs proof of a restart: a different boot id, or at least one poll in
	// which the device was unreachable. Answering as before, with the same boot, would
	// mean the reboot never happened and the restore is not in effect.
	sawDown := false
	for time.Now().Before(deadline) && ctx.Err() == nil {
		msg, bootNow, ok := a.verifyAfterRestore(ctx, deviceID)
		switch {
		case msg != "":
			return msg
		case ok && (sawDown || (bootBefore != "" && bootNow != "" && bootNow != bootBefore)):
			return ""
		case ok && time.Until(deadline) < restoreReturnTimeout-restoreDownWait:
			return "the device answered but did not restart after the restore, so the restored configuration is not in effect yet. Reboot it, then check it"
		case !ok:
			sawDown = true
		}
		select {
		case <-time.After(restorePollInterval):
		case <-ctx.Done():
		}
	}
	return "the device did not come back with the pinned host key and OpenManage's key within " + restoreReturnTimeout.String() +
		". Wait a few minutes, power-cycle it if needed, then check it before restoring again"
}

// verifyAfterRestore tries one fresh SSH connection. It returns ok with the device's
// boot id on success, a message when retrying is pointless, and ("", "", false) when the
// device is simply not reachable yet.
func (a *app) verifyAfterRestore(ctx context.Context, deviceID string) (msg, bootID string, ok bool) {
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		_, m := targetError(err)
		return m, "", false
	}
	session, err := openDeviceSession(ctx, target, 10*time.Second)
	if err != nil {
		if errors.Is(err, errHostKeyMismatch) {
			return "the device came back with a different SSH host key than the one OpenManage pinned", "", false
		}
		return "", "", false
	}
	defer session.Close()
	res := session.run(readOperation("system_board", "System information", "ubus call system board"))
	if res.Status != "ok" {
		return "", "", false
	}
	if boot := session.run(opBootID); boot.Status == "ok" {
		bootID = boot.Output
	}
	return "", bootID, true
}
