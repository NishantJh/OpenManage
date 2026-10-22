package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
)

func enrollmentErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("error response is not JSON: %v", err)
	}
	if out.Error == "" {
		t.Fatal("error response must include a human-readable message")
	}
	return out.Code
}

func TestEnrollDeviceReportsEncryptionKeyCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		code string
	}{
		{name: "missing", key: "", code: enrollCodeKeyMissing},
		{name: "not base64", key: "not base64!", code: enrollCodeKeyInvalid},
		{name: "wrong length", key: base64.StdEncoding.EncodeToString([]byte("too short")), code: enrollCodeKeyInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENMANAGE_ENCRYPTION_KEY", tc.key)
			rec := performEnrollmentRequest(t, "550e8400-e29b-41d4-a716-446655440000", "admin", "{}")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d", rec.Code)
			}
			if got := enrollmentErrorCode(t, rec.Body.Bytes()); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestEnrollDeviceReportsInvalidRequestCode(t *testing.T) {
	t.Setenv("OPENMANAGE_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32)))
	for _, body := range []string{"{", `{"username":"u"} {}`, `{"username":"","private_key":"k","expected_fingerprint":"SHA256:x"}`} {
		rec := performEnrollmentRequest(t, "550e8400-e29b-41d4-a716-446655440000", "admin", body)
		if got := enrollmentErrorCode(t, rec.Body.Bytes()); got != enrollCodeInvalidRequest {
			t.Errorf("body %q: code = %q, want %q", body, got, enrollCodeInvalidRequest)
		}
	}
}

// The classification must hold for real errors from the SSH library, not just sentinels.
func TestClassifyEnrollmentSSHErrorFromRealFailures(t *testing.T) {
	authorized, _ := generatedSigner(t)
	other, _ := generatedSigner(t)
	addr, hostFP := startTestSSHServer(t, authorized.PublicKey())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := ln.Addr().String()
	ln.Close()

	for _, tc := range []struct {
		name   string
		addr   string
		fp     string
		status int
		code   string
	}{
		{name: "unreachable", addr: closedAddr, fp: hostFP, status: http.StatusBadGateway, code: enrollCodeUnreachable},
		{name: "fingerprint mismatch", addr: addr, fp: "SHA256:" + strings.Repeat("A", 43), status: http.StatusUnprocessableEntity, code: enrollCodeHostKeyMismatch},
		{name: "unauthorized key", addr: addr, fp: hostFP, status: http.StatusUnprocessableEntity, code: enrollCodeAuthFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signer := authorized
			if tc.code == enrollCodeAuthFailed {
				signer = other
			}
			_, err := verifyDeviceSSH(context.Background(), tc.addr, "root", signer, tc.fp)
			if err == nil {
				t.Fatal("expected verification to fail")
			}
			status, code, message := classifyEnrollmentSSHError(err)
			if status != tc.status || code != tc.code {
				t.Fatalf("got (%d, %q), want (%d, %q); err: %v", status, code, tc.status, tc.code, err)
			}
			if strings.Contains(message, err.Error()) {
				t.Fatal("message must not include raw SSH error text")
			}
		})
	}
}

// A TCP/22 listener that drops the connection before the SSH handshake completes must
// be reported as unreachable, not as a rejected key.
func TestClassifyEnrollmentSSHErrorHandshakeDropIsUnreachable(t *testing.T) {
	signer, _ := generatedSigner(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	_, err = verifyDeviceSSH(context.Background(), ln.Addr().String(), "root", signer, "SHA256:x")
	if err == nil {
		t.Fatal("expected handshake failure")
	}
	if status, code, _ := classifyEnrollmentSSHError(err); status != http.StatusBadGateway || code != enrollCodeUnreachable {
		t.Fatalf("got (%d, %q) for %v", status, code, err)
	}
}
