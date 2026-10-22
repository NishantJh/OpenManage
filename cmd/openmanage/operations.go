package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

// Every SSH action against a device goes through this file. An operation is a named,
// server-owned definition; callers pick operations by name and never supply command
// text. Health checks and monitoring use the read-only operations below. Write
// operations are not executable yet: they need transactional audit and a rollback
// path (plan.md Section 3) before runOperation will accept one.

type operationClass string

const (
	operationRead  operationClass = "read"
	operationWrite operationClass = "write"
)

// auditPolicy says how an operation's outcome must be recorded. Read-only operations
// may audit best-effort, so a failed audit write never hides a device result. Write
// operations must audit in the same transaction as the state change they make.
type auditPolicy string

const (
	auditBestEffort    auditPolicy = "best_effort"
	auditTransactional auditPolicy = "transactional"
)

type operation struct {
	Name        string
	Label       string
	Class       operationClass
	Roles       []string // roles allowed to start it from the API; system jobs bypass this
	Timeout     time.Duration
	OutputLimit int
	Audit       auditPolicy
	command     string
}

const (
	healthCheckTimeout = 20 * time.Second
	healthOutputLimit  = 8 * 1024
	operationErrLimit  = 1024
)

var readOnlyRoles = []string{"admin", "operator", "readonly"}

func readOperation(name, label, command string) operation {
	return operation{
		Name: name, Label: label, Class: operationRead, Roles: readOnlyRoles,
		Timeout: healthCheckTimeout, OutputLimit: healthOutputLimit, Audit: auditBestEffort,
		command: command,
	}
}

// healthOperations is the read-only set run by on-demand health checks and monitoring.
var healthOperations = []operation{
	readOperation("system_board", "System information", "ubus call system board"),
	readOperation("uptime", "Uptime and load", "uptime"),
	readOperation("memory", "Memory", "free"),
	readOperation("overlay_storage", "Overlay storage", "df -h /overlay"),
}

func (op operation) allows(role string) bool {
	for _, allowed := range op.Roles {
		if allowed == role {
			return true
		}
	}
	return false
}

// requireOperationRoles answers 401 or 403, as requireAnyRole does, unless the
// caller's role may start every operation in ops.
func requireOperationRoles(w http.ResponseWriter, r *http.Request, ops []operation) bool {
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return false
	}
	for _, op := range ops {
		if !op.allows(user.Role) {
			writeError(w, http.StatusForbidden, "insufficient role for this action")
			return false
		}
	}
	return true
}

type operationResult struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Status   string `json:"status"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration_ms"`
}

var errWriteOperationDisabled = errors.New("write operations are not enabled")

// Errors from loadDeviceTarget. Each caller maps them to its own response.
var (
	errTargetNotFound       = errors.New("device not found")
	errTargetNotPrivate     = errors.New("device address is not a private IP")
	errTargetNotEnrolled    = errors.New("device is not enrolled")
	errTargetKeyUnavailable = errors.New("server encryption key is unavailable")
	errTargetDecrypt        = errors.New("stored credential could not be decrypted")
	errTargetBadCredential  = errors.New("stored credential is not a supported private key")
)

type deviceTarget struct {
	ID          string
	Name        string
	Address     string
	Username    string
	Fingerprint string
	signer      ssh.Signer
}

// loadDeviceTarget loads a device and its decrypted credential, enforcing the
// private-address boundary before any credential is read.
func (a *app) loadDeviceTarget(ctx context.Context, deviceID string) (deviceTarget, error) {
	target := deviceTarget{ID: deviceID}
	err := a.db.QueryRow(ctx, "SELECT name,address FROM devices WHERE id=$1", deviceID).Scan(&target.Name, &target.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, errTargetNotFound
	}
	if err != nil {
		return target, err
	}
	if !isPrivateDeviceIP(target.Address) {
		return target, errTargetNotPrivate
	}
	var ciphertext []byte
	err = a.db.QueryRow(ctx, "SELECT username,private_key_ciphertext,host_key_fingerprint FROM device_credentials WHERE device_id=$1", deviceID).
		Scan(&target.Username, &ciphertext, &target.Fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, errTargetNotEnrolled
	}
	if err != nil {
		return target, err
	}
	key, err := encryptionKey()
	if err != nil {
		return target, errTargetKeyUnavailable
	}
	privateKey, err := decryptSecret(key, ciphertext)
	if err != nil {
		return target, errTargetDecrypt
	}
	if target.signer, err = ssh.ParsePrivateKey(privateKey); err != nil {
		return target, errTargetBadCredential
	}
	return target, nil
}

// deviceSession is one pinned SSH connection used for a sequence of operations.
type deviceSession struct {
	client *ssh.Client
	done   chan struct{}
}

// openDeviceSession dials the target with its pinned host key. The connection is
// closed when ctx ends, so a hung device cannot hold a worker past its deadline.
// Network failures return *sshDialError, as dialPinnedSSH does.
func openDeviceSession(ctx context.Context, target deviceTarget, timeout time.Duration) (*deviceSession, error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	client, err := dialPinnedSSH(ctx, net.JoinHostPort(target.Address, "22"), target.Username, target.signer, target.Fingerprint, deadline)
	if err != nil {
		return nil, err
	}
	return watchDeviceSession(ctx, client), nil
}

func watchDeviceSession(ctx context.Context, client *ssh.Client) *deviceSession {
	s := &deviceSession{client: client, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-s.done:
		}
	}()
	return s
}

func (s *deviceSession) Close() {
	close(s.done)
	_ = s.client.Close()
}

// run executes one operation with its own timeout and output limit. Results never
// include stderr or raw SSH errors.
func (s *deviceSession) run(op operation) operationResult {
	result := operationResult{Name: op.Name, Label: op.Label}
	start := time.Now()
	finish := func(status, msg string) operationResult {
		result.Status, result.Error = status, msg
		result.Duration = time.Since(start).Milliseconds()
		return result
	}
	if op.Class != operationRead {
		return finish("error", errWriteOperationDisabled.Error())
	}
	session, err := s.client.NewSession()
	if err != nil {
		return finish("error", "could not open SSH session")
	}
	defer session.Close()

	stdout := &boundedBuffer{limit: op.OutputLimit}
	session.Stdout = stdout
	session.Stderr = &boundedBuffer{limit: operationErrLimit}
	var timedOut atomic.Bool
	timer := time.AfterFunc(op.Timeout, func() {
		timedOut.Store(true)
		_ = session.Close()
	})
	err = session.Run(op.command)
	timer.Stop()
	result.Output = strings.TrimSpace(stdout.String())
	switch {
	case err == nil:
		return finish("ok", "")
	case timedOut.Load():
		return finish("error", "command timed out")
	case strings.Contains(err.Error(), "output limit exceeded"):
		return finish("error", "device output exceeded the allowed limit")
	default:
		return finish("error", "command failed or is unsupported by this firmware")
	}
}
