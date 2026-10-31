package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	configBatchSetting     = "settings.batch"
	configBatchBodyLimit   = 32 * 1024
	configBatchMaxItems    = 12
	configBatchMaxGroups   = 4
	configBatchMaxPackages = 128
)

type configBatchInput struct {
	Setting        string `json:"setting"`
	Value          string `json:"value"`
	UnlockExisting bool   `json:"unlock_existing"`
}

type configBatchRequest struct {
	BaseVersions          map[string]string  `json:"base_versions"`
	Changes               []configBatchInput `json:"changes"`
	AcknowledgeDisruption bool               `json:"acknowledge_disruption"`
}

type configBatchItem struct {
	Setting        string `json:"setting"`
	Label          string `json:"label"`
	OldValue       string `json:"old_value"`
	NewValue       string `json:"new_value"`
	UnlockExisting bool   `json:"unlock_existing,omitempty"`
}

type configChangeBatch struct {
	configChangeRecord
	Changes                []configBatchItem `json:"changes"`
	DisruptionAcknowledged bool              `json:"disruption_acknowledged"`
}

func validateConfigBatch(in configBatchRequest) (bool, string) {
	if len(in.Changes) == 0 || len(in.Changes) > configBatchMaxItems {
		return false, "a batch must contain between 1 and 12 settings"
	}
	if len(in.BaseVersions) == 0 || len(in.BaseVersions) > configBatchMaxPackages {
		return false, "refresh the settings page before previewing changes"
	}
	seen := make(map[string]bool, len(in.Changes))
	groups := make(map[string]bool, len(in.Changes))
	needsDisruptionAck := false
	for _, change := range in.Changes {
		if seen[change.Setting] {
			return false, "a setting may appear only once in a batch"
		}
		seen[change.Setting] = true
		f, ok := settingsField(change.Setting)
		if !ok || !batchEditableField(f) {
			return false, "one or more selected settings cannot be changed through this workflow"
		}
		if change.UnlockExisting && !f.UnlockRequired {
			return false, "unlock_existing is only accepted for fields that require an explicit unlock"
		}
		groups[f.Config+"\x00"+f.Section] = true
		if f.Risk == riskDisruptive {
			needsDisruptionAck = true
		}
		if msg := f.validate(change.Value); msg != "" {
			return false, msg
		}
	}
	if len(groups) > configBatchMaxGroups {
		return false, "a batch may update at most four UCI sections"
	}
	if needsDisruptionAck && !in.AcknowledgeDisruption {
		return false, "acknowledge that these changes may disrupt client or management connectivity"
	}
	for pkg, version := range in.BaseVersions {
		if pkg == "" || len(pkg) > 64 || version == "" || len(version) > 128 {
			return false, "refresh the settings page before previewing changes"
		}
	}
	return needsDisruptionAck, ""
}

func settingsVersionsMatch(cfg uciConfig, key []byte, expected map[string]string) bool {
	current := groupSettings(cfg, key)
	if len(current) != len(expected) || len(current) > configBatchMaxPackages {
		return false
	}
	for pkg, settings := range current {
		if expected[pkg] != settings.Version {
			return false
		}
	}
	return true
}

func readSettingFromConfig(cfg uciConfig, f settingField) (string, bool) {
	sectionPath := f.Config + "." + f.Section
	if _, ok := cfg[sectionPath]; !ok {
		return "", false
	}
	value, ok := cfg[sectionPath+"."+f.Option]
	if !ok {
		return "", true
	}
	return value, true
}

func requiresExistingValueUnlock(f settingField, current string) bool {
	return f.UnlockRequired && current != ""
}

func settingUnlockMatches(f settingField, current string, unlocked bool) bool {
	return requiresExistingValueUnlock(f, current) == unlocked
}

func configBatchOldValuesMatch(cfg uciConfig, fields []settingField, changes []configBatchItem) bool {
	if len(fields) != len(changes) {
		return false
	}
	for i, f := range fields {
		value, exists := readSettingFromConfig(cfg, f)
		if !exists || value != changes[i].OldValue {
			return false
		}
	}
	return true
}

func readDeviceUCIExport(ctx context.Context, a *app, deviceID string) (deviceTarget, uciConfig, error) {
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		return target, nil, err
	}
	session, err := openDeviceSession(ctx, target, changeStepTimeout)
	if err != nil {
		return target, nil, errors.New("could not open a verified SSH session to the device")
	}
	defer session.Close()
	cfg, err := exportDeviceConfig(session)
	return target, cfg, err
}

