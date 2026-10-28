package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	monitorPollInterval       = 15 * time.Second
	monitorBatchLimit         = 5
	monitorWorkers            = 3
	monitorAdvisoryLock int64 = 0x4f50454e4d4f4e
	monitorCheckTimeout       = 20 * time.Second
)

type monitoringSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
	// SnapshotsEnabled turns on scheduled configuration snapshots, independently of
	// health monitoring; SnapshotIntervalSeconds is how often each device is exported.
	SnapshotsEnabled        bool `json:"snapshots_enabled"`
	SnapshotIntervalSeconds int  `json:"snapshot_interval_seconds"`
	// BackupsEnabled turns on scheduled device configuration backups (archives);
	// BackupIntervalSeconds is at least one day.
	BackupsEnabled        bool `json:"backups_enabled"`
	BackupIntervalSeconds int  `json:"backup_interval_seconds"`
	// BackupRetention is how many unpinned backups are kept per device (1 to 20).
	BackupRetention int `json:"backup_retention"`
	// ClientTrackingEnabled turns on Wi-Fi client tracking for the device location
	// map; ClientPollIntervalSeconds is how often each AP is asked for its clients.
	ClientTrackingEnabled     bool      `json:"client_tracking_enabled"`
	ClientPollIntervalSeconds int       `json:"client_poll_interval_seconds"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

const monitoringSettingsColumns = `enabled, interval_seconds, snapshots_enabled, snapshot_interval_seconds,
	client_tracking_enabled, client_poll_interval_seconds, updated_at, backups_enabled, backup_interval_seconds, backup_retention`

func scanMonitoringSettings(row pgx.Row, s *monitoringSettings) error {
	return row.Scan(&s.Enabled, &s.IntervalSeconds, &s.SnapshotsEnabled, &s.SnapshotIntervalSeconds,
		&s.ClientTrackingEnabled, &s.ClientPollIntervalSeconds, &s.UpdatedAt, &s.BackupsEnabled, &s.BackupIntervalSeconds, &s.BackupRetention)
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
	err := scanMonitoringSettings(a.db.QueryRow(r.Context(), `SELECT `+monitoringSettingsColumns+`
		FROM monitoring_settings WHERE id=TRUE`), &settings)
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
	// The snapshot fields are optional so older clients keep the current values.
	var in struct {
		Enabled                   bool  `json:"enabled"`
		IntervalSeconds           int   `json:"interval_seconds"`
		SnapshotsEnabled          *bool `json:"snapshots_enabled"`
		SnapshotIntervalSeconds   *int  `json:"snapshot_interval_seconds"`
		ClientTrackingEnabled     *bool `json:"client_tracking_enabled"`
		ClientPollIntervalSeconds *int  `json:"client_poll_interval_seconds"`
		BackupsEnabled            *bool `json:"backups_enabled"`
		BackupIntervalSeconds     *int  `json:"backup_interval_seconds"`
		BackupRetention           *int  `json:"backup_retention"`
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
	if in.SnapshotIntervalSeconds != nil && (*in.SnapshotIntervalSeconds < 900 || *in.SnapshotIntervalSeconds > 86400) {
		writeError(w, http.StatusBadRequest, "snapshot interval must be between 900 and 86400 seconds")
		return
	}
	if in.ClientPollIntervalSeconds != nil && (*in.ClientPollIntervalSeconds < clientPollMin || *in.ClientPollIntervalSeconds > clientPollMax) {
		writeError(w, http.StatusBadRequest, "client poll interval must be between 60 and 600 seconds")
		return
	}
	if in.BackupIntervalSeconds != nil && (*in.BackupIntervalSeconds < 86400 || *in.BackupIntervalSeconds > 2592000) {
		writeError(w, http.StatusBadRequest, "backup interval must be between 1 and 30 days")
		return
	}
	if in.BackupRetention != nil && (*in.BackupRetention < minBackupRetention || *in.BackupRetention > maxBackupRetention) {
		writeError(w, http.StatusBadRequest, "backup retention must be between 1 and 20")
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
	err = scanMonitoringSettings(tx.QueryRow(r.Context(), `UPDATE monitoring_settings
		SET enabled=$1, interval_seconds=$2, snapshots_enabled=COALESCE($3, snapshots_enabled),
			snapshot_interval_seconds=COALESCE($4, snapshot_interval_seconds),
			client_tracking_enabled=COALESCE($5, client_tracking_enabled),
			client_poll_interval_seconds=COALESCE($6, client_poll_interval_seconds),
			backups_enabled=COALESCE($7, backups_enabled), backup_interval_seconds=COALESCE($8, backup_interval_seconds),
			backup_retention=COALESCE($9, backup_retention), updated_at=NOW()
		WHERE id=TRUE
		RETURNING `+monitoringSettingsColumns, in.Enabled, in.IntervalSeconds, in.SnapshotsEnabled, in.SnapshotIntervalSeconds,
		in.ClientTrackingEnabled, in.ClientPollIntervalSeconds, in.BackupsEnabled, in.BackupIntervalSeconds, in.BackupRetention), &settings)
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "monitoring.settings.update", "monitoring", "settings",
			map[string]any{"enabled": settings.Enabled, "interval_seconds": settings.IntervalSeconds,
				"snapshots_enabled": settings.SnapshotsEnabled, "snapshot_interval_seconds": settings.SnapshotIntervalSeconds,
				"client_tracking_enabled": settings.ClientTrackingEnabled, "client_poll_interval_seconds": settings.ClientPollIntervalSeconds,
				"backups_enabled": settings.BackupsEnabled, "backup_interval_seconds": settings.BackupIntervalSeconds,
				"backup_retention": settings.BackupRetention})
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
	a.pruneWifiClients(ctx)
}

// runDueMonitoringChecks runs due health checks and, separately opt-in, due
// configuration snapshots.
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
	err = scanMonitoringSettings(lockConn.QueryRow(settingsCtx, `SELECT `+monitoringSettingsColumns+`
		FROM monitoring_settings WHERE id=TRUE`), &settings)
	cancel()
	if err != nil {
		return
	}
	if settings.Enabled {
		a.runMonitoringBatch(ctx, lockConn, settings)
	}
	if settings.SnapshotsEnabled {
		a.runSnapshotBatch(ctx, lockConn, settings)
	}
	if settings.BackupsEnabled {
		a.runBackupBatch(ctx, lockConn, settings)
	}
	if settings.ClientTrackingEnabled {
		a.runClientBatch(ctx, lockConn, settings)
	}
}

func (a *app) runMonitoringBatch(ctx context.Context, lockConn *pgxpool.Conn, settings monitoringSettings) {
	queryCtx, queryCancel := context.WithTimeout(ctx, 5*time.Second)
	ids, err := queryDeviceIDs(queryCtx, lockConn, `SELECT d.id::text
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
	queryCancel()
	if err != nil {
		log.Printf("monitoring schedule query failed")
		return
	}
	runBounded(ctx, ids, a.runPersistentCheck)
}

