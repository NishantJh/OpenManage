package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type firmwareRolloutUnsupportedPathError struct{ reason string }

func (e firmwareRolloutUnsupportedPathError) Error() string { return e.reason }

type rolloutQueryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadDevicePackageSetRevision(ctx context.Context, db *pgxpool.Pool, deviceID string, revision int) (packageSetRevision, error) {
	out := packageSetRevision{
		Revision:         revision,
		OfficialPackages: []string{},
		RemovedDefaults:  []string{},
		Sources:          []packageSetSource{},
	}
	if revision == 0 {
		return out, nil
	}

	var revisionID int64
	err := db.QueryRow(ctx, `SELECT r.id,r.revision,r.official_packages,r.removed_defaults,
		COALESCE(r.created_by::text,''),r.created_at,r.based_on_revision
		FROM device_package_sets s
		JOIN device_package_set_revisions r ON r.package_set_id=s.id
		WHERE s.device_id=$1 AND r.revision=$2`, deviceID, revision).Scan(
		&revisionID, &out.Revision, &out.OfficialPackages, &out.RemovedDefaults,
		&out.CreatedBy, &out.CreatedAt, &out.BasedOnRevision)
	if err != nil {
		return out, err
	}

	rows, err := db.Query(ctx, `SELECT package_name,tracking_mode,source_id,COALESCE(source_version,'')
		FROM device_package_set_revision_sources WHERE revision_id=$1 ORDER BY package_name`, revisionID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var source packageSetSource
		if err := rows.Scan(&source.Name, &source.Mode, &source.SourceID, &source.Version); err != nil {
			return out, err
		}
		out.Sources = append(out.Sources, source)
	}
	return out, rows.Err()
}

