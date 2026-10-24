package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

// Password bootstrap: the administrator supplies the router's root password once.
// OpenManage uses it for a single pinned SSH login to install a freshly generated
// key, then verifies key login and forgets the password. The password is never
// stored, logged, or audited.

const bootstrapTimeout = 15 * time.Second

// bootstrapLimiter bounds root-password guesses per target address: five wrong
// passwords block that address for ten minutes.
var bootstrapLimiter = newLoginRateLimiter(5, 10*time.Minute, 10*time.Minute, 256)

const (
	bootstrapCodeNoPassword   = "no_root_password"
	bootstrapCodePasswordBad  = "ssh_password_rejected"
	bootstrapCodeInstallError = "key_install_failed"
)

// installKeyCommand is fixed. The public key arrives on stdin (never interpolated
// into the command), is appended to dropbear's authorized_keys once, and the file
// is kept private to root.
const installKeyCommand = `umask 077; f=/etc/dropbear/authorized_keys; mkdir -p /etc/dropbear && touch "$f" && chmod 600 "$f" && IFS= read -r k && [ -n "$k" ] && { grep -qxF "$k" "$f" || { [ -z "$(tail -c1 "$f")" ] || echo >> "$f"; printf '%s\n' "$k" >> "$f"; }; }`

type bootstrapProbeRequest struct {
	Address string `json:"address"`
}

type bootstrapProbeResponse struct {
	Address     string `json:"address"`
	Fingerprint string `json:"host_key_fingerprint"`
	Algorithm   string `json:"algorithm"`
}

type bootstrapRequest struct {
	Name                string `json:"name"`
	Address             string `json:"address"`
	Role                string `json:"role"`
	Username            string `json:"username"`
	Password            string `json:"password"`
	ExpectedFingerprint string `json:"expected_fingerprint"`
}

// generateManagementKey returns a new ed25519 key as an unencrypted OpenSSH private
// key plus the matching authorized_keys line.
func generateManagementKey() (privatePEM []byte, authorizedLine string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "openmanage")
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " openmanage"
	return pem.EncodeToMemory(block), line, nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// probeHostKey reads the device's SSH host-key fingerprint without authenticating.
func probeHostKey(ctx context.Context, hostPort string) (fingerprint, algorithm string, err error) {
	deadline := time.Now().Add(bootstrapTimeout)
	var lastErr error
	for _, algorithms := range hostKeyAlgorithmFamilies {
		var seen, seenAlgo string
		config := &ssh.ClientConfig{
			User:              "root",
			HostKeyAlgorithms: algorithms,
			Timeout:           time.Until(deadline),
			HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
				seen, seenAlgo = ssh.FingerprintSHA256(key), key.Type()
				return errors.New("probe only")
			},
		}
		dialer := net.Dialer{Deadline: deadline}
		conn, derr := dialer.DialContext(ctx, "tcp", hostPort)
		if derr != nil {
			return "", "", &sshDialError{derr}
		}
		_ = conn.SetDeadline(deadline)
		_, _, _, herr := ssh.NewClientConn(conn, hostPort, config)
		conn.Close()
		if seen != "" {
			return seen, seenAlgo, nil
		}
		lastErr = herr
	}
	if lastErr == nil {
		lastErr = errors.New("no host key offered")
	}
	return "", "", lastErr
}

func (a *app) bootstrapProbe(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator role required")
		return
	}
	var req bootstrapProbeRequest
	if err := decodeJSONBody(w, r, deviceBodyLimit, &req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, enrollCodeInvalidRequest, "Invalid request")
		return
	}
	req.Address = strings.TrimSpace(req.Address)
	if !isPrivateDeviceIP(req.Address) {
		writeErrorCode(w, http.StatusUnprocessableEntity, enrollCodeAddressRejected, "Only private IP addresses are accepted; DNS names, link-local, and public addresses are rejected")
		return
	}
	fp, algo, err := probeHostKey(r.Context(), net.JoinHostPort(req.Address, "22"))
	if err != nil {
		status, code, message := classifyEnrollmentSSHError(err)
		writeErrorCode(w, status, code, message)
		return
	}
	writeJSON(w, http.StatusOK, bootstrapProbeResponse{Address: req.Address, Fingerprint: fp, Algorithm: algo})
}

