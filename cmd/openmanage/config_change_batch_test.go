package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBatchEditableFieldSafetyPolicy(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"system.hostname", true},
		{"wireless.@wifi-iface[0].ssid", true},
		{"wireless.radio0.channel", true},
		{"wireless.@wifi-iface[0].key", false},
		{"dropbear.port", false},
		{"network.lan.ipaddr", false},
		{"firewall.@zone[0].input", false},
		{"system.ntp.server", false},
	}
	for _, tc := range cases {
		f, ok := settingsField(tc.key)
		if !ok {
			t.Fatalf("field %q missing from schema", tc.key)
		}
		if got := batchEditableField(f); got != tc.want {
			t.Errorf("batchEditableField(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestValidateConfigBatchRequiresDisruptionAckAndRejectsUnsafeInputs(t *testing.T) {
	versions := map[string]string{"wireless": "opaque-version"}
	cases := []struct {
		name string
		in   configBatchRequest
		want string
	}{
		{"safe hostname", configBatchRequest{BaseVersions: versions, Changes: []configBatchInput{{Setting: "system.hostname", Value: "hall-ap"}}}, ""},
		{"wireless without acknowledgment", configBatchRequest{BaseVersions: versions, Changes: []configBatchInput{{Setting: "wireless.@wifi-iface[0].ssid", Value: "guest"}}}, "acknowledge"},
		{"wireless with acknowledgment", configBatchRequest{BaseVersions: versions, Changes: []configBatchInput{{Setting: "wireless.@wifi-iface[0].ssid", Value: "guest"}}, AcknowledgeDisruption: true}, ""},
		{"unlock flag on unsupported field", configBatchRequest{BaseVersions: versions, Changes: []configBatchInput{{Setting: "system.hostname", Value: "router-2", UnlockExisting: true}}}, "only accepted for fields"},
		{"secret rejected", configBatchRequest{BaseVersions: versions, Changes: []configBatchInput{{Setting: "wireless.@wifi-iface[0].key", Value: "fixture-only-secret"}}, AcknowledgeDisruption: true}, "cannot be changed"},
		{"management field rejected", configBatchRequest{BaseVersions: versions, Changes: []configBatchInput{{Setting: "dropbear.port", Value: "22"}}}, "cannot be changed"},
		{"list field rejected", configBatchRequest{BaseVersions: versions, Changes: []configBatchInput{{Setting: "system.ntp.server", Value: "pool.example"}}}, "cannot be changed"},
		{"invalid hostname", configBatchRequest{BaseVersions: versions, Changes: []configBatchInput{{Setting: "system.hostname", Value: "not a hostname"}}}, "hostname"},
		{"duplicate settings", configBatchRequest{BaseVersions: versions, Changes: []configBatchInput{{Setting: "system.hostname", Value: "one"}, {Setting: "system.hostname", Value: "two"}}}, "only once"},
		{"no changes", configBatchRequest{BaseVersions: versions}, "between 1 and 12"},
		{"missing versions", configBatchRequest{Changes: []configBatchInput{{Setting: "system.hostname", Value: "router"}}}, "refresh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := validateConfigBatch(tc.in)
			if tc.want == "" && got != "" {
				t.Fatalf("unexpected validation error: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("error %q does not contain %q", got, tc.want)
			}
		})
	}
}

func TestValidateConfigBatchBoundsItemCountAndUCISections(t *testing.T) {
	versions := map[string]string{"system": "opaque-version"}
	tooMany := make([]configBatchInput, configBatchMaxItems+1)
	if _, msg := validateConfigBatch(configBatchRequest{BaseVersions: versions, Changes: tooMany}); !strings.Contains(msg, "between 1 and 12") {
		t.Fatalf("too many items error = %q", msg)
	}
	changes := []configBatchInput{
		{Setting: "system.hostname", Value: "router-2"},
		{Setting: "system.ntp.enabled", Value: "1"},
		{Setting: "dhcp.lan.interface", Value: "lan"},
		{Setting: "firewall.@zone[0].forward", Value: "ACCEPT"},
		{Setting: "wireless.radio0.channel", Value: "auto"},
	}
	if _, msg := validateConfigBatch(configBatchRequest{BaseVersions: versions, Changes: changes, AcknowledgeDisruption: true}); !strings.Contains(msg, "at most four UCI sections") {
		t.Fatalf("too many sections error = %q", msg)
	}
}

func TestSettingsVersionsCoverSectionsAndValues(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	base := uciConfig{
		"system.@system[0]":          "system",
		"system.@system[0].hostname": "router",
		"system.ntp":                 "timeserver",
	}
	versions := map[string]string{}
	for pkg, settings := range groupSettings(base, key) {
		versions[pkg] = settings.Version
	}
	if !settingsVersionsMatch(base, key, versions) {
		t.Fatal("fresh page version did not match the device config")
	}
	changedValue := uciConfig{}
	for path, value := range base {
		changedValue[path] = value
	}
	changedValue["system.@system[0].hostname"] = "changed-router"
	if settingsVersionsMatch(changedValue, key, versions) {
		t.Fatal("settings version matched after an option changed")
	}
	emptySection := uciConfig{}
	for path, value := range base {
		emptySection[path] = value
	}
	emptySection["system.ntp"] = "timeserver-changed"
	if settingsVersionsMatch(emptySection, key, versions) {
		t.Fatal("settings version matched after an empty section type changed")
	}
	if settingsVersionsMatch(base, key, map[string]string{"system": versions["system"], "wireless": "extra"}) {
		t.Fatal("settings version accepted an extra package")
	}
}

func TestReadSettingFromConfigRequiresExistingSection(t *testing.T) {
	f, _ := settingsField("system.hostname")
	if value, ok := readSettingFromConfig(uciConfig{"system.@system[0]": "system"}, f); !ok || value != "" {
		t.Fatalf("unset option = %q, %v; want empty, true", value, ok)
	}
	if _, ok := readSettingFromConfig(uciConfig{}, f); ok {
		t.Fatal("missing UCI section was treated as editable")
	}
}

func TestExistingSSIDsRequireAnExplicitUnlock(t *testing.T) {
	ssid, ok := settingsField("wireless.@wifi-iface[0].ssid")
	if !ok || !ssid.UnlockRequired {
		t.Fatal("existing SSID field is not marked as requiring unlock")
	}
	if !requiresExistingValueUnlock(ssid, "Home") {
		t.Fatal("existing SSID did not require unlock")
	}
	if requiresExistingValueUnlock(ssid, "") {
		t.Fatal("unset SSID unexpectedly required unlock")
	}
	if settingUnlockMatches(ssid, "Home", false) || !settingUnlockMatches(ssid, "Home", true) {
		t.Fatal("existing SSID unlock flag did not match the current value")
	}
	if !settingUnlockMatches(ssid, "", false) || settingUnlockMatches(ssid, "", true) {
		t.Fatal("unset SSID unlock flag did not match the current value")
	}
	hostname, _ := settingsField("system.hostname")
	if requiresExistingValueUnlock(hostname, "router") || !settingUnlockMatches(hostname, "router", false) || settingUnlockMatches(hostname, "router", true) {
		t.Fatal("hostname unexpectedly required unlock")
	}
}

func TestConfigBatchOldValuesMustAllMatch(t *testing.T) {
	hostname, _ := settingsField("system.hostname")
	ssid, _ := settingsField("wireless.@wifi-iface[0].ssid")
	fields := []settingField{hostname, ssid}
	changes := []configBatchItem{{Setting: hostname.Key, OldValue: "router"}, {Setting: ssid.Key, OldValue: "guest"}}
	cfg := uciConfig{
		"system.@system[0]":            "system",
		"system.@system[0].hostname":   "router",
		"wireless.@wifi-iface[0]":      "wifi-iface",
		"wireless.@wifi-iface[0].ssid": "guest",
	}
	if !configBatchOldValuesMatch(cfg, fields, changes) {
		t.Fatal("matching values were rejected")
	}
	cfg["wireless.@wifi-iface[0].ssid"] = "changed"
	if configBatchOldValuesMatch(cfg, fields, changes) {
		t.Fatal("changed value was accepted")
	}
}

func TestBatchSetGroupsByConfigAndSection(t *testing.T) {
	fields := []settingField{}
	changes := []configBatchItem{}
	for _, item := range []struct{ key, value string }{
		{"system.hostname", "router-2"},
		{"system.timezone", "UTC"},
		{"wireless.@wifi-iface[0].ssid", "guest"},
	} {
		f, ok := settingsField(item.key)
		if !ok {
			t.Fatalf("field %q missing", item.key)
		}
		fields = append(fields, f)
		changes = append(changes, configBatchItem{Setting: f.Key, NewValue: item.value})
	}
	groups := batchSetGroups(fields, changes)
	if len(groups) != 2 {
		t.Fatalf("got %d UCI set groups, want 2", len(groups))
	}
	if groups[0].Config != "system" || groups[0].Values["hostname"] != "router-2" || groups[0].Values["timezone"] != "UTC" {
		t.Fatalf("system group = %+v", groups[0])
	}
	if groups[1].Config != "wireless" || groups[1].Section != "@wifi-iface[0]" || groups[1].Values["ssid"] != "guest" {
		t.Fatalf("wireless group = %+v", groups[1])
	}
}

func TestConfigBatchEndpointsRequireAdmin(t *testing.T) {
	id := "550e8400-e29b-41d4-a716-446655440000"
	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		change  string
	}{
		{"list", (&app{}).listConfigChangeBatches, http.MethodGet, ""},
		{"preview", (&app{}).previewConfigChangeBatch, http.MethodPost, "{}"},
		{"apply", (&app{}).applyConfigChangeBatch, http.MethodPost, ""},
	}
	for _, tc := range cases {
		for _, role := range []string{"operator", "readonly"} {
			t.Run(tc.name+"/"+role, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, "/api/devices/"+id+"/config-change-batches", strings.NewReader(tc.change))
				req.SetPathValue("id", id)
				req.SetPathValue("change", "1")
				req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: role}))
				rec := httptest.NewRecorder()
				tc.handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status %d, want 403", rec.Code)
				}
			})
		}
	}
}

