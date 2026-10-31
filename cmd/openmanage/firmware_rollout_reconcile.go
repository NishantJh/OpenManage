package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

func (a *app) firmwareRolloutReconcilerLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	a.reconcileFirmwareRollouts(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.reconcileFirmwareRollouts(ctx)
		}
	}
}

func (a *app) reconcileFirmwareRollouts(ctx context.Context) {
	rows, err := a.db.Query(ctx, `SELECT id FROM firmware_rollouts
		WHERE status IN ('canary_running','batch_running') ORDER BY id`)
	if err != nil {
		log.Printf("firmware rollout reconciliation query failed")
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return
	}
	rows.Close()
	for _, id := range ids {
		a.reconcileFirmwareRollout(ctx, id)
	}
}

func (a *app) reconcileFirmwareRollout(ctx context.Context, id int64) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var status, createdBy string
	var currentBatch int
	err = tx.QueryRow(ctx, `SELECT status,current_batch,COALESCE(created_by::text,'')
		FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, id).Scan(&status, &currentBatch, &createdBy)
	if err != nil || (status != "canary_running" && status != "batch_running") {
		return
	}
	rows, err := tx.Query(ctx, `SELECT rd.device_id_snapshot::text,rd.device_name,rd.batch_number,
		rd.status,u.status,u.acknowledged_at,u.detail
		FROM firmware_rollout_devices rd LEFT JOIN firmware_upgrades u ON u.id=rd.upgrade_id
		WHERE rd.rollout_id=$1 ORDER BY rd.batch_number,rd.position`, id)
	if err != nil {
		return
	}
	type memberState struct {
		deviceID string
		name string
		batch int
		memberStatus string
		jobStatus *string
		acknowledgedAt *time.Time
		detail string
	}
	members := []memberState{}
	for rows.Next() {
		var m memberState
		if err := rows.Scan(&m.deviceID, &m.name, &m.batch, &m.memberStatus, &m.jobStatus, &m.acknowledgedAt, &m.detail); err != nil {
			rows.Close()
			return
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return
	}
	rows.Close()

	pauseReason := ""
	for _, member := range members {
		if member.jobStatus == nil {
			continue
		}
		next := member.memberStatus
		switch *member.jobStatus {
		case "preparing", "ready", "flashing", "verifying", "done":
			next = *member.jobStatus
		case "done_with_warnings", "needs_attention", "stopped", "cancelled":
			next = "needs_attention"
			pauseReason = fmt.Sprintf("%s: firmware job ended as %s. Review the device and recover it before creating a new rollout.", member.name, *member.jobStatus)
		}
		if next != member.memberStatus {
			if _, err := tx.Exec(ctx, `UPDATE firmware_rollout_devices SET status=$3,updated_at=NOW()
				WHERE rollout_id=$1 AND device_id_snapshot=$2`, id, member.deviceID, next); err != nil {
				return
			}
		}
	}
	if pauseReason != "" {
		if _, err := tx.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW()
			WHERE id=$1 AND status IN ('canary_running','batch_running')`, id, pauseReason); err != nil {
			return
		}
		if createdBy != "" {
			_ = insertAuditEvent(ctx, tx, createdBy, "firmware.rollout_paused", "firmware_rollout",
				fmt.Sprint(id), map[string]any{"reason": pauseReason})
		}
		_ = tx.Commit(ctx)
		return
	}

	var total, acknowledged int
	err = tx.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE u.status='done' AND u.acknowledged_at IS NOT NULL)
		FROM firmware_rollout_devices rd LEFT JOIN firmware_upgrades u ON u.id=rd.upgrade_id
		WHERE rd.rollout_id=$1 AND rd.batch_number=$2`, id, currentBatch).Scan(&total, &acknowledged)
	if err != nil || total == 0 || acknowledged != total {
		_ = tx.Commit(ctx)
		return
	}
	var nextBatch *int
	err = tx.QueryRow(ctx, `SELECT min(batch_number) FROM firmware_rollout_devices
		WHERE rollout_id=$1 AND batch_number>$2`, id, currentBatch).Scan(&nextBatch)
	if err != nil {
		return
	}
	var action string
	var details map[string]any
	if nextBatch == nil {
		_, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='complete',completed_at=NOW(),
			pause_reason='',updated_at=NOW() WHERE id=$1 AND current_batch=$2
			AND status IN ('canary_running','batch_running')`, id, currentBatch)
		action = "firmware.rollout_complete"
		details = map[string]any{"completed_batch": currentBatch}
	} else {
		_, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='awaiting_batch',current_batch=$2,
			pause_reason='',updated_at=NOW() WHERE id=$1 AND current_batch=$3
			AND status IN ('canary_running','batch_running')`, id, *nextBatch, currentBatch)
		action = "firmware.rollout_batch_advance"
		details = map[string]any{"completed_batch": currentBatch, "released_batch": *nextBatch}
	}
	if err != nil {
		return
	}
	if createdBy != "" {
		if err = insertAuditEvent(ctx, tx, createdBy, action, "firmware_rollout", fmt.Sprint(id), details); err != nil {
			return
		}
	}
	_ = tx.Commit(ctx)
}

var _ = pgx.ErrNoRows
