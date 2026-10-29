package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Preparing a firmware upgrade (docs/firmware-upgrades.md, "Flash and verify design").
// Preparation builds the image through the build server, checks its hash, takes and pins
// a backup, stages the image on the device and runs sysupgrade -T. The only device write
// is one file under /tmp, which a reboot clears, so clients notice nothing. Nothing here
// flashes: that is a separate, confirmed step.
//
//   - every device command is a fixed string; the image goes in on stdin to a fresh
//     directory that only root can have created, and is hash-checked on the device;
//   - the image is held in memory for the job only; the database keeps its name, size
//     and hash;
//   - at most one open job per device (partial unique index), and a restore or a config
//     change refuses to start while a job is open;
//   - a staged image expires after upgradeReadyTTL and is removed from the device.

const (
	upgradeDir             = "/tmp/.om-firmware"
	upgradeImagePath       = upgradeDir + "/image.bin"
	upgradeImageLimit      = 64 << 20
	upgradeRAMMargin       = 16 << 20
	upgradeBuildLimit      = 15 * time.Minute
	upgradeJobTimeout      = 40 * time.Minute
	upgradeReadyTTL        = 30 * time.Minute
	upgradeUploadTimeout   = 5 * time.Minute
	upgradeCheckTimeout    = 60 * time.Second
	upgradeDownloadTimeout = 5 * time.Minute
	upgradeBodyLimit       = 512
	asuBodyLimit           = 1 << 20
	upgradeListLimit       = 10
)

// upgradeImageSlots bounds how many images are held in memory at once (download to
// staged), since each can be up to upgradeImageLimit.
var upgradeImageSlots = make(chan struct{}, 2)

// upgradeBuildPoll is how often a pending build is polled; tests shorten it.
var upgradeBuildPoll = 5 * time.Second

var (
	asuHashPattern   = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)
	asuImagePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
	imageHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func upgradeOperation(name, label, command string, timeout time.Duration) operation {
	return operation{
		Name: name, Label: label, Class: operationWrite, Roles: []string{"admin", "operator"},
		Timeout: timeout, OutputLimit: healthOutputLimit, Audit: auditTransactional,
		command: command,
	}
}

var (
	// mkdir fails if the name exists, so a local process cannot pre-create the path and
	// swap the file between the hash check and the validation.
	opUpgradeUpload = upgradeOperation("firmware_stage", "Stage firmware image",
		"umask 077; mkdir -m 700 "+upgradeDir+" && cat > "+upgradeImagePath, upgradeUploadTimeout)
	opUpgradeHash = upgradeOperation("firmware_stage_hash", "Check staged firmware image",
		"sha256sum "+upgradeImagePath, healthCheckTimeout)
	// sysupgrade -T is OpenWrt's own test that the image fits this board. Its output is
	// dropped; only the exit status matters.
	opUpgradeCheck = upgradeOperation("firmware_check", "Validate staged firmware image",
		"sysupgrade -T "+upgradeImagePath+" >/dev/null 2>&1", upgradeCheckTimeout)
	opUpgradeCleanup = upgradeOperation("firmware_stage_cleanup", "Remove staged firmware image",
		"rm -rf "+upgradeDir, healthCheckTimeout)
)

var upgradeOperations = []operation{opUpgradeUpload, opUpgradeHash, opUpgradeCheck, opUpgradeCleanup}

const openUpgradeSQL = `SELECT EXISTS(SELECT 1 FROM firmware_upgrades WHERE device_id=$1
	AND status IN ('preparing','ready','flashing','verifying'))`

// lockDeviceSQL takes a lock on the device's row for the rest of the transaction. Every
// path that claims a device for a long job (upgrade, restore, config change) takes it
// first, so two claims cannot both see the device as idle. NO KEY UPDATE does not block
// inserts of rows that reference the device.
const lockDeviceSQL = `SELECT 1 FROM devices WHERE id=$1 FOR NO KEY UPDATE`

const openUpgradeMessage = "a firmware upgrade is open on this device; cancel or finish it first"

