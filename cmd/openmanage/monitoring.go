package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

const (
	monitorPollInterval       = 15 * time.Second
	monitorBatchLimit         = 5
	monitorWorkers            = 3
	monitorAdvisoryLock int64 = 0x4f50454e4d4f4e
	monitorCheckTimeout       = 20 * time.Second
)

type monitoringSettings struct {
	Enabled         bool      `json:"enabled"`
	IntervalSeconds int       `json:"interval_seconds"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type monitoringCheck struct {
	ID         int64     `json:"id"`
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMS int64     `json:"duration_ms"`
}

func (a *app) getMonitoringSettings(w http.ResponseWriter, r *http.Request) {
	var settings monitoringSettings
	err := a.db.QueryRow(r.Context(), `SELECT enabled, interval_seconds, updated_at
		FROM monitoring_settings WHERE id=TRUE`).Scan(&settings.Enabled, &settings.IntervalSeconds, &settings.UpdatedAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load monitoring settings")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (a *app) updateMonitoringSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in struct {
		Enabled         bool `json:"enabled"`
		IntervalSeconds int  `json:"interval_seconds"`
	}
	if err := decodeJSONBody(w, r, monitoringBodyLimit, &in); err != nil {
		if errors.Is(err, errTrailingJSON) {
			writeError(w, http.StatusBadRequest, "request must contain a single JSON object")
		} else {
			writeError(w, http.StatusBadRequest, "invalid monitoring settings")
		}
		return
	}
	if in.IntervalSeconds < 60 || in.IntervalSeconds > 3600 {
		writeError(w, http.StatusBadRequest, "monitoring interval must be between 60 and 3600 seconds")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save monitoring settings")
		return
	}
	defer tx.Rollback(r.Context())
	var settings monitoringSettings
	err = tx.QueryRow(r.Context(), `UPDATE monitoring_settings
		SET enabled=$1, interval_seconds=$2, updated_at=NOW()
		WHERE id=TRUE
		RETURNING enabled, interval_seconds, updated_at`, in.Enabled, in.IntervalSeconds).
		Scan(&settings.Enabled, &settings.IntervalSeconds, &settings.UpdatedAt)
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "monitoring.settings.update", "monitoring", "settings",
			map[string]any{"enabled": in.Enabled, "interval_seconds": in.IntervalSeconds})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save monitoring settings and audit event")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (a *app) listMonitoringHistory(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	rows, err := a.db.Query(r.Context(), `SELECT c.id, c.device_id::text, d.name, c.status,
		c.reason, c.started_at, c.finished_at, c.duration_ms
		FROM monitoring_checks c JOIN devices d ON d.id=c.device_id
		ORDER BY c.finished_at DESC, c.id DESC LIMIT $1`, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load monitoring history")
		return
	}
	defer rows.Close()
	out := make([]monitoringCheck, 0, limit)
	for rows.Next() {
		var c monitoringCheck
		if err := rows.Scan(&c.ID, &c.DeviceID, &c.DeviceName, &c.Status, &c.Reason,
			&c.StartedAt, &c.FinishedAt, &c.DurationMS); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read monitoring history")
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not read monitoring history")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(monitorPollInterval)
	defer ticker.Stop()
	lastPrune := time.Time{}
	log.Printf("persistent monitoring scheduler started")
	for {
		select {
		case <-ctx.Done():
			log.Printf("persistent monitoring scheduler stopped")
			return
		case <-ticker.C:
			if lastPrune.IsZero() || time.Since(lastPrune) >= 24*time.Hour {
				a.pruneMonitoringHistory(ctx)
				lastPrune = time.Now()
			}
			a.runDueMonitoringChecks(ctx)
		}
	}
}

func (a *app) pruneMonitoringHistory(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	// Keep a bounded, useful history without requiring a separate maintenance service.
	_, _ = a.db.Exec(ctx, "DELETE FROM monitoring_checks WHERE finished_at < NOW() - INTERVAL '30 days'")
}

func (a *app) runDueMonitoringChecks(ctx context.Context) {
	// A PostgreSQL session-level advisory lock prevents duplicate polling if a
	// second OpenManage instance is started during an upgrade or recovery.
	lockCtx, lockCancel := context.WithTimeout(ctx, 3*time.Second)
	lockConn, err := a.db.Acquire(lockCtx)
	lockCancel()
	if err != nil {
		return
	}
	defer lockConn.Release()
	var locked bool
	if err := lockConn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", monitorAdvisoryLock).Scan(&locked); err != nil || !locked {
		return
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var unlocked bool
		if err := lockConn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", monitorAdvisoryLock).Scan(&unlocked); err != nil || !unlocked {
			log.Printf("monitoring advisory lock release failed")
		}
	}()

	var settings monitoringSettings
	settingsCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err = lockConn.QueryRow(settingsCtx, `SELECT enabled, interval_seconds, updated_at
		FROM monitoring_settings WHERE id=TRUE`).Scan(&settings.Enabled, &settings.IntervalSeconds, &settings.UpdatedAt)
	cancel()
	if err != nil || !settings.Enabled {
		return
	}

	queryCtx, queryCancel := context.WithTimeout(ctx, 5*time.Second)
	rows, err := lockConn.Query(queryCtx, `SELECT d.id::text
		FROM devices d
		JOIN device_credentials dc ON dc.device_id=d.id
		LEFT JOIN LATERAL (
			SELECT finished_at FROM monitoring_checks mc
			WHERE mc.device_id=d.id ORDER BY mc.finished_at DESC, mc.id DESC LIMIT 1
		) last_check ON TRUE
		WHERE last_check.finished_at IS NULL
		   OR last_check.finished_at <= NOW() - ($1 * INTERVAL '1 second')
		ORDER BY COALESCE(last_check.finished_at, TIMESTAMPTZ 'epoch'), d.created_at
		LIMIT $2`, settings.IntervalSeconds, monitorBatchLimit)
	if err != nil {
		queryCancel()
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			queryCancel()
			log.Printf("monitoring schedule query returned an invalid row")
			return
		}
		ids = append(ids, id)
	}
	rowsErr := rows.Err()
	rows.Close()
	queryCancel()
	if rowsErr != nil {
		log.Printf("monitoring schedule query failed while reading rows")
		return
	}
	if len(ids) == 0 {
		return
	}

	// Run a small bounded worker pool so one slow router does not delay every
	// other router in the batch, without opening an unbounded number of SSH sessions.
	workers := monitoringWorkerCount(len(ids))
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					continue
				}
				a.runPersistentCheck(ctx, id)
			}
		}()
	}
dispatch:
	for _, id := range ids {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- id:
		}
	}
	close(jobs)
	wg.Wait()
}

func monitoringWorkerCount(batchSize int) int {
	if batchSize <= 0 {
		return 0
	}
	if batchSize < monitorWorkers {
		return batchSize
	}
	return monitorWorkers
}

func (a *app) runPersistentCheck(parent context.Context, deviceID string) {
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(parent, monitorCheckTimeout+3*time.Second)
	defer cancel()

	status, reason := a.monitorDevice(ctx, deviceID)
	finished := time.Now().UTC()
	duration := finished.Sub(started).Milliseconds()

	tx, err := a.db.Begin(ctx)
	if err != nil {
		log.Printf("monitoring result transaction could not start")
		return
	}
	defer tx.Rollback(ctx)
	if status != "error" {
		if status == "online" || status == "degraded" {
			_, err = tx.Exec(ctx, `UPDATE devices SET status=$1,last_seen_at=$2,updated_at=NOW() WHERE id=$3`, status, finished, deviceID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE devices SET status=$1,updated_at=NOW() WHERE id=$2`, status, deviceID)
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO monitoring_checks(device_id,status,reason,started_at,finished_at,duration_ms)
			VALUES($1,$2,$3,$4,$5,$6)`, deviceID, status, reason, started, finished, duration)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		log.Printf("monitoring result persistence failed")
	}
}

func (a *app) monitorDevice(ctx context.Context, deviceID string) (string, string) {
	var address, username, fingerprint string
	var ciphertext []byte
	err := a.db.QueryRow(ctx, `SELECT d.address, c.username, c.private_key_ciphertext, c.host_key_fingerprint
		FROM devices d JOIN device_credentials c ON c.device_id=d.id WHERE d.id=$1`, deviceID).
		Scan(&address, &username, &ciphertext, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return "error", "device is not enrolled"
	}
	if err != nil {
		return "error", "could not load device credentials"
	}
	if !isPrivateDeviceIP(address) {
		return "error", "device address is not an allowed private IP"
	}
	key, err := encryptionKey()
	if err != nil {
		return "error", "server encryption key is unavailable"
	}
	privateKey, err := decryptSecret(key, ciphertext)
	if err != nil {
		return "error", "stored credential could not be decrypted"
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return "error", "stored credential is invalid"
	}

	deadline := time.Now().Add(monitorCheckTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	client, err := dialPinnedSSH(ctx, net.JoinHostPort(address, "22"), username, signer, fingerprint, deadline)
	var dialErr *sshDialError
	if errors.As(err, &dialErr) {
		return "offline", "SSH port unreachable"
	}
	if err != nil {
		// Do not label authentication or host-key verification failures as device outages.
		return "error", "SSH authentication or pinned host-key verification failed"
	}
	defer client.Close()
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	failures := 0
	for _, command := range openWrtHealthCommands {
		if ctx.Err() != nil {
			return "degraded", "health check timed out"
		}
		result := runHealthCommand(client, command)
		if result.Status != "ok" {
			failures++
		}
	}
	if failures > 0 {
		return "degraded", strings.TrimSpace(strconv.Itoa(failures) + " read-only health command(s) failed")
	}
	return "online", "SSH and read-only health checks passed"
}
