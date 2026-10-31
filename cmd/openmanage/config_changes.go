package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A controlled change sets one server-defined UCI option on one device:
//
//  1. Preview reads the current value and stores old and new values.
//  2. Apply, by an administrator, re-reads the value (refusing if it moved), takes a
//     baseline snapshot, then stages the change in an rpcd session and runs
//     `uci apply` with rollback armed.
//  3. A fresh SSH connection proves the management path survived and reads the new
//     value back. Only then is the change confirmed, from the same rpcd session.
//     Anything else rolls back, or leaves rpcd's timer to do it.
//
// Values travel to the device as JSON on stdin, never inside command text.

const (
	changePreviewTTL = 10 * time.Minute
	changeBodyLimit  = 4 * 1024
	// changeRollbackSeconds is how long rpcd waits for confirm before reverting. It
	// is longer than an API request, so a request that dies mid-apply still reverts.
	changeRollbackSeconds = 60
	// staleApplyAfter marks an "applying" row as interrupted (the server restarted mid
	// apply); by then rpcd has rolled the device back on its own.
	staleApplyAfter   = 5 * time.Minute
	changeStepTimeout = 10 * time.Second
)

// writableSetting is one UCI option OpenManage may change. Old and new values are
// stored in config_changes and the audit log in plain text, so a secret option (a
// Wi-Fi key, a RADIUS secret) must not be added here until those are encrypted and
// redacted the way snapshots are.
type writableSetting struct {
	Key      string
	Label    string
	Config   string
	Section  string
	Option   string
	Validate func(string) string // returns a problem, or "" when the value is acceptable
	Help     string
}

var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// writableSettings is the complete list of what OpenManage may change.
var writableSettings = map[string]writableSetting{
	"system.hostname": {
		Key: "system.hostname", Label: "Hostname", Config: "system", Section: "@system[0]", Option: "hostname",
		Help: "1-63 letters, digits and hyphens; no leading or trailing hyphen.",
		Validate: func(v string) string {
			if !hostnamePattern.MatchString(v) {
				return "hostname must be 1-63 letters, digits and hyphens, and cannot start or end with a hyphen"
			}
			return ""
		},
	},
}

func ubusWriteOperation(name, label, command string) operation {
	return operation{
		Name: name, Label: label, Class: operationWrite, Roles: []string{"admin"},
		Timeout: changeStepTimeout, OutputLimit: healthOutputLimit, Audit: auditTransactional,
		command: command,
	}
}

// The change steps. Each reads its parameters as one JSON line from stdin.
var (
	opSessionCreate = ubusWriteOperation("rpcd_session_create", "Open rpcd session",
		`read -r p && ubus call session create "$p"`)
	opSessionGrant = ubusWriteOperation("rpcd_session_grant", "Grant UCI access",
		`read -r p && ubus call session grant "$p"`)
	opUCISet = ubusWriteOperation("uci_set", "Stage change",
		`read -r p && ubus call uci set "$p"`)
	opUCIApply = ubusWriteOperation("uci_apply", "Apply with rollback",
		`read -r p && ubus call uci apply "$p"`)
	opUCIConfirm = ubusWriteOperation("uci_confirm", "Confirm",
		`read -r p && ubus call uci confirm "$p"`)
	opUCIRollback = ubusWriteOperation("uci_rollback", "Roll back",
		`read -r p && ubus call uci rollback "$p"`)
	opSessionDestroy = ubusWriteOperation("rpcd_session_destroy", "Close rpcd session",
		`read -r p && ubus call session destroy "$p"`)
	// opUCIGet is a read, but takes its option name on stdin like the writes do.
	opUCIGet = ubusWriteOperation("uci_get", "Read value", `read -r p && ubus call uci get "$p"`)
)

// changeOperations lists every operation the workflow can run, for role checks.
var changeOperations = []operation{opSessionCreate, opSessionGrant, opUCISet, opUCIApply, opUCIConfirm, opUCIRollback, opSessionDestroy, opUCIGet}

