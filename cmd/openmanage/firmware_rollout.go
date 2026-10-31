package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	firmwareRolloutBodyLimit    = 16 << 10
	firmwareRolloutListLimit    = 20
	firmwareRolloutMaxDevices   = 20
	firmwareRolloutMaxBatches   = 4
	firmwareRolloutMaxBatchSize = 5
)

type firmwareRolloutRequest struct {
	Version              string     `json:"version"`
	CanaryDeviceID       string     `json:"canary_device_id"`
	Batches              [][]string `json:"batches"`
	MaxConcurrentFlashes int        `json:"max_concurrent_flashes,omitempty"`
}

type firmwareRolloutMember struct {
	DeviceID                  string          `json:"device_id"`
	DeviceName                string          `json:"device_name"`
	Model                     string          `json:"model"`
	CurrentRelease            string          `json:"current_release"`
	BoardName                 string          `json:"board_name"`
	Target                    string          `json:"target"`
	InventoryID               int64           `json:"inventory_id"`
	InventoryDigest           string          `json:"inventory_digest"`
	InventoryVerifiedAt       time.Time       `json:"inventory_verified_at"`
	DesiredPackageSetRevision int             `json:"desired_package_set_revision"`
	BatchNumber               int             `json:"batch_number"`
	Position                  int             `json:"position"`
	Status                    string          `json:"status"`
	Plan                      json.RawMessage `json:"plan,omitempty"`
	PlanDigest                string          `json:"plan_digest,omitempty"`
	InventoryManifest         json.RawMessage `json:"-"`
	UpgradeID                 *int64          `json:"upgrade_id,omitempty"`
}

