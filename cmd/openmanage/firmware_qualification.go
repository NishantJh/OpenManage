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
	"github.com/jackc/pgx/v5/pgxpool"
)

const firmwareHardwareQualificationBodyLimit = 16 << 10
const firmwareHardwareQualificationListLimit = 100
const firmwareHardwareQualificationMaxAgeDays = 365

var requiredFirmwareUpgradeChecks = []string{
	"image_hash_verified",
	"pre_flash_backup_verified",
	"sysupgrade_test_passed",
	"post_reboot_reachable",
	"post_reboot_inventory_verified",
	"management_access_verified",
}

var allowedFirmwareUpgradeChecks = map[string]bool{
	"image_hash_verified":             true,
	"pre_flash_backup_verified":       true,
	"sysupgrade_test_passed":          true,
	"post_reboot_reachable":           true,
	"post_reboot_inventory_verified":  true,
	"management_access_verified":      true,
	"service_health_verified":         true,
	"custom_packages_reported":        true,
}

var allowedFirmwareRecoveryMethods = map[string]bool{
	"failsafe":        true,
	"factory_reset":   true,
	"serial_console":  true,
	"tftp":            true,
	"dual_image":      true,
	"not_exercised":   true,
}

type firmwareHardwareQualificationRequest struct {
	BoardName         string   `json:"board_name"`
	Model             string   `json:"model"`
	SourceRelease     string   `json:"source_release"`
	TargetRelease     string   `json:"target_release"`
	TestedOn          string   `json:"tested_on"`
	PhysicalHardware  bool     `json:"physical_hardware"`
	UpgradeChecks     []string `json:"upgrade_checks"`
	UpgradeOutcome    string   `json:"upgrade_outcome"`
	RecoveryMethod    string   `json:"recovery_method"`
	RecoveryOutcome   string   `json:"recovery_outcome"`
	EvidenceReference string   `json:"evidence_reference"`
	Notes             string   `json:"notes"`
}