type configChangeRecord struct {
	ID         int64      `json:"id"`
	DeviceID   string     `json:"device_id"`
	Setting    string     `json:"setting"`
	Label      string     `json:"label"`
	OldValue   string     `json:"old_value"`
	NewValue   string     `json:"new_value"`
	Status     string     `json:"status"`
	Detail     string     `json:"detail"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

const configChangeColumns = `id, device_id::text, setting, old_value, new_value, status, detail, created_at, finished_at`

func scanConfigChange(row pgx.Row, c *configChangeRecord) error {
	err := row.Scan(&c.ID, &c.DeviceID, &c.Setting, &c.OldValue, &c.NewValue, &c.Status, &c.Detail, &c.CreatedAt, &c.FinishedAt)
	if err == nil {
		if c.Setting == configBatchSetting {
			c.Label = "Settings batch"
		} else {
			c.Label = writableSettings[c.Setting].Label
		}
		c.ExpiresAt = c.CreatedAt.Add(changePreviewTTL)
	}
	return err
}

// readSetting returns the device's current value; an unset option reads as "".
func readSetting(s *deviceSession, setting writableSetting) (string, error) {
	res := s.runWrite(opUCIGet, map[string]string{"config": setting.Config, "section": setting.Section, "option": setting.Option})
	if res.Status != "ok" {
		return "", errors.New(res.Error)
	}
	if res.Output == "" {
		return "", nil
	}
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		return "", errors.New("device returned an unreadable value")
	}
	var value string
	if err := json.Unmarshal(out.Value, &value); err != nil {
		// A list option comes back as an array; none of the writable settings are lists.
		return "", errors.New("device returned an unexpected value type")
	}
	return value, nil
}

func (a *app) listWritableSettings(w http.ResponseWriter, r *http.Request) {
	if !requireOperationRoles(w, r, changeOperations) {
		return
	}
	out := []map[string]string{}
	for _, s := range writableSettings {
		out = append(out, map[string]string{"key": s.Key, "label": s.Label, "help": s.Help})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["label"] < out[j]["label"] })
	writeJSON(w, http.StatusOK, out)
}

func (a *app) listConfigChanges(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, changeOperations) {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT `+configChangeColumns+` FROM config_changes
		WHERE device_id=$1 ORDER BY created_at DESC, id DESC LIMIT 50`, deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load configuration changes")
		return
	}
	defer rows.Close()
	out := []configChangeRecord{}
	for rows.Next() {
		var c configChangeRecord
		if err := scanConfigChange(rows, &c); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read configuration changes")
			return
		}
		out = append(out, c)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read configuration changes")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// previewConfigChange reads the current value from the device and records what the
