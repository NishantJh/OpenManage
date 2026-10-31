package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Flashing a prepared firmware upgrade and verifying the device afterwards
// (docs/firmware-upgrades.md, "Flash and verify design").
//
//   - flashing is admin only, per device, and needs the device name typed exactly;
//     every warning in the plan must be acknowledged;
//   - the server re-checks that the job is still ready, that the device has not
//     rebooted, that its packages are unchanged, that the staged image still has its
//     hash and that sysupgrade -T still accepts it; any failure stops the job with
//     nothing flashed;
//   - the one flash command is fixed text: no -n (wipe settings) and no -F (force), and
//     it re-checks the image hash on the device before handing over to sysupgrade;
//   - after the command the server only watches; it never sends a second flash;
//   - the outcome is done, done_with_warnings or needs_attention, and the pinned backup
//     stays pinned until a person acknowledges the result.

const (
	// A device that is still reachable with the same boot id this long after the flash
	// command did not restart, so the image was not written.
	flashDownWait      = 3 * time.Minute
	flashReturnTimeout = 10 * time.Minute
	flashJobTimeout    = 30 * time.Minute
	flashBodyLimit     = 512
)

// flashPollInterval is how often the device is tried after the flash command; tests
// shorten it.
var flashPollInterval = 5 * time.Second

// The expected hash arrives on stdin, never inside the command text. The sysupgrade call
// is detached so the command's exit status reaches OpenManage before the connection
// drops, as restore does for its reboot.
var opUpgradeFlash = func() operation {
	op := upgradeOperation("firmware_flash", "Flash firmware image",
		"read -r want; cd "+upgradeDir+" && echo \"$want  "+upgradeImageName+"\" | sha256sum -c -s && { (sleep 2; sysupgrade "+upgradeImageName+") >/dev/null 2>&1 & }",
		30*time.Second)
	op.Roles = []string{"admin"}
	return op
}()

var flashOperations = []operation{opUpgradeFlash}

type upgradeCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warning or failed
	Detail string `json:"detail"`
}

// upgradeOutcome is the job status that follows from its checks.
func upgradeOutcome(checks []upgradeCheck) string {
	outcome := "done"
	for _, c := range checks {
		switch c.Status {
		case "failed":
			return "needs_attention"
		case "warning":
			outcome = "done_with_warnings"
		}
	}
	return outcome
}

// expectedPackages are the packages the build request asked for, minus the "-name"
// removals.
func expectedPackages(plan firmwarePlan) []string {
	var out []string
	for _, p := range plan.Packages {
		if !strings.HasPrefix(p, "-") {
			out = append(out, p)
		}
	}
	return out
}

