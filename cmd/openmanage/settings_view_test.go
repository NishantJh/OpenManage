package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGroupSettingsRedactsSecretsAndVersionsPackage(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	first := uciConfig{
		"wireless.radio0":         "wifi-device",
		"wireless.radio0.channel": "auto",
		"wireless.ap":             "wifi-iface",
		"wireless.ap.ssid":        "test-network",
		"wireless.ap.key":         "fixture-only-value",
	}
	got := groupSettings(first, key)
	if got["wireless"].Version == "" {
		t.Fatal("wireless package has no version token")
	}
	var foundSecret bool
	for _, section := range got["wireless"].Sections {
		for _, option := range section.Options {
			if option.Name == "key" {
				foundSecret = true
				if !option.Secret || option.Value != "" {
					t.Fatalf("secret option was not redacted: %+v", option)
				}
			}
		}
	}
	if !foundSecret {
		t.Fatal("wireless key option was not returned as redacted metadata")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "fixture-only-value") {
		t.Fatal("serialized settings response contains a secret value")
	}

	changed := uciConfig{}
	for path, value := range first {
		changed[path] = value
	}
	changed["wireless.ap.key"] = "different-fixture-value"
	if groupSettings(changed, key)["wireless"].Version == got["wireless"].Version {
		t.Fatal("package version did not change after a secret option changed")
	}
}