type firmwareUpgrade struct {
	ID          int64           `json:"id"`
	DeviceID    string          `json:"device_id"`
	Version     string          `json:"version"`
	Status      string          `json:"status"`
	Step        string          `json:"step"`
	Detail      string          `json:"detail"`
	Plan        json.RawMessage `json:"plan"`
	ImageName   string          `json:"image_name"`
	ImageSHA256 string          `json:"image_sha256"`
	ImageSize   int64           `json:"image_size"`
	BackupID    *string         `json:"backup_id"`
	CreatedAt   time.Time       `json:"created_at"`
	ReadyAt     *time.Time      `json:"ready_at"`
	ExpiresAt   *time.Time      `json:"expires_at"`
	FinishedAt  *time.Time      `json:"finished_at"`
}

const firmwareUpgradeColumns = `id, device_id::text, version, status, step, detail, plan, image_name, image_sha256, image_size,
	backup_id::text, created_at, ready_at, expires_at, finished_at`

func scanFirmwareUpgrade(row pgx.Row, u *firmwareUpgrade) error {
	return row.Scan(&u.ID, &u.DeviceID, &u.Version, &u.Status, &u.Step, &u.Detail, &u.Plan, &u.ImageName, &u.ImageSHA256,
		&u.ImageSize, &u.BackupID, &u.CreatedAt, &u.ReadyAt, &u.ExpiresAt, &u.FinishedAt)
}

// upgradePathProblem says why an upgrade from one release to another is not allowed in
// this step, or "" when it is: the same release family or the next one only, never a
// downgrade, never a snapshot. branches lists the release branches the build server
// offers, so "next" means the next one it knows.
func upgradePathProblem(from, to string, branches []string) string {
	rvFrom, okFrom := parseReleaseVersion(from)
	rvTo, okTo := parseReleaseVersion(to)
	if !okFrom || !okTo {
		return "the release numbers are not recognised"
	}
	if rvTo.snapshot || rvTo.branchSn {
		return "snapshots are not flashed by OpenManage yet"
	}
	if rvFrom.snapshot || rvFrom.branchSn {
		return "this device runs a snapshot, which OpenManage does not upgrade yet"
	}
	if compareReleases(to, from) < 0 {
		return "downgrades are not supported"
	}
	fromBranch, toBranch := releaseBranch(from), releaseBranch(to)
	if fromBranch == toBranch {
		return ""
	}
	if !containsString(branches, fromBranch) {
		return "this device runs OpenWrt " + fromBranch + ", which the build server no longer lists, so the next release cannot be worked out"
	}
	next := ""
	for _, b := range branches {
		if b > fromBranch && (next == "" || b < next) {
			next = b
		}
	}
	if toBranch != next {
		return "only the same release family or the next one (" + next + ") is allowed; upgrade step by step"
	}
	return ""
}

// asuImage is one image in a build result.
type asuImage struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Filesystem string `json:"filesystem"`
	SHA256     string `json:"sha256"`
}

// asuBuildResult is the part of ASU's build answer the upgrade uses. Pending builds
// answer HTTP 202 with a request_hash to poll.
type asuBuildResult struct {
	Detail        string     `json:"detail"`
	RequestHash   string     `json:"request_hash"`
	QueuePosition int        `json:"queue_position"`
	BinDir        string     `json:"bin_dir"`
	Images        []asuImage `json:"images"`
}

// pickSysupgradeImage chooses the image to flash: the sysupgrade image, preferring
// squashfs when the build offers several filesystems. It refuses to guess otherwise.
func pickSysupgradeImage(images []asuImage) (asuImage, error) {
	var all, squash []asuImage
	for _, img := range images {
		if img.Type != "sysupgrade" || !asuImagePattern.MatchString(img.Name) || !imageHashPattern.MatchString(strings.ToLower(img.SHA256)) {
			continue
		}
		all = append(all, img)
		if img.Filesystem == "squashfs" {
			squash = append(squash, img)
		}
	}
	switch {
	case len(all) == 1:
		return all[0], nil
	case len(squash) == 1:
		return squash[0], nil
	case len(all) == 0:
		return asuImage{}, errors.New("the build has no sysupgrade image with a hash")
	}
	return asuImage{}, errors.New("the build offers several sysupgrade images and none can be chosen safely")
}

