package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"
)

type healthCheckResponse struct {
	DeviceID  string            `json:"device_id"`
	CheckedAt time.Time         `json:"checked_at"`
	Status    string            `json:"status"`
	Results   []operationResult `json:"results"`
}

// boundedBuffer keeps at most limit bytes. It must not embed bytes.Buffer: io.Copy
// would then use the promoted ReadFrom and bypass the limit in Write.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		return 0, errors.New("output limit exceeded")
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		return remaining, errors.New("output limit exceeded")
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) String() string { return b.buf.String() }

func (a *app) runDeviceHealthCheck(w http.ResponseWriter, r *http.Request) {
	if !validUUID(r.PathValue("id")) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, healthOperations) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	target, err := a.loadDeviceTarget(ctx, r.PathValue("id"))
	switch {
	case err == nil:
	case errors.Is(err, errTargetNotFound):
		writeError(w, http.StatusNotFound, "device not found")
		return
	case errors.Is(err, errTargetNotPrivate):
		writeError(w, http.StatusUnprocessableEntity, "health checks only permit private IP addresses")
		return
	case errors.Is(err, errTargetNotEnrolled):
		writeError(w, http.StatusConflict, "device must be enrolled before running a health check")
		return
	case errors.Is(err, errTargetKeyUnavailable):
		writeError(w, http.StatusServiceUnavailable, "SSH operations are disabled: server encryption key is not configured")
		return
	case errors.Is(err, errTargetDecrypt):
		writeError(w, http.StatusInternalServerError, "stored SSH credential could not be decrypted; verify the configured encryption key and restore")
		return
	case errors.Is(err, errTargetBadCredential):
		writeError(w, http.StatusInternalServerError, "stored SSH credential is not a supported private key")
		return
	default:
		writeError(w, http.StatusInternalServerError, "could not load device")
		return
	}

	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	var dialErr *sshDialError
	if errors.As(err, &dialErr) {
		a.recordHealthCheckOutcome(r, "device.health_check.failed", target.ID, "connection_failed")
		writeError(w, http.StatusBadGateway, "could not connect to the enrolled device over SSH")
		return
	}
	if err != nil {
		a.recordHealthCheckOutcome(r, "device.health_check.failed", target.ID, "ssh_or_host_key_failed")
		writeError(w, http.StatusBadGateway, "SSH connection or pinned host-key verification failed")
		return
	}
	defer session.Close()

	results := make([]operationResult, 0, len(healthOperations))
	failures := 0
	for _, op := range healthOperations {
		if ctx.Err() != nil {
			failures++
			results = append(results, operationResult{Name: op.Name, Label: op.Label, Status: "error", Error: "health check timed out"})
			break
		}
		result := session.run(op)
		if result.Status != "ok" {
			failures++
		}
		results = append(results, result)
	}

	deviceStatus := "online"
	outcome := "success"
	if failures > 0 {
		deviceStatus = "degraded"
		outcome = "partial"
	}
	inv := inventoryFromResults(results)
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "health check completed but its result could not be persisted")
		return
	}
	defer tx.Rollback(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE devices SET status=$1,last_seen_at=NOW(),updated_at=NOW(),
			model=COALESCE(NULLIF($3,''),model), firmware=COALESCE(NULLIF($4,''),firmware) WHERE id=$2`,
			deviceStatus, r.PathValue("id"), inv.Model, inv.Firmware)
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, r.Context().Value(userContextKey).(authUser).ID, "device.health_check", "device", r.PathValue("id"), map[string]any{
			"device_name": target.Name, "outcome": outcome, "commands_run": len(results), "commands_failed": failures,
			"inventory_reported": inv.Model != "" || inv.Firmware != "",
		})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "health check completed but its result could not be persisted")
		return
	}

	writeJSON(w, http.StatusOK, healthCheckResponse{
		DeviceID: r.PathValue("id"), CheckedAt: time.Now().UTC(), Status: deviceStatus, Results: results,
	})
}

// recordHealthCheckOutcome writes a minimal, secret-free audit record for failed connection attempts.
func (a *app) recordHealthCheckOutcome(r *http.Request, action, deviceID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok {
		return
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	if err = insertAuditEvent(ctx, tx, user.ID, action, "device", deviceID, map[string]string{"reason": reason}); err == nil {
		_ = tx.Commit(ctx)
	}
}