// change would do. Nothing on the device changes.
func (a *app) previewConfigChange(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, changeOperations) {
		return
	}
	var in struct {
		Setting string `json:"setting"`
		Value   string `json:"value"`
	}
	if err := decodeJSONBody(w, r, changeBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid change request")
		return
	}
	setting, ok := writableSettings[in.Setting]
	if !ok {
		writeError(w, http.StatusBadRequest, "this setting cannot be changed by OpenManage")
		return
	}
	if msg := setting.Validate(in.Value); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		status, msg := targetError(err)
		writeError(w, status, msg)
		return
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not open a verified SSH session to the device")
		return
	}
	current, err := readSetting(session, setting)
	session.Close()
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the current value: "+err.Error())
		return
	}
	if current == in.Value {
		writeError(w, http.StatusConflict, "the device already has this value")
		return
	}
	var c configChangeRecord
	err = scanConfigChange(a.db.QueryRow(ctx, `INSERT INTO config_changes(device_id,setting,old_value,new_value,status,created_by)
		VALUES($1,$2,$3,$4,'previewed',$5) RETURNING `+configChangeColumns,
		deviceID, setting.Key, current, in.Value, user.ID), &c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not record the change preview")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// applyConfigChange runs a previewed change. It answers 200 with the final record
// whether the change was applied or rolled back; the status says which.
func (a *app) applyConfigChange(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	changeID, err := strconv.ParseInt(r.PathValue("change"), 10, 64)
	if !validUUID(deviceID) || err != nil || changeID < 1 {
		writeError(w, http.StatusBadRequest, "invalid device or change ID")
		return
	}
	if !requireOperationRoles(w, r, changeOperations) {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	ctx := r.Context()

	change, status, msg := a.claimConfigChange(ctx, deviceID, changeID, user.ID)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	setting := writableSettings[change.Setting]
	outcome, detail := a.runConfigChange(ctx, change, setting, user.ID)

	// The outcome is recorded with a fresh context: the request may be close to its
	// deadline, and the record must not be lost.
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := a.db.Begin(saveCtx)
	if err == nil {
		defer tx.Rollback(saveCtx)
		err = scanConfigChange(tx.QueryRow(saveCtx, `UPDATE config_changes SET status=$2, detail=$3, finished_at=NOW()
			WHERE id=$1 RETURNING `+configChangeColumns, change.ID, outcome, detail), &change)
	}
	if err == nil {
		err = insertAuditEvent(saveCtx, tx, user.ID, "device.config_change."+outcome, "device", deviceID, map[string]any{
			"change_id": change.ID, "setting": change.Setting, "old": change.OldValue, "new": change.NewValue, "detail": detail,
		})
	}
	if err == nil {
		err = tx.Commit(saveCtx)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the change finished as "+outcome+" but could not be recorded")
		return
	}
	writeJSON(w, http.StatusOK, change)
}

// claimConfigChange moves a fresh preview to "applying". The partial unique index
// keeps a second change on the same device from starting meanwhile.
func (a *app) claimConfigChange(ctx context.Context, deviceID string, changeID int64, userID string) (configChangeRecord, int, string) {
	var c configChangeRecord
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return c, http.StatusInternalServerError, "could not start the change"
	}
	defer tx.Rollback(ctx)
	// Serialised with restores and firmware upgrades on this device.
	_, err = tx.Exec(ctx, lockDeviceSQL, deviceID)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE config_changes SET status='failed', finished_at=NOW(),
		detail='interrupted while applying; the device rolled back on its own timer'
		WHERE device_id=$1 AND status='applying' AND created_at < NOW() - ($2 * INTERVAL '1 second')`,
			deviceID, int(staleApplyAfter.Seconds()))
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE config_changes SET status='expired', finished_at=NOW()
			WHERE device_id=$1 AND status='previewed' AND created_at < NOW() - ($2 * INTERVAL '1 second')`,
			deviceID, int(changePreviewTTL.Seconds()))
	}
	var restoring bool
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_restores WHERE device_id=$1 AND status='running')`, deviceID).Scan(&restoring)
	}
	if err == nil && restoring {
		return c, http.StatusConflict, "a backup restore is running on this device"
	}
	var upgrading bool
	if err == nil {
		err = tx.QueryRow(ctx, openUpgradeSQL, deviceID).Scan(&upgrading)
	}
	if err == nil && upgrading {
		return c, http.StatusConflict, openUpgradeMessage
	}
	var installing bool
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT `+packageInstallRunningSQL, deviceID).Scan(&installing)
	}
	if err == nil && installing {
		return c, http.StatusConflict, packageInstallBusyMessage
	}
	if err == nil {
		err = scanConfigChange(tx.QueryRow(ctx, `UPDATE config_changes SET status='applying', applied_by=$3
			WHERE id=$1 AND device_id=$2 AND status='previewed' RETURNING `+configChangeColumns, changeID, deviceID, userID), &c)
	}
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return c, http.StatusConflict, "another change is being applied to this device"
	case errors.Is(err, pgx.ErrNoRows):
		return c, http.StatusConflict, "this preview was already used or has expired; preview the change again"
	case err != nil:
		return c, http.StatusInternalServerError, "could not start the change"
	}
	if _, ok := writableSettings[c.Setting]; !ok {
		return c, http.StatusConflict, "this setting can no longer be changed"
	}
	if err := tx.Commit(ctx); err != nil {
		return c, http.StatusInternalServerError, "could not start the change"
	}
	return c, 0, ""
}

