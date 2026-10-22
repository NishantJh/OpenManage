package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	configExportLimit        = 512 * 1024
	configSnapshotsPerDevice = 100
)

// configExportOperation reads the full UCI configuration. Its output can contain
// secrets, so only administrators may run it and the text is never returned raw.
var configExportOperation = operation{
	Name: "uci_export", Label: "Configuration export", Class: operationRead,
	Roles: []string{"admin"}, Timeout: healthCheckTimeout, OutputLimit: configExportLimit,
	Audit: auditBestEffort, command: "uci export",
}

type configSnapshot struct {
	ID             int64      `json:"id"`
	DeviceID       string     `json:"device_id"`
	CapturedAt     time.Time  `json:"captured_at"`
	LastVerifiedAt time.Time  `json:"last_verified_at"`
	SizeBytes      int        `json:"size_bytes"`
	OptionCount    int        `json:"option_count"`
	Drift          bool       `json:"drift"`
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
}

const configSnapshotColumns = `id, device_id::text, captured_at, last_verified_at, size_bytes, option_count, drift, acknowledged_at`

func scanConfigSnapshot(row pgx.Row, s *configSnapshot) error {
	return row.Scan(&s.ID, &s.DeviceID, &s.CapturedAt, &s.LastVerifiedAt, &s.SizeBytes, &s.OptionCount, &s.Drift, &s.AcknowledgedAt)
}

type captureResult struct {
	Snapshot configSnapshot `json:"snapshot"`
	// First is true for a device's first snapshot; Changed when the configuration
	// differs from the previous snapshot (drift). Unchanged captures refresh
	// last_verified_at instead of storing a copy.
	First       bool `json:"first"`
	Changed     bool `json:"changed"`
	ChangeCount int  `json:"change_count"`
}

func configSnapshotAAD(deviceID string) string {
	return "openmanage:config-snapshot:v1:" + deviceID
}

// configDigest is keyed so equal digests reveal nothing to someone without the key.
func configDigest(key []byte, text string) string {
	sub := hmac.New(sha256.New, key)
	sub.Write([]byte("openmanage:config-digest:v1"))
	mac := hmac.New(sha256.New, sub.Sum(nil))
	mac.Write([]byte(text))
	return hex.EncodeToString(mac.Sum(nil))
}

// targetError maps loadDeviceTarget failures to a status and a message safe to show.
func targetError(err error) (int, string) {
	switch {
	case errors.Is(err, errTargetNotFound):
		return http.StatusNotFound, "device not found"
	case errors.Is(err, errTargetNotPrivate):
		return http.StatusUnprocessableEntity, "device operations only permit private IP addresses"
	case errors.Is(err, errTargetNotEnrolled):
		return http.StatusConflict, "device must be enrolled first"
	case errors.Is(err, errTargetKeyUnavailable):
		return http.StatusServiceUnavailable, "SSH operations are disabled: server encryption key is not configured"
	case errors.Is(err, errTargetDecrypt):
		return http.StatusInternalServerError, "stored SSH credential could not be decrypted; verify the configured encryption key and restore"
	case errors.Is(err, errTargetBadCredential):
		return http.StatusInternalServerError, "stored SSH credential is not a supported private key"
	default:
		return http.StatusInternalServerError, "could not load device"
	}
}

// captureFailure says why a capture failed: a status and message safe to show, and a
// short reason for the audit trail. Device-side failures have a non-empty reason.
type captureFailure struct {
	status int
	msg    string
	reason string
}

