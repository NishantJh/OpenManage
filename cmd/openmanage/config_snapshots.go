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
	ID             int64     `json:"id"`
	DeviceID       string    `json:"device_id"`
	CapturedAt     time.Time `json:"captured_at"`
	LastVerifiedAt time.Time `json:"last_verified_at"`
	SizeBytes      int       `json:"size_bytes"`
	OptionCount    int       `json:"option_count"`
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

	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		status, msg := targetError(err)
		writeError(w, status, msg)
		return
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		a.recordHealthCheckOutcome(r, "device.config_snapshot.failed", deviceID, "ssh_failed")
		writeError(w, http.StatusBadGateway, "could not open a verified SSH session to the device")
		return
	}
	result := session.run(configExportOperation)
	session.Close()
	if result.Status != "ok" {
		a.recordHealthCheckOutcome(r, "device.config_snapshot.failed", deviceID, "export_failed")
		writeError(w, http.StatusBadGateway, "configuration export failed: "+result.Error)
		return
	}
	cfg, err := parseUCIExport(result.Output)
	if err != nil {
		a.recordHealthCheckOutcome(r, "device.config_snapshot.failed", deviceID, "not_uci")
		writeError(w, http.StatusBadGateway, "the device did not return a UCI configuration export")
		return
	}
	key, err := encryptionKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SSH operations are disabled: server encryption key is not configured")
		return
	}
	digest := configDigest(key, result.Output)

	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not store the configuration snapshot")
		return
	}
	defer tx.Rollback(ctx)
	var out captureResult
	var prevID int64
	var prevDigest string
	var prevCiphertext []byte
	err = tx.QueryRow(ctx, `SELECT id, digest, ciphertext FROM config_snapshots WHERE device_id=$1
		ORDER BY captured_at DESC, id DESC LIMIT 1 FOR UPDATE`, deviceID).Scan(&prevID, &prevDigest, &prevCiphertext)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		out.First = true
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not load the previous snapshot")
		return
	}

	if !out.First && prevDigest == digest {
		err = tx.QueryRow(ctx, `UPDATE config_snapshots SET last_verified_at=NOW() WHERE id=$1
			RETURNING id, device_id::text, captured_at, last_verified_at, size_bytes, option_count`, prevID).
			Scan(&out.Snapshot.ID, &out.Snapshot.DeviceID, &out.Snapshot.CapturedAt, &out.Snapshot.LastVerifiedAt, &out.Snapshot.SizeBytes, &out.Snapshot.OptionCount)
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
			err = tx.QueryRow(ctx, `INSERT INTO config_snapshots(device_id,captured_by,digest,size_bytes,option_count,ciphertext)
				VALUES($1,$2,$3,$4,$5,$6)
				RETURNING id, device_id::text, captured_at, last_verified_at, size_bytes, option_count`,
				deviceID, user.ID, digest, len(result.Output), len(cfg), ciphertext).
				Scan(&out.Snapshot.ID, &out.Snapshot.DeviceID, &out.Snapshot.CapturedAt, &out.Snapshot.LastVerifiedAt, &out.Snapshot.SizeBytes, &out.Snapshot.OptionCount)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM config_snapshots WHERE device_id=$1 AND id NOT IN (
				SELECT id FROM config_snapshots WHERE device_id=$1 ORDER BY captured_at DESC, id DESC LIMIT $2)`,
				deviceID, configSnapshotsPerDevice)
		}
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, user.ID, "device.config_snapshot", "device", deviceID, map[string]any{
			"snapshot_id": out.Snapshot.ID, "first": out.First, "changed": out.Changed, "change_count": out.ChangeCount,
			"size_bytes": out.Snapshot.SizeBytes,
		})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not store the configuration snapshot")
		return
	}
	writeJSON(w, http.StatusOK, out)
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
	rows, err := a.db.Query(r.Context(), `SELECT id, device_id::text, captured_at, last_verified_at, size_bytes, option_count
		FROM config_snapshots WHERE device_id=$1 ORDER BY captured_at DESC, id DESC LIMIT $2`, deviceID, configSnapshotsPerDevice)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load configuration snapshots")
		return
	}
	defer rows.Close()
	out := []configSnapshot{}
	for rows.Next() {
		var s configSnapshot
		if err := rows.Scan(&s.ID, &s.DeviceID, &s.CapturedAt, &s.LastVerifiedAt, &s.SizeBytes, &s.OptionCount); err != nil {
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