func asuBuildRequestBody(plan firmwarePlan) ([]byte, error) {
	return json.Marshal(map[string]any{
		"distro": "openwrt", "version": plan.Version, "version_code": plan.VersionCode,
		"target": plan.Target, "profile": plan.Profile, "packages": plan.Packages, "diff_packages": false,
	})
}

func asuDo(ctx context.Context, method, rawURL string, body []byte) (int, asuBuildResult, error) {
	var out asuBuildResult
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return 0, out, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "OpenManage")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := buildServer.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, out, ctx.Err()
		}
		return 0, out, errors.New("could not reach the build server")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, asuBodyLimit+1))
	if err != nil || len(raw) > asuBodyLimit {
		return 0, out, errors.New("the build server's answer could not be read")
	}
	// Error answers need not be JSON; the status code is what counts.
	_ = json.Unmarshal(raw, &out)
	out.Detail = cleanInventoryField(out.Detail)
	return resp.StatusCode, out, nil
}

// buildFirmwareImage asks the build server for the planned image and waits for it,
// up to upgradeBuildLimit.
func buildFirmwareImage(ctx context.Context, serverURL string, plan firmwarePlan, progress func(string)) (asuBuildResult, error) {
	ctx, cancel := context.WithTimeout(ctx, upgradeBuildLimit)
	defer cancel()
	body, err := asuBuildRequestBody(plan)
	if err != nil {
		return asuBuildResult{}, err
	}
	code, res, err := asuDo(ctx, http.MethodPost, serverURL+"/api/v1/build", body)
	for {
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return res, errors.New("the build did not finish within " + upgradeBuildLimit.String())
			}
			return res, err
		}
		switch code {
		case http.StatusOK:
			return res, nil
		case http.StatusAccepted:
		default:
			msg := fmt.Sprintf("the build server refused the build (HTTP %d)", code)
			if res.Detail != "" {
				msg += ": " + res.Detail
			}
			return res, errors.New(msg)
		}
		if !asuHashPattern.MatchString(res.RequestHash) {
			return res, errors.New("the build server did not return a usable build id")
		}
		if res.QueuePosition > 0 {
			progress(fmt.Sprintf("building (queue position %d)", res.QueuePosition))
		} else {
			progress("building")
		}
		hash := res.RequestHash
		select {
		case <-time.After(upgradeBuildPoll):
		case <-ctx.Done():
			return res, ctx.Err()
		}
		code, res, err = asuDo(ctx, http.MethodGet, serverURL+"/api/v1/build/"+hash, nil)
	}
}

var imageClient = &http.Client{Timeout: upgradeDownloadTimeout, CheckRedirect: refuseCrossHostRedirect}

func downloadFirmwareImage(ctx context.Context, serverURL, binDir, name string) ([]byte, error) {
	if !asuHashPattern.MatchString(binDir) || !asuImagePattern.MatchString(name) {
		return nil, errors.New("the build server returned an unusable image address")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/store/"+binDir+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "OpenManage")
	resp, err := imageClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("could not download the image from the build server")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the build server answered HTTP %d for the image", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, upgradeImageLimit+1))
	if err != nil {
		return nil, errors.New("the image download was cut off")
	}
	if len(data) > upgradeImageLimit {
		return nil, errors.New("the image is larger than the 64 MiB limit")
	}
	if len(data) == 0 {
		return nil, errors.New("the image is empty")
	}
	return data, nil
}

// upgradeRuns tracks the running preparation of each job so it can be cancelled.
var upgradeRuns = struct {
	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
}{cancels: map[int64]context.CancelFunc{}}

func registerUpgradeRun(id int64, cancel context.CancelFunc) {
	upgradeRuns.mu.Lock()
	upgradeRuns.cancels[id] = cancel
	upgradeRuns.mu.Unlock()
}

func releaseUpgradeRun(id int64) {
	upgradeRuns.mu.Lock()
	delete(upgradeRuns.cancels, id)
	upgradeRuns.mu.Unlock()
}

func cancelUpgradeRun(id int64) bool {
	upgradeRuns.mu.Lock()
	cancel, ok := upgradeRuns.cancels[id]
	upgradeRuns.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// markUpgradeStopped ends a job that is still in status from, unpins the backup the job
// pinned, and writes the audit event in the same transaction. It reports whether this
// call ended it.
func (a *app) markUpgradeStopped(ctx context.Context, id int64, from, reason, actorID string) (bool, string) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return false, ""
	}
	defer tx.Rollback(ctx)
	var deviceID, version string
	var backupID *string
	var pinned bool
	err = tx.QueryRow(ctx, `UPDATE firmware_upgrades SET status='stopped', step='', detail=$3, finished_at=NOW()
		WHERE id=$1 AND status=$2 RETURNING device_id::text, version, backup_id::text, backup_pinned_by_job`,
		id, from, reason).Scan(&deviceID, &version, &backupID, &pinned)
	if err != nil {
		return false, ""
	}
	if pinned && backupID != nil {
		_, err = tx.Exec(ctx, `UPDATE device_backups SET pinned=FALSE WHERE id=$1`, *backupID)
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, actorID, "device.firmware_upgrade_stopped", "device", deviceID,
			map[string]any{"upgrade_id": id, "version": version, "reason": reason})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		log.Printf("firmware upgrade %d: could not record that it stopped", id)
		return false, ""
	}
	return true, deviceID
}