func firmwareRolloutMemberCurrent(ctx context.Context, q rolloutQueryRower, member firmwareRolloutMember) (bool, error) {
	var name, model, digest string
	var inventoryID int64
	var revision int
	err := q.QueryRow(ctx, `SELECT d.name,d.model,COALESCE(fi.id,0),COALESCE(fi.digest,''),
		COALESCE((SELECT r.revision FROM device_package_sets s
			JOIN device_package_set_revisions r ON r.id=s.current_revision_id
			WHERE s.device_id=d.id),0)
		FROM devices d
		LEFT JOIN LATERAL (SELECT id,digest FROM firmware_inventories
			WHERE device_id=d.id ORDER BY captured_at DESC,id DESC LIMIT 1) fi ON TRUE
		WHERE d.id=$1`, member.DeviceID).Scan(&name, &model, &inventoryID, &digest, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return name == member.DeviceName &&
		model == member.Model &&
		inventoryID == member.InventoryID &&
		digest == member.InventoryDigest &&
		revision == member.DesiredPackageSetRevision, nil
}

type firmwareRolloutPlanDigestInput struct {
	DeviceID                  string       `json:"device_id"`
	DeviceName                string       `json:"device_name"`
	Model                     string       `json:"model"`
	CurrentRelease            string       `json:"current_release"`
	BoardName                 string       `json:"board_name"`
	Target                    string       `json:"target"`
	InventoryID               int64        `json:"inventory_id"`
	InventoryDigest           string       `json:"inventory_digest"`
	DesiredPackageSetRevision int          `json:"desired_package_set_revision"`
	Version                   string       `json:"version"`
	Plan                      firmwarePlan `json:"plan"`
}

func firmwareRolloutPlanDigest(member firmwareRolloutMember, version string, plan firmwarePlan) (string, error) {
	input := firmwareRolloutPlanDigestInput{
		DeviceID:                  member.DeviceID,
		DeviceName:                member.DeviceName,
		Model:                     member.Model,
		CurrentRelease:            member.CurrentRelease,
		BoardName:                 member.BoardName,
		Target:                    member.Target,
		InventoryID:               member.InventoryID,
		InventoryDigest:           member.InventoryDigest,
		DesiredPackageSetRevision: member.DesiredPackageSetRevision,
		Version:                   version,
		Plan:                      plan,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func (a *app) firmwarePlanForRolloutMember(ctx context.Context, serverURL string, rollout firmwareRollout, member firmwareRolloutMember) (firmwarePlan, error) {
	var manifest firmwareManifest
	if len(member.InventoryManifest) == 0 || string(member.InventoryManifest) == "null" {
		return firmwarePlan{}, errors.New("the rollout inventory snapshot is missing")
	}
	if err := json.Unmarshal(member.InventoryManifest, &manifest); err != nil {
		return firmwarePlan{}, errors.New("the rollout inventory snapshot is invalid")
	}
	if manifest.Release != member.CurrentRelease || manifest.BoardName != member.BoardName || manifest.Target != member.Target {
		return firmwarePlan{}, errors.New("the rollout inventory snapshot does not match its recorded identity")
	}

	overview, err := fetchOverview(ctx, serverURL)
	if err != nil {
		return firmwarePlan{}, err
	}
	branches := make([]string, 0, len(overview.Branches))
	for name := range overview.Branches {
		if !strings.Contains(name, "SNAPSHOT") {
			branches = append(branches, name)
		}
	}
	sort.Strings(branches)
	if problem := upgradePathProblem(manifest.Release, rollout.Version, branches); problem != "" {
		return firmwarePlan{}, firmwareRolloutUnsupportedPathError{reason: problem}
	}
	plan, err := firmwarePlanFor(ctx, serverURL, manifest, rollout.Version)
	if err != nil {
		return plan, err
	}
	revision, err := loadDevicePackageSetRevision(ctx, a.db, member.DeviceID, member.DesiredPackageSetRevision)
	if err != nil {
		return plan, err
	}
	if revision.Revision > 0 {
		plan.DesiredSources, err = a.resolveDesiredPackageSources(ctx, revision.Sources, &plan)
		if err != nil {
			return plan, err
		}
		applyDesiredPackageSet(&plan, revision)
	}
	return plan, nil
}

func (a *app) previewFirmwareRollout(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("rollout"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid rollout ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*buildServerTimeout)
	defer cancel()

	rollout, err := loadFirmwareRollout(ctx, a.db, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "rollout not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the rollout")
		return
	}
	if rollout.Status != "draft" {
		writeError(w, http.StatusConflict, "only an unpreviewed draft can be previewed")
		return
	}

	for _, member := range rollout.Devices {
		current, err := firmwareRolloutMemberCurrent(ctx, a.db, member)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not validate rollout snapshots")
			return
		}
		if !current {
			writeError(w, http.StatusConflict, "a selected device, firmware inventory, or desired package-set revision has changed; create a new rollout draft")
			return
		}
	}

	serverURL, err := a.firmwareBuildServer(ctx)
	if err != nil {
		buildServerError(w, err)
		return
	}
	type plannedMember struct {
		member firmwareRolloutMember
		plan   json.RawMessage
		digest string
	}
	planned := make([]plannedMember, len(rollout.Devices))
	planErrors := make([]error, len(rollout.Devices))
	// Bound build-server fan-out while avoiding serial per-device network latency.
	// Results are written by index so response/error selection remains deterministic.
	const rolloutPlanConcurrency = 4
	semaphore := make(chan struct{}, rolloutPlanConcurrency)
	var planWG sync.WaitGroup
	for i, member := range rollout.Devices {
		planWG.Add(1)
		go func(i int, member firmwareRolloutMember) {
			defer planWG.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				planErrors[i] = ctx.Err()
				return
			}
			defer func() { <-semaphore }()
			plan, err := a.firmwarePlanForRolloutMember(ctx, serverURL, rollout, member)
			if err != nil {
				planErrors[i] = err
				return
			}
			encoded, err := json.Marshal(plan)
			if err != nil {
				planErrors[i] = err
				return
			}
			digest, err := firmwareRolloutPlanDigest(member, rollout.Version, plan)
			if err != nil {
				planErrors[i] = err
				return
			}
			planned[i] = plannedMember{member: member, plan: encoded, digest: digest}
		}(i, member)
	}
	planWG.Wait()
	for i, item := range planned {
		if err := planErrors[i]; err != nil {
			var pathErr firmwareRolloutUnsupportedPathError
			if errors.As(err, &pathErr) {
				writeError(w, http.StatusConflict, pathErr.Error())
			} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				writeError(w, http.StatusGatewayTimeout, "rollout preview exceeded the request time limit; no plans were saved")
			} else {
				buildServerError(w, err)
			}
			return
		}
		var plan firmwarePlan
		if err := json.Unmarshal(item.plan, &plan); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read a computed rollout plan")
			return
		}
		if len(plan.Blockers) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":    "rollout preview is blocked by one or more device plans",
				"device":   item.member.DeviceName,
				"blockers": plan.Blockers,
			})
			return
		}
	}

	ids := make([]string, 0, len(planned))
	for _, item := range planned {
		ids = append(ids, item.member.DeviceID)
	}
	sort.Strings(ids)
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the rollout preview")
		return
	}
	defer tx.Rollback(ctx)

	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM firmware_rollouts WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "rollout not found")
		} else {
			writeError(w, http.StatusInternalServerError, "could not lock the rollout")
		}
		return
	}
	if status != "draft" {
		writeError(w, http.StatusConflict, "only an unpreviewed draft can be previewed")
		return
	}

	rows, err := tx.Query(ctx, `SELECT id::text FROM devices WHERE id::text=ANY($1::text[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not lock rollout devices")
		return
	}
	locked := make([]string, 0, len(ids))
	for rows.Next() {
		var deviceID string
		if err = rows.Scan(&deviceID); err != nil {
			break
		}
		locked = append(locked, deviceID)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not lock rollout devices")
		return
	}
	if len(locked) != len(ids) {
		writeError(w, http.StatusConflict, "a selected device was removed; create a new rollout draft")
		return
	}
	for _, item := range planned {
		current, err := firmwareRolloutMemberCurrent(ctx, tx, item.member)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not revalidate rollout snapshots")
			return
		}
		if !current {
			writeError(w, http.StatusConflict, "a selected device, firmware inventory, or desired package-set revision changed during preview; create a new rollout draft")
			return
		}
	}

	user := r.Context().Value(userContextKey).(authUser)
	digests := make([]string, 0, len(planned))
	for _, item := range planned {
		tag, err := tx.Exec(ctx, `UPDATE firmware_rollout_devices SET plan=$3::jsonb,plan_digest=$4,updated_at=NOW()
			WHERE rollout_id=$1 AND device_id_snapshot=$2 AND plan IS NULL AND plan_digest=''`,
			id, item.member.DeviceID, string(item.plan), item.digest)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not save a rollout device plan")
			return
		}
		if tag.RowsAffected() != 1 {
			writeError(w, http.StatusConflict, "a rollout plan was already saved; reload the campaign")
			return
		}
		digests = append(digests, item.digest)
	}
	if _, err = tx.Exec(ctx, `UPDATE firmware_rollouts SET status='previewed',updated_at=NOW() WHERE id=$1 AND status='draft'`, id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the rollout preview")
		return
	}
	if err = insertAuditEvent(ctx, tx, user.ID, "firmware.rollout_preview", "firmware_rollout", strconv.FormatInt(id, 10), map[string]any{
		"version": rollout.Version, "device_count": len(planned), "plan_digests": digests,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not audit the rollout preview")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the rollout preview")
		return
	}
	out, err := loadFirmwareRollout(ctx, a.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rollout preview was saved but could not be reloaded")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