type firmwareRollout struct {
	ID             int64                   `json:"id"`
	Version        string                  `json:"version"`
	Status         string                  `json:"status"`
	CanaryDeviceID string                  `json:"canary_device_id"`
	CreatedBy      string                  `json:"created_by"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt            time.Time               `json:"updated_at"`
	CurrentBatch         int                     `json:"current_batch"`
	MaxConcurrentFlashes int                     `json:"max_concurrent_flashes"`
	StartedAt            *time.Time              `json:"started_at,omitempty"`
	CompletedAt          *time.Time              `json:"completed_at,omitempty"`
	PauseReason           string                  `json:"pause_reason,omitempty"`
	Devices        []firmwareRolloutMember `json:"devices"`
}

const firmwareRolloutColumns = `id, version, status, canary_device_id::text, COALESCE(created_by::text,''), created_at, updated_at, current_batch, max_concurrent_flashes, started_at, completed_at, pause_reason`

func normalizeFirmwareRolloutRequest(in firmwareRolloutRequest) (firmwareRolloutRequest, error) {
	in.Version = strings.TrimSpace(in.Version)
	rv, ok := parseReleaseVersion(in.Version)
	if !ok || rv.snapshot || rv.branchSn || rv.rc > 0 {
		return in, errors.New("choose a final, stable OpenWrt release")
	}
	in.CanaryDeviceID = strings.ToLower(strings.TrimSpace(in.CanaryDeviceID))
	if !validUUID(in.CanaryDeviceID) {
		return in, errors.New("choose one valid canary device")
	}
	if in.MaxConcurrentFlashes == 0 {
		in.MaxConcurrentFlashes = 2
	}
	if in.MaxConcurrentFlashes < 1 || in.MaxConcurrentFlashes > 2 {
		return in, errors.New("maximum concurrent flashes must be 1 or 2")
	}
	if len(in.Batches) > firmwareRolloutMaxBatches {
		return in, errors.New("too many rollout batches")
	}
	seen := map[string]bool{in.CanaryDeviceID: true}
	total := 1
	for i := range in.Batches {
		if len(in.Batches[i]) == 0 || len(in.Batches[i]) > firmwareRolloutMaxBatchSize {
			return in, errors.New("each rollout batch must contain between 1 and 5 devices")
		}
		for j := range in.Batches[i] {
			id := strings.ToLower(strings.TrimSpace(in.Batches[i][j]))
			if !validUUID(id) || seen[id] {
				return in, errors.New("device IDs must be valid and appear only once, including the canary")
			}
			seen[id] = true
			in.Batches[i][j] = id
			total++
		}
	}
	if total > firmwareRolloutMaxDevices {
		return in, errors.New("a rollout draft can contain at most 20 devices")
	}
	return in, nil
}

// createFirmwareRollout stores a metadata-only draft. It deliberately does not
// contact a device or the build server and cannot prepare or flash an upgrade.
func (a *app) createFirmwareRollout(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in firmwareRolloutRequest
	if err := decodeJSONBody(w, r, firmwareRolloutBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid firmware rollout draft")
		return
	}
	in, err := normalizeFirmwareRolloutRequest(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	ids := []string{in.CanaryDeviceID}
	for _, batch := range in.Batches {
		ids = append(ids, batch...)
	}
	// Lock device rows in a stable order while taking the snapshots. Package-set
	// edits also lock the device row, so the captured desired revision is coherent.
	sort.Strings(ids)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create the rollout draft")
		return
	}
	defer tx.Rollback(r.Context())
	rows, err := tx.Query(r.Context(), `SELECT id::text FROM devices WHERE id::text=ANY($1::text[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not validate rollout devices")
		return
	}
	found := make([]string, 0, len(ids))
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		found = append(found, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not validate rollout devices")
		return
	}
	if len(found) != len(ids) {
		writeError(w, http.StatusNotFound, "one or more selected devices no longer exist")
		return
	}

	var out firmwareRollout
	err = tx.QueryRow(r.Context(), `INSERT INTO firmware_rollouts(version,status,canary_device_id,created_by,max_concurrent_flashes)
		VALUES($1,'draft',$2,$3,$4) RETURNING `+firmwareRolloutColumns,
		in.Version, in.CanaryDeviceID, user.ID, in.MaxConcurrentFlashes).Scan(&out.ID, &out.Version, &out.Status, &out.CanaryDeviceID,
		&out.CreatedBy, &out.CreatedAt, &out.UpdatedAt, &out.CurrentBatch, &out.MaxConcurrentFlashes, &out.StartedAt, &out.CompletedAt, &out.PauseReason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create the rollout draft")
		return
	}

	type selection struct {
		id       string
		batch    int
		position int
	}
	selections := make([]selection, 0, len(ids))
	selections = append(selections, selection{id: in.CanaryDeviceID, batch: 0, position: 0})
	for batchIndex, batch := range in.Batches {
		for position, id := range batch {
			selections = append(selections, selection{id: id, batch: batchIndex + 1, position: position})
		}
	}
	for _, selected := range selections {
		var member firmwareRolloutMember
		var manifestJSON []byte
		err = tx.QueryRow(r.Context(), `SELECT d.id::text,d.name,d.model,
			COALESCE(fi.id,0),COALESCE(fi.digest,''),COALESCE(fi.manifest,'null'::jsonb),
			COALESCE(fi.last_verified_at,'epoch'::timestamptz),
			COALESCE((SELECT r.revision FROM device_package_sets s
				JOIN device_package_set_revisions r ON r.id=s.current_revision_id WHERE s.device_id=d.id),0)
			FROM devices d
			LEFT JOIN LATERAL (SELECT id,digest,manifest,last_verified_at FROM firmware_inventories
				WHERE device_id=d.id ORDER BY captured_at DESC,id DESC LIMIT 1) fi ON TRUE
			WHERE d.id=$1`, selected.id).Scan(&member.DeviceID, &member.DeviceName, &member.Model,
			&member.InventoryID, &member.InventoryDigest, &manifestJSON, &member.InventoryVerifiedAt,
			&member.DesiredPackageSetRevision)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not snapshot a rollout device")
			return
		}
		if member.InventoryID == 0 || len(manifestJSON) == 0 || string(manifestJSON) == "null" {
			writeError(w, http.StatusConflict, "capture a firmware inventory for every selected device before creating a rollout draft")
			return
		}
		var manifest firmwareManifest
		if err = json.Unmarshal(manifestJSON, &manifest); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read a stored firmware inventory")
			return
		}
		member.CurrentRelease, member.BoardName, member.Target = manifest.Release, manifest.BoardName, manifest.Target
		member.BatchNumber, member.Position, member.Status = selected.batch, selected.position, "queued"
		member.InventoryManifest = append(json.RawMessage(nil), manifestJSON...)
		_, err = tx.Exec(r.Context(), `INSERT INTO firmware_rollout_devices
			(rollout_id,device_id,device_id_snapshot,device_name,model,current_release,board_name,target,
			 inventory_id,inventory_digest,inventory_manifest,inventory_verified_at,desired_package_set_revision,
			 batch_number,position,status)
			VALUES($1,$2,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'queued')`,
			out.ID, selected.id, member.DeviceName, member.Model, member.CurrentRelease, member.BoardName, member.Target,
			member.InventoryID, member.InventoryDigest, manifestJSON, member.InventoryVerifiedAt,
			member.DesiredPackageSetRevision, selected.batch, selected.position)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not store the rollout device snapshot")
			return
		}
		out.Devices = append(out.Devices, member)
	}
	if err = insertAuditEvent(r.Context(), tx, user.ID, "firmware.rollout_draft_create", "firmware_rollout",
		strconv.FormatInt(out.ID, 10), map[string]any{
			"version": out.Version, "canary_device_id": out.CanaryDeviceID,
			"device_count": len(out.Devices), "batch_count": len(in.Batches),
		}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create the rollout draft")
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func loadFirmwareRollout(ctx context.Context, db *pgxpool.Pool, id int64) (firmwareRollout, error) {
	var out firmwareRollout
	err := db.QueryRow(ctx, `SELECT `+firmwareRolloutColumns+` FROM firmware_rollouts WHERE id=$1`, id).
		Scan(&out.ID, &out.Version, &out.Status, &out.CanaryDeviceID, &out.CreatedBy, &out.CreatedAt, &out.UpdatedAt, &out.CurrentBatch, &out.MaxConcurrentFlashes, &out.StartedAt, &out.CompletedAt)
	if err != nil {
		return out, err
	}
	rows, err := db.Query(ctx, `SELECT device_id_snapshot::text,device_name,model,current_release,board_name,target,
		inventory_id,inventory_digest,inventory_verified_at,desired_package_set_revision,batch_number,position,status,
		plan,plan_digest,inventory_manifest,upgrade_id
		FROM firmware_rollout_devices WHERE rollout_id=$1 ORDER BY batch_number,position`, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Devices = []firmwareRolloutMember{}
	for rows.Next() {
		var member firmwareRolloutMember
		if err := rows.Scan(&member.DeviceID, &member.DeviceName, &member.Model, &member.CurrentRelease,
			&member.BoardName, &member.Target, &member.InventoryID, &member.InventoryDigest,
			&member.InventoryVerifiedAt, &member.DesiredPackageSetRevision, &member.BatchNumber,
			&member.Position, &member.Status, &member.Plan, &member.PlanDigest, &member.InventoryManifest, &member.UpgradeID); err != nil {
			return out, err
		}
		out.Devices = append(out.Devices, member)
	}
	return out, rows.Err()
}

func (a *app) getFirmwareRollout(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("rollout"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid rollout ID")
		return
	}
	out, err := loadFirmwareRollout(r.Context(), a.db, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "rollout not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the rollout")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) listFirmwareRollouts(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT id FROM firmware_rollouts ORDER BY created_at DESC,id DESC LIMIT $1`, firmwareRolloutListLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list firmware rollouts")
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "could not list firmware rollouts")
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, http.StatusInternalServerError, "could not list firmware rollouts")
		return
	}
	rows.Close()
	out := make([]firmwareRollout, 0, len(ids))
	for _, id := range ids {
		rollout, err := loadFirmwareRollout(r.Context(), a.db, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not load firmware rollouts")
			return
		}
		out = append(out, rollout)
	}
	writeJSON(w, http.StatusOK, out)
}