// cleanupStaged removes the staged image from the device. It is best effort: the file
// lives in /tmp, so a reboot clears it anyway.
func (a *app) cleanupStaged(ctx context.Context, deviceID string) {
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		return
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		return
	}
	defer session.Close()
	_ = session.runWriteRaw(opUpgradeCleanup, nil)
}

// endReadyUpgrade ends a prepared job (cancelled by a person, or expired) and removes
// the staged image. It reports whether the job was still ready.
func (a *app) endReadyUpgrade(ctx context.Context, id int64, reason, actorID string) bool {
	stopped, deviceID := a.markUpgradeStopped(ctx, id, "ready", reason, actorID)
	if stopped {
		a.cleanupStaged(ctx, deviceID)
	}
	return stopped
}

type upgradeJob struct {
	a        *app
	id       int64
	deviceID string
	version  string
	userID   string
	staged   bool
}

func (j *upgradeJob) setStep(ctx context.Context, step string) {
	_, _ = j.a.db.Exec(ctx, `UPDATE firmware_upgrades SET step=$2 WHERE id=$1 AND status='preparing'`, j.id, step)
}

// stop ends the job as stopped. It uses its own context because the job's may be the
// reason it is stopping.
func (j *upgradeJob) stop(ctx context.Context, reason string, actorID string) {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		reason = "cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		reason = "the preparation took too long"
	}
	sctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if j.staged {
		j.a.cleanupStaged(sctx, j.deviceID)
	}
	j.a.markUpgradeStopped(sctx, j.id, "preparing", reason, actorID)
}

