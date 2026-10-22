package main

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// deviceInventory is what a device reports about itself. Empty fields mean "not
// reported" and leave the stored value unchanged.
type deviceInventory struct {
	Model    string
	Firmware string
}

// parseSystemBoard reads `ubus call system board` output. Real hardware reports model
// and board_name; containers and some x86 builds report neither.
func parseSystemBoard(output string) deviceInventory {
	var board struct {
		Model     string `json:"model"`
		BoardName string `json:"board_name"`
		Release   struct {
			Distribution string `json:"distribution"`
			Version      string `json:"version"`
			Description  string `json:"description"`
		} `json:"release"`
	}
	if json.Unmarshal([]byte(output), &board) != nil {
		return deviceInventory{}
	}
	inv := deviceInventory{Model: cleanInventoryField(board.Model)}
	if inv.Model == "" {
		inv.Model = cleanInventoryField(board.BoardName)
	}
	inv.Firmware = cleanInventoryField(board.Release.Description)
	if inv.Firmware == "" {
		inv.Firmware = cleanInventoryField(strings.TrimSpace(board.Release.Distribution + " " + board.Release.Version))
	}
	return inv
}

// inventoryFromResults returns what the system_board operation reported, if it succeeded.
func inventoryFromResults(results []operationResult) deviceInventory {
	for _, r := range results {
		if r.Name == "system_board" && r.Status == "ok" {
			return parseSystemBoard(r.Output)
		}
	}
	return deviceInventory{}
}

// cleanInventoryField drops control characters and caps the value at the same byte
// length the API allows for operator-entered fields.
func cleanInventoryField(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s))
	for len(s) > maxDeviceFieldLength {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return strings.TrimSpace(s)
}