func (a *app) captureConfigSnapshot(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, []operation{configExportOperation}) {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()
	out, fail := a.captureSnapshot(ctx, deviceID, user.ID, false)
	if fail != nil {
		if fail.reason != "" {
			a.recordHealthCheckOutcome(r, "device.config_snapshot.failed", deviceID, fail.reason)
		}
		writeError(w, fail.status, fail.msg)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// captureSnapshot exports a device's configuration and stores it if it changed. An
// empty userID means the scheduler captured it; unchanged scheduled captures are not
// audited, so a quiet fleet does not fill the audit log. ownChange marks a capture
// taken right after OpenManage applied a change, which is not drift.
func (a *app) captureSnapshot(ctx context.Context, deviceID, userID string, ownChange bool) (captureResult, *captureFailure) {
	var out captureResult
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		status, msg := targetError(err)
		return out, &captureFailure{status: status, msg: msg}
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		return out, &captureFailure{http.StatusBadGateway, "could not open a verified SSH session to the device", "ssh_failed"}
	}
	result := session.run(configExportOperation)
	session.Close()
	if result.Status != "ok" {
		return out, &captureFailure{http.StatusBadGateway, "configuration export failed: " + result.Error, "export_failed"}
	}
	cfg, err := parseUCIExport(result.Output)
	if err != nil {
		return out, &captureFailure{http.StatusBadGateway, "the device did not return a UCI configuration export", "not_uci"}
	}
	key, err := encryptionKey()
	if err != nil {
		return out, &captureFailure{status: http.StatusServiceUnavailable, msg: "SSH operations are disabled: server encryption key is not configured"}
	}
	digest := configDigest(key, result.Output)
	storeFailed := &captureFailure{status: http.StatusInternalServerError, msg: "could not store the configuration snapshot"}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return out, storeFailed
	}
	defer tx.Rollback(ctx)
	var prevID int64
	var prevDigest string
	var prevCiphertext []byte
	err = tx.QueryRow(ctx, `SELECT id, digest, ciphertext FROM config_snapshots WHERE device_id=$1
		ORDER BY captured_at DESC, id DESC LIMIT 1 FOR UPDATE`, deviceID).Scan(&prevID, &prevDigest, &prevCiphertext)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		out.First = true
	case err != nil:
		return out, &captureFailure{status: http.StatusInternalServerError, msg: "could not load the previous snapshot"}
	}

	if !out.First && prevDigest == digest {
		err = scanConfigSnapshot(tx.QueryRow(ctx, `UPDATE config_snapshots SET last_verified_at=NOW() WHERE id=$1
			RETURNING `+configSnapshotColumns, prevID), &out.Snapshot)
	} else {
		if !out.First {
			out.Changed = true
			if prevText, derr := openSecret(key, prevCiphertext, configSnapshotAAD(deviceID)); derr == nil {
				if prevCfg, perr := parseUCIExport(string(prevText)); perr == nil {
					out.ChangeCount = len(diffUCI(prevCfg, cfg))
				}
			}
		}
		var ciphertext []byte
		ciphertext, err = sealSecret(key, []byte(result.Output), configSnapshotAAD(deviceID))
		if err == nil {
			err = scanConfigSnapshot(tx.QueryRow(ctx, `INSERT INTO config_snapshots(device_id,captured_by,digest,size_bytes,option_count,ciphertext,drift)
				VALUES($1,NULLIF($2,'')::uuid,$3,$4,$5,$6,$7)
				RETURNING `+configSnapshotColumns,
				deviceID, userID, digest, len(result.Output), len(cfg), ciphertext, out.Changed && !ownChange), &out.Snapshot)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM config_snapshots WHERE device_id=$1 AND id NOT IN (
				SELECT id FROM config_snapshots WHERE device_id=$1 ORDER BY captured_at DESC, id DESC LIMIT $2)`,
				deviceID, configSnapshotsPerDevice)
		}
	}
	if err == nil && (userID != "" || out.First || out.Changed) {
		err = insertAuditEvent(ctx, tx, userID, "device.config_snapshot", "device", deviceID, map[string]any{
			"snapshot_id": out.Snapshot.ID, "first": out.First, "changed": out.Changed, "change_count": out.ChangeCount,
			"size_bytes": out.Snapshot.SizeBytes, "scheduled": userID == "",
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

func (a *app) listConfigSnapshots(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, []operation{configExportOperation}) {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT `+configSnapshotColumns+`
		FROM config_snapshots WHERE device_id=$1 ORDER BY captured_at DESC, id DESC LIMIT $2`, deviceID, configSnapshotsPerDevice)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load configuration snapshots")
		return
	}
	defer rows.Close()
	out := []configSnapshot{}
	for rows.Next() {
		var s configSnapshot
		if err := scanConfigSnapshot(rows, &s); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read configuration snapshots")
			return
		}
		out = append(out, s)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read configuration snapshots")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// configSnapshotChanges returns what changed between a snapshot and the one before
