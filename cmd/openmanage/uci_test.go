package main

import (
	"os"
	"testing"
)

func TestParseUCIExportRealOpenWrt(t *testing.T) {
	raw, err := os.ReadFile("testdata/openwrt-24.10-uci-export.txt")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseUCIExport(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"dhcp.@dnsmasq[0]":           "dnsmasq",
		"dhcp.lan.leasetime":         "12h",
		"dhcp.lan.ra_flags":          "managed-config\nother-config",
		"dropbear.main.Port":         "22",
		"firewall.@zone[1].name":     "wan",
		"network.lan.proto":          "static",
		"system.@system[0].hostname": "OpenWrt",
	} {
		if got, ok := cfg[path]; !ok || got != want {
			t.Errorf("%s = %q (present %v), want %q", path, got, ok, want)
		}
	}
}

func TestParseUCIExportQuoting(t *testing.T) {
	cfg, err := parseUCIExport("package system\n\nconfig system\n\toption description 'it'\\''s a \\ test'\n\toption note \"say \\\"hi\\\"\" # comment\n\toption bare plain\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg["system.@system[0].description"]; got != `it's a \ test` {
		t.Errorf("description = %q", got)
	}
	if got := cfg["system.@system[0].note"]; got != `say "hi"` {
		t.Errorf("note = %q", got)
	}
	if got := cfg["system.@system[0].bare"]; got != "plain" {
		t.Errorf("bare = %q", got)
	}
}

func TestParseUCIExportRejectsNonUCI(t *testing.T) {
	for _, text := range []string{"", "ash: uci: not found", "option x 'y'", "package p\nconfig a b c d", "package p\nconfig t\n\toption k 'unterminated"} {
		if _, err := parseUCIExport(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestDiffUCIReportsAndRedacts(t *testing.T) {
	before := uciConfig{
		"wireless.@wifi-iface[0]":      "wifi-iface",
		"wireless.@wifi-iface[0].ssid": "home",
		"wireless.@wifi-iface[0].key":  "old-secret-psk",
		"network.lan.ipaddr":           "192.168.1.1",
		"network.guest.proto":          "static",
	}
	after := uciConfig{
		"wireless.@wifi-iface[0]":      "wifi-iface",
		"wireless.@wifi-iface[0].ssid": "home2",
		"wireless.@wifi-iface[0].key":  "new-secret-psk",
		"network.lan.ipaddr":           "192.168.1.1",
		"system.ntp.server":            "0.pool.ntp.org\n1.pool.ntp.org",
	}
	got := diffUCI(before, after)
	want := []configChange{
		{Path: "network.guest.proto", Kind: "removed", Old: "static"},
		{Path: "system.ntp.server", Kind: "added", New: "0.pool.ntp.org\n1.pool.ntp.org"},
		{Path: "wireless.@wifi-iface[0].key", Kind: "changed", Redacted: true},
		{Path: "wireless.@wifi-iface[0].ssid", Kind: "changed", Old: "home", New: "home2"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d = %#v, want %#v", i, got[i], want[i])
		}
	}
	if len(diffUCI(after, after)) != 0 {
		t.Error("identical configs must have no changes")
	}
}

func TestSecretOptionNamesAreRedacted(t *testing.T) {
	for _, name := range []string{"key", "key1", "password", "passwd", "auth_secret", "acct_secret", "sae_password", "psk", "private_key", "token", "RootPasswordAuth"} {
		if !secretOption.MatchString(name) {
			t.Errorf("%q must be treated as secret", name)
		}
	}
	for _, name := range []string{"ssid", "proto", "ipaddr", "hostname", "channel"} {
		if secretOption.MatchString(name) {
			t.Errorf("%q should not be redacted", name)
		}
	}
}
