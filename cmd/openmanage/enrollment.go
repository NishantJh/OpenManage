package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"
)

const enrollmentTimeout = 8 * time.Second

type enrollRequest struct {
	Username            string `json:"username"`
	PrivateKey          string `json:"private_key"`
	ExpectedFingerprint string `json:"expected_fingerprint"`
}

type enrollResponse struct {
	DeviceID    string    `json:"device_id"`
	Username    string    `json:"username"`
	Fingerprint string    `json:"host_key_fingerprint"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// encryptionKey reads a 32-byte base64-encoded key. It is deliberately not
// generated automatically: losing this key makes stored SSH credentials unrecoverable.
func encryptionKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv("OPENMANAGE_ENCRYPTION_KEY"))
	if raw == "" {
		return nil, errEncryptionKeyMissing
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errEncryptionKeyInvalid
	}
	return key, nil
}

var (
	errEncryptionKeyMissing = errors.New("OPENMANAGE_ENCRYPTION_KEY is not configured")
	errEncryptionKeyInvalid = errors.New("OPENMANAGE_ENCRYPTION_KEY must be base64 encoding of exactly 32 random bytes")
	errHostKeyMismatch      = errors.New("host key fingerprint mismatch")
)

// Stable enrollment error codes returned to the UI alongside the human-readable message.
const (
	enrollCodeKeyMissing      = "encryption_key_missing"
	enrollCodeKeyInvalid      = "encryption_key_invalid"
	enrollCodeInvalidRequest  = "invalid_request"
	enrollCodeDeviceNotFound  = "device_not_found"
	enrollCodeAddressRejected = "address_not_private"
	enrollCodeInvalidKey      = "invalid_private_key"
	enrollCodeUnreachable     = "device_unreachable"
	enrollCodeHostKeyMismatch = "host_key_mismatch"
	enrollCodeAuthFailed      = "ssh_auth_failed"
	enrollCodeKeyNotVerified  = "key_not_verified"
	enrollCodeServerError     = "server_error"
)

func encryptSecret(key []byte, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, gcm.Seal(nil, nonce, plaintext, []byte("openmanage:ssh-private-key:v1"))...), nil
}

func decryptSecret(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("encrypted credential is truncated")
	}
	nonce, payload := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, payload, []byte("openmanage:ssh-private-key:v1"))
}

func isPrivateDeviceIP(address string) bool {
	ip := net.ParseIP(strings.TrimSpace(address))
	return ip != nil && ip.IsPrivate()
}

func pinnedHostKeyCallback(expected string, observed *string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, hostKey ssh.PublicKey) error {
		fingerprint := ssh.FingerprintSHA256(hostKey)
		*observed = fingerprint
		if fingerprint != expected {
			return errHostKeyMismatch
		}
		return nil
	}
}

func (a *app) enrollDevice(w http.ResponseWriter, r *http.Request) {
	if !validUUID(r.PathValue("id")) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator role required for SSH enrollment")
		return
	}
	key, err := encryptionKey()
	if errors.Is(err, errEncryptionKeyInvalid) {
		writeErrorCode(w, http.StatusServiceUnavailable, enrollCodeKeyInvalid, "SSH enrollment is disabled: server encryption key is not a base64-encoded 32-byte key")
		return
	}
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, enrollCodeKeyMissing, "SSH enrollment is disabled: server encryption key is not configured")
		return
	}

	var req enrollRequest
	if err := decodeJSONBody(w, r, enrollmentBodyLimit, &req); err != nil {
		if errors.Is(err, errTrailingJSON) {
			writeErrorCode(w, http.StatusBadRequest, enrollCodeInvalidRequest, "Request must contain a single JSON object")
		} else {
			writeErrorCode(w, http.StatusBadRequest, enrollCodeInvalidRequest, "Invalid enrollment request")
		}
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.ExpectedFingerprint = strings.TrimSpace(req.ExpectedFingerprint)
	if req.Username == "" || len(req.Username) > 128 || len(req.PrivateKey) == 0 || len(req.PrivateKey) > 24*1024 || !strings.HasPrefix(req.ExpectedFingerprint, "SHA256:") || len(req.ExpectedFingerprint) > 100 {
		writeErrorCode(w, http.StatusBadRequest, enrollCodeInvalidRequest, "Username, private key, and an independently verified SHA256 host-key fingerprint are required")
		return
	}

	var address string
	if err := a.db.QueryRow(r.Context(), "SELECT address FROM devices WHERE id=$1", r.PathValue("id")).Scan(&address); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorCode(w, http.StatusNotFound, enrollCodeDeviceNotFound, "Device not found")
		} else {
			writeErrorCode(w, http.StatusInternalServerError, enrollCodeServerError, "Could not load device")
		}
		return
	}
	if !isPrivateDeviceIP(address) {
		writeErrorCode(w, http.StatusUnprocessableEntity, enrollCodeAddressRejected, "Enrollment only permits private IP addresses; DNS names, link-local, and public addresses are rejected")
		return
	}

	signer, err := ssh.ParsePrivateKey([]byte(req.PrivateKey))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, enrollCodeInvalidKey, "Private key is not a supported, unencrypted SSH private key")
		return
	}
	observedFingerprint, err := verifyDeviceSSH(r.Context(), net.JoinHostPort(address, "22"), req.Username, signer, req.ExpectedFingerprint)
	if err != nil {
		status, code, message := classifyEnrollmentSSHError(err)
		writeErrorCode(w, status, code, message)
		return
	}

	ciphertext, err := encryptSecret(key, []byte(req.PrivateKey))
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, enrollCodeServerError, "Could not protect SSH credential")
		return
	}
	auditDetails, err := json.Marshal(map[string]string{
		"username": req.Username, "host_key_fingerprint": observedFingerprint,
	})
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, enrollCodeServerError, "Could not prepare enrollment audit event")
		return
	}
	updated, err := storeEnrollment(r.Context(), a.db, r.PathValue("id"), user.ID, req.Username, ciphertext, observedFingerprint, string(auditDetails))
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, enrollCodeServerError, "Could not persist enrollment and audit event")
		return
	}
	writeJSON(w, http.StatusOK, enrollResponse{
		DeviceID: r.PathValue("id"), Username: req.Username,
		Fingerprint: observedFingerprint, UpdatedAt: updated,
	})
}

// classifyEnrollmentSSHError maps a verifyDeviceSSH failure to an HTTP status, a
// stable code, and a sanitized message. Underlying error text is never returned.
func classifyEnrollmentSSHError(err error) (int, string, string) {
	var dialErr *sshDialError
	switch {
	case errors.As(err, &dialErr):
		return http.StatusBadGateway, enrollCodeUnreachable, "Could not connect to SSH on the device"
	case errors.Is(err, errKeyNotUsed):
		return http.StatusUnprocessableEntity, enrollCodeKeyNotVerified, "The device allowed login without checking the SSH key, so the key could not be verified"
	case errors.Is(err, errHostKeyMismatch):
		return http.StatusUnprocessableEntity, enrollCodeHostKeyMismatch, "The device's SSH host key did not match the expected fingerprint"
	case isSSHTransportFailure(err):
		return http.StatusBadGateway, enrollCodeUnreachable, "Connected to TCP/22, but the SSH handshake did not complete"
	default:
		return http.StatusUnprocessableEntity, enrollCodeAuthFailed, "SSH authentication failed with the supplied username and key"
	}
}

// isSSHTransportFailure reports a handshake that died from the connection itself
// (closed, reset, or timed out) before authentication could be judged.
func isSSHTransportFailure(err error) bool {
	var netErr net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.ECONNRESET) ||
		(errors.As(err, &netErr) && netErr.Timeout())
}

// sshDialError marks failures to reach the device, as opposed to SSH handshake
// failures such as a host-key mismatch or rejected credentials.
type sshDialError struct{ err error }

func (e *sshDialError) Error() string { return e.err.Error() }
func (e *sshDialError) Unwrap() error { return e.err }

// verifyDeviceSSH completes an SSH handshake against hostPort, accepting only the
// pinned host-key fingerprint, and returns the fingerprint that was observed.
func verifyDeviceSSH(ctx context.Context, hostPort, username string, signer ssh.Signer, expectedFingerprint string) (string, error) {
	client, err := dialPinnedSSH(ctx, hostPort, username, signer, expectedFingerprint, time.Now().Add(enrollmentTimeout))
	if err != nil {
		return "", err
	}
	_ = client.Close()
	return expectedFingerprint, nil
}

// storeEnrollment upserts the device credential, marks the device online, and
// writes the audit event in one transaction so none is persisted without the others.
func storeEnrollment(ctx context.Context, db *pgxpool.Pool, deviceID, userID, username string, ciphertext []byte, fingerprint, auditDetails string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(ctx)
	var updated time.Time
	err = tx.QueryRow(ctx,
		"INSERT INTO device_credentials (device_id, username, private_key_ciphertext, host_key_fingerprint, updated_at) "+
			"VALUES ($1, $2, $3, $4, NOW()) "+
			"ON CONFLICT (device_id) DO UPDATE SET username=EXCLUDED.username, "+
			"private_key_ciphertext=EXCLUDED.private_key_ciphertext, host_key_fingerprint=EXCLUDED.host_key_fingerprint, updated_at=NOW() "+
			"RETURNING updated_at",
		deviceID, username, ciphertext, fingerprint).Scan(&updated)
	if err == nil {
		_, err = tx.Exec(ctx, "UPDATE devices SET status='online', last_seen_at=NOW(), updated_at=NOW() WHERE id=$1", deviceID)
	}
	if err == nil {
		_, err = tx.Exec(ctx,
			"INSERT INTO audit_events(user_id,action,resource_type,resource_id,details) VALUES($1,$2,$3,$4,$5)",
			userID, "device.enrolled", "device", deviceID, auditDetails)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	return updated, err
}
