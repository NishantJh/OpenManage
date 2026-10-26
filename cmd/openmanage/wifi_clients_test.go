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
	c1, c2 := clientKey(k1, mac), clientKey(k1, mac)
	if string(c1) != string(c2) || string(c1) == string(clientKey(k1, "aa:bb:cc:11:22:34")) {
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

func TestParsePreamble(t *testing.T) {
	out := "##leases\nAA:BB:CC:11:22:33 nishants-iphone\nde:ad:be:ef:00:01 <script>\n" +
		"##neigh\naa:bb:cc:11:22:33 192.168.1.50\nde:ad:be:ef:00:01 not-an-ip\nde:ad:be:ef:00:02 fe80::1\n" + sampleClients
	leases, neigh := parsePreamble(leaseSection(out))
	if len(leases) != 1 || leases["aa:bb:cc:11:22:33"] != "nishants-iphone" {
		t.Fatalf("unexpected leases: %v", leases)
	}
	if len(neigh) != 1 || neigh["aa:bb:cc:11:22:33"] != "192.168.1.50" {
		t.Fatalf("unexpected neighbours: %v", neigh)
	}
	for _, c := range parseWifiClients(out) {
		if c.Name != "" {
			t.Fatalf("parseWifiClients must not name clients, got %q for %s", c.Name, c.MAC)
		}
	}
	if l, n := parsePreamble(""); len(l) != 0 || len(n) != 0 {
		t.Fatal("empty output should have no side tables")
	}
}

func TestLeaseCacheNamesAcrossDevices(t *testing.T) {
	var c leaseCache
	c.set("router", map[string]string{"aa:bb:cc:11:22:33": "nishants-iphone"})
	c.set("ap", nil)
	if got := c.lookup("aa:bb:cc:11:22:33"); got != "nishants-iphone" {
		t.Fatalf("lookup = %q", got)
	}
	if got := c.lookup("de:ad:be:ef:00:01"); got != "" {
		t.Fatalf("unknown MAC should have no name, got %q", got)
	}
	c.set("router", nil)
	if got := c.lookup("aa:bb:cc:11:22:33"); got != "" {
		t.Fatalf("a device that reports no leases must drop its names, got %q", got)
	}
}