// abiFree drops the date suffix some library packages carry, which can differ between
// the name requested and the name installed.
func abiFree(name string) string {
	if m := abiSuffixPattern.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// missingPackages lists the expected packages the device does not report.
func missingPackages(expected []string, installed []firmwarePackage) []string {
	have := map[string]bool{}
	for _, p := range installed {
		have[p.Name] = true
		have[abiFree(p.Name)] = true
	}
	var out []string
	for _, name := range expected {
		if !have[name] && !have[abiFree(name)] {
			out = append(out, name)
		}
	}
	return out
}

// restartVerdict decides whether the device restarted after the flash command. It
// restarted when it answers with another boot id, or, when a boot id is unknown, when it
// was seen unreachable first. It never restarted when it still answers with the same
// boot id long after the command.
func restartVerdict(bootNow, bootBefore string, reachable, sawDown bool, sinceFlash time.Duration) string {
	if !reachable {
		return "waiting"
	}
	known := bootNow != "" && bootBefore != ""
	switch {
	case known && bootNow != bootBefore:
		return "restarted"
	case !known && sawDown:
		return "restarted"
	case sinceFlash > flashDownWait:
		return "never_restarted"
	}
	return "waiting"
}

type upgradeRecord struct {
	ID              int64
	DeviceID        string
	Version         string
	Status          string
	Plan            firmwarePlan
	ImageSHA256     string
	BootID          string
	InventoryDigest string
	FlashSentAt     *time.Time
	RolloutID       int64
}

func (a *app) loadUpgradeRecord(ctx context.Context, id int64) (upgradeRecord, error) {
	var r upgradeRecord
	var plan []byte
	err := a.db.QueryRow(ctx, `SELECT u.id, u.device_id::text, u.version, u.status, u.plan, u.image_sha256, u.boot_id, u.inventory_digest, u.flash_sent_at,
		COALESCE(rd.rollout_id,0)
		FROM firmware_upgrades u LEFT JOIN firmware_rollout_devices rd ON rd.upgrade_id=u.id WHERE u.id=$1`, id).Scan(&r.ID, &r.DeviceID, &r.Version, &r.Status, &plan, &r.ImageSHA256, &r.BootID, &r.InventoryDigest, &r.FlashSentAt, &r.RolloutID)
	if err != nil {
		return r, err
	}
	if len(plan) > 0 {
		if err := json.Unmarshal(plan, &r.Plan); err != nil {
			return r, err
		}
	}
	return r, nil
}

type flashRequest struct {
	ConfirmName         string `json:"confirm_name"`
	AcknowledgeWarnings bool   `json:"acknowledge_warnings"`
}

// flashFirmwareUpgrade claims a ready job for flashing and starts it. It returns at once.
func (a *app) flashFirmwareUpgrade(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	var id int64
	if _, err := fmt.Sscanf(r.PathValue("job"), "%d", &id); err != nil || !validUUID(deviceID) || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid device or upgrade")
		return
	}
	if !requireOperationRoles(w, r, flashOperations) {
		return
	}
	var in flashRequest
	if err := decodeJSONBody(w, r, flashBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid flash request")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	var rolloutID int64
	rolloutLookupErr := a.db.QueryRow(r.Context(), `SELECT rollout_id FROM firmware_rollout_devices
		WHERE upgrade_id=$1 AND device_id_snapshot=$2`, id, deviceID).Scan(&rolloutID)
	if rolloutLookupErr != nil && !errors.Is(rolloutLookupErr, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "could not inspect rollout membership")
		return
	}
	if rolloutID > 0 && user.Role != "admin" {
		writeError(w, http.StatusForbidden, "only an administrator can flash a rollout device")
		return
	}

	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the flash")
		return
	}
	defer tx.Rollback(r.Context())
	var rolloutStatus, canaryDeviceID, rolloutVersion string
	var currentBatch, maxConcurrent int
	if rolloutID > 0 {
		err = tx.QueryRow(r.Context(), `SELECT status,current_batch,max_concurrent_flashes,canary_device_id::text,version FROM firmware_rollouts WHERE id=$1 FOR UPDATE`,
			rolloutID).Scan(&rolloutStatus, &currentBatch, &maxConcurrent, &canaryDeviceID, &rolloutVersion)
		if err != nil {
			writeError(w, http.StatusConflict, "rollout is no longer available for flashing")
			return
		}
	}
	var deviceName string
	err = tx.QueryRow(r.Context(), `SELECT name FROM devices WHERE id=$1 FOR NO KEY UPDATE`, deviceID).Scan(&deviceName)
	if errors.Is(err, pgx.ErrNoRows) {
		if rolloutID > 0 {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "campaign device was removed before flash", user.ID)
		}
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the flash")
		return
	}
	if in.ConfirmName != deviceName {
		writeError(w, http.StatusBadRequest, "type the device name exactly to confirm the flash")
		return
	}
	var status, upgradeVersion, upgradeInventoryDigest string
	var planJSON []byte
	var expired bool
	err = tx.QueryRow(r.Context(), `SELECT status, plan, COALESCE(expires_at < NOW(), TRUE),version,inventory_digest FROM firmware_upgrades
		WHERE id=$1 AND device_id=$2 FOR UPDATE`, id, deviceID).Scan(&status, &planJSON, &expired, &upgradeVersion, &upgradeInventoryDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		if rolloutID > 0 {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "campaign upgrade job disappeared before flash", user.ID)
		}
		writeError(w, http.StatusNotFound, "upgrade not found")
		return
	}
	if err != nil {
		if rolloutID > 0 {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "campaign upgrade record could not be read before flash", user.ID)
		}
		writeError(w, http.StatusInternalServerError, "could not start the flash")
		return
	}
	if status != "ready" || expired {
		if rolloutID > 0 && expired {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "the staged image expired before flash confirmation", user.ID)
		}
		writeError(w, http.StatusConflict, "this upgrade is not ready, or its staged image has expired; prepare it again")
		return
	}
	var plan firmwarePlan
	if json.Unmarshal(planJSON, &plan) != nil {
		if rolloutID > 0 {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "the staged job plan could not be decoded", user.ID)
		}
		writeError(w, http.StatusInternalServerError, "could not read the upgrade plan")
		return
	}
	if rolloutID > 0 && len(plan.Warnings) > 0 {
		_ = tx.Rollback(r.Context())
		_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "campaign plans with warnings cannot be flashed", user.ID)
		writeError(w, http.StatusConflict, "campaign plans with warnings cannot be flashed")
		return
	}
	if len(plan.Warnings) > 0 && !in.AcknowledgeWarnings {
		writeError(w, http.StatusBadRequest, "acknowledge every warning in the plan to flash")
		return
	}
	var rolloutMember firmwareRolloutMember
	if rolloutID > 0 {
		var memberStatus string
		err = tx.QueryRow(r.Context(), `SELECT device_id_snapshot::text,device_name,model,current_release,board_name,target,
			inventory_id,inventory_digest,desired_package_set_revision,batch_number,status,plan,plan_digest
			FROM firmware_rollout_devices WHERE rollout_id=$1 AND upgrade_id=$2 FOR UPDATE`, rolloutID, id).Scan(
			&rolloutMember.DeviceID, &rolloutMember.DeviceName, &rolloutMember.Model, &rolloutMember.CurrentRelease,
			&rolloutMember.BoardName, &rolloutMember.Target, &rolloutMember.InventoryID, &rolloutMember.InventoryDigest,
			&rolloutMember.DesiredPackageSetRevision, &rolloutMember.BatchNumber, &memberStatus,
			&rolloutMember.Plan, &rolloutMember.PlanDigest)
		if err != nil || rolloutMember.DeviceID != deviceID || memberStatus != "ready" ||
			upgradeVersion != rolloutVersion || upgradeInventoryDigest != rolloutMember.InventoryDigest ||
			rolloutMember.BatchNumber != currentBatch ||
			(currentBatch == 0 && (rolloutStatus != "canary_running" || deviceID != canaryDeviceID)) ||
			(currentBatch > 0 && rolloutStatus != "batch_running") {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "campaign device was not ready in the active batch", user.ID)
			writeError(w, http.StatusConflict, "campaign device is not ready in the active batch")
			return
		}
		rolloutPlan, planErr := firmwareRolloutMemberPlan(rolloutMember, upgradeVersion)
		if planErr != nil {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "stale rollout plan: "+planErr.Error(), user.ID)
			writeError(w, http.StatusConflict, planErr.Error())
			return
		}
		jobPlanDigest, digestErr := firmwareRolloutPlanDigest(rolloutMember, upgradeVersion, plan)
		if digestErr != nil || jobPlanDigest != rolloutMember.PlanDigest || len(rolloutPlan.Warnings) > 0 || len(rolloutPlan.Blockers) > 0 {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "firmware job plan does not match the immutable rollout preview", user.ID)
			writeError(w, http.StatusConflict, "firmware job plan does not match the immutable rollout preview")
			return
		}
		current, currentErr := firmwareRolloutMemberCurrent(r.Context(), tx, rolloutMember)
		if currentErr != nil || !current {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "device, inventory, or desired package-set revision changed before flash", user.ID)
			writeError(w, http.StatusConflict, "device, inventory, or desired package-set revision changed before flash")
			return
		}
		qualification, qualificationErr := findMatchingFirmwareHardwareQualification(r.Context(), tx, rolloutMember, upgradeVersion)
		var linked bool
		if qualificationErr == nil {
			qualificationErr = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM firmware_rollout_qualifications
				WHERE rollout_id=$1 AND qualification_id=$2)`, rolloutID, qualification.ID).Scan(&linked)
		}
		if qualificationErr != nil || !linked {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "physical hardware qualification expired or no longer matches", user.ID)
			writeError(w, http.StatusConflict, "current physical hardware qualification is required before flash")
			return
		}
		var activeFlashes int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM firmware_rollout_devices
			WHERE rollout_id=$1 AND status IN ('flashing','verifying')`, rolloutID).Scan(&activeFlashes); err != nil {
			writeError(w, http.StatusInternalServerError, "could not check rollout flash concurrency")
			return
		}
		if activeFlashes >= maxConcurrent {
			writeError(w, http.StatusConflict, "the rollout has reached its concurrent flash limit")
			return
		}
	}
	var busy string
	err = tx.QueryRow(r.Context(), `SELECT CASE
		WHEN EXISTS(SELECT 1 FROM device_restores WHERE device_id=$1 AND status='running') THEN 'a backup restore is running on this device'
		WHEN EXISTS(SELECT 1 FROM config_changes WHERE device_id=$1 AND status='applying') THEN 'a configuration change is being applied to this device'
		WHEN `+packageInstallRunningSQL+` THEN '`+packageInstallBusyMessage+`'
		ELSE '' END`, deviceID).Scan(&busy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the flash")
		return
	}
	if busy != "" {
		writeError(w, http.StatusConflict, busy)
		return
	}
	var up firmwareUpgrade
	var imageHash string
	err = scanFirmwareUpgrade(tx.QueryRow(r.Context(), `UPDATE firmware_upgrades SET status='flashing', step='checking the device'
		WHERE id=$1 RETURNING `+firmwareUpgradeColumns, id), &up)
	if err == nil {
		imageHash = up.ImageSHA256
		err = insertAuditEvent(r.Context(), tx, user.ID, "device.firmware_upgrade_flash", "device", deviceID,
			map[string]any{"upgrade_id": id, "version": up.Version, "image_sha256": imageHash, "warnings_acknowledged": len(plan.Warnings)})
	}
	if err == nil && rolloutID > 0 {
		tag, updateErr := tx.Exec(r.Context(), `UPDATE firmware_rollout_devices SET status='flashing',updated_at=NOW()
			WHERE rollout_id=$1 AND upgrade_id=$2 AND status='ready'`, rolloutID, id)
		if updateErr != nil {
			err = updateErr
		} else if tag.RowsAffected() != 1 {
			err = errors.New("rollout device state changed before flash")
		}
	}
	if err == nil && rolloutID > 0 {
		err = insertAuditEvent(r.Context(), tx, user.ID, "firmware.rollout_device_flash", "firmware_rollout",
			fmt.Sprint(rolloutID), map[string]any{"device_id": deviceID, "upgrade_id": id, "batch_number": rolloutMember.BatchNumber})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if rolloutID > 0 {
			_ = tx.Rollback(r.Context())
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "audit or persistence failure while starting a campaign flash", user.ID)
		}
		writeError(w, http.StatusInternalServerError, "could not start the flash")
		return
	}
	go a.runFlash(id)
	writeJSON(w, http.StatusAccepted, up)
}