func exportDeviceConfig(session *deviceSession) (uciConfig, error) {
	result := session.run(settingsExportOperation)
	if result.Status != "ok" {
		return nil, errors.New("configuration export failed")
	}
	cfg, err := parseUCIExport(result.Output)
	if err != nil {
		return nil, errors.New("the device did not return a UCI configuration export")
	}
	return cfg, nil
}

func (a *app) previewConfigChangeBatch(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in configBatchRequest
	if err := decodeJSONBody(w, r, configBatchBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings batch")
		return
	}
	needsDisruptionAck, msg := validateConfigBatch(in)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	key, err := encryptionKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "settings changes are disabled: server encryption key is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()
	_, cfg, err := readDeviceUCIExport(ctx, a, deviceID)
	if err != nil {
		status, msg := settingsReadError(err)
		writeError(w, status, msg)
		return
	}
	if !settingsVersionsMatch(cfg, key, in.BaseVersions) {
		writeError(w, http.StatusConflict, "device settings changed since this page was loaded; refresh and review the current values")
		return
	}
	items := make([]configBatchItem, 0, len(in.Changes))
	for _, change := range in.Changes {
		f, _ := settingsField(change.Setting)
		oldValue, sectionExists := readSettingFromConfig(cfg, f)
		if !sectionExists {
			writeError(w, http.StatusConflict, "a selected setting is not available on this device")
			return
		}
		if oldValue == change.Value {
			writeError(w, http.StatusConflict, "one or more selected settings already have the requested value")
			return
		}
		if !settingUnlockMatches(f, oldValue, change.UnlockExisting) {
			writeError(w, http.StatusBadRequest, "unlock status does not match this setting's current value")
			return
		}
		items = append(items, configBatchItem{Setting: f.Key, Label: f.Label, OldValue: oldValue, NewValue: change.Value, UnlockExisting: change.UnlockExisting})
	}
	changesJSON, err := json.Marshal(items)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not record the settings preview")
		return
	}
	versionsJSON, err := json.Marshal(in.BaseVersions)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings version tokens")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not record the settings preview")
		return
	}
	defer tx.Rollback(ctx)
	var record configChangeRecord
	err = scanConfigChange(tx.QueryRow(ctx, `INSERT INTO config_changes
		(device_id,setting,old_value,new_value,status,created_by,batch_changes,base_versions,disruption_acknowledged)
		VALUES($1,$2,'','', 'previewed',$3,$4,$5,$6) RETURNING `+configChangeColumns,
		deviceID, configBatchSetting, user.ID, changesJSON, versionsJSON, needsDisruptionAck), &record)
	if err == nil {
		keys := make([]string, 0, len(items))
		unlocked := make([]string, 0)
		for _, item := range items {
			keys = append(keys, item.Setting)
			if item.UnlockExisting {
				unlocked = append(unlocked, item.Setting)
			}
		}
		sort.Strings(keys)
		sort.Strings(unlocked)
		err = insertAuditEvent(ctx, tx, user.ID, "device.config_batch.preview", "device", deviceID,
			map[string]any{"change_id": record.ID, "settings": keys, "unlocked_settings": unlocked,
				"count": len(keys), "disruption_acknowledged": needsDisruptionAck})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not record the settings preview")
		return
	}
	writeJSON(w, http.StatusCreated, configChangeBatch{configChangeRecord: record, Changes: items, DisruptionAcknowledged: needsDisruptionAck})
}

func scanConfigChangeBatch(row pgx.Row, out *configChangeBatch, versions *map[string]string) error {
	var rawChanges, rawVersions []byte
	c := &out.configChangeRecord
	err := row.Scan(&c.ID, &c.DeviceID, &c.Setting, &c.OldValue, &c.NewValue, &c.Status, &c.Detail, &c.CreatedAt, &c.FinishedAt,
		&rawChanges, &rawVersions, &out.DisruptionAcknowledged)
	if err != nil {
		return err
	}
	c.Label = "Settings batch"
	c.ExpiresAt = c.CreatedAt.Add(changePreviewTTL)
	if err := json.Unmarshal(rawChanges, &out.Changes); err != nil {
		return err
	}
	if versions != nil {
		if err := json.Unmarshal(rawVersions, versions); err != nil {
			return err
		}
	}
	return nil
}