// it, with secret-looking values redacted. The raw configuration is never returned.
func (a *app) configSnapshotChanges(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	snapshotID, err := strconv.ParseInt(r.PathValue("snapshot"), 10, 64)
	if !validUUID(deviceID) || err != nil || snapshotID < 1 {
		writeError(w, http.StatusBadRequest, "invalid device or snapshot ID")
		return
	}
	if !requireOperationRoles(w, r, []operation{configExportOperation}) {
		return
	}
	key, err := encryptionKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server encryption key is not configured")
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT id, ciphertext FROM config_snapshots
		WHERE device_id=$1 AND (captured_at, id) <= (SELECT captured_at, id FROM config_snapshots WHERE id=$2 AND device_id=$1)
		ORDER BY captured_at DESC, id DESC LIMIT 2`, deviceID, snapshotID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load configuration snapshots")
		return
	}
	var configs []uciConfig
	for rows.Next() {
		var id int64
		var ciphertext []byte
		if err := rows.Scan(&id, &ciphertext); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "could not read configuration snapshots")
			return
		}
		text, err := openSecret(key, ciphertext, configSnapshotAAD(deviceID))
		if err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "stored snapshot could not be decrypted; verify the configured encryption key")
			return
		}
		cfg, err := parseUCIExport(string(text))
		if err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "stored snapshot could not be parsed")
			return
		}
		configs = append(configs, cfg)
	}
	rows.Close()
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read configuration snapshots")
		return
	}
	switch len(configs) {
	case 0:
		writeError(w, http.StatusNotFound, "snapshot not found")
	case 1:
		writeJSON(w, http.StatusOK, map[string]any{"snapshot_id": snapshotID, "first": true, "changes": []configChange{}})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"snapshot_id": snapshotID, "first": false, "changes": diffUCI(configs[1], configs[0])})
	}
}

// acknowledgeConfigDrift marks a snapshot's drift, and any earlier unacknowledged
// drift on the device, as reviewed so it no longer counts on the dashboard.
func (a *app) acknowledgeConfigDrift(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	snapshotID, err := strconv.ParseInt(r.PathValue("snapshot"), 10, 64)
	if !validUUID(deviceID) || err != nil || snapshotID < 1 {
		writeError(w, http.StatusBadRequest, "invalid device or snapshot ID")
		return
	}
	if !requireOperationRoles(w, r, []operation{configExportOperation}) {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not acknowledge configuration drift")
		return
	}
	defer tx.Rollback(r.Context())
	var capturedAt time.Time
	err = tx.QueryRow(r.Context(), `SELECT captured_at FROM config_snapshots WHERE id=$1 AND device_id=$2`, snapshotID, deviceID).Scan(&capturedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	var acknowledged int64
	if err == nil {
		var tag pgconn.CommandTag
		tag, err = tx.Exec(r.Context(), `UPDATE config_snapshots SET acknowledged_at=NOW(), acknowledged_by=$1
			WHERE device_id=$2 AND drift AND acknowledged_at IS NULL AND (captured_at, id) <= ($3, $4)`,
			user.ID, deviceID, capturedAt, snapshotID)
		acknowledged = tag.RowsAffected()
	}
	if err == nil && acknowledged > 0 {
		err = insertAuditEvent(r.Context(), tx, user.ID, "device.config_drift.acknowledge", "device", deviceID,
			map[string]any{"snapshot_id": snapshotID, "acknowledged": acknowledged})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not acknowledge configuration drift")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot_id": snapshotID, "acknowledged": acknowledged})
}