// preFlashProblem re-checks everything Ready promised, on the live device. It returns a
// reason to stop, or "" when the flash may be sent.
func (a *app) preFlashProblem(session *deviceSession, rec upgradeRecord) string {
	if rec.BootID != "" {
		boot := session.run(opBootID)
		if boot.Status != "ok" || boot.Output != rec.BootID {
			return "the device restarted after the image was staged"
		}
	}
	inv := session.run(firmwareInventoryOperation)
	if inv.Status != "ok" {
		return "could not re-read the device's packages"
	}
	m, err := parseFirmwareInventory(inv.Output)
	if err != nil {
		return "could not re-read the device's packages"
	}
	if digest, err := firmwareManifestDigest(m); err != nil || digest != rec.InventoryDigest {
		return "the device's packages or release changed after the image was prepared"
	}
	res := session.runWriteRaw(opUpgradeHash, nil)
	if res.Status != "ok" || !strings.HasPrefix(res.Output, rec.ImageSHA256) {
		return "the staged image is missing or changed"
	}
	if res := session.runWriteRaw(opUpgradeCheck, nil); res.Status != "ok" {
		return "the device's own check (sysupgrade -T) no longer accepts the image"
	}
	return ""
}

// runFlash re-checks the device, sends the one flash command and hands over to the
// verification. A failure before the command ends the job as stopped: nothing flashed.
func (a *app) runFlash(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), flashJobTimeout)
	defer cancel()
	stopUnflashed := func(reason string) {
		sctx, scancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer scancel()
		if stopped, deviceID := a.markUpgradeStopped(sctx, id, "flashing", "not flashed: "+reason, ""); stopped {
			a.cleanupStaged(sctx, deviceID)
		}
		a.pauseFirmwareRolloutForUpgrade(sctx, id, "stopped", "flash did not complete safely: "+reason)
	}
	rec, err := a.loadUpgradeRecord(ctx, id)
	if err != nil {
		stopUnflashed("could not load the upgrade")
		return
	}
	// The hash is the command's only input; it must be exactly what preparation stored.
	if !imageHashPattern.MatchString(rec.ImageSHA256) {
		stopUnflashed("the upgrade has no valid image hash")
		return
	}
	target, err := a.loadDeviceTarget(ctx, rec.DeviceID)
	if err != nil {
		_, msg := targetError(err)
		stopUnflashed(msg)
		return
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		stopUnflashed("could not open a verified SSH session to the device")
		return
	}
	if problem := a.preFlashProblem(session, rec); problem != "" {
		session.Close()
		stopUnflashed(problem)
		return
	}
	// Recorded before the command is sent: from here on the device may have flashed, and
	// a restarted server must verify rather than assume nothing happened.
	sent := time.Now()
	if _, err := a.db.Exec(ctx, `UPDATE firmware_upgrades SET flash_sent_at=$2, step='flashing' WHERE id=$1 AND status='flashing'`, id, sent); err != nil {
		session.Close()
		stopUnflashed("could not record the flash")
		return
	}
	res := session.runWriteRaw(opUpgradeFlash, []byte(rec.ImageSHA256+"\n"))
	session.Close()
	if res.Status != "ok" {
		// Not decisive: the connection may have dropped as the device began. The
		// verification tells whether it restarted.
		log.Printf("firmware upgrade %d: the flash command did not report success (%s)", id, res.Error)
	}
	if _, err := a.db.Exec(ctx, `UPDATE firmware_upgrades SET status='verifying', step='waiting for the device' WHERE id=$1 AND status='flashing'`, id); err != nil {
		log.Printf("firmware upgrade %d: could not record that verification started", id)
		a.pauseFirmwareRolloutForUpgrade(context.Background(), id, "needs_attention", "audit/state failure after the flash command; verification state is ambiguous")
	} else if rec.RolloutID > 0 {
		if err := a.setFirmwareRolloutDeviceStatus(ctx, id, "verifying", ""); err != nil {
			a.pauseFirmwareRolloutForUpgrade(context.Background(), id, "needs_attention", "could not record rollout verification state")
		}
	}
	rec.FlashSentAt = &sent
	a.verifyUpgrade(ctx, rec)
}

