package main

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const firmwareRolloutDefaultMaxConcurrentFlashes = 2

func (a *app) startFirmwareRollout(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("rollout"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid rollout ID")
		return
	}
	rollout, err := loadFirmwareRollout(r.Context(), a.db, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "rollout not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the rollout")
		return
	}
	if rollout.Status != "previewed" {
		writeError(w, http.StatusConflict, "only a completed preview can be released to the canary")
		return
	}
	for _, member := range rollout.Devices {
		if len(member.Plan) == 0 || member.PlanDigest == "" {
			writeError(w, http.StatusConflict, "every device must have a persisted plan preview before the rollout can start")
			return
		}
		current, err := firmwareRolloutMemberCurrent(r.Context(), a.db, member)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not validate rollout snapshots")
			return
		}
		if !current {
			writeError(w, http.StatusConflict, "a device identity, inventory, or desired package set changed; create a new rollout draft")
			return
		}
	}
	qualification, err := a.firmwareRolloutQualificationStatus(r.Context(), rollout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not verify hardware qualifications")
		return
	}
	if !qualification.Qualified {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "matching physical-hardware upgrade and recovery qualification is required for every hardware group",
			"qualification": qualification,
		})
		return
	}

	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the rollout")
		return
	}
	defer tx.Rollback(r.Context())
	deviceIDs := make([]string, 0, len(rollout.Devices))
	for _, member := range rollout.Devices {
		deviceIDs = append(deviceIDs, member.DeviceID)
	}
	sort.Strings(deviceIDs)
	rows, err := tx.Query(r.Context(), `SELECT id::text FROM devices WHERE id::text=ANY($1::text[]) ORDER BY id FOR UPDATE`, deviceIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not lock rollout devices")
		return
	}
	locked := 0
	for rows.Next() {
		var deviceID string
		if err = rows.Scan(&deviceID); err != nil {
			break
		}
		locked++
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not lock rollout devices")
		return
	}
	if locked != len(deviceIDs) {
		writeError(w, http.StatusConflict, "a rollout device was removed; create a new draft")
		return
	}
	var status string
	err = tx.QueryRow(r.Context(), `SELECT status FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "rollout not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not lock the rollout")
		return
	}
	if status != "previewed" {
		writeError(w, http.StatusConflict, "the rollout changed; reload it before starting")
		return
	}
	for _, member := range rollout.Devices {
		current, err := firmwareRolloutMemberCurrent(r.Context(), tx, member)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not recheck rollout snapshots")
			return
		}
		if !current {
			writeError(w, http.StatusConflict, "a device identity, inventory, or desired package set changed; create a new rollout draft")
			return
		}
		var conflict bool
		err = tx.QueryRow(r.Context(), `SELECT
			EXISTS(SELECT 1 FROM firmware_upgrades WHERE device_id=$1 AND status IN ('preparing','ready','flashing','verifying'))
			OR EXISTS(SELECT 1 FROM firmware_rollout_devices rd
				JOIN firmware_rollouts other ON other.id=rd.rollout_id
				WHERE rd.device_id_snapshot=$1 AND rd.rollout_id<>$2
				AND other.status IN ('awaiting_canary','canary_running','awaiting_batch','batch_running','paused'))`,
			member.DeviceID, id).Scan(&conflict)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not check device rollout conflicts")
			return
		}
		if conflict {
			writeError(w, http.StatusConflict, member.DeviceName+" has an open upgrade or is reserved by another rollout")
			return
		}
		q, err := findMatchingFirmwareHardwareQualification(r.Context(), tx, member, rollout.Version)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "a matching physical-hardware qualification is no longer available; review qualifications and create a new rollout")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not recheck hardware qualifications")
			return
		}
		if _, err = tx.Exec(r.Context(), `INSERT INTO firmware_rollout_qualifications(rollout_id,qualification_id)
			VALUES($1,$2) ON CONFLICT DO NOTHING`, id, q.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "could not link hardware qualification evidence")
			return
		}
	}
	tag, err := tx.Exec(r.Context(), `UPDATE firmware_rollouts SET status='awaiting_canary',current_batch=0,
		max_concurrent_flashes=$2,pause_reason='',started_at=NOW(),completed_at=NULL,updated_at=NOW()
		WHERE id=$1 AND status='previewed'`, id, firmwareRolloutDefaultMaxConcurrentFlashes)
	if err != nil || tag.RowsAffected() != 1 {
		writeError(w, http.StatusInternalServerError, "could not release the canary")
		return
	}
	if err = insertAuditEvent(r.Context(), tx, user.ID, "firmware.rollout_start", "firmware_rollout",
		strconv.FormatInt(id, 10), map[string]any{
			"version": rollout.Version, "canary_device_id": rollout.CanaryDeviceID,
			"device_count": len(rollout.Devices), "max_concurrent_flashes": firmwareRolloutDefaultMaxConcurrentFlashes,
		}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the rollout")
		return
	}
	out, err := loadFirmwareRollout(r.Context(), a.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rollout started but could not be reloaded")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) pauseFirmwareRollout(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("rollout"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid rollout ID")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, 2<<10, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid pause request")
			return
		}
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > 500 {
		writeError(w, http.StatusBadRequest, "pause reason must be 500 characters or fewer")
		return
	}
	if in.Reason == "" {
		in.Reason = "Paused by administrator."
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not pause the rollout")
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW()
		WHERE id=$1 AND status IN ('awaiting_canary','canary_running','awaiting_batch','batch_running')`, id, in.Reason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not pause the rollout")
		return
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM firmware_rollouts WHERE id=$1)`, id).Scan(&exists); err != nil {
			writeError(w, http.StatusInternalServerError, "could not pause the rollout")
			return
		}
		if !exists {
			writeError(w, http.StatusNotFound, "rollout not found")
		} else {
			writeError(w, http.StatusConflict, "this rollout is not active")
		}
		return
	}
	if err = insertAuditEvent(r.Context(), tx, user.ID, "firmware.rollout_pause", "firmware_rollout",
		strconv.FormatInt(id, 10), map[string]any{"reason": in.Reason}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not pause the rollout")
		return
	}
	out, err := loadFirmwareRollout(r.Context(), a.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not reload the rollout")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) cancelFirmwareRollout(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("rollout"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid rollout ID")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not cancel the rollout")
		return
	}
	defer tx.Rollback(r.Context())
	var activeFlash bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(
		SELECT 1 FROM firmware_rollout_devices rd
		JOIN firmware_upgrades u ON u.id=rd.upgrade_id
		WHERE rd.rollout_id=$1 AND u.flash_sent_at IS NOT NULL AND u.status IN ('flashing','verifying'))`, id).Scan(&activeFlash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not check active flashes")
		return
	}
	if activeFlash {
		writeError(w, http.StatusConflict, "a flash has already been sent; pause the rollout instead and allow the device operation to settle")
		return
	}
	tag, err := tx.Exec(r.Context(), `UPDATE firmware_rollouts SET status='cancelled',pause_reason='Cancelled by administrator.',
		updated_at=NOW() WHERE id=$1 AND status IN ('awaiting_canary','canary_running','awaiting_batch','batch_running','paused')`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not cancel the rollout")
		return
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM firmware_rollouts WHERE id=$1)`, id).Scan(&exists); err != nil {
			writeError(w, http.StatusInternalServerError, "could not cancel the rollout")
			return
		}
		if !exists {
			writeError(w, http.StatusNotFound, "rollout not found")
		} else {
			writeError(w, http.StatusConflict, "this rollout cannot be cancelled")
		}
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE firmware_rollout_devices SET status='skipped',updated_at=NOW()
		WHERE rollout_id=$1 AND status='queued'`, id); err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "firmware.rollout_cancel", "firmware_rollout",
			strconv.FormatInt(id, 10), map[string]any{"reason": "administrator cancelled before an active flash"})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not cancel the rollout")
		return
	}
	out, err := loadFirmwareRollout(r.Context(), a.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not reload the rollout")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// lockFirmwareRolloutMemberForPreparation reserves only the current canary/batch
// member. It does not send a flash; the existing per-device flash action remains separate.
func lockFirmwareRolloutMemberForPreparation(ctx context.Context, tx pgx.Tx, rolloutID int64, deviceID, version string) (firmwareRolloutMember, error) {
	var out firmwareRolloutMember
	var status, rolloutVersion string
	var currentBatch int
	err := tx.QueryRow(ctx, `SELECT status,version,current_batch FROM firmware_rollouts WHERE id=$1 FOR UPDATE`,
		rolloutID).Scan(&status, &rolloutVersion, &currentBatch)
	if err != nil {
		return out, err
	}
	if (status != "awaiting_canary" && status != "awaiting_batch") || rolloutVersion != version {
		return out, errors.New("rollout is not accepting preparation for this release")
	}
	err = tx.QueryRow(ctx, `SELECT device_id_snapshot::text,device_name,model,current_release,board_name,target,
		inventory_id,inventory_digest,inventory_verified_at,desired_package_set_revision,batch_number,position,status,
		plan,plan_digest,inventory_manifest
		FROM firmware_rollout_devices WHERE rollout_id=$1 AND device_id_snapshot=$2 FOR UPDATE`,
		rolloutID, deviceID).Scan(&out.DeviceID, &out.DeviceName, &out.Model, &out.CurrentRelease,
		&out.BoardName, &out.Target, &out.InventoryID, &out.InventoryDigest, &out.InventoryVerifiedAt,
		&out.DesiredPackageSetRevision, &out.BatchNumber, &out.Position, &out.Status, &out.Plan,
		&out.PlanDigest, &out.InventoryManifest)
	if err != nil {
		return out, err
	}
	if out.Status != "queued" || out.BatchNumber != currentBatch || len(out.Plan) == 0 || out.PlanDigest == "" {
		return out, errors.New("device is not in the currently released rollout batch")
	}
	current, err := firmwareRolloutMemberCurrent(ctx, tx, out)
	if err != nil {
		return out, err
	}
	if !current {
		return out, errors.New("device inventory or desired package set changed; create a new rollout draft")
	}
	return out, nil
}

// firmwareRolloutMemberPlanCurrent ignores inventory row ID changes caused by the
// upgrade job's own fresh inventory capture, but still requires identical content,
// device identity, and desired package-set revision.
func firmwareRolloutMemberPlanCurrent(ctx context.Context, q rolloutQueryRower, member firmwareRolloutMember) (bool, error) {
	var name, model, digest string
	var revision int
	err := q.QueryRow(ctx, `SELECT d.name,d.model,COALESCE(fi.digest,''),
		COALESCE((SELECT r.revision FROM device_package_sets s
			JOIN device_package_set_revisions r ON r.id=s.current_revision_id WHERE s.device_id=d.id),0)
		FROM devices d LEFT JOIN LATERAL (
			SELECT digest FROM firmware_inventories WHERE device_id=d.id ORDER BY captured_at DESC,id DESC LIMIT 1
		) fi ON TRUE WHERE d.id=$1`, member.DeviceID).Scan(&name, &model, &digest, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return name == member.DeviceName && model == member.Model && digest == member.InventoryDigest &&
		revision == member.DesiredPackageSetRevision, nil
}
