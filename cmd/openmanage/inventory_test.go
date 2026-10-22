package main

import (
	"strings"
	"testing"
)

func TestParseSystemBoard(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		want         deviceInventory
	}{
		{
			name:   "hardware",
			output: `{"model":"GL.iNet GL-MT6000","board_name":"glinet,gl-mt6000","release":{"distribution":"OpenWrt","version":"24.10.4","description":"OpenWrt 24.10.4 r28959-29397011cc"}}`,
			want:   deviceInventory{Model: "GL.iNet GL-MT6000", Firmware: "OpenWrt 24.10.4 r28959-29397011cc"},
		},
		{
			name:   "board name only",
			output: `{"board_name":"qemu-standard-pc","release":{"distribution":"OpenWrt","version":"23.05.5"}}`,
			want:   deviceInventory{Model: "qemu-standard-pc", Firmware: "OpenWrt 23.05.5"},
		},
		{
			name:   "container reports no model",
			output: `{"kernel":"6.6","release":{"distribution":"OpenWrt","version":"24.10.4","description":"OpenWrt 24.10.4 r28959-29397011cc"}}`,
			want:   deviceInventory{Firmware: "OpenWrt 24.10.4 r28959-29397011cc"},
		},
		{name: "not JSON", output: "ubus: command not found"},
		{name: "empty", output: ""},
		{
			name:   "control characters stripped",
			output: "{\"model\":\"Router\\u001b[31m X\\n\",\"release\":{\"description\":\"\\tOpenWrt\"}}",
			want:   deviceInventory{Model: "Router[31m X", Firmware: "OpenWrt"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSystemBoard(tc.output); got != tc.want {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCleanInventoryFieldCapsLengthOnRuneBoundary(t *testing.T) {
	got := cleanInventoryField(strings.Repeat("é", maxDeviceFieldLength))
	if len(got) > maxDeviceFieldLength || !strings.HasPrefix(strings.Repeat("é", maxDeviceFieldLength), got) {
		t.Fatalf("bad cap: len=%d", len(got))
	}
}

func TestInventoryFromResultsUsesOnlySuccessfulSystemBoard(t *testing.T) {
	board := `{"model":"M","release":{"description":"F"}}`
	if got := inventoryFromResults([]operationResult{{Name: "system_board", Status: "error", Output: board}}); got != (deviceInventory{}) {
		t.Fatalf("failed operation must not update inventory, got %#v", got)
	}
	if got := inventoryFromResults([]operationResult{{Name: "uptime", Status: "ok"}, {Name: "system_board", Status: "ok", Output: board}}); got != (deviceInventory{Model: "M", Firmware: "F"}) {
		t.Fatalf("got %#v", got)
	}
}
