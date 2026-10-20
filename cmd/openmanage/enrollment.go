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
	"time"

	"github.com/jackc/pgx/v5"
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
		return nil, errors.New("OPENMANAGE_ENCRYPTION_KEY is not configured")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errors.New("OPENMANAGE_ENCRYPTION_KEY must be base64 encoding of exactly 32 random bytes")
	}
	return key, nil
}

func encryptSecret(key []byte, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil { return nil, err }
	gcm, err := cipher.NewGCM(block)
	if err != nil { return nil, err }
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil { return nil, err }
	return append(nonce, gcm.Seal(nil, nonce, plaintext, []byte("openmanage:ssh-private-key:v1"))...), nil
}

func decryptSecret(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil { return nil, err }
	gcm, err := cipher.NewGCM(block)
	if err != nil { return nil, err }
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
		if fingerprint != expected { return errors.New("host key fingerprint mismatch") }
		return nil
	}
}

func (a *app) enrollDevice(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator role required for SSH enrollment")
		return
	}
	key, err := encryptionKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SSH enrollment is disabled: server encryption key is not configured")
		return
	}

	var req enrollRequest
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid enrollment request")
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "Request must contain a single JSON object")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.ExpectedFingerprint = strings.TrimSpace(req.ExpectedFingerprint)
	if req.Username == "" || len(req.Username) > 128 || len(req.PrivateKey) == 0 || len(req.PrivateKey) > 24*1024 || !strings.HasPrefix(req.ExpectedFingerprint, "SHA256:") || len(req.ExpectedFingerprint) > 100 {
		writeError(w, http.StatusBadRequest, "Username, private key, and an independently verified SHA256 host-key fingerprint are required")
		return
	}

	var address string
	if err := a.db.QueryRow(r.Context(), "SELECT address FROM devices WHERE id=$1", r.PathValue("id")).Scan(&address); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Device not found")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not load device")
		}
		return
	}
	if !isPrivateDeviceIP(address) {
		writeError(w, http.StatusUnprocessableEntity, "Enrollment only permits private IP addresses; DNS names, link-local, and public addresses are rejected")
		return
	}

	signer, err := ssh.ParsePrivateKey([]byte(req.PrivateKey))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Private key is not a supported, unencrypted SSH private key")
		return
	}
	var observedFingerprint string
	config := &ssh.ClientConfig{
		User: req.Username,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: pinnedHostKeyCallback(req.ExpectedFingerprint, &observedFingerprint),
		Timeout: enrollmentTimeout,
	}
	dialer := net.Dialer{Timeout: enrollmentTimeout}
	conn, err := dialer.DialContext(r.Context(), "tcp", net.JoinHostPort(address, "22"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "Could not connect to SSH on the device")
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(enrollmentTimeout))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(address, "22"), config)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "SSH authentication failed or host-key fingerprint did not match")
		return
	}
	_ = ssh.NewClient(sshConn, chans, reqs).Close()
	_ = conn.SetDeadline(time.Time{})

	ciphertext, err := encryptSecret(key, []byte(req.PrivateKey))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not protect SSH credential")
		return
	}
	auditDetails, err := json.Marshal(map[string]string{
		"username": req.Username, "host_key_fingerprint": observedFingerprint,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not prepare enrollment audit event")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not start credential transaction")
		return
	}
	defer tx.Rollback(ctx)
	var updated time.Time
	err = tx.QueryRow(ctx,
		"INSERT INTO device_credentials (device_id, username, private_key_ciphertext, host_key_fingerprint, updated_at) "+
			"VALUES ($1, $2, $3, $4, NOW()) "+
			"ON CONFLICT (device_id) DO UPDATE SET username=EXCLUDED.username, "+
			"private_key_ciphertext=EXCLUDED.private_key_ciphertext, host_key_fingerprint=EXCLUDED.host_key_fingerprint, updated_at=NOW() "+
			"RETURNING updated_at",
		r.PathValue("id"), req.Username, ciphertext, observedFingerprint).Scan(&updated)
	if err == nil {
		_, err = tx.Exec(ctx, "UPDATE devices SET status='online', last_seen_at=NOW(), updated_at=NOW() WHERE id=$1", r.PathValue("id"))
	}
	if err == nil {
		_, err = tx.Exec(ctx,
			"INSERT INTO audit_events(user_id,action,resource_type,resource_id,details) VALUES($1,$2,$3,$4,$5)",
			user.ID, "device.enrolled", "device", r.PathValue("id"), string(auditDetails))
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not persist enrollment and audit event")
		return
	}
	writeJSON(w, http.StatusOK, enrollResponse{
		DeviceID: r.PathValue("id"), Username: req.Username,
		Fingerprint: observedFingerprint, UpdatedAt: updated,
	})
}
