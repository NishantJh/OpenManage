package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

const (
	healthCheckTimeout = 20 * time.Second
	healthOutputLimit  = 8 * 1024
)

type healthCommand struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Command string `json:"-"`
}

type healthResult struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Status   string `json:"status"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration_ms"`
}

type healthCheckResponse struct {
	DeviceID  string         `json:"device_id"`
	CheckedAt time.Time      `json:"checked_at"`
	Status    string         `json:"status"`
	Results   []healthResult `json:"results"`
}

var openWrtHealthCommands = []healthCommand{
	{Name: "system_board", Label: "System information", Command: "ubus call system board"},
	{Name: "uptime", Label: "Uptime and load", Command: "uptime"},
	{Name: "memory", Label: "Memory", Command: "free"},
	{Name: "overlay_storage", Label: "Overlay storage", Command: "df -h /overlay"},
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		return 0, errors.New("output limit exceeded")
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		return remaining, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func runHealthCommand(client *ssh.Client, command healthCommand) healthResult {
	result := healthResult{Name: command.Name, Label: command.Label}
	start := time.Now()
	session, err := client.NewSession()
	if err != nil {
		result.Status = "error"
		result.Error = "could not open SSH session"
		result.Duration = time.Since(start).Milliseconds()
		return result
	}
	defer session.Close()

	stdout := &boundedBuffer{limit: healthOutputLimit}
	stderr := &boundedBuffer{limit: 1024}
	session.Stdout = stdout
	session.Stderr = stderr
	err = session.Run(command.Command)
	result.Duration = time.Since(start).Milliseconds()
	result.Output = strings.TrimSpace(stdout.String())
	if err != nil {
		result.Status = "error"
		if strings.Contains(err.Error(), "output limit exceeded") {
			result.Error = "device output exceeded the allowed limit"
		} else {
			result.Error = "command failed or is unsupported by this firmware"
		}
		return result
	}
	result.Status = "ok"
	return result
}

func (a *app) runDeviceHealthCheck(w http.ResponseWriter, r *http.Request) {
	if !validUUID(r.PathValue("id")) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin", "operator", "readonly") {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	var address, name string
	err := a.db.QueryRow(ctx, "SELECT name,address FROM devices WHERE id=$1", r.PathValue("id")).Scan(&name, &address)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load device")
		return
	}
	if !isPrivateDeviceIP(address) {
		writeError(w, http.StatusUnprocessableEntity, "health checks only permit private IP addresses")
		return
	}

	var username, fingerprint string
	var ciphertext []byte
	err = a.db.QueryRow(ctx, "SELECT username,private_key_ciphertext,host_key_fingerprint FROM device_credentials WHERE device_id=$1", r.PathValue("id")).Scan(&username, &ciphertext, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "device must be enrolled before running a health check")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load device credentials")
		return
	}

	key, err := encryptionKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SSH operations are disabled: server encryption key is not configured")
		return
	}
	privateKey, err := decryptSecret(key, ciphertext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stored SSH credential could not be decrypted; verify the configured encryption key and restore")
		return
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stored SSH credential is not a supported private key")
		return
	}

	deadline := time.Now().Add(healthCheckTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	client, err := dialPinnedSSH(ctx, net.JoinHostPort(address, "22"), username, signer, fingerprint, deadline)
	var dialErr *sshDialError
	if errors.As(err, &dialErr) {
		a.recordHealthCheckOutcome(r, "device.health_check.failed", r.PathValue("id"), "connection_failed")
		writeError(w, http.StatusBadGateway, "could not connect to the enrolled device over SSH")
		return
	}
	if err != nil {
		a.recordHealthCheckOutcome(r, "device.health_check.failed", r.PathValue("id"), "ssh_or_host_key_failed")
		writeError(w, http.StatusBadGateway, "SSH connection or pinned host-key verification failed")
		return
	}
	defer client.Close()

	results := make([]healthResult, 0, len(openWrtHealthCommands))
	failures := 0
	for _, command := range openWrtHealthCommands {
		if ctx.Err() != nil {
			failures++
			results = append(results, healthResult{Name: command.Name, Label: command.Label, Status: "error", Error: "health check timed out"})
			break
		}
		result := runHealthCommand(client, command)
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
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "health check completed but its result could not be persisted")
		return
	}
	defer tx.Rollback(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, "UPDATE devices SET status=$1,last_seen_at=NOW(),updated_at=NOW() WHERE id=$2", deviceStatus, r.PathValue("id"))
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, r.Context().Value(userContextKey).(authUser).ID, "device.health_check", "device", r.PathValue("id"), map[string]any{
			"device_name": name, "outcome": outcome, "commands_run": len(results), "commands_failed": failures,
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
