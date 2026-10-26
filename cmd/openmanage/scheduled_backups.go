package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// runBackupBatch takes a backup of enrolled devices whose newest backup was last
// verified more than the backup interval ago (never less than a day, enforced by the
// settings validation and the database). It runs under the monitoring lock.
func (a *app) runBackupBatch(ctx context.Context, lockConn *pgxpool.Conn, settings monitoringSettings) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ids, err := queryDeviceIDs(queryCtx, lockConn, `SELECT d.id::text
		FROM devices d
		JOIN device_credentials dc ON dc.device_id=d.id
		LEFT JOIN LATERAL (
			SELECT last_verified_at FROM device_backups b
			WHERE b.device_id=d.id ORDER BY b.captured_at DESC, b.id DESC LIMIT 1
		) last_backup ON TRUE
		WHERE (last_backup.last_verified_at IS NULL
		   OR last_backup.last_verified_at <= NOW() - ($1 * INTERVAL '1 second'))
		  AND NOT (d.id = ANY($2::uuid[]))
		ORDER BY COALESCE(last_backup.last_verified_at, TIMESTAMPTZ 'epoch'), d.created_at
		LIMIT $3`, settings.BackupIntervalSeconds, a.backupRetry.waiting(time.Now(), snapshotRetryDelay), monitorBatchLimit)
	cancel()
	if err != nil {
		log.Printf("backup schedule query failed")
		return
	}
	runBounded(ctx, ids, a.runScheduledBackup)
}

func (a *app) runScheduledBackup(parent context.Context, deviceID string) {
	ctx, cancel := context.WithTimeout(parent, backupJobTimeout)
	defer cancel()
	if _, fail := a.captureBackup(ctx, deviceID, ""); fail != nil {
		a.backupRetry.fail(deviceID, time.Now())
		// The message is one of captureBackup's fixed, secret-free strings.
		log.Printf("scheduled configuration backup failed for device %s: %s", deviceID, fail.msg)
		return
	}
	a.backupRetry.succeed(deviceID)
}