// runVerify resumes verification of a job after a server restart.
func (a *app) runVerify(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), flashJobTimeout)
	defer cancel()
	rec, err := a.loadUpgradeRecord(ctx, id)
	if err != nil || rec.FlashSentAt == nil {
		return
	}
	_, _ = a.db.Exec(ctx, `UPDATE firmware_upgrades SET status='verifying', step='waiting for the device' WHERE id=$1 AND status='flashing'`, id)
	if rec.RolloutID > 0 {
		if err := a.setFirmwareRolloutDeviceStatus(ctx, id, "verifying", ""); err != nil {
			a.pauseFirmwareRolloutForUpgrade(ctx, id, "needs_attention", "could not restore rollout verification state after restart")
		}
	}
	a.verifyUpgrade(ctx, rec)
}

const recoveryGuidance = "Wait the full ten minutes (some devices write flash slowly), power-cycle it once, then try failsafe mode, " +
	"then serial or TFTP recovery for this model (see its OpenWrt wiki page). The backup taken before the flash stays pinned."

// verifyUpgrade waits for the device to restart and come back, then runs the post-flash
// checks and records the outcome.
func (a *app) verifyUpgrade(ctx context.Context, rec upgradeRecord) {
	sent := time.Now()
	if rec.FlashSentAt != nil {
		sent = *rec.FlashSentAt
	}
	deadline := sent.Add(flashReturnTimeout)
	sawDown := false
	for {
		msg, boot, ok := a.verifyAfterRestore(ctx, rec.DeviceID)
		if msg != "" {
			a.finishUpgrade(rec, "needs_attention", msg+". OpenManage never re-pins a host key by itself; check the device before enrolling it again.",
				[]upgradeCheck{{"Same device", "failed", msg}})
			return
		}
		switch restartVerdict(boot, rec.BootID, ok, sawDown, time.Since(sent)) {
		case "restarted":
			a.finishUpgrade(rec, "", "", a.postFlashChecks(ctx, rec))
			return
		case "never_restarted":
			// Nothing was written, so the staged image is only taking up memory.
			cctx, ccancel := context.WithTimeout(context.Background(), 40*time.Second)
			a.cleanupStaged(cctx, rec.DeviceID)
			ccancel()
			a.finishUpgrade(rec, "needs_attention",
				"The device kept running after the flash command and did not restart, so the new firmware was probably not written (the image may have been refused). It is probably still on its old release.",
				[]upgradeCheck{{"Went down", "failed", "same boot after " + flashDownWait.String()}})
			return
		}
		if !ok {
			sawDown = true
		}
		if time.Now().After(deadline) {
			a.finishUpgrade(rec, "needs_attention", "The device did not come back within "+flashReturnTimeout.String()+". "+recoveryGuidance,
				[]upgradeCheck{{"Came back", "failed", "no answer within " + flashReturnTimeout.String()}})
			return
		}
		select {
		case <-time.After(flashPollInterval):
		case <-ctx.Done():
			a.finishUpgrade(rec, "needs_attention", "OpenManage stopped waiting for the device. "+recoveryGuidance,
				[]upgradeCheck{{"Came back", "failed", "stopped waiting"}})
			return
		}
	}
}

