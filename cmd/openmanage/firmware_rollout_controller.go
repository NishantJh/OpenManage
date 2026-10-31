package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func firmwareRolloutMemberPlan(member firmwareRolloutMember, version string) (firmwarePlan, error) {
	var plan firmwarePlan
	if len(member.Plan) == 0 || string(member.Plan) == "null" || member.PlanDigest == "" {
		return plan, errors.New("the rollout member has no immutable plan")
	}
	if err := json.Unmarshal(member.Plan, &plan); err != nil {
		return plan, errors.New("the stored rollout plan is invalid")
	}
	if plan.Version != version {
		return plan, errors.New("the stored rollout plan target does not match the campaign")
	}
	if len(plan.Blockers) > 0 || len(plan.Warnings) > 0 {
		return plan, errors.New("rollout plans with blockers or warnings cannot be executed")
	}
	digest, err := firmwareRolloutPlanDigest(member, version, plan)
	if err != nil || digest != member.PlanDigest {
		return plan, errors.New("the stored rollout plan digest does not match its snapshot")
	}
	return plan, nil
}

func findFirmwareRolloutMember(rollout firmwareRollout, deviceID string) (firmwareRolloutMember, bool) {
	for _, member := range rollout.Devices {
		if member.DeviceID == deviceID {
			return member, true
		}
	}
	return firmwareRolloutMember{}, false
}