// queryDeviceIDs runs a query that returns one device ID per row, on a pool or a held connection.
func queryDeviceIDs(ctx context.Context, conn interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, sql string, args ...any) ([]string, error) {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// runBounded runs fn for each device on a small bounded worker pool, so one slow
// router does not delay every other router in the batch, without opening an
// unbounded number of SSH sessions.
func runBounded(ctx context.Context, ids []string, fn func(context.Context, string)) {
	if len(ids) == 0 {
		return
	}
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
				fn(ctx, id)
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

	status, reason, inv := a.monitorDevice(ctx, deviceID)
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
			_, err = tx.Exec(ctx, `UPDATE devices SET status=$1,last_seen_at=$2,updated_at=NOW(),
				model=COALESCE(NULLIF($4,''),model), firmware=COALESCE(NULLIF($5,''),firmware) WHERE id=$3`,
				status, finished, deviceID, inv.Model, inv.Firmware)
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

// monitorDevice returns the device status, a short reason, and any inventory it reported.
func (a *app) monitorDevice(ctx context.Context, deviceID string) (string, string, deviceInventory) {
	target, err := a.loadDeviceTarget(ctx, deviceID)
	switch {
	case err == nil:
	case errors.Is(err, errTargetNotFound), errors.Is(err, errTargetNotEnrolled):
		return "error", "device is not enrolled", deviceInventory{}
	case errors.Is(err, errTargetNotPrivate):
		return "error", "device address is not an allowed private IP", deviceInventory{}
	case errors.Is(err, errTargetKeyUnavailable):
		return "error", "server encryption key is unavailable", deviceInventory{}
	case errors.Is(err, errTargetDecrypt):
		return "error", "stored credential could not be decrypted", deviceInventory{}
	case errors.Is(err, errTargetBadCredential):
		return "error", "stored credential is invalid", deviceInventory{}
	default:
		return "error", "could not load device credentials", deviceInventory{}
	}

	session, err := openDeviceSession(ctx, target, monitorCheckTimeout)
	var dialErr *sshDialError
	if errors.As(err, &dialErr) {
		return "offline", "SSH port unreachable", deviceInventory{}
	}
	if err != nil {
		// Do not label authentication or host-key verification failures as device outages.
		return "error", "SSH authentication or pinned host-key verification failed", deviceInventory{}
	}
	defer session.Close()

	failures := 0
	results := make([]operationResult, 0, len(healthOperations))
	for _, op := range healthOperations {
		if ctx.Err() != nil {
			return "degraded", "health check timed out", inventoryFromResults(results)
		}
		result := session.run(op)
		if result.Status != "ok" {
			failures++
		}
		results = append(results, result)
	}
	inv := inventoryFromResults(results)
	if failures > 0 {
		return "degraded", strings.TrimSpace(strconv.Itoa(failures) + " read-only health command(s) failed"), inv
	}
	return "online", "SSH and read-only health checks passed", inv
}