func (a *app) bootstrapDevice(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "administrator role required")
		return
	}
	encKey, err := encryptionKey()
	if errors.Is(err, errEncryptionKeyInvalid) {
		writeErrorCode(w, http.StatusServiceUnavailable, enrollCodeKeyInvalid, "SSH enrollment is disabled: server encryption key is not a base64-encoded 32-byte key")
		return
	}
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, enrollCodeKeyMissing, "SSH enrollment is disabled: server encryption key is not configured")
		return
	}
	var req bootstrapRequest
	if err := decodeJSONBody(w, r, enrollmentBodyLimit, &req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, enrollCodeInvalidRequest, "Invalid request")
		return
	}
	password := []byte(req.Password)
	req.Password = ""
	defer zeroBytes(password)

	req.Name = strings.TrimSpace(req.Name)
	req.Address = strings.TrimSpace(req.Address)
	req.Username = strings.TrimSpace(req.Username)
	req.ExpectedFingerprint = strings.TrimSpace(req.ExpectedFingerprint)
	if req.Username != "" && req.Username != "root" {
		writeErrorCode(w, http.StatusBadRequest, enrollCodeInvalidRequest, "Only the root account is supported; its key file is /etc/dropbear/authorized_keys")
		return
	}
	req.Username = "root"
	if req.Role == "" {
		req.Role = "access_point"
	}
	if req.Name == "" {
		req.Name = req.Address
	}
	if !validDeviceRole(req.Role) || len(req.Name) > maxDeviceFieldLength || len(req.Username) > 128 || len(password) == 0 || len(password) > 512 ||
		!strings.HasPrefix(req.ExpectedFingerprint, "SHA256:") || len(req.ExpectedFingerprint) > 100 {
		writeErrorCode(w, http.StatusBadRequest, enrollCodeInvalidRequest, "Address, password, and the confirmed host-key fingerprint are required")
		return
	}
	if !isPrivateDeviceIP(req.Address) {
		writeErrorCode(w, http.StatusUnprocessableEntity, enrollCodeAddressRejected, "Only private IP addresses are accepted; DNS names, link-local, and public addresses are rejected")
		return
	}

	if blocked, wait := bootstrapLimiter.isBlocked(req.Address, time.Now()); blocked {
		writeLoginRateLimited(w, wait)
		return
	}
	var existing int
	if err := a.db.QueryRow(r.Context(), "SELECT count(*) FROM devices WHERE address=$1", req.Address).Scan(&existing); err != nil {
		writeErrorCode(w, http.StatusInternalServerError, enrollCodeServerError, "Could not check existing devices")
		return
	}
	if existing > 0 {
		writeErrorCode(w, http.StatusConflict, "device_exists", "A device with this address already exists. Remove it first, or use Enroll on its row.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), bootstrapTimeout)
	defer cancel()
	hostPort := net.JoinHostPort(req.Address, "22")

	privatePEM, authorizedLine, err := generateManagementKey()
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, enrollCodeServerError, "Could not generate a management key")
		return
	}
	defer zeroBytes(privatePEM)

	if status, code, message := installManagementKey(ctx, hostPort, req.Username, password, req.ExpectedFingerprint, authorizedLine); code != "" {
		if code == bootstrapCodePasswordBad {
			bootstrapLimiter.recordFailure(req.Address, time.Now())
		}
		writeErrorCode(w, status, code, message)
		return
	}
	bootstrapLimiter.clear(req.Address)
	// The key is on the device now, so finish verifying and saving even if the
	// browser goes away, rather than leaving an untracked key behind.
	ctx, cancel = context.WithTimeout(context.WithoutCancel(r.Context()), bootstrapTimeout)
	defer cancel()
	zeroBytes(password)

	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, enrollCodeServerError, "Could not load the generated key")
		return
	}
	if _, err := verifyDeviceSSH(ctx, hostPort, req.Username, signer, req.ExpectedFingerprint); err != nil {
		status, code, message := classifyEnrollmentSSHError(err)
		writeErrorCode(w, status, code, "The key was installed but key login did not verify: "+message)
		return
	}

	ciphertext, err := encryptSecret(encKey, privatePEM)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, enrollCodeServerError, "Could not protect SSH credential")
		return
	}
	d, err := a.storeBootstrap(ctx, user.ID, req, ciphertext)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, enrollCodeServerError, "The key was installed on the device but could not be saved; remove the openmanage line from /etc/dropbear/authorized_keys and try again")
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// installManagementKey logs in with the password against the pinned host key and
// appends authorizedLine to the device's authorized_keys. It returns a non-empty
// code on failure. The password is only handed to the SSH library.
func installManagementKey(ctx context.Context, hostPort, username string, password []byte, fingerprint, authorizedLine string) (int, string, string) {
	deadline := time.Now().Add(bootstrapTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	sawMismatch := false
	for _, algorithms := range hostKeyAlgorithmFamilies {
		var observed string
		passwordAsked := false
		config := &ssh.ClientConfig{
			User: username,
			Auth: []ssh.AuthMethod{ssh.PasswordCallback(func() (string, error) {
				passwordAsked = true
				return string(password), nil
			})},
			HostKeyCallback:   pinnedHostKeyCallback(fingerprint, &observed),
			HostKeyAlgorithms: algorithms,
			Timeout:           time.Until(deadline),
		}
		dialer := net.Dialer{Deadline: deadline}
		conn, err := dialer.DialContext(ctx, "tcp", hostPort)
		if err != nil {
			return http.StatusBadGateway, enrollCodeUnreachable, "Could not connect to SSH on the device"
		}
		_ = conn.SetDeadline(deadline)
		sshConn, channels, requests, err := ssh.NewClientConn(conn, hostPort, config)
		if err != nil {
			conn.Close()
			switch {
			case errors.Is(err, errHostKeyMismatch):
				sawMismatch = true
				continue
			case strings.Contains(err.Error(), "no common algorithm"):
				continue
			case isSSHTransportFailure(err):
				return http.StatusBadGateway, enrollCodeUnreachable, "Connected to TCP/22, but the SSH handshake did not complete"
			default:
				return http.StatusUnprocessableEntity, bootstrapCodePasswordBad, "The device rejected the username or password"
			}
		}
		client := ssh.NewClient(sshConn, channels, requests)
		defer client.Close()
		stop := context.AfterFunc(ctx, func() { _ = client.Close() })
		defer stop()
		if !passwordAsked {
			return http.StatusUnprocessableEntity, bootstrapCodeNoPassword, "The device let OpenManage in without a password; set a root password first"
		}
		session, err := client.NewSession()
		if err != nil {
			return http.StatusBadGateway, bootstrapCodeInstallError, "Could not open an SSH session to install the key"
		}
		defer session.Close()
		session.Stdin = strings.NewReader(authorizedLine + "\n")
		session.Stderr = &boundedBuffer{limit: operationErrLimit}
		session.Stdout = &boundedBuffer{limit: operationErrLimit}
		if err := session.Run(installKeyCommand); err != nil {
			return http.StatusBadGateway, bootstrapCodeInstallError, "The device refused to store the key"
		}
		return 0, "", ""
	}
	if sawMismatch {
		return http.StatusUnprocessableEntity, enrollCodeHostKeyMismatch, "The device's SSH host key did not match the confirmed fingerprint"
	}
	return http.StatusBadGateway, enrollCodeUnreachable, "The SSH handshake did not complete"
}

// storeBootstrap creates the device, its credential, and the audit event in one
// transaction. The audit details never include the password or key.
func (a *app) storeBootstrap(ctx context.Context, userID string, req bootstrapRequest, ciphertext []byte) (device, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return device{}, err
	}
	defer tx.Rollback(ctx)
	var d device
	err = tx.QueryRow(ctx, `INSERT INTO devices(name,address,model,firmware,status,role,last_seen_at) VALUES($1,$2,'','','online',$3,NOW()) RETURNING id::text,name,address,model,firmware,status,last_seen_at,created_at,FALSE,role,dhcp_host`,
		req.Name, req.Address, req.Role).
		Scan(&d.ID, &d.Name, &d.Address, &d.Model, &d.Firmware, &d.Status, &d.LastSeenAt, &d.CreatedAt, &d.ConfigDrift, &d.Role, &d.DHCPHost)
	if err == nil {
		_, err = tx.Exec(ctx,
			"INSERT INTO device_credentials (device_id, username, private_key_ciphertext, host_key_fingerprint, updated_at) VALUES ($1,$2,$3,$4,NOW())",
			d.ID, req.Username, ciphertext, req.ExpectedFingerprint)
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, userID, "device.bootstrapped", "device", d.ID, map[string]string{
			"username": req.Username, "host_key_fingerprint": req.ExpectedFingerprint, "address": req.Address,
		})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	d.Enrollable = true
	return d, err
}