func lockFirmwareRolloutDevices(ctx context.Context, tx pgx.Tx, rollout firmwareRollout) error {
	ids := make([]string, 0, len(rollout.Devices))
	for _, member := range rollout.Devices {
		ids = append(ids, member.DeviceID)
	}
	sort.Strings(ids)
	rows, err := tx.Query(ctx, `SELECT id::text FROM devices WHERE id::text=ANY($1::text[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(ids) {
		return errors.New("a rollout device was removed")
	}
	return nil
}

func validateFirmwareRolloutPlans(rollout firmwareRollout) error {
	for _, member := range rollout.Devices {
		if member.Status != "queued" || member.UpgradeID != nil {
			return errors.New("device " + member.DeviceName + ": rollout membership is not in its initial queued state")
		}
		plan, err := firmwareRolloutMemberPlan(member, rollout.Version)
		if err != nil {
			return errors.New("device " + member.DeviceName + ": " + err.Error())
		}
		if plan.Version != "" && plan.Version != rollout.Version {
			return errors.New("device " + member.DeviceName + ": the plan target does not match the campaign")
		}
	}
	return nil
}

func firmwareRolloutBranches(ctx context.Context, serverURL string) ([]string, error) {
	overview, err := fetchOverview(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	branches := make([]string, 0, len(overview.Branches))
	for name := range overview.Branches {
		if !strings.Contains(name, "SNAPSHOT") {
			branches = append(branches, name)
		}
	}
	sort.Strings(branches)
	return branches, nil
}

func (a *app) startFirmwareRollout(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("rollout"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid rollout ID")
		return
	}
	ctx := r.Context()
	rollout, err := loadFirmwareRollout(ctx, a.db, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "rollout not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the rollout")
		return
	}
	if rollout.Status != "previewed" {
		writeError(w, http.StatusConflict, "only a previewed rollout can be started")
		return
	}
	if err := validateFirmwareRolloutPlans(rollout); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	qualificationStatus, err := a.firmwareRolloutQualificationStatus(ctx, rollout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not validate physical hardware qualifications")
		return
	}
	if !qualificationStatus.Qualified {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "current physical hardware qualification is required for every hardware/release group",
			"qualification_status": qualificationStatus,
		})
		return
	}
	serverURL, err := a.firmwareBuildServer(ctx)
	if err != nil {
		buildServerError(w, err)
		return
	}
	branches, err := firmwareRolloutBranches(ctx, serverURL)
	if err != nil {
		buildServerError(w, err)
		return
	}
	for _, member := range rollout.Devices {
		if problem := upgradePathProblem(member.CurrentRelease, rollout.Version, branches); problem != "" {
			writeError(w, http.StatusConflict, "device "+member.DeviceName+": "+problem)
			return
		}
		current, err := firmwareRolloutMemberCurrent(ctx, a.db, member)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not validate rollout snapshots")
			return
		}
		if !current {
			writeError(w, http.StatusConflict, "a selected device, inventory, or desired package-set revision changed; create a new rollout draft")
			return
		}
	}

	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the rollout")
		return
	}
	defer tx.Rollback(ctx)
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		writeError(w, http.StatusInternalServerError, "could not lock the rollout")
		return
	}
	if status != "previewed" {
		writeError(w, http.StatusConflict, "the rollout is no longer in previewed state")
		return
	}
	if err = lockFirmwareRolloutDevices(ctx, tx, rollout); err != nil {
		writeError(w, http.StatusConflict, "a selected device was removed; create a new rollout draft")
		return
	}
	for _, member := range rollout.Devices {
		current, err := firmwareRolloutMemberCurrent(ctx, tx, member)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not revalidate rollout snapshots")
			return
		}
		if !current {
			writeError(w, http.StatusConflict, "a selected device, inventory, or desired package-set revision changed during start; create a new rollout draft")
			return
		}
		qualification, err := findMatchingFirmwareHardwareQualification(ctx, tx, member, rollout.Version)
		if err != nil {
			writeError(w, http.StatusConflict, "physical hardware qualification changed or expired; refresh qualification evidence")
			return
		}
		if _, err = tx.Exec(ctx, `INSERT INTO firmware_rollout_qualifications(rollout_id,qualification_id)
			VALUES($1,$2) ON CONFLICT DO NOTHING`, id, qualification.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "could not link hardware qualification evidence")
			return
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='awaiting_canary',current_batch=0,
		started_at=NOW(),pause_reason='',updated_at=NOW() WHERE id=$1`, id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the rollout")
		return
	}
	if err = insertAuditEvent(ctx, tx, user.ID, "firmware.rollout_start", "firmware_rollout", strconv.FormatInt(id, 10), map[string]any{
		"version": rollout.Version, "device_count": len(rollout.Devices),
		"qualification_ids": qualificationIDs(qualificationStatus.Matched),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not audit rollout start")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the rollout")
		return
	}
	out, err := loadFirmwareRollout(ctx, a.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rollout started but could not be reloaded")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func qualificationIDs(records []firmwareHardwareQualification) []int64 {
	ids := make([]int64, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (a *app) prepareFirmwareRolloutDevice(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	rolloutID, err := strconv.ParseInt(r.PathValue("rollout"), 10, 64)
	deviceID := strings.ToLower(r.PathValue("device"))
	if err != nil || rolloutID <= 0 || !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid rollout or device ID")
		return
	}
	ctx := r.Context()
	rollout, err := loadFirmwareRollout(ctx, a.db, rolloutID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "rollout not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the rollout")
		return
	}
	member, ok := findFirmwareRolloutMember(rollout, deviceID)
	if !ok {
		writeError(w, http.StatusNotFound, "device is not part of this rollout")
		return
	}
	if member.BatchNumber != rollout.CurrentBatch || member.Status != "queued" || member.UpgradeID != nil {
		writeError(w, http.StatusConflict, "the device is not queued in the currently released batch")
		return
	}
	if rollout.CurrentBatch == 0 && (rollout.Status != "awaiting_canary" || deviceID != rollout.CanaryDeviceID) {
		writeError(w, http.StatusConflict, "only the canary can be prepared first")
		return
	}
	if rollout.CurrentBatch > 0 && rollout.Status != "batch_running" {
		writeError(w, http.StatusConflict, "the next batch has not been released")
		return
	}
	plan, err := firmwareRolloutMemberPlan(member, rollout.Version)
	if err != nil {
		a.setFirmwareRolloutPaused(ctx, rolloutID, "stale or invalid plan: "+err.Error(), r.Context().Value(userContextKey).(authUser).ID)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	current, err := firmwareRolloutMemberCurrent(ctx, a.db, member)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not validate the device snapshot")
		return
	}
	if !current {
		a.setFirmwareRolloutPaused(ctx, rolloutID, "device, inventory, or desired package-set revision changed", r.Context().Value(userContextKey).(authUser).ID)
		writeError(w, http.StatusConflict, "device, inventory, or desired package-set revision changed; create a new rollout draft")
		return
	}
	if _, err = findMatchingFirmwareHardwareQualification(ctx, a.db, member, rollout.Version); err != nil {
		a.setFirmwareRolloutPaused(ctx, rolloutID, "physical hardware qualification expired or no longer matches", r.Context().Value(userContextKey).(authUser).ID)
		writeError(w, http.StatusConflict, "current physical hardware qualification is required before preparing this device")
		return
	}
	serverURL, err := a.firmwareBuildServer(ctx)
	if err != nil {
		buildServerError(w, err)
		return
	}
	branches, err := firmwareRolloutBranches(ctx, serverURL)
	if err != nil {
		buildServerError(w, err)
		return
	}
	if problem := upgradePathProblem(member.CurrentRelease, rollout.Version, branches); problem != "" {
		a.setFirmwareRolloutPaused(ctx, rolloutID, "unsupported release path: "+problem, r.Context().Value(userContextKey).(authUser).ID)
		writeError(w, http.StatusConflict, problem)
		return
	}
	if _, err := encryptionKey(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "server encryption key is not configured")
		return
	}

	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start device preparation")
		return
	}
	defer tx.Rollback(ctx)
	var status string
	var currentBatch int
	err = tx.QueryRow(ctx, `SELECT status,current_batch FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, rolloutID).Scan(&status, &currentBatch)
	if err != nil || currentBatch != rollout.CurrentBatch || status != rollout.Status {
		writeError(w, http.StatusConflict, "rollout state changed; reload before preparing a device")
		return
	}
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM devices WHERE id=$1 FOR UPDATE`, deviceID).Scan(&locked); err != nil {
		_ = tx.Rollback(ctx)
		_ = a.setFirmwareRolloutPaused(ctx, rolloutID, "selected device was removed during preparation", user.ID)
		writeError(w, http.StatusConflict, "device no longer exists")
		return
	}
	current, err = firmwareRolloutMemberCurrent(ctx, tx, member)
	if err != nil || !current {
		_ = tx.Rollback(ctx)
		_ = a.setFirmwareRolloutPaused(ctx, rolloutID, "device, inventory, or desired package-set revision changed during preparation", user.ID)
		writeError(w, http.StatusConflict, "device, inventory, or desired package-set revision changed; create a new rollout draft")
		return
	}
	qualification, err := findMatchingFirmwareHardwareQualification(ctx, tx, member, rollout.Version)
	if err != nil {
		_ = tx.Rollback(ctx)
		_ = a.setFirmwareRolloutPaused(ctx, rolloutID, "physical hardware qualification expired during preparation", user.ID)
		writeError(w, http.StatusConflict, "current physical hardware qualification is required")
		return
	}
	var linked bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM firmware_rollout_qualifications
		WHERE rollout_id=$1 AND qualification_id=$2)`, rolloutID, qualification.ID).Scan(&linked); err != nil || !linked {
		_ = tx.Rollback(ctx)
		_ = a.setFirmwareRolloutPaused(ctx, rolloutID, "campaign qualification evidence link is missing", user.ID)
		writeError(w, http.StatusConflict, "hardware qualification was not recorded for this campaign")
		return
	}
	var busy string
	err = tx.QueryRow(ctx, `SELECT CASE
		WHEN EXISTS(SELECT 1 FROM device_restores WHERE device_id=$1 AND status='running') THEN 'a backup restore is running on this device'
		WHEN EXISTS(SELECT 1 FROM config_changes WHERE device_id=$1 AND status='applying') THEN 'a configuration change is being applied to this device'
		WHEN `+packageInstallRunningSQL+` THEN '`+packageInstallBusyMessage+`'
		ELSE '' END`, deviceID).Scan(&busy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not check device activity")
		return
	}
	if busy != "" {
		writeError(w, http.StatusConflict, busy)
		return
	}
	var up firmwareUpgrade
	err = scanFirmwareUpgrade(tx.QueryRow(ctx, `INSERT INTO firmware_upgrades(device_id,version,status,step,plan,created_by)
		VALUES($1,$2,'preparing','starting',$3::jsonb,$4) RETURNING `+firmwareUpgradeColumns,
		deviceID, rollout.Version, string(member.Plan), user.ID), &up)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, openUpgradeMessage)
		return
	}
	if err == nil {
		var tag pgconn.CommandTag
		tag, err = tx.Exec(ctx, `UPDATE firmware_rollout_devices SET status='preparing',upgrade_id=$3,updated_at=NOW()
			WHERE rollout_id=$1 AND device_id_snapshot=$2 AND status='queued' AND plan_digest=$4`,
			rolloutID, deviceID, up.ID, member.PlanDigest)
		if err == nil && tag.RowsAffected() != 1 {
			err = errors.New("rollout device state changed during preparation")
		}
	}
	if err == nil {
		nextStatus := "batch_running"
		if rollout.CurrentBatch == 0 {
			nextStatus = "canary_running"
		}
		_, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status=$2,updated_at=NOW() WHERE id=$1 AND status IN ('awaiting_canary','batch_running','canary_running')`, rolloutID, nextStatus)
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, user.ID, "device.firmware_upgrade_prepare", "device", deviceID,
			map[string]any{"upgrade_id": up.ID, "version": rollout.Version, "rollout_id": rolloutID})
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, user.ID, "firmware.rollout_device_prepare", "firmware_rollout", strconv.FormatInt(rolloutID, 10),
			map[string]any{"device_id": deviceID, "upgrade_id": up.ID, "batch_number": member.BatchNumber})
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		_ = a.setFirmwareRolloutPaused(ctx, rolloutID, "audit or persistence failure while preparing a rollout device", user.ID)
		writeError(w, http.StatusInternalServerError, "could not start rollout device preparation")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		_ = a.setFirmwareRolloutPaused(ctx, rolloutID, "commit failure while preparing a rollout device", user.ID)
		writeError(w, http.StatusInternalServerError, "could not start rollout device preparation")
		return
	}

	jobCtx, cancel := context.WithTimeout(context.Background(), upgradeJobTimeout)
	registerUpgradeRun(up.ID, cancel)
	job := &upgradeJob{
		a: a, id: up.ID, deviceID: deviceID, version: rollout.Version, userID: user.ID,
		rolloutID: rolloutID, rolloutMember: member, rolloutPlan: plan, rolloutPlanDigest: member.PlanDigest,
	}
	go func() {
		defer cancel()
		defer releaseUpgradeRun(job.id)
		job.run(jobCtx)
	}()
	writeJSON(w, http.StatusAccepted, up)
}

