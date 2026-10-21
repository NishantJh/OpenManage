package main

import "testing"

func TestParseWiFiScan(t *testing.T) {
	output := `Cell 01 - Address: AA:BB:CC:DD:EE:01
          ESSID: "Hall"
          Channel: 36
          Signal: -54 dBm
          Quality: 54/70
Cell 02 - Address: AA:BB:CC:DD:EE:02
          ESSID: "Neighbour"
          Channel: 1
          Signal: -78 dBm
`
	got := parseWiFiScan("device-id", "Hall AP", "wlan0", "AA:BB:CC:DD:EE:00", output)
	if len(got) != 2 {
		t.Fatalf("expected 2 observations, got %d", len(got))
	}
	if got[0].ObservedBSSID != "AA:BB:CC:DD:EE:01" || got[0].SignalDBM != -54 || got[0].Channel != 36 || got[0].SSID != "Hall" {
		t.Fatalf("unexpected first observation: %#v", got[0])
	}
	if got[1].SignalDBM != -78 || got[1].SSID != "Neighbour" {
		t.Fatalf("unexpected second observation: %#v", got[1])
	}
}

func TestValidateWiFiLayout(t *testing.T) {
	valid := wifiLayout{WidthM: 12, HeightM: 8, Rooms: []wifiRoom{{ID: "r1", Name: "Living room", X: 0, Y: 0, Width: 5, Height: 4}}, Walls: []wifiWall{}, APs: []wifiAPPlacement{}}
	if err := validateWiFiLayout(valid); err != nil { t.Fatalf("valid layout rejected: %v", err) }
	valid.Rooms[0].Width = 20
	if err := validateWiFiLayout(valid); err == nil { t.Fatal("expected out-of-bounds room to be rejected") }
}
