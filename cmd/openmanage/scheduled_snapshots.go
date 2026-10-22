package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// snapshotRetryDelay keeps the scheduler from re-exporting an unreachable or broken
// device on every poll; it is retried after this delay instead of on the next tick.
const snapshotRetryDelay = 15 * time.Minute

// retryTracker remembers recent failures per device. It lives in memory: after a
// restart every device is simply tried once more.
type retryTracker struct {
	mu     sync.Mutex
	failed map[string]time.Time
}

func (t *retryTracker) fail(id string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed == nil {
		t.failed = map[string]time.Time{}
	}
	t.failed[id] = at
}

func (t *retryTracker) succeed(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failed, id)
}

// waiting lists devices that failed less than delay ago, and forgets older failures.
func (t *retryTracker) waiting(now time.Time, delay time.Duration) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := []string{}
	for id, at := range t.failed {
		if now.Sub(at) < delay {
			ids = append(ids, id)
		} else {
			delete(t.failed, id)
		}
	}
	return ids
}

// runSnapshotBatch exports the configuration of enrolled devices whose newest
// snapshot is older than the snapshot interval. It runs under the monitoring lock.
func (a *app) runSnapshotBatch(ctx context.Context, lockConn *pgxpool.Conn, settings monitoringSettings) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ids, err := queryDeviceIDs(queryCtx, lockConn, `SELECT d.id::text
		FROM devices d
		JOIN device_credentials dc ON dc.device_id=d.id
		LEFT JOIN LATERAL (
			SELECT last_verified_at FROM config_snapshots cs
			WHERE cs.device_id=d.id ORDER BY cs.captured_at DESC, cs.id DESC LIMIT 1
		) last_snapshot ON TRUE
		WHERE (last_snapshot.last_verified_at IS NULL
		   OR last_snapshot.last_verified_at <= NOW() - ($1 * INTERVAL '1 second'))
		  AND NOT (d.id = ANY($2::uuid[]))
		ORDER BY COALESCE(last_snapshot.last_verified_at, TIMESTAMPTZ 'epoch'), d.created_at
		LIMIT $3`, settings.SnapshotIntervalSeconds, a.snapshotRetry.waiting(time.Now(), snapshotRetryDelay), monitorBatchLimit)
	cancel()
	if err != nil {
		log.Printf("snapshot schedule query failed")
		return
	}
	runBounded(ctx, ids, a.runScheduledSnapshot)
}

func (a *app) runScheduledSnapshot(parent context.Context, deviceID string) {
	ctx, cancel := context.WithTimeout(parent, healthCheckTimeout+3*time.Second)
	defer cancel()
	if _, fail := a.captureSnapshot(ctx, deviceID, "", false); fail != nil {
		a.snapshotRetry.fail(deviceID, time.Now())
		// The message is one of captureSnapshot's fixed, secret-free strings.
		log.Printf("scheduled configuration snapshot failed for device %s: %s", deviceID, fail.msg)
		return
	}
	a.snapshotRetry.succeed(deviceID)
}