func (a *app) advanceFirmwareRollout(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("rollout"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid rollout ID")
		return
	}
	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not advance the rollout")
		return
	}
	defer tx.Rollback(ctx)
	var status string
	var currentBatch int
	err = tx.QueryRow(ctx, `SELECT status,current_batch FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, id).Scan(&status, &currentBatch)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "rollout not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load rollout state")
		return
	}
	if status != "canary_running" && status != "batch_running" && status != "awaiting_batch" {
		writeError(w, http.StatusConflict, "the rollout is not waiting for a successful batch")
		return
	}
	var total, acknowledged int
	err = tx.QueryRow(ctx, `SELECT count(*)::int,
		count(*) FILTER (WHERE rd.status='done' AND u.status='done' AND u.acknowledged_at IS NOT NULL)::int
		FROM firmware_rollout_devices rd
		LEFT JOIN firmware_upgrades u ON u.id=rd.upgrade_id
		WHERE rd.rollout_id=$1 AND rd.batch_number=$2`, id, currentBatch).Scan(&total, &acknowledged)
	if err != nil || total == 0 || acknowledged != total {
		writeError(w, http.StatusConflict, "every device in the current batch must finish all checks as done and be acknowledged before advancing")
		return
	}
	var nextCount int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM firmware_rollout_devices WHERE rollout_id=$1 AND batch_number=$2`, id, currentBatch+1).Scan(&nextCount)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not inspect the next batch")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	if nextCount == 0 {
		_, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='complete',completed_at=NOW(),updated_at=NOW() WHERE id=$1`, id)
	} else {
		_, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='batch_running',current_batch=current_batch+1,updated_at=NOW() WHERE id=$1`, id)
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, user.ID, "firmware.rollout_advance", "firmware_rollout", strconv.FormatInt(id, 10), map[string]any{
			"completed_batch": currentBatch, "next_batch": currentBatch + 1, "completed": nextCount == 0,
		})
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		_ = a.setFirmwareRolloutPaused(ctx, id, "audit or persistence failure while advancing a batch", user.ID)
		writeError(w, http.StatusInternalServerError, "could not audit rollout advancement")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		_ = a.setFirmwareRolloutPaused(ctx, id, "commit failure while advancing a batch", user.ID)
		writeError(w, http.StatusInternalServerError, "could not advance the rollout")
		return
	}
	out, err := loadFirmwareRollout(ctx, a.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rollout advanced but could not be reloaded")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type firmwareRolloutPauseRequest struct {
	Reason string `json:"reason"`
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
	var in firmwareRolloutPauseRequest
	if err := decodeJSONBody(w, r, 2048, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid pause request")
		return
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = "paused by administrator"
	}
	if len(reason) > 500 || strings.ContainsAny(reason, "\r\n") {
		writeError(w, http.StatusBadRequest, "pause reason must be 500 characters or fewer on one line")
		return
	}
	if err := a.setFirmwareRolloutPaused(r.Context(), id, reason, r.Context().Value(userContextKey).(authUser).ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "rollout not found")
		} else {
			writeError(w, http.StatusConflict, "could not pause the rollout")
		}
		return
	}
	out, err := loadFirmwareRollout(r.Context(), a.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rollout paused but could not be reloaded")
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
	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not cancel the rollout")
		return
	}
	defer tx.Rollback(ctx)
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, id).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "rollout not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load rollout state")
		return
	}
	if status == "complete" || status == "cancelled" {
		writeError(w, http.StatusConflict, "a completed or cancelled rollout cannot be cancelled again")
		return
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM firmware_rollout_devices rd
		JOIN firmware_upgrades u ON u.id=rd.upgrade_id
		WHERE rd.rollout_id=$1 AND (u.status IN ('preparing','ready','flashing','verifying') OR u.flash_sent_at IS NOT NULL)`, id).Scan(&active); err != nil {
		writeError(w, http.StatusInternalServerError, "could not inspect rollout jobs")
		return
	}
	if active > 0 {
		writeError(w, http.StatusConflict, "cancel is allowed only before any device job starts; pause the rollout while jobs are active")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	if _, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='cancelled',updated_at=NOW(),pause_reason='cancelled by administrator' WHERE id=$1`, id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not cancel the rollout")
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE firmware_rollout_devices SET status='skipped',updated_at=NOW() WHERE rollout_id=$1 AND status='queued'`, id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not cancel queued rollout devices")
		return
	}
	if err = insertAuditEvent(ctx, tx, user.ID, "firmware.rollout_cancel", "firmware_rollout", strconv.FormatInt(id, 10), map[string]any{"previous_status": status}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not audit rollout cancellation")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "could not cancel the rollout")
		return
	}
	out, err := loadFirmwareRollout(ctx, a.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rollout cancelled but could not be reloaded")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) setFirmwareRolloutPaused(ctx context.Context, rolloutID int64, reason, actorID string) error {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		_, _ = a.db.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW()
			WHERE id=$1 AND status NOT IN ('complete','cancelled')`, rolloutID, reason)
		return err
	}
	defer tx.Rollback(ctx)
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, rolloutID).Scan(&status); err != nil {
		return err
	}
	if status == "complete" || status == "cancelled" {
		return errors.New("rollout is already terminal")
	}
	if _, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW() WHERE id=$1`, rolloutID, reason); err != nil {
		return err
	}
	if actorID != "" {
		if err = insertAuditEvent(ctx, tx, actorID, "firmware.rollout_pause", "firmware_rollout", strconv.FormatInt(rolloutID, 10), map[string]any{"reason": reason}); err != nil {
			// Fail closed: keep the campaign paused even if its audit event cannot be saved.
			_ = tx.Rollback(ctx)
			_, _ = a.db.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW()
				WHERE id=$1 AND status NOT IN ('complete','cancelled')`, rolloutID, "audit failure while pausing: "+reason)
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		_, _ = a.db.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW()
			WHERE id=$1 AND status NOT IN ('complete','cancelled')`, rolloutID, "commit failure while pausing: "+reason)
		return err
	}
	return nil
}

func (a *app) pauseFirmwareRolloutForUpgrade(ctx context.Context, upgradeID int64, memberStatus, reason string) {
	var rolloutID int64
	if err := a.db.QueryRow(ctx, `SELECT rollout_id FROM firmware_rollout_devices WHERE upgrade_id=$1`, upgradeID).Scan(&rolloutID); err != nil {
		return
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		_, _ = a.db.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW() WHERE id=$1 AND status NOT IN ('complete','cancelled')`, rolloutID, reason)
		_, _ = a.db.Exec(ctx, `UPDATE firmware_rollout_devices SET status=$2,updated_at=NOW() WHERE upgrade_id=$1`, upgradeID, memberStatus)
		return
	}
	defer tx.Rollback(ctx)
	var lockedID int64
	if err = tx.QueryRow(ctx, `SELECT id FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, rolloutID).Scan(&lockedID); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE firmware_rollout_devices SET status=$2,updated_at=NOW() WHERE upgrade_id=$1`, upgradeID, memberStatus); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW() WHERE id=$1 AND status NOT IN ('complete','cancelled')`, rolloutID, reason); err != nil {
		return
	}
	if err = insertAuditEvent(ctx, tx, "", "firmware.rollout_pause", "firmware_rollout", strconv.FormatInt(rolloutID, 10), map[string]any{
		"upgrade_id": upgradeID, "member_status": memberStatus, "reason": reason,
	}); err != nil {
		_ = tx.Rollback(ctx)
		_, _ = a.db.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW() WHERE id=$1 AND status NOT IN ('complete','cancelled')`, rolloutID, "audit failure: "+reason)
		_, _ = a.db.Exec(ctx, `UPDATE firmware_rollout_devices SET status=$2,updated_at=NOW() WHERE upgrade_id=$1`, upgradeID, memberStatus)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		_, _ = a.db.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW() WHERE id=$1 AND status NOT IN ('complete','cancelled')`, rolloutID, reason)
		_, _ = a.db.Exec(ctx, `UPDATE firmware_rollout_devices SET status=$2,updated_at=NOW() WHERE upgrade_id=$1`, upgradeID, memberStatus)
	}
}

func (a *app) setFirmwareRolloutDeviceStatus(ctx context.Context, upgradeID int64, memberStatus string, pauseReason string) error {
	var rolloutID int64
	if err := a.db.QueryRow(ctx, `SELECT rollout_id FROM firmware_rollout_devices WHERE upgrade_id=$1`, upgradeID).Scan(&rolloutID); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var lockedID int64
	if err = tx.QueryRow(ctx, `SELECT id FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, rolloutID).Scan(&lockedID); err != nil {
		return err
	}
	var batchNumber int
	if err = tx.QueryRow(ctx, `SELECT batch_number FROM firmware_rollout_devices WHERE upgrade_id=$1 FOR UPDATE`, upgradeID).Scan(&batchNumber); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE firmware_rollout_devices SET status=$2,updated_at=NOW() WHERE upgrade_id=$1`, upgradeID, memberStatus)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("rollout device link disappeared")
	}
	if pauseReason != "" {
		if _, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='paused',pause_reason=$2,updated_at=NOW() WHERE id=$1 AND status NOT IN ('complete','cancelled')`, rolloutID, pauseReason); err != nil {
			return err
		}
	}
	if memberStatus == "done" {
		var campaignStatus string
		var currentBatch int
		if err = tx.QueryRow(ctx, `SELECT status,current_batch FROM firmware_rollouts WHERE id=$1`, rolloutID).Scan(&campaignStatus, &currentBatch); err != nil {
			return err
		}
		if (campaignStatus == "canary_running" || campaignStatus == "batch_running") && currentBatch == batchNumber {
			var total, acknowledged int
			err = tx.QueryRow(ctx, `SELECT count(*)::int,
				count(*) FILTER (WHERE rd.status='done' AND u.status='done' AND u.acknowledged_at IS NOT NULL)::int
				FROM firmware_rollout_devices rd LEFT JOIN firmware_upgrades u ON u.id=rd.upgrade_id
				WHERE rd.rollout_id=$1 AND rd.batch_number=$2`, rolloutID, batchNumber).Scan(&total, &acknowledged)
			if err != nil {
				return err
			}
			if total > 0 && total == acknowledged {
				if _, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='awaiting_batch',updated_at=NOW() WHERE id=$1`, rolloutID); err != nil {
					return err
				}
				if err = insertAuditEvent(ctx, tx, "", "firmware.rollout_batch_ready", "firmware_rollout", strconv.FormatInt(rolloutID, 10), map[string]any{
					"batch_number": batchNumber,
				}); err != nil {
					return err
				}
			}
		}
	}
	if err = insertAuditEvent(ctx, tx, "", "firmware.rollout_device_"+memberStatus, "firmware_rollout", strconv.FormatInt(rolloutID, 10), map[string]any{
		"upgrade_id": upgradeID, "member_status": memberStatus, "pause_reason": pauseReason,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