// postFlashChecks runs once the device has restarted and answers on its pinned key.
func (a *app) postFlashChecks(ctx context.Context, rec upgradeRecord) []upgradeCheck {
	checks := []upgradeCheck{
		{"Went down", "ok", "the device restarted"},
		{"Came back", "ok", "answered over SSH on the pinned host key with OpenManage's key"},
	}
	var m firmwareManifest
	var failure *captureFailure
	for attempt := 0; attempt < 6; attempt++ {
		var inv firmwareInventoryResult
		if inv, failure = a.takeFirmwareInventory(ctx, rec.DeviceID, ""); failure == nil {
			m = inv.Inventory.Manifest
			break
		}
		select {
		case <-time.After(flashPollInterval):
		case <-ctx.Done():
		}
	}
	if failure != nil {
		return append(checks, upgradeCheck{"New release", "failed", "could not read the device: " + failure.msg})
	}
	if m.Release == rec.Version {
		checks = append(checks, upgradeCheck{"New release", "ok", "runs OpenWrt " + m.Release})
	} else {
		checks = append(checks, upgradeCheck{"New release", "failed", "runs OpenWrt " + m.Release + ", expected " + rec.Version})
	}
	var before firmwareManifest
	if rec.FlashSentAt != nil && a.db.QueryRow(ctx, `SELECT manifest FROM firmware_inventories WHERE device_id=$1 AND captured_at <= $2
		ORDER BY captured_at DESC, id DESC LIMIT 1`, rec.DeviceID, *rec.FlashSentAt).Scan(&before) == nil &&
		before.BoardName != "" && m.BoardName != before.BoardName {
		checks = append(checks, upgradeCheck{"Same board", "failed", "reports board " + m.BoardName + ", was " + before.BoardName})
	}
	checks = append(checks, a.postFlashHealth(ctx, rec.DeviceID))
	if missing := missingPackages(expectedPackages(rec.Plan), m.Packages); len(missing) > 0 {
		checks = append(checks, upgradeCheck{"Packages", "warning", "missing after the upgrade: " + strings.Join(missing, ", ")})
	} else {
		checks = append(checks, upgradeCheck{"Packages", "ok", "every package the plan asked for is installed"})
	}
	// Packages the official feeds lack were left out of the image on purpose. Report which
	// are missing and which confirmed sources could restore them; never install on its own.
	if len(rec.Plan.Unavailable) > 0 || len(rec.Plan.DesiredSources) > 0 {
		candidates, err := a.reinstallCandidatesWithDesired(ctx, rec.Plan.Unavailable, rec.Plan.DesiredSources, m)
		if err != nil {
			checks = append(checks, upgradeCheck{"Third-party packages", "warning", "could not look up package sources"})
		} else {
			checks = append(checks, reinstallCheck(candidates))
		}
	}
	// Release migrations change settings, so differences are for a person to review, not
	// a failure and never rewritten.
	snap, sfail := a.captureSnapshot(ctx, rec.DeviceID, "", false)
	switch {
	case sfail != nil:
		checks = append(checks, upgradeCheck{"Settings", "warning", "could not take a configuration snapshot to compare: " + sfail.msg})
	case snap.Changed:
		checks = append(checks, upgradeCheck{"Settings", "warning", fmt.Sprintf("%d settings differ from before the upgrade; review them in the device's configuration view", snap.ChangeCount)})
	default:
		checks = append(checks, upgradeCheck{"Settings", "ok", "the configuration matches what it was before"})
	}
	return checks
}