func settingsReadError(err error) (int, string) {
	switch {
	case errors.Is(err, errTargetNotFound), errors.Is(err, errTargetNotPrivate), errors.Is(err, errTargetNotEnrolled),
		errors.Is(err, errTargetKeyUnavailable), errors.Is(err, errTargetDecrypt), errors.Is(err, errTargetBadCredential):
		return targetError(err)
	default:
		return http.StatusBadGateway, "could not read current settings from the device over verified SSH"
	}
}

func (a *app) listConfigChangeBatches(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin") {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT `+configChangeColumns+`, batch_changes, base_versions, disruption_acknowledged
		FROM config_changes WHERE device_id=$1 AND batch_changes IS NOT NULL
		ORDER BY created_at DESC,id DESC LIMIT 50`, deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load settings history")
		return
	}
	defer rows.Close()
	out := []configChangeBatch{}
	for rows.Next() {
		var batch configChangeBatch
		if err := scanConfigChangeBatch(rows, &batch, nil); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read settings history")
			return
		}
		out = append(out, batch)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read settings history")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) applyConfigChangeBatch(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	changeID, err := strconv.ParseInt(r.PathValue("change"), 10, 64)
	if !validUUID(deviceID) || err != nil || changeID < 1 {
		writeError(w, http.StatusBadRequest, "invalid device or change ID")
		return
	}
	if !requireAnyRole(w, r, "admin") {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	ctx := r.Context()
	batch, versions, status, msg := a.claimConfigChangeBatch(ctx, deviceID, changeID, user.ID)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	outcome, detail := a.runConfigChangeBatch(ctx, batch, versions, user.ID)
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := a.db.Begin(saveCtx)
	if err == nil {
		defer tx.Rollback(saveCtx)
		updated := batch.configChangeRecord
		err = scanConfigChange(tx.QueryRow(saveCtx, `UPDATE config_changes SET status=$2,detail=$3,finished_at=NOW()
			WHERE id=$1 RETURNING `+configChangeColumns, batch.ID, outcome, detail), &updated)
		if err == nil {
			keys := make([]string, 0, len(batch.Changes))
			unlocked := make([]string, 0)
			for _, item := range batch.Changes {
				keys = append(keys, item.Setting)
				if item.UnlockExisting {
					unlocked = append(unlocked, item.Setting)
				}
			}
			sort.Strings(keys)
			sort.Strings(unlocked)
			err = insertAuditEvent(saveCtx, tx, user.ID, "device.config_batch."+outcome, "device", deviceID,
				map[string]any{"change_id": batch.ID, "settings": keys, "unlocked_settings": unlocked, "detail": detail,
					"disruption_acknowledged": batch.DisruptionAcknowledged})
		}
		if err == nil {
			err = tx.Commit(saveCtx)
		}
		if err == nil {
			batch.configChangeRecord = updated
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the settings batch finished as "+outcome+" but could not be recorded")
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (a *app) claimConfigChangeBatch(ctx context.Context, deviceID string, changeID int64, userID string) (configChangeBatch, map[string]string, int, string) {
	var batch configChangeBatch
	var versions map[string]string
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return batch, nil, http.StatusInternalServerError, "could not start the settings batch"
	}
	defer tx.Rollback(ctx)
	var locked int
	err = tx.QueryRow(ctx, lockDeviceSQL, deviceID).Scan(&locked)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return batch, nil, http.StatusNotFound, "device not found"
		}
		return batch, nil, http.StatusInternalServerError, "could not start the settings batch"
	}
	if _, err = tx.Exec(ctx, `UPDATE config_changes SET status='failed',finished_at=NOW(),
		detail='interrupted while applying; the device rolled back on its own timer'
		WHERE device_id=$1 AND status='applying' AND created_at < NOW() - ($2 * INTERVAL '1 second')`, deviceID, int(staleApplyAfter.Seconds())); err != nil {
		return batch, nil, http.StatusInternalServerError, "could not start the settings batch"
	}
	if _, err = tx.Exec(ctx, `UPDATE config_changes SET status='expired',finished_at=NOW()
		WHERE device_id=$1 AND status='previewed' AND created_at < NOW() - ($2 * INTERVAL '1 second')`, deviceID, int(changePreviewTTL.Seconds())); err != nil {
		return batch, nil, http.StatusInternalServerError, "could not start the settings batch"
	}
	var restoring, upgrading, operating bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_restores WHERE device_id=$1 AND status='running')`, deviceID).Scan(&restoring); err == nil && restoring {
		return batch, nil, http.StatusConflict, "a backup restore is running on this device"
	}
	if err == nil {
		err = tx.QueryRow(ctx, openUpgradeSQL, deviceID).Scan(&upgrading)
	}
	if err == nil && upgrading {
		return batch, nil, http.StatusConflict, openUpgradeMessage
	}
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT `+packageInstallRunningSQL, deviceID).Scan(&operating)
	}
	if err == nil && operating {
		return batch, nil, http.StatusConflict, packageInstallBusyMessage
	}
	if err != nil {
		return batch, nil, http.StatusInternalServerError, "could not start the settings batch"
	}
	var rawChanges, rawVersions []byte
	var pgErr *pgconn.PgError
	err = tx.QueryRow(ctx, `UPDATE config_changes SET status='applying',applied_by=$3
		WHERE id=$1 AND device_id=$2 AND status='previewed' AND batch_changes IS NOT NULL
		RETURNING `+configChangeColumns+`,batch_changes,base_versions,disruption_acknowledged`, changeID, deviceID, userID).
		Scan(&batch.ID, &batch.DeviceID, &batch.Setting, &batch.OldValue, &batch.NewValue, &batch.Status, &batch.Detail,
			&batch.CreatedAt, &batch.FinishedAt, &rawChanges, &rawVersions, &batch.DisruptionAcknowledged)
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return batch, nil, http.StatusConflict, "another configuration change is being applied to this device"
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return batch, nil, http.StatusConflict, "this settings preview was already used or has expired; preview the changes again"
	}
	if err != nil {
		return batch, nil, http.StatusInternalServerError, "could not start the settings batch"
	}
	if err = json.Unmarshal(rawChanges, &batch.Changes); err == nil {
		err = json.Unmarshal(rawVersions, &versions)
	}
	if err != nil || len(batch.Changes) == 0 || len(batch.Changes) > configBatchMaxItems {
		return batch, nil, http.StatusInternalServerError, "could not read the settings batch"
	}
	batch.Label = "Settings batch"
	batch.ExpiresAt = batch.CreatedAt.Add(changePreviewTTL)
	if err := tx.Commit(ctx); err != nil {
		return batch, nil, http.StatusInternalServerError, "could not start the settings batch"
	}
	return batch, versions, 0, ""
}

func (a *app) runConfigChangeBatch(ctx context.Context, batch configChangeBatch, versions map[string]string, userID string) (string, string) {
	key, err := encryptionKey()
	if err != nil {
		return "failed", "server encryption key is not configured"
	}
	target, cfg, err := readDeviceUCIExport(ctx, a, batch.DeviceID)
	if err != nil {
		return "failed", "could not read current settings over a verified SSH connection"
	}
	if !settingsVersionsMatch(cfg, key, versions) {
		return "failed", "device settings changed after preview; nothing was applied"
	}
	fields := make([]settingField, len(batch.Changes))
	needsDisruptionAck := false
	for i, item := range batch.Changes {
		f, ok := settingsField(item.Setting)
		if !ok || !batchEditableField(f) {
			return "failed", "a setting is no longer allowed; nothing was applied"
		}
		needsDisruptionAck = needsDisruptionAck || f.Risk == riskDisruptive
		current, sectionExists := readSettingFromConfig(cfg, f)
		if !sectionExists || current != item.OldValue {
			return "failed", "a setting changed after preview; nothing was applied"
		}
		if !settingUnlockMatches(f, item.OldValue, item.UnlockExisting) {
			return "failed", "setting unlock state did not match its preview; nothing was applied"
		}
		if msg := f.validate(item.NewValue); msg != "" {
			return "failed", "a setting no longer passes validation; nothing was applied"
		}
		fields[i] = f
	}
	if needsDisruptionAck && !batch.DisruptionAcknowledged {
		return "failed", "disruptive changes were not acknowledged; nothing was applied"
	}
	if _, fail := a.captureSnapshot(ctx, batch.DeviceID, userID, false); fail != nil {
		return "failed", "could not take a baseline configuration snapshot"
	}
	session, err := openDeviceSession(ctx, target, changeStepTimeout)
	if err != nil {
		return "failed", "could not open a verified SSH session to the device"
	}
	defer session.Close()
	res := session.runWrite(opSessionCreate, map[string]int{"timeout": changeRollbackSeconds * 2})
	var rpcSession struct {
		ID string `json:"ubus_rpc_session"`
	}
	if res.Status != "ok" || json.Unmarshal([]byte(res.Output), &rpcSession) != nil || rpcSession.ID == "" {
		return "failed", "could not open an rpcd session on the device"
	}
	defer session.runWrite(opSessionDestroy, map[string]string{"ubus_rpc_session": rpcSession.ID})
	if res = session.runWrite(opSessionGrant, map[string]any{"ubus_rpc_session": rpcSession.ID, "scope": "uci", "objects": batchGrantObjects(fields)}); res.Status != "ok" {
		return "failed", "could not grant access to the selected settings"
	}
	for _, change := range batchSetGroups(fields, batch.Changes) {
		if res = session.runWrite(opUCISet, map[string]any{"ubus_rpc_session": rpcSession.ID, "config": change.Config,
			"section": change.Section, "values": change.Values}); res.Status != "ok" {
			return "failed", "could not stage the selected settings"
		}
	}
	// Recheck the exact page snapshot after staging but before committing UCI. This
	// narrows the race with LuCI or another configuration writer during preflight.
	preapply, err := openDeviceSession(ctx, target, changeStepTimeout)
	if err != nil {
		return "failed", "could not recheck settings before applying; nothing was applied"
	}
	preapplyConfig, err := exportDeviceConfig(preapply)
	preapply.Close()
	if err != nil || !settingsVersionsMatch(preapplyConfig, key, versions) || !configBatchOldValuesMatch(preapplyConfig, fields, batch.Changes) {
		return "failed", "device settings changed while preparing; nothing was applied"
	}
	if res = session.runWrite(opUCIApply, map[string]any{"ubus_rpc_session": rpcSession.ID, "rollback": true, "timeout": changeRollbackSeconds}); res.Status != "ok" {
		return "failed", "the device refused to apply (another apply may be pending); nothing was applied"
	}
	rollback := func(reason string) (string, string) {
		if res := session.runWrite(opUCIRollback, map[string]string{"ubus_rpc_session": rpcSession.ID}); res.Status != "ok" {
			return "rolled_back", reason + "; the device rollback timer will restore the previous configuration within " + strconv.Itoa(changeRollbackSeconds) + " seconds"
		}
		return "rolled_back", reason + "; the previous configuration was restored"
	}
	verify, err := openDeviceSession(ctx, target, changeStepTimeout)
	if err != nil {
		return rollback("OpenManage could not reconnect after applying")
	}
	verifiedConfig, err := exportDeviceConfig(verify)
	if err != nil {
		verify.Close()
		return rollback("OpenManage could not read settings after applying")
	}
	for i, item := range batch.Changes {
		got, sectionExists := readSettingFromConfig(verifiedConfig, fields[i])
		if !sectionExists || got != item.NewValue {
			verify.Close()
			return rollback("the new settings did not read back")
		}
	}
	res = verify.runWrite(opUCIConfirm, map[string]string{"ubus_rpc_session": rpcSession.ID})
	verify.Close()
	if res.Status != "ok" {
		return rollback("the device did not accept the confirmation")
	}
	detail := "settings applied, read back over a fresh connection, and confirmed"
	if _, fail := a.captureSnapshot(ctx, batch.DeviceID, userID, true); fail != nil {
		detail += "; the follow-up snapshot failed, so the next snapshot may show drift"
	}
	return "applied", detail
}

type batchUCISet struct {
	Config  string
	Section string
	Values  map[string]string
}

func batchGrantObjects(fields []settingField) [][]string {
	configs := map[string]bool{}
	for _, f := range fields {
		configs[f.Config] = true
	}
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	objects := make([][]string, 0, len(names)*2)
	for _, name := range names {
		objects = append(objects, []string{name, "read"}, []string{name, "write"})
	}
	return objects
}

func batchSetGroups(fields []settingField, changes []configBatchItem) []batchUCISet {
	groups := map[string]*batchUCISet{}
	for i, f := range fields {
		groupKey := f.Config + "\x00" + f.Section
		group := groups[groupKey]
		if group == nil {
			group = &batchUCISet{Config: f.Config, Section: f.Section, Values: map[string]string{}}
			groups[groupKey] = group
		}
		group.Values[f.Option] = changes[i].NewValue
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]batchUCISet, 0, len(keys))
	for _, key := range keys {
		out = append(out, *groups[key])
	}
	return out
}