func TestConfigSchemaEditableMetadataAdminOnly(t *testing.T) {
	for _, role := range []string{"admin", "operator"} {
		req := httptest.NewRequest(http.MethodGet, "/api/config-schema", nil)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: role}))
		rec := httptest.NewRecorder()
		(&app{}).getConfigSchema(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", role, rec.Code)
		}
		var response struct {
			Pages []settingsPage `json:"pages"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		var hostnameEditable bool
		var ssidEditable, ssidUnlockRequired bool
		for _, page := range response.Pages {
			for _, f := range page.Fields {
				if f.Key == "system.hostname" {
					hostnameEditable = f.Editable
				}
				if f.Key == "wireless.@wifi-iface[0].ssid" {
					ssidEditable, ssidUnlockRequired = f.Editable, f.UnlockRequired
				}
				if f.Secret && f.Editable {
					t.Fatalf("%s received editable secret field %q", role, f.Key)
				}
			}
		}
		if hostnameEditable != (role == "admin") {
			t.Fatalf("%s editable metadata = %v", role, hostnameEditable)
		}
		if ssidEditable != (role == "admin") || !ssidUnlockRequired {
			t.Fatalf("%s SSID metadata editable=%v unlock_required=%v", role, ssidEditable, ssidUnlockRequired)
		}
	}
}

func TestConfigChangeBatchMigrationKeepsLegacyRowsCompatible(t *testing.T) {
	db := newEnrollmentTestDB(t)
	deviceID, userID := seedDeviceAndUser(t, db)
	var legacyNullBatch, legacyNullVersions, legacyAck bool
	err := db.QueryRow(context.Background(), `INSERT INTO config_changes(device_id,setting,old_value,new_value,status,created_by)
		VALUES($1,'system.hostname','old','new','previewed',$2)
		RETURNING batch_changes IS NULL,base_versions IS NULL,disruption_acknowledged`, deviceID, userID).
		Scan(&legacyNullBatch, &legacyNullVersions, &legacyAck)
	if err != nil {
		t.Fatal(err)
	}
	if !legacyNullBatch || !legacyNullVersions || legacyAck {
		t.Fatalf("legacy defaults: batch null=%v versions null=%v ack=%v", legacyNullBatch, legacyNullVersions, legacyAck)
	}
	var stored string
	err = db.QueryRow(context.Background(), `INSERT INTO config_changes
		(device_id,setting,old_value,new_value,status,created_by,batch_changes,base_versions,disruption_acknowledged)
		VALUES($1,$2,'','', 'previewed',$3,'[{"setting":"system.hostname","old_value":"router","new_value":"router-2"}]'::jsonb,
		'{"system":"opaque-version"}'::jsonb,false) RETURNING batch_changes::text`, deviceID, configBatchSetting, userID).Scan(&stored)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, "system.hostname") {
		t.Fatalf("stored batch JSON = %s", stored)
	}
}
