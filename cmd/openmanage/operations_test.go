package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fakeCommand struct {
	output string
	exit   uint32
	delay  time.Duration
}

// startExecSSHServer accepts authorizedKey and answers exec requests from commands.
// Unknown commands exit 127, like a shell.
func startExecSSHServer(t *testing.T, authorizedKey ssh.PublicKey, commands map[string]fakeCommand) (string, string) {
	t.Helper()
	hostSigner, _ := generatedSigner(t)
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorizedKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	config.AddHostKey(hostSigner)
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
			go serveExecConn(conn, config, commands)
		}
	}()
	return ln.Addr().String(), ssh.FingerprintSHA256(hostSigner.PublicKey())
}

func serveExecConn(conn net.Conn, config *ssh.ServerConfig, commands map[string]fakeCommand) {
	defer conn.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range chReqs {
				if req.Type != "exec" || len(req.Payload) < 4 {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				cmd, ok := commands[string(req.Payload[4:])]
				if !ok {
					cmd = fakeCommand{exit: 127}
				}
				time.Sleep(cmd.delay)
				_, _ = ch.Write([]byte(cmd.output))
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, cmd.exit)
				_, _ = ch.SendRequest("exit-status", false, status)
				return
			}
		}()
	}
}

func testSession(t *testing.T, ctx context.Context, commands map[string]fakeCommand) *deviceSession {
	t.Helper()
	signer, _ := generatedSigner(t)
	addr, fp := startExecSSHServer(t, signer.PublicKey(), commands)
	host, port, _ := net.SplitHostPort(addr)
	if port == "22" {
		t.Skip("test server unexpectedly on port 22")
	}
	// openDeviceSession always dials port 22, so dial the test port directly.
	client, err := dialPinnedSSH(context.Background(), net.JoinHostPort(host, port), "root", signer, fp, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	s := watchDeviceSession(ctx, client)
	t.Cleanup(s.Close)
	return s
}

func TestRunOperationResults(t *testing.T) {
	s := testSession(t, context.Background(), map[string]fakeCommand{
		"ok":   {output: " fine \n"},
		"fail": {output: "partial", exit: 1},
		"big":  {output: strings.Repeat("x", 64)},
		"slow": {delay: 2 * time.Second},
	})
	op := func(command string) operation {
		o := readOperation(command, command, command)
		o.OutputLimit = 32
		o.Timeout = 300 * time.Millisecond
		return o
	}
	for _, tc := range []struct {
		command, status, output, errText string
	}{
		{command: "ok", status: "ok", output: "fine"},
		{command: "fail", status: "error", output: "partial", errText: "command failed or is unsupported by this firmware"},
		{command: "missing", status: "error", errText: "command failed or is unsupported by this firmware"},
		{command: "big", status: "error", errText: "device output exceeded the allowed limit"},
		{command: "slow", status: "error", errText: "command timed out"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got := s.run(op(tc.command))
			if got.Status != tc.status || got.Error != tc.errText {
				t.Fatalf("got status %q error %q, want %q %q", got.Status, got.Error, tc.status, tc.errText)
			}
			if tc.output != "" && got.Output != tc.output {
				t.Fatalf("output %q, want %q", got.Output, tc.output)
			}
			if got.Name != tc.command || got.Duration < 0 {
				t.Fatalf("result metadata wrong: %#v", got)
			}
		})
	}
}

func TestRunOperationRefusesWriteOperations(t *testing.T) {
	write := readOperation("commit", "Commit", "uci commit")
	write.Class = operationWrite
	write.Audit = auditTransactional
	// No client: reaching execution would panic, so this proves refusal comes first.
	got := (&deviceSession{}).run(write)
	if got.Status != "error" || got.Error != errWriteOperationDisabled.Error() {
		t.Fatalf("write operation must be refused before execution, got %#v", got)
	}
}

func TestDeviceSessionClosesWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := testSession(t, ctx, map[string]fakeCommand{"slow": {delay: 3 * time.Second}})
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if got := s.run(readOperation("slow", "slow", "slow")); got.Status != "error" {
		t.Fatalf("expected error after cancellation, got %#v", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancelled context did not interrupt the running operation")
	}
}

func TestRequireOperationRoles(t *testing.T) {
	ops := []operation{readOperation("a", "A", "uptime")}
	adminOnly := ops[0]
	adminOnly.Roles = []string{"admin"}
	for _, tc := range []struct {
		name   string
		user   *authUser
		ops    []operation
		ok     bool
		status int
	}{
		{name: "no user", ops: ops, status: http.StatusUnauthorized},
		{name: "readonly allowed", user: &authUser{Role: "readonly"}, ops: ops, ok: true},
		{name: "readonly denied", user: &authUser{Role: "readonly"}, ops: append(ops, adminOnly), status: http.StatusForbidden},
		{name: "unknown role", user: &authUser{Role: "guest"}, ops: ops, status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.user != nil {
				req = req.WithContext(context.WithValue(req.Context(), userContextKey, *tc.user))
			}
			rec := httptest.NewRecorder()
			if got := requireOperationRoles(rec, req, tc.ops); got != tc.ok {
				t.Fatalf("allowed=%v, want %v", got, tc.ok)
			}
			if !tc.ok && rec.Code != tc.status {
				t.Fatalf("status %d, want %d", rec.Code, tc.status)
			}
		})
	}
}