// run prepares the image. Every failure ends the job as stopped with a plain reason;
// nothing here flashes.
func (j *upgradeJob) run(ctx context.Context) {
	a := j.a
	fail := func(reason string) { j.stop(ctx, reason, "") }

	j.setStep(ctx, "reading the device")
	inv, failure := a.takeFirmwareInventory(ctx, j.deviceID, j.userID)
	if failure != nil {
		fail("could not read the device: " + failure.msg)
		return
	}
	manifest := inv.Inventory.Manifest
	digest, err := firmwareManifestDigest(manifest)
	if err != nil {
		fail("could not fingerprint the device's packages")
		return
	}

	j.setStep(ctx, "planning")
	serverURL, err := a.firmwareBuildServer(ctx)
	if err != nil {
		fail(err.Error())
		return
	}
	ov, err := fetchOverview(ctx, serverURL)
	if err != nil {
		fail(err.Error())
		return
	}
	var branches []string
	for name := range ov.Branches {
		if !strings.Contains(name, "SNAPSHOT") {
			branches = append(branches, name)
		}
	}
	sort.Strings(branches)
	if problem := upgradePathProblem(manifest.Release, j.version, branches); problem != "" {
		fail(problem)
		return
	}
	plan, err := firmwarePlanFor(ctx, serverURL, manifest, j.version)
	if err != nil {
		fail(err.Error())
		return
	}
	if len(plan.Blockers) > 0 {
		fail(strings.Join(plan.Blockers, " "))
		return
	}
	if encoded, err := json.Marshal(plan); err == nil {
		_, _ = a.db.Exec(ctx, `UPDATE firmware_upgrades SET plan=$2 WHERE id=$1`, j.id, encoded)
	}

	j.setStep(ctx, "building")
	built, err := buildFirmwareImage(ctx, serverURL, plan, func(s string) { j.setStep(ctx, s) })
	if err != nil {
		fail(err.Error())
		return
	}
	img, err := pickSysupgradeImage(built.Images)
	if err != nil {
		fail(err.Error())
		return
	}

	j.setStep(ctx, "waiting for a download slot")
	select {
	case upgradeImageSlots <- struct{}{}:
	case <-ctx.Done():
		fail("")
		return
	}
	defer func() { <-upgradeImageSlots }()
	j.setStep(ctx, "downloading")
	image, err := downloadFirmwareImage(ctx, serverURL, built.BinDir, img.Name)
	if err != nil {
		fail(err.Error())
		return
	}
	sum := sha256.Sum256(image)
	imageHash := hex.EncodeToString(sum[:])
	if imageHash != strings.ToLower(img.SHA256) {
		fail("the downloaded image does not match the hash the build server reported")
		return
	}
	if manifest.MemAvailableKB <= 0 {
		fail("the device did not report its free memory, so it cannot be checked for room to flash")
		return
	}
	if int64(manifest.MemAvailableKB)*1024 < int64(len(image))+upgradeRAMMargin {
		fail("the device does not have enough free memory to stage and flash this image")
		return
	}

	j.setStep(ctx, "backing up")
	backup, bfail := a.captureBackup(ctx, j.deviceID, j.userID)
	if bfail != nil {
		fail("could not take the pre-flash backup: " + bfail.msg)
		return
	}
	// Pinned and recorded together, so a stop or a restart unpins exactly what this
	// job pinned.
	if err := j.pinBackup(ctx, backup.Backup); err != nil {
		fail("could not pin the pre-flash backup")
		return
	}

	j.setStep(ctx, "staging")
	bootID, reason := j.stageOnDevice(ctx, image, imageHash)
	if reason != "" {
		fail(reason)
		return
	}
	imageSize := len(image)
	image = nil

	tx, err := a.db.Begin(ctx)
	if err != nil {
		fail("could not record that the image is ready")
		return
	}
	_, err = tx.Exec(ctx, `UPDATE firmware_upgrades SET status='ready', step='', detail='', image_name=$2, image_sha256=$3,
		image_size=$4, boot_id=$5, inventory_digest=$6, ready_at=NOW(), expires_at=NOW() + ($7 * INTERVAL '1 second')
		WHERE id=$1 AND status='preparing'`, j.id, img.Name, imageHash, imageSize, bootID, digest, int(upgradeReadyTTL.Seconds()))
	if err == nil {
		err = insertAuditEvent(ctx, tx, j.userID, "device.firmware_upgrade_ready", "device", j.deviceID,
			map[string]any{"upgrade_id": j.id, "version": j.version, "image_sha256": imageHash, "backup_id": backup.Backup.ID})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		// Released before stopping, which updates the same row in its own transaction.
		_ = tx.Rollback(ctx)
		fail("could not record that the image is ready")
		return
	}
	id := j.id
	time.AfterFunc(upgradeReadyTTL, func() {
		ectx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		a.endReadyUpgrade(ectx, id, "the staged image expired; prepare the upgrade again", "")
	})
}

func (j *upgradeJob) pinBackup(ctx context.Context, b deviceBackup) error {
	tx, err := j.a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE device_backups SET pinned=TRUE WHERE id=$1 AND device_id=$2`, b.ID, j.deviceID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE firmware_upgrades SET backup_id=$2, backup_pinned_by_job=$3 WHERE id=$1`, j.id, b.ID, !b.Pinned); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// stageOnDevice uploads the image, checks its hash on the device, notes the boot id