// runConfigChange performs the device side and returns the final status and a short,
// secret-free detail.
func (a *app) runConfigChange(ctx context.Context, change configChangeRecord, setting writableSetting, userID string) (string, string) {
	target, err := a.loadDeviceTarget(ctx, change.DeviceID)
	if err != nil {
		_, msg := targetError(err)
		return "failed", msg
	}
	session, err := openDeviceSession(ctx, target, changeStepTimeout)
	if err != nil {
		return "failed", "could not open a verified SSH session to the device"
	}
	defer session.Close()
	current, err := readSetting(session, setting)
	if err != nil {
		return "failed", "could not read the current value"
	}
	if current != change.OldValue {
		return "failed", "the value on the device changed since the preview; nothing was applied"
	}
	if _, fail := a.captureSnapshot(ctx, change.DeviceID, userID, false); fail != nil {
		return "failed", "could not take a baseline snapshot: " + fail.msg
	}

	res := session.runWrite(opSessionCreate, map[string]int{"timeout": changeRollbackSeconds * 2})
	var sess struct {
		ID string `json:"ubus_rpc_session"`
	}
	if res.Status != "ok" || json.Unmarshal([]byte(res.Output), &sess) != nil || sess.ID == "" {
		return "failed", "could not open an rpcd session on the device"
	}
	defer session.runWrite(opSessionDestroy, map[string]string{"ubus_rpc_session": sess.ID})
	steps := []struct {
		op    operation
		input any
	}{
		{opSessionGrant, map[string]any{"ubus_rpc_session": sess.ID, "scope": "uci",
			"objects": [][]string{{setting.Config, "read"}, {setting.Config, "write"}}}},
		{opUCISet, map[string]any{"ubus_rpc_session": sess.ID, "config": setting.Config, "section": setting.Section,
			"values": map[string]string{setting.Option: change.NewValue}}},
	}
	for _, step := range steps {
		if res := session.runWrite(step.op, step.input); res.Status != "ok" {
			return "failed", step.op.Label + " failed; nothing was applied"
		}
	}
	if res := session.runWrite(opUCIApply, map[string]any{"ubus_rpc_session": sess.ID, "rollback": true, "timeout": changeRollbackSeconds}); res.Status != "ok" {
		return "failed", "the device refused to apply (another apply may be pending); nothing was applied"
	}

	// From here the change is live with rollback armed. Verify over a new connection.
	rollback := func(reason string) (string, string) {
		if res := session.runWrite(opUCIRollback, map[string]string{"ubus_rpc_session": sess.ID}); res.Status != "ok" {
			return "rolled_back", reason + "; the device's rollback timer will restore the previous configuration within " + strconv.Itoa(changeRollbackSeconds) + " seconds"
		}
		return "rolled_back", reason + "; the previous configuration was restored"
	}
	verify, err := openDeviceSession(ctx, target, changeStepTimeout)
	if err != nil {
		return rollback("OpenManage could not reconnect after applying")
	}
	got, err := readSetting(verify, setting)
	if err != nil || got != change.NewValue {
		verify.Close()
		return rollback("the new value did not read back")
	}
	res = verify.runWrite(opUCIConfirm, map[string]string{"ubus_rpc_session": sess.ID})
	verify.Close()
	if res.Status != "ok" {
		return rollback("the device did not accept the confirmation")
	}
	detail := "applied, verified over a new connection, and confirmed"
	if _, fail := a.captureSnapshot(ctx, change.DeviceID, userID, true); fail != nil {
		detail += "; the follow-up snapshot failed, so the next snapshot will show this change as drift"
	}
	return "applied", detail
}
