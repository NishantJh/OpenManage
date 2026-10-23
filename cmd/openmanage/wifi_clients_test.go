package main

import (
	"strings"
	"testing"
	"time"
)

const sampleClients = `@@ hostapd.phy0-ap0
{"freq":2437,"clients":{"AA:BB:CC:11:22:33":{"auth":true,"assoc":true,"signal":-58},"aa:bb:cc:44:55:66":{"auth":true,"assoc":false}}}
@@ hostapd.phy1-ap0
{"freq":5180,"clients":{"de:ad:be:ef:00:01":{"auth":true,"assoc":true,"signal":-61},"not-a-mac":{"assoc":true}}}
@@ hostapd.bad name!
{"clients":{"11:22:33:44:55:66":{"assoc":true}}}
@@ hostapd.phy2-ap0
this is not json
`

func TestParseWifiClients(t *testing.T) {
	got := parseWifiClients(sampleClients)
	byMAC := map[string]polledClient{}
	for _, c := range got {
		byMAC[c.MAC] = c
	}
	if len(got) != 2 {
		t.Fatalf("want 2 associated clients, got %d: %+v", len(got), got)
	}
	a := byMAC["aa:bb:cc:11:22:33"]
	if a.Interface != "phy0-ap0" || a.Signal == nil || *a.Signal != -58 {
		t.Fatalf("unexpected client: %+v", a)
	}
	if b := byMAC["de:ad:be:ef:00:01"]; b.Interface != "phy1-ap0" {
		t.Fatalf("unexpected client: %+v", b)
	}
	if len(parseWifiClients("")) != 0 {
		t.Fatal("empty output should list no clients")
	}
}

func TestParseWifiClientsBounds(t *testing.T) {
	var b strings.Builder
	for i := 0; i < clientMaxInterfaces+4; i++ {
		b.WriteString("@@ hostapd.wlan" + string(rune('a'+i)) + "\n{\"clients\":{\"02:00:00:00:00:" + string("0123456789abcdef"[i%16]) + string("0123456789abcdef"[i/16]) + "\":{\"assoc\":true,\"signal\":40}}}\n")
	}
	got := parseWifiClients(b.String())
	if len(got) != clientMaxInterfaces {
		t.Fatalf("interfaces should be capped at %d, got %d clients", clientMaxInterfaces, len(got))
	}
	if got[0].Signal != nil {
		t.Fatal("a positive signal is not a valid dBm reading and should be dropped")
	}
}

func TestClassifyAssociation(t *testing.T) {
	cases := []struct {
		name string
		prev *clientState
		want string
	}{
		{"new client", nil, "connect"},
		{"came back after leaving", &clientState{DeviceID: "a", Connected: false}, "connect"},
		{"came back on another AP", &clientState{DeviceID: "b", Connected: false}, "connect"},
		{"moved between APs", &clientState{DeviceID: "b", Connected: true}, "roam"},
		{"still on the same AP", &clientState{DeviceID: "a", Connected: true}, ""},
	}
	for _, c := range cases {
		if got := classifyAssociation(c.prev, "a"); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestClientKeyAndMask(t *testing.T) {
	k1, k2 := make([]byte, 32), make([]byte, 32)
	k2[0] = 1
	mac := "aa:bb:cc:11:22:33"
	if string(clientKey(k1, mac)) == string(clientKey(k2, mac)) {
		t.Fatal("client key must depend on the server key")
	}
	if string(clientKey(k1, mac)) != string(clientKey(k1, mac)) || string(clientKey(k1, mac)) == string(clientKey(k1, "aa:bb:cc:11:22:34")) {
		t.Fatal("client key must be stable per MAC and differ between MACs")
	}
	if got := maskMAC(mac); got != "aa:bb:cc:••:••:••" || strings.Contains(got, "11") {
		t.Fatalf("mask leaks the device part: %s", got)
	}
	if _, ok := normalizeMAC("aa-bb-cc-11-22-33"); !ok {
		t.Fatal("dash-separated MAC should parse")
	}
	if _, ok := normalizeMAC("aa:bb:cc:11:22:33:44:55"); ok {
		t.Fatal("8-octet address is not a MAC-48")
	}
}

func TestPollTracker(t *testing.T) {
	var p pollTracker
	now := time.Now()
	if !p.due("a", now, time.Minute) {
		t.Fatal("never-polled AP should be due")
	}
	p.mark("a", now)
	if p.due("a", now.Add(30*time.Second), time.Minute) || !p.due("a", now.Add(time.Minute), time.Minute) {
		t.Fatal("AP should be due exactly one interval after its last poll")
	}
}

func TestParseWifiClientsNamesFromLeases(t *testing.T) {
	out := "##leases\nAA:BB:CC:11:22:33 nishants-iphone\n" +
		"de:ad:be:ef:00:01 *\n" + sampleClients
	byMAC := map[string]string{}
	for _, c := range parseWifiClients(out) {
		byMAC[c.MAC] = c.Name
	}
	if byMAC["aa:bb:cc:11:22:33"] != "nishants-iphone" || byMAC["de:ad:be:ef:00:01"] != "" {
		t.Fatalf("unexpected names: %v", byMAC)
	}
	bad := parseDHCPLeases("##leases\naa:bb:cc:11:22:33 <script>\naa:bb:cc:11:22:34 ok\n")
	if len(bad) != 1 || bad["aa:bb:cc:11:22:34"] != "ok" {
		t.Fatalf("unsafe or malformed lease names must be dropped: %v", bad)
	}
}