// and runs sysupgrade -T. It returns the boot id, or a reason the job must stop.
func (j *upgradeJob) stageOnDevice(ctx context.Context, image []byte, imageHash string) (string, string) {
	a := j.a
	target, err := a.loadDeviceTarget(ctx, j.deviceID)
	if err != nil {
		_, msg := targetError(err)
		return "", msg
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		return "", "could not open a verified SSH session to the device; nothing was changed"
	}
	defer session.Close()
	// The build took time; free memory is read again right before staging.
	inv := session.run(firmwareInventoryOperation)
	if inv.Status != "ok" {
		return "", "could not re-read the device's free memory"
	}
	m, err := parseFirmwareInventory(inv.Output)
	if err != nil || int64(m.MemAvailableKB)*1024 < int64(len(image))+upgradeRAMMargin {
		return "", "the device no longer has enough free memory to stage this image"
	}
	// Clear anything an earlier attempt left, or a local process squatting on the name;
	// the upload then creates the directory itself or fails.
	_ = session.runWriteRaw(opUpgradeCleanup, nil)
	j.staged = true
	if res := session.runWriteRaw(opUpgradeUpload, image); res.Status != "ok" {
		return "", "uploading the image failed: " + res.Error
	}
	res := session.runWriteRaw(opUpgradeHash, nil)
	if res.Status != "ok" || !strings.HasPrefix(res.Output, imageHash) {
		return "", "the staged image did not match what was sent"
	}
	bootID := ""
	if r := session.run(opBootID); r.Status == "ok" {
		bootID = r.Output
	}
	j.setStep(ctx, "validating")
	if res := session.runWriteRaw(opUpgradeCheck, nil); res.Status != "ok" {
		return "", "the device's own check (sysupgrade -T) rejected this image"
	}
	return bootID, ""
}

type startUpgradeRequest struct {
	Version string `json:"version"`
}

// startFirmwareUpgrade begins preparing an upgrade. It returns at once; the job runs on.
func (a *app) startFirmwareUpgrade(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, upgradeOperations) {
		return
	}
	var in startUpgradeRequest
	if err := decodeJSONBody(w, r, upgradeBodyLimit, &in); err != nil || !releaseVersionPattern.MatchString(in.Version) {
		writeError(w, http.StatusBadRequest, "choose a release to prepare")
		return
	}
	if _, err := encryptionKey(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "server encryption key is not configured")
		return
	}
	if _, err := a.firmwareBuildServer(r.Context()); err != nil {
		buildServerError(w, err)
		return
	}
	user := r.Context().Value(userContextKey).(authUser)

	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the upgrade")
		return
	}
	defer tx.Rollback(r.Context())
	var locked int
	err = tx.QueryRow(r.Context(), lockDeviceSQL, deviceID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the upgrade")
		return
	}
	var busy string
	err = tx.QueryRow(r.Context(), `SELECT CASE
		WHEN EXISTS(SELECT 1 FROM device_restores WHERE device_id=$1 AND status='running') THEN 'a backup restore is running on this device'
		WHEN EXISTS(SELECT 1 FROM config_changes WHERE device_id=$1 AND status='applying') THEN 'a configuration change is being applied to this device'
		ELSE '' END`, deviceID).Scan(&busy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the upgrade")
		return
	}
	if busy != "" {
		writeError(w, http.StatusConflict, busy)
		return
	}
	var up firmwareUpgrade
	err = scanFirmwareUpgrade(tx.QueryRow(r.Context(), `INSERT INTO firmware_upgrades(device_id,version,status,step,created_by)
		VALUES($1,$2,'preparing','starting',$3) RETURNING `+firmwareUpgradeColumns, deviceID, in.Version, user.ID), &up)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, openUpgradeMessage)
		return
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "device.firmware_upgrade_prepare", "device", deviceID,
			map[string]any{"upgrade_id": up.ID, "version": in.Version})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the upgrade")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), upgradeJobTimeout)
	registerUpgradeRun(up.ID, cancel)
	job := &upgradeJob{a: a, id: up.ID, deviceID: deviceID, version: in.Version, userID: user.ID}
	go func() {
		defer cancel()
		defer releaseUpgradeRun(job.id)
		job.run(ctx)
	}()
	writeJSON(w, http.StatusAccepted, up)
}

