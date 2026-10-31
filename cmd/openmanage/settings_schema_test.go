package main

import (
	"strings"
	"testing"
)

func TestSettingsSchemaWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, page := range settingsPages {
		if page.Key == "" || page.Label == "" || page.Group == "" {
			t.Fatalf("page %q has an empty key, label or group", page.Key)
		}
		if seen[page.Key] {
			t.Fatalf("duplicate page key %q", page.Key)
		}
		seen[page.Key] = true
		for _, f := range page.Fields {
			if f.Key == "" || f.Label == "" || f.Config == "" || f.Section == "" || f.Option == "" {
				t.Fatalf("page %q field %q is missing key/label/config/section/option", page.Key, f.Key)
			}
			if f.Kind == kindEnum && len(f.Options) == 0 {
				t.Fatalf("page %q field %q is an enum without options", page.Key, f.Key)
			}
			if f.Secret && f.Kind != kindSecret {
				t.Fatalf("page %q field %q is marked secret but is not a secret kind", page.Key, f.Key)
			}
			if f.UnlockRequired && f.Key != "wireless.@wifi-iface[0].ssid" {
				t.Fatalf("page %q field %q unexpectedly requires unlock", page.Key, f.Key)
			}
		}
	}
}

func TestSettingsFieldValidators(t *testing.T) {
	byKey := map[string]settingField{}
	for _, page := range settingsPages {
		for _, f := range page.Fields {
			byKey[f.Key] = f
		}
	}
	cases := []struct {
		key, value string
		wantOK     bool
	}{
		{"system.hostname", "hall-ap", true},
		{"system.hostname", "-bad", false},
		{"system.hostname", "has space", false},
		{"network.lan.ipaddr", "203.0.113.1", true},
		{"network.lan.ipaddr", "2001:db8::1", false},
		{"network.lan.ipaddr", "not-an-ip", false},
		{"network.lan.proto", "static", true},
		{"network.lan.proto", "bogus", false},
		{"dropbear.port", "22", true},
		{"dropbear.port", "70000", false},
		{"dropbear.port", "ssh", false},
		{"wireless.@wifi-iface[0].key", "correct-horse-battery", true},
		{"wireless.@wifi-iface[0].key", "short", false},
		{"wireless.radio0.channel", "auto", true},
		{"wireless.radio0.channel", "233", true},
		{"wireless.radio0.channel", "+2", false},
		{"wireless.radio0.channel", "234", false},
		{"wireless.@wifi-iface[0].ssid", "OpenManage guest", true},
		{"wireless.@wifi-iface[0].ssid", strings.Repeat("x", 33), false},
		{"wireless.@wifi-iface[0].ssid", "guest\nnetwork", false},
		{"wireless.@wifi-iface[0].disabled", "0", true},
		{"wireless.@wifi-iface[0].disabled", "yes", false},
		{"dhcp.lan.start", "100", true},
		{"dhcp.lan.start", "300", false},
	}
	for _, c := range cases {
		f, ok := byKey[c.key]
		if !ok {
			t.Fatalf("no field with key %q", c.key)
		}
		if f.validate == nil {
			t.Fatalf("field %q has no validator", c.key)
		}
		got := f.validate(c.value) == ""
		if got != c.wantOK {
			t.Errorf("validate(%q, %q) ok=%v, want %v", c.key, c.value, got, c.wantOK)
		}
	}
}
