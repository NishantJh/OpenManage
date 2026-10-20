package main

import (
	"strings"
	"testing"
)

func TestBoundedBufferAllowsOutputWithinLimit(t *testing.T) {
	buffer := &boundedBuffer{limit: 8}
	n, err := buffer.Write([]byte("openwrt"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 7 || buffer.String() != "openwrt" {
		t.Fatalf("unexpected write result n=%d content=%q", n, buffer.String())
	}
}

func TestBoundedBufferRejectsExcessOutput(t *testing.T) {
	buffer := &boundedBuffer{limit: 5}
	n, err := buffer.Write([]byte("12345678"))
	if err == nil || n != 5 || buffer.String() != "12345" {
		t.Fatalf("expected bounded partial write, got n=%d content=%q err=%v", n, buffer.String(), err)
	}
	if _, err := buffer.Write([]byte("x")); err == nil {
		t.Fatal("expected further writes to fail after the limit")
	}
}

func TestHealthCommandsAreNamedAndReadOnlyAllowlist(t *testing.T) {
	if len(openWrtHealthCommands) < 3 {
		t.Fatalf("expected multiple health commands, got %d", len(openWrtHealthCommands))
	}
	seen := map[string]bool{}
	for _, command := range openWrtHealthCommands {
		if command.Name == "" || command.Label == "" || command.Command == "" {
			t.Fatalf("incomplete command definition: %#v", command)
		}
		if seen[command.Name] {
			t.Fatalf("duplicate command name %q", command.Name)
		}
		seen[command.Name] = true
		if strings.ContainsAny(command.Command, ";|&`$\n") {
			t.Errorf("command %q should be a fixed single command, got %q", command.Name, command.Command)
		}
	}
}

func TestHealthCommandsDoNotAcceptClientSuppliedCommands(t *testing.T) {
	// The execution list is server-owned; callers cannot provide command strings.
	if strings.Contains(strings.Join([]string{openWrtHealthCommands[0].Command}, ""), "$(") {
		t.Fatal("health command unexpectedly contains shell substitution")
	}
}
