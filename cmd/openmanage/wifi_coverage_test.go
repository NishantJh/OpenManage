package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

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
	if err := validateWiFiLayout(valid); err != nil {
		t.Fatalf("valid layout rejected: %v", err)
	}
	valid.Rooms[0].Width = 20
	if err := validateWiFiLayout(valid); err == nil {
		t.Fatal("expected out-of-bounds room to be rejected")
	}
	valid.Rooms[0].Width = 5
	valid.Walls = []wifiWall{{ID: "w1", X1: 1, Y1: 1, X2: 2, Y2: 2, Material: "unknown"}}
	if err := validateWiFiLayout(valid); err == nil {
		t.Fatal("expected unsupported wall material to be rejected")
	}
}

func TestWiFiCommandResultRejectsTruncatedOutput(t *testing.T) {
	for _, tc := range []struct {
		name              string
		stdoutTruncated   bool
		stderrTruncated   bool
		commandErr        error
	}{
		{name: "stdout", stdoutTruncated: true},
		{name: "stderr", stderrTruncated: true},
		{name: "both", stdoutTruncated: true, stderrTruncated: true},
		{name: "command failure", commandErr: errors.New("exit status 1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := wifiCommandResult("partial scan output", tc.stdoutTruncated, tc.stderrTruncated, tc.commandErr)
			if err == nil {
				t.Fatal("expected command output to be rejected")
			}
			if (tc.stdoutTruncated || tc.stderrTruncated) && !errors.Is(err, errWiFiOutputTruncated) {
				t.Fatalf("expected truncation error, got %v", err)
			}
			if !strings.Contains(out, "partial scan output") {
				t.Fatalf("expected bounded output to be returned for diagnostics, got %q", out)
			}
		})
	}
}

func TestWiFiOutputBufferReportsTruncationAndBoundsMemory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		limit int
		want  string
		trunc bool
	}{
		{name: "within limit", input: "1234", limit: 4, want: "1234"},
		{name: "exact limit", input: "12345", limit: 4, want: "1234", trunc: true},
		{name: "zero limit", input: "x", limit: 0, want: "", trunc: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &wifiOutputBuffer{limit: tc.limit}
			if _, err := b.Write([]byte(tc.input)); err != nil {
				t.Fatalf("Write returned error: %v", err)
			}
			if b.String() != tc.want || b.truncated != tc.trunc {
				t.Fatalf("got output %q truncated=%v; want %q truncated=%v", b.String(), b.truncated, tc.want, tc.trunc)
			}
		})
	}
}

func TestPersistWiFiScanRejectsCanceledContextBeforePersistence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := persistWiFiScan(ctx, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if called {
		t.Fatal("persistence callback ran after cancellation")
	}
}

func TestPersistWiFiScanRejectsExpiredDeadlineBeforePersistence(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	called := false
	err := persistWiFiScan(ctx, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if called {
		t.Fatal("persistence callback ran after deadline expiry")
	}
}

func TestValidateWiFiLayoutRejectsNonFiniteNumbers(t *testing.T) {
	base := func() wifiLayout {
		return wifiLayout{
			WidthM: 12, HeightM: 8,
			Rooms: []wifiRoom{{ID: "r1", Name: "Living room", X: 0, Y: 0, Width: 5, Height: 4}},
			Walls: []wifiWall{{ID: "w1", X1: 1, Y1: 1, X2: 2, Y2: 2, Material: "brick"}},
			APs: []wifiAPPlacement{{DeviceID: "123e4567-e89b-12d3-a456-426614174000", X: 2, Y: 2}},
		}
	}
	bad := []struct {
		name string
		set  func(*wifiLayout, float64)
	}{
		{"layout width", func(l *wifiLayout, v float64) { l.WidthM = v }},
		{"layout height", func(l *wifiLayout, v float64) { l.HeightM = v }},
		{"room x", func(l *wifiLayout, v float64) { l.Rooms[0].X = v }},
		{"room y", func(l *wifiLayout, v float64) { l.Rooms[0].Y = v }},
		{"room width", func(l *wifiLayout, v float64) { l.Rooms[0].Width = v }},
		{"room height", func(l *wifiLayout, v float64) { l.Rooms[0].Height = v }},
		{"wall x1", func(l *wifiLayout, v float64) { l.Walls[0].X1 = v }},
		{"wall y1", func(l *wifiLayout, v float64) { l.Walls[0].Y1 = v }},
		{"wall x2", func(l *wifiLayout, v float64) { l.Walls[0].X2 = v }},
		{"wall y2", func(l *wifiLayout, v float64) { l.Walls[0].Y2 = v }},
		{"AP x", func(l *wifiLayout, v float64) { l.APs[0].X = v }},
		{"AP y", func(l *wifiLayout, v float64) { l.APs[0].Y = v }},
	}
	for _, value := range []struct {
		name string
		v    float64
	}{
		{name: "NaN", v: math.NaN()},
		{name: "+Inf", v: math.Inf(1)},
		{name: "-Inf", v: math.Inf(-1)},
	} {
		for _, field := range bad {
			t.Run(field.name+"/"+value.name, func(t *testing.T) {
				layout := base()
				field.set(&layout, value.v)
				if err := validateWiFiLayout(layout); err == nil {
					t.Fatalf("expected %s=%v to be rejected", field.name, value.v)
				}
			})
		}
	}
}

func TestFinalizeWiFiScanOutcomeReportsPartialAndInterfaceCap(t *testing.T) {
	outcome := wifiScanOutcome{
		InterfacesDiscovered: 6,
		InterfacesScanned: 3,
		InterfacesFailed: []wifiInterfaceFailure{{Interface: "wlan1", Reason: "interface_scan_failed"}},
		InterfacesOmitted: 2,
		Observations: []wifiObservation{{ObservedBSSID: "AA:BB:CC:DD:EE:FF"}},
	}
	finalizeWiFiScanOutcome(&outcome)
	if outcome.Status != "partial" || outcome.Count != 1 {
		t.Fatalf("unexpected outcome: %#v", outcome)
	}
	details := wifiScanAuditDetails(outcome, "")
	if details["interfaces_discovered"] != 6 || details["interfaces_scanned"] != 3 || details["interfaces_omitted"] != 2 || details["status"] != "partial" {
		t.Fatalf("audit details do not reflect partial scan/cap: %#v", details)
	}
	failed, ok := details["interfaces_failed"].([]wifiInterfaceFailure)
	if !ok || len(failed) != 1 || failed[0].Interface != "wlan1" {
		t.Fatalf("audit details lost interface failure: %#v", details["interfaces_failed"])
	}
}

func TestFinalizeWiFiScanOutcomeCompleteWhenNoFailuresOrOmissions(t *testing.T) {
	outcome := wifiScanOutcome{InterfacesDiscovered: 2, InterfacesScanned: 2, InterfacesFailed: []wifiInterfaceFailure{}, InterfacesOmitted: 0, Observations: []wifiObservation{{}, {}}}
	finalizeWiFiScanOutcome(&outcome)
	if outcome.Status != "complete" || outcome.Count != 2 {
		t.Fatalf("unexpected outcome: %#v", outcome)
	}
}