type firmwareHardwareQualification struct {
	ID                int64     `json:"id"`
	BoardName         string    `json:"board_name"`
	Model             string    `json:"model"`
	SourceRelease     string    `json:"source_release"`
	TargetRelease     string    `json:"target_release"`
	TestedOn          string    `json:"tested_on"`
	PhysicalHardware  bool      `json:"physical_hardware"`
	UpgradeChecks     []string  `json:"upgrade_checks"`
	UpgradeOutcome    string    `json:"upgrade_outcome"`
	RecoveryMethod    string    `json:"recovery_method"`
	RecoveryOutcome   string    `json:"recovery_outcome"`
	EvidenceReference string    `json:"evidence_reference"`
	Notes             string    `json:"notes,omitempty"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
	Qualifies         bool      `json:"qualifies"`
}

type firmwareRolloutQualificationGap struct {
	DeviceIDs     []string `json:"device_ids"`
	BoardName     string   `json:"board_name"`
	Model         string   `json:"model"`
	SourceRelease string   `json:"source_release"`
	TargetRelease string   `json:"target_release"`
}

type firmwareRolloutQualificationStatus struct {
	RolloutID        int64                              `json:"rollout_id"`
	Qualified        bool                               `json:"qualified"`
	RequiredHardware int                                `json:"required_hardware"`
	Matched          []firmwareHardwareQualification    `json:"matched"`
	Missing          []firmwareRolloutQualificationGap  `json:"missing"`
}

func firmwareQualificationLabel(value string, max int) bool {
	if value == "" || len(value) > max || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func normalizeFirmwareHardwareQualification(in firmwareHardwareQualificationRequest) (firmwareHardwareQualificationRequest, error) {
	in.BoardName = strings.TrimSpace(in.BoardName)
	in.Model = strings.TrimSpace(in.Model)
	in.SourceRelease = strings.TrimSpace(in.SourceRelease)
	in.TargetRelease = strings.TrimSpace(in.TargetRelease)
	in.TestedOn = strings.TrimSpace(in.TestedOn)
	in.UpgradeOutcome = strings.ToLower(strings.TrimSpace(in.UpgradeOutcome))
	in.RecoveryMethod = strings.ToLower(strings.TrimSpace(in.RecoveryMethod))
	in.RecoveryOutcome = strings.ToLower(strings.TrimSpace(in.RecoveryOutcome))
	in.EvidenceReference = strings.TrimSpace(in.EvidenceReference)
	in.Notes = strings.TrimSpace(in.Notes)

	if !firmwareQualificationLabel(in.BoardName, 128) || !firmwareQualificationLabel(in.Model, 128) {
		return in, errors.New("board name and model are required and must be 128 characters or fewer")
	}
	for _, release := range []string{in.SourceRelease, in.TargetRelease} {
		rv, ok := parseReleaseVersion(release)
		if !ok || rv.snapshot || rv.branchSn || rv.rc > 0 {
			return in, errors.New("source and target must be final stable OpenWrt releases")
		}
	}
	if compareReleases(in.TargetRelease, in.SourceRelease) <= 0 {
		return in, errors.New("the qualification target must be newer than the source release")
	}
	testedOn, err := time.Parse("2006-01-02", in.TestedOn)
	if err != nil {
		return in, errors.New("tested_on must use YYYY-MM-DD")
	}
	today := time.Now().UTC()
	if testedOn.After(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)) {
		return in, errors.New("tested_on cannot be in the future")
	}
	if !in.PhysicalHardware && in.UpgradeOutcome == "passed" && in.RecoveryOutcome == "passed" {
		// Virtual test records are allowed for traceability, but never count as qualification.
	}
	if in.UpgradeOutcome != "passed" && in.UpgradeOutcome != "failed" && in.UpgradeOutcome != "not_run" {
		return in, errors.New("upgrade_outcome must be passed, failed, or not_run")
	}
	if in.RecoveryOutcome != "passed" && in.RecoveryOutcome != "failed" && in.RecoveryOutcome != "not_run" {
		return in, errors.New("recovery_outcome must be passed, failed, or not_run")
	}
	if !allowedFirmwareRecoveryMethods[in.RecoveryMethod] {
		return in, errors.New("recovery_method is not supported")
	}
	if len(in.UpgradeChecks) > len(allowedFirmwareUpgradeChecks) {
		return in, errors.New("too many upgrade checks")
	}
	seen := make(map[string]bool, len(in.UpgradeChecks))
	for i, check := range in.UpgradeChecks {
		check = strings.ToLower(strings.TrimSpace(check))
		if !allowedFirmwareUpgradeChecks[check] || seen[check] {
			return in, errors.New("upgrade_checks contains an unsupported or duplicate check")
		}
		seen[check] = true
		in.UpgradeChecks[i] = check
	}
	sort.Strings(in.UpgradeChecks)
	if in.PhysicalHardware && in.UpgradeOutcome == "passed" && in.RecoveryOutcome == "passed" &&
		in.RecoveryMethod != "not_exercised" && len(in.EvidenceReference) == 0 {
		return in, errors.New("a successful physical qualification requires an evidence reference")
	}
	if len(in.EvidenceReference) > 512 || strings.ContainsAny(in.EvidenceReference, "\r\n") {
		return in, errors.New("evidence_reference must be 512 characters or fewer on one line")
	}
	if len(in.Notes) > 4000 {
		return in, errors.New("notes must be 4000 characters or fewer")
	}
	for _, r := range in.Notes {
		if (r < 32 && r != '\n' && r != '\t') || r == 127 {
			return in, errors.New("notes contain unsupported control characters")
		}
	}
	return in, nil
}

func firmwareHardwareQualificationQualifies(q firmwareHardwareQualification) bool {
	if !q.PhysicalHardware || q.UpgradeOutcome != "passed" || q.RecoveryOutcome != "passed" || q.RecoveryMethod == "not_exercised" || q.EvidenceReference == "" {
		return false
	}
	testedOn, err := time.Parse("2006-01-02", q.TestedOn)
	if err != nil {
		return false
	}
	today := time.Now().UTC()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	if testedOn.After(today) || testedOn.Before(today.AddDate(0, 0, -firmwareHardwareQualificationMaxAgeDays)) {
		return false
	}
	checks := make(map[string]bool, len(q.UpgradeChecks))
	for _, check := range q.UpgradeChecks {
		checks[check] = true
	}
	for _, required := range requiredFirmwareUpgradeChecks {
		if !checks[required] {
			return false
		}
	}
	return true
}

const firmwareHardwareQualificationColumns = `id,board_name,model,source_release,target_release,tested_on::text,
	physical_hardware,upgrade_checks,upgrade_outcome,recovery_method,recovery_outcome,evidence_reference,
	notes,COALESCE(created_by::text,''),created_at`

func scanFirmwareHardwareQualification(row pgx.Row, out *firmwareHardwareQualification) error {
	if err := row.Scan(&out.ID, &out.BoardName, &out.Model, &out.SourceRelease, &out.TargetRelease,
		&out.TestedOn, &out.PhysicalHardware, &out.UpgradeChecks, &out.UpgradeOutcome,
		&out.RecoveryMethod, &out.RecoveryOutcome, &out.EvidenceReference, &out.Notes,
		&out.CreatedBy, &out.CreatedAt); err != nil {
		return err
	}
	out.Qualifies = firmwareHardwareQualificationQualifies(*out)
	return nil
}

func (a *app) createFirmwareHardwareQualification(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in firmwareHardwareQualificationRequest
	if err := decodeJSONBody(w, r, firmwareHardwareQualificationBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid hardware qualification record")
		return
	}
	in, err := normalizeFirmwareHardwareQualification(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not record hardware qualification")
		return
	}
	defer tx.Rollback(ctx)

	var out firmwareHardwareQualification
	err = scanFirmwareHardwareQualification(tx.QueryRow(ctx, `INSERT INTO firmware_hardware_qualifications
		(board_name,model,source_release,target_release,tested_on,physical_hardware,upgrade_checks,
		 upgrade_outcome,recovery_method,recovery_outcome,evidence_reference,notes,created_by)
		VALUES($1,$2,$3,$4,$5::date,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+firmwareHardwareQualificationColumns,
		in.BoardName, in.Model, in.SourceRelease, in.TargetRelease, in.TestedOn, in.PhysicalHardware,
		in.UpgradeChecks, in.UpgradeOutcome, in.RecoveryMethod, in.RecoveryOutcome,
		in.EvidenceReference, in.Notes, user.ID), &out)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not record hardware qualification")
		return
	}
	if err = insertAuditEvent(ctx, tx, user.ID, "firmware.hardware_qualification_record", "firmware_hardware_qualification",
		strconv.FormatInt(out.ID, 10), map[string]any{
			"board_name": out.BoardName, "model": out.Model, "source_release": out.SourceRelease,
			"target_release": out.TargetRelease, "tested_on": out.TestedOn,
			"physical_hardware": out.PhysicalHardware, "upgrade_outcome": out.UpgradeOutcome,
			"recovery_method": out.RecoveryMethod, "recovery_outcome": out.RecoveryOutcome,
			"qualifies": out.Qualifies,
		}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not audit hardware qualification")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "could not record hardware qualification")
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *app) listFirmwareHardwareQualifications(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, readOnlyRoles...) {
		return
	}
	board := strings.TrimSpace(r.URL.Query().Get("board_name"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	source := strings.TrimSpace(r.URL.Query().Get("source_release"))
	target := strings.TrimSpace(r.URL.Query().Get("target_release"))
	rows, err := a.db.Query(r.Context(), `SELECT `+firmwareHardwareQualificationColumns+`
		FROM firmware_hardware_qualifications
		WHERE ($1='' OR board_name=$1) AND ($2='' OR model=$2)
			AND ($3='' OR source_release=$3) AND ($4='' OR target_release=$4)
		ORDER BY tested_on DESC,id DESC LIMIT $5`, board, model, source, target, firmwareHardwareQualificationListLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list hardware qualifications")
		return
	}
	defer rows.Close()
	out := make([]firmwareHardwareQualification, 0)
	for rows.Next() {
		var q firmwareHardwareQualification
		if err := scanFirmwareHardwareQualification(rows, &q); err != nil {
			writeError(w, http.StatusInternalServerError, "could not list hardware qualifications")
			return
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not list hardware qualifications")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"qualifications": out})
}

func findMatchingFirmwareHardwareQualification(ctx context.Context, q rolloutQueryRower, member firmwareRolloutMember, targetRelease string) (firmwareHardwareQualification, error) {
	var out firmwareHardwareQualification
	err := scanFirmwareHardwareQualification(q.QueryRow(ctx, `SELECT `+firmwareHardwareQualificationColumns+`
		FROM firmware_hardware_qualifications
		WHERE board_name=$1 AND model=$2 AND source_release=$3 AND target_release=$4
			AND physical_hardware=TRUE AND upgrade_outcome='passed' AND recovery_outcome='passed'
			AND recovery_method<>'not_exercised'
			AND tested_on BETWEEN CURRENT_DATE - $5::int AND CURRENT_DATE
			AND upgrade_checks @> $6::text[]
		ORDER BY tested_on DESC,id DESC LIMIT 1`,
		member.BoardName, member.Model, member.CurrentRelease, targetRelease,
		firmwareHardwareQualificationMaxAgeDays, requiredFirmwareUpgradeChecks), &out)
	return out, err
}

func (a *app) firmwareRolloutQualificationStatus(ctx context.Context, rollout firmwareRollout) (firmwareRolloutQualificationStatus, error) {
	type hardwareKey struct {
		board   string
		model   string
		source  string
	}
	grouped := make(map[hardwareKey][]string)
	for _, member := range rollout.Devices {
		key := hardwareKey{board: member.BoardName, model: member.Model, source: member.CurrentRelease}
		grouped[key] = append(grouped[key], member.DeviceID)
	}
	keys := make([]hardwareKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].board != keys[j].board {
			return keys[i].board < keys[j].board
		}
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		return keys[i].source < keys[j].source
	})

	out := firmwareRolloutQualificationStatus{
		RolloutID: rollout.ID,
		Qualified: true,
		RequiredHardware: len(keys),
		Matched: []firmwareHardwareQualification{},
		Missing: []firmwareRolloutQualificationGap{},
	}
	for _, key := range keys {
		member := firmwareRolloutMember{DeviceID: grouped[key][0], BoardName: key.board, Model: key.model, CurrentRelease: key.source}
		qualification, err := findMatchingFirmwareHardwareQualification(ctx, a.db, member, rollout.Version)
		if errors.Is(err, pgx.ErrNoRows) {
			out.Qualified = false
			out.Missing = append(out.Missing, firmwareRolloutQualificationGap{
				DeviceIDs: grouped[key], BoardName: key.board, Model: key.model,
				SourceRelease: key.source, TargetRelease: rollout.Version,
			})
			continue
		}
		if err != nil {
			return out, err
		}
		out.Matched = append(out.Matched, qualification)
	}
	return out, nil
}

func (a *app) getFirmwareRolloutQualificationStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, readOnlyRoles...) {
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
	status, err := a.firmwareRolloutQualificationStatus(r.Context(), rollout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not check hardware qualifications")
		return
	}
	writeJSON(w, http.StatusOK, status)
}