func (a *app) postFlashHealth(ctx context.Context, deviceID string) upgradeCheck {
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		return upgradeCheck{"Health", "warning", "could not load the device"}
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		return upgradeCheck{"Health", "warning", "could not open an SSH session for the health check"}
	}
	defer session.Close()
	var failed []string
	for _, op := range healthOperations {
		if res := session.run(op); res.Status != "ok" {
			failed = append(failed, op.Label)
		}
	}
	if len(failed) > 0 {
		return upgradeCheck{"Health", "warning", "these checks failed: " + strings.Join(failed, ", ")}
	}
	return upgradeCheck{"Health", "ok", "the normal health check passes"}
}

// finishUpgrade records the outcome. An empty status means it follows from the checks.
func (a *app) finishUpgrade(rec upgradeRecord, status, detail string, checks []upgradeCheck) {
	if status == "" {
		status = upgradeOutcome(checks)
		warnings := 0
		for _, c := range checks {
			if c.Status != "ok" {
				warnings++
			}
		}
		switch status {
		case "done":
			detail = "The device runs OpenWrt " + rec.Version + " and passed every check."
		case "done_with_warnings":
			detail = fmt.Sprintf("The device runs OpenWrt %s, but %d checks need a look.", rec.Version, warnings)
		default:
			detail = "The device came back but failed a check; see the results."
			for _, c := range checks {
				if c.Status == "failed" {
					detail = "The device came back, but: " + c.Detail
					break
				}
			}
		}
	}
	result, _ := json.Marshal(checks)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		log.Printf("firmware upgrade %d finished as %s but could not be recorded", rec.ID, status)
		if rec.RolloutID > 0 {
			a.pauseFirmwareRolloutForUpgrade(ctx, rec.ID, "needs_attention", "could not begin result persistence")
		}
		return
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE firmware_upgrades SET status=$2, detail=$3, result=$4, step='', finished_at=NOW()
		WHERE id=$1 AND status IN ('flashing','verifying')`, rec.ID, status, detail, result)
	if err == nil && tag.RowsAffected() != 1 {
		err = errors.New("firmware upgrade state changed before result recording")
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, "", "device.firmware_upgrade_"+status, "device", rec.DeviceID,
			map[string]any{"upgrade_id": rec.ID, "version": rec.Version, "detail": detail})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		log.Printf("firmware upgrade %d finished as %s but could not be recorded", rec.ID, status)
		if rec.RolloutID > 0 {
			a.pauseFirmwareRolloutForUpgrade(ctx, rec.ID, "needs_attention", "could not persist or audit the flash verification result")
		}
		return
	}
	if rec.RolloutID > 0 {
		if status == "done" {
			if err := a.setFirmwareRolloutDeviceStatus(ctx, rec.ID, "done", ""); err != nil {
				a.pauseFirmwareRolloutForUpgrade(ctx, rec.ID, "needs_attention", "could not audit successful rollout device verification")
			}
		} else {
			a.pauseFirmwareRolloutForUpgrade(ctx, rec.ID, status, detail)
		}
	}
}

// acknowledgeFirmwareUpgrade records that a person reviewed the outcome, which releases
// the backup the job pinned.
func (a *app) acknowledgeFirmwareUpgrade(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	var id int64
	if _, err := fmt.Sscanf(r.PathValue("job"), "%d", &id); err != nil || !validUUID(deviceID) || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid device or upgrade")
		return
	}
	if !requireOperationRoles(w, r, upgradeOperations) {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	var rolloutID int64
	lookupErr := a.db.QueryRow(r.Context(), `SELECT rollout_id FROM firmware_rollout_devices
		WHERE upgrade_id=$1 AND device_id_snapshot=$2`, id, deviceID).Scan(&rolloutID)
	if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "could not inspect rollout membership")
		return
	}
	if rolloutID > 0 && user.Role != "admin" {
		writeError(w, http.StatusForbidden, "only an administrator can acknowledge a campaign result")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not acknowledge the upgrade")
		return
	}
	defer tx.Rollback(r.Context())
	var lockedDevice string
	if err = tx.QueryRow(r.Context(), `SELECT id::text FROM devices WHERE id=$1 FOR UPDATE`, deviceID).Scan(&lockedDevice); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "could not lock the device")
		return
	}
	var lockedUpgradeStatus string
	if err = tx.QueryRow(r.Context(), `SELECT status FROM firmware_upgrades WHERE id=$1 AND device_id=$2 FOR UPDATE`, id, deviceID).Scan(&lockedUpgradeStatus); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "upgrade not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "could not lock the upgrade")
		return
	}
	var rolloutStatus string
	var currentBatch int
	if rolloutID > 0 {
		if err = tx.QueryRow(r.Context(), `SELECT status,current_batch FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, rolloutID).Scan(&rolloutStatus, &currentBatch); err != nil {
			writeError(w, http.StatusInternalServerError, "could not lock the rollout")
			return
		}
	}
	var backupID *string
	var pinned bool
	err = tx.QueryRow(r.Context(), `UPDATE firmware_upgrades SET acknowledged_at=NOW(), acknowledged_by=$3
		WHERE id=$1 AND device_id=$2 AND status IN ('done','done_with_warnings','needs_attention') AND acknowledged_at IS NULL
		RETURNING backup_id::text, backup_pinned_by_job`, id, deviceID, user.ID).Scan(&backupID, &pinned)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "this upgrade has no finished result to acknowledge, or it was already acknowledged")
		return
	}
	if err == nil && pinned && backupID != nil {
		_, err = tx.Exec(r.Context(), `UPDATE device_backups SET pinned=FALSE WHERE id=$1`, *backupID)
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "device.firmware_upgrade_acknowledge", "device", deviceID, map[string]any{"upgrade_id": id})
	}
	if err == nil && rolloutID > 0 && (rolloutStatus == "canary_running" || rolloutStatus == "batch_running") {
		var total, acknowledged int
		err = tx.QueryRow(r.Context(), `SELECT count(*)::int,
			count(*) FILTER (WHERE rd.status='done' AND u.status='done' AND u.acknowledged_at IS NOT NULL)::int
			FROM firmware_rollout_devices rd LEFT JOIN firmware_upgrades u ON u.id=rd.upgrade_id
			WHERE rd.rollout_id=$1 AND rd.batch_number=$2`, rolloutID, currentBatch).Scan(&total, &acknowledged)
		if err == nil && total > 0 && total == acknowledged {
			_, err = tx.Exec(r.Context(), `UPDATE firmware_rollouts SET status='awaiting_batch',updated_at=NOW() WHERE id=$1`, rolloutID)
			if err == nil {
				err = insertAuditEvent(r.Context(), tx, user.ID, "firmware.rollout_batch_acknowledged", "firmware_rollout",
					fmt.Sprint(rolloutID), map[string]any{"batch_number": currentBatch})
			}
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		_ = tx.Rollback(r.Context())
		if rolloutID > 0 {
			_ = a.setFirmwareRolloutPaused(r.Context(), rolloutID, "audit or persistence failure while acknowledging a campaign result", user.ID)
		}
		writeError(w, http.StatusInternalServerError, "could not acknowledge the upgrade")
		return
	}
	var up firmwareUpgrade
	if err := scanFirmwareUpgrade(a.db.QueryRow(r.Context(), `SELECT `+firmwareUpgradeColumns+` FROM firmware_upgrades WHERE id=$1`, id), &up); err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the upgrade")
		return
	}
	writeJSON(w, http.StatusOK, up)
}
