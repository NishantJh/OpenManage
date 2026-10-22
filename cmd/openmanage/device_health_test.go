package main

import (
	"io"
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

func TestBoundedBufferLimitHoldsThroughIOCopy(t *testing.T) {
	buffer := &boundedBuffer{limit: 5}
	if _, err := io.Copy(buffer, strings.NewReader("12345678")); err == nil {
		t.Fatal("io.Copy must fail once the limit is exceeded")
	}
	if buffer.String() != "12345" {
		t.Fatalf("buffer kept %q", buffer.String())
	}
}

func TestHealthOperationsAreNamedReadOnlyAllowlist(t *testing.T) {
	if len(healthOperations) < 3 {
		t.Fatalf("expected multiple health operations, got %d", len(healthOperations))
	}
	seen := map[string]bool{}
	for _, op := range healthOperations {
		if op.Name == "" || op.Label == "" || op.command == "" {
			t.Fatalf("incomplete operation definition: %#v", op)
		}
		if seen[op.Name] {
			t.Fatalf("duplicate operation name %q", op.Name)
		}
		seen[op.Name] = true
		if op.Class != operationRead || op.Audit != auditBestEffort {
			t.Errorf("health operation %q must be read-only with best-effort audit", op.Name)
		}
		if op.Timeout <= 0 || op.Timeout > healthCheckTimeout || op.OutputLimit <= 0 {
			t.Errorf("health operation %q needs a bounded timeout and output limit", op.Name)
		}
		if len(op.Roles) == 0 {
			t.Errorf("health operation %q allows no roles", op.Name)
		}
		if strings.ContainsAny(op.command, ";|&`$\n<>") {
			t.Errorf("operation %q should be a fixed single command, got %q", op.Name, op.command)
		}
	}
}