// listFirmwareUpgrades returns a device's newest upgrade jobs.
func (a *app) listFirmwareUpgrades(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT `+firmwareUpgradeColumns+` FROM firmware_upgrades WHERE device_id=$1
		ORDER BY created_at DESC, id DESC LIMIT $2`, deviceID, upgradeListLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load firmware upgrades")
		return
	}
	defer rows.Close()
	out := []firmwareUpgrade{}
	for rows.Next() {
		var u firmwareUpgrade
		if err := scanFirmwareUpgrade(rows, &u); err != nil {
			writeError(w, http.StatusInternalServerError, "could not load firmware upgrades")
			return
		}
		out = append(out, u)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not load firmware upgrades")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// cancelFirmwareUpgrade stops a job that is preparing or ready. Nothing has been
// flashed at either point.
func (a *app) cancelFirmwareUpgrade(w http.ResponseWriter, r *http.Request) {
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
	var status string
	err := a.db.QueryRow(r.Context(), `SELECT status FROM firmware_upgrades WHERE id=$1 AND device_id=$2`, id, deviceID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "upgrade not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the upgrade")
		return
	}
	switch status {
	case "preparing":
		if !cancelUpgradeRun(id) {
			writeError(w, http.StatusConflict, "this upgrade is not running any more; try again in a moment")
			return
		}
		// The job records the stop itself; this records who asked.
		if tx, err := a.db.Begin(r.Context()); err == nil {
			if insertAuditEvent(r.Context(), tx, user.ID, "device.firmware_upgrade_cancel", "device", deviceID, map[string]any{"upgrade_id": id}) == nil {
				_ = tx.Commit(r.Context())
			}
			_ = tx.Rollback(r.Context())
		}
		// The worker may have committed "ready" just before it saw the cancellation;
		// wait for it to settle and end that job here.
		for range 20 {
			if err := a.db.QueryRow(r.Context(), `SELECT status FROM firmware_upgrades WHERE id=$1`, id).Scan(&status); err != nil || status != "preparing" {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if status == "ready" {
			a.endReadyUpgrade(r.Context(), id, "cancelled", user.ID)
		}
	case "ready":
		if !a.endReadyUpgrade(r.Context(), id, "cancelled", user.ID) {
			writeError(w, http.StatusConflict, "this upgrade changed state; reload it")
			return
		}
	default:
		writeError(w, http.StatusConflict, "this upgrade can no longer be cancelled")
		return
	}
	var up firmwareUpgrade
	if err := scanFirmwareUpgrade(a.db.QueryRow(r.Context(), `SELECT `+firmwareUpgradeColumns+` FROM firmware_upgrades WHERE id=$1`, id), &up); err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the upgrade")
		return
	}
	writeJSON(w, http.StatusOK, up)
}

// recoverFirmwareUpgrades runs once at startup, for jobs created before startedAt (a job
// started through this process is not touched). A job that was preparing or ready when
// the server stopped has no image in memory and no timer, so it is ended and the staged
// file removed. Nothing was flashed by either state.
func (a *app) recoverFirmwareUpgrades(ctx context.Context, startedAt time.Time) error {
	rows, err := a.db.Query(ctx, `SELECT id, status FROM firmware_upgrades WHERE status IN ('preparing','ready') AND created_at < $1`, startedAt)
	if err != nil {
		return err
	}
	type open struct {
		id     int64
		status string
	}
	var jobs []open
	for rows.Next() {
		var o open
		if rows.Scan(&o.id, &o.status) == nil {
			jobs = append(jobs, o)
		}
	}
	rows.Close()
	for _, o := range jobs {
		sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		stopped, deviceID := a.markUpgradeStopped(sctx, o.id, o.status, "the server restarted before this upgrade was flashed; prepare it again", "")
		if stopped {
			a.cleanupStaged(sctx, deviceID)
		}
		cancel()
	}
	return nil
}

// recoverFirmwareUpgradesAtStartup retries while the database is unavailable, since jobs
// left open block the device's other jobs until they are ended.
func (a *app) recoverFirmwareUpgradesAtStartup(ctx context.Context) {
	startedAt := time.Now()
	for attempt := 0; attempt < 20; attempt++ {
		if err := a.recoverFirmwareUpgrades(ctx, startedAt); err == nil {
			return
		}
		log.Printf("firmware upgrades: could not look for interrupted jobs; retrying")
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}
