package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"
)

func generatedSigner(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// startTestSSHServer runs an in-process SSH server on a loopback port that accepts
// only authorizedKey, and returns its address and host-key fingerprint.
func startTestSSHServer(t *testing.T, authorizedKey ssh.PublicKey) (string, string) {
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
			go func() {
				defer conn.Close()
				sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer sconn.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "no sessions")
				}
			}()
		}
	}()
	return ln.Addr().String(), ssh.FingerprintSHA256(hostSigner.PublicKey())
}

func TestVerifyDeviceSSHSuccess(t *testing.T) {
	signer, _ := generatedSigner(t)
	addr, hostFP := startTestSSHServer(t, signer.PublicKey())
	got, err := verifyDeviceSSH(context.Background(), addr, "root", signer, hostFP)
	if err != nil {
		t.Fatalf("expected successful verification: %v", err)
	}
	if got != hostFP {
		t.Fatalf("observed fingerprint %q, want %q", got, hostFP)
	}
}

func TestVerifyDeviceSSHRejectsWrongHostKeyFingerprint(t *testing.T) {
	signer, _ := generatedSigner(t)
	addr, _ := startTestSSHServer(t, signer.PublicKey())
	wrong := "SHA256:" + strings.Repeat("A", 43)
	got, err := verifyDeviceSSH(context.Background(), addr, "root", signer, wrong)
	if err == nil {
		t.Fatal("mismatched host key fingerprint must be rejected")
	}
	var dialErr *sshDialError
	if errors.As(err, &dialErr) {
		t.Fatalf("mismatch must not be reported as a dial failure: %v", err)
	}
	if got != "" {
		t.Fatalf("no fingerprint should be returned on failure, got %q", got)
	}
}

func TestVerifyDeviceSSHRejectsUnauthorizedKey(t *testing.T) {
	authorized, _ := generatedSigner(t)
	other, _ := generatedSigner(t)
	addr, hostFP := startTestSSHServer(t, authorized.PublicKey())
	_, err := verifyDeviceSSH(context.Background(), addr, "root", other, hostFP)
	if err == nil {
		t.Fatal("authentication with an unauthorized key must fail")
	}
	var dialErr *sshDialError
	if errors.As(err, &dialErr) {
		t.Fatalf("auth failure must not be reported as a dial failure: %v", err)
	}
}

func TestVerifyDeviceSSHReportsDialFailure(t *testing.T) {
	signer, _ := generatedSigner(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, err = verifyDeviceSSH(context.Background(), addr, "root", signer, "SHA256:x")
	var dialErr *sshDialError
	if !errors.As(err, &dialErr) {
		t.Fatalf("expected dial error, got %v", err)
	}
}

var testSchemaCounter atomic.Int64

// newEnrollmentTestDB connects to OPENMANAGE_TEST_DATABASE_URL, applies the
// embedded migrations into a throwaway schema, and drops it on cleanup.
func newEnrollmentTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("OPENMANAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OPENMANAGE_TEST_DATABASE_URL not set; skipping PostgreSQL-backed enrollment tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("enroll_test_%d_%d", os.Getpid(), testSchemaCounter.Add(1))
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	if err := applyMigrations(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedDeviceAndUser(t *testing.T, db *pgxpool.Pool) (deviceID, userID string) {
	t.Helper()
	ctx := context.Background()
	if err := db.QueryRow(ctx, "INSERT INTO devices(name,address) VALUES('test-router','192.168.1.1') RETURNING id").Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "INSERT INTO users(username,password_hash,role) VALUES('admin','not-a-real-hash','admin') RETURNING id").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	return deviceID, userID
}

func credentialRow(t *testing.T, db *pgxpool.Pool, deviceID string) (username string, ciphertext []byte, fingerprint string, found bool) {
	t.Helper()
	err := db.QueryRow(context.Background(),
		"SELECT username, private_key_ciphertext, host_key_fingerprint FROM device_credentials WHERE device_id=$1", deviceID).
		Scan(&username, &ciphertext, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return username, ciphertext, fingerprint, true
}

func auditCount(t *testing.T, db *pgxpool.Pool, deviceID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		"SELECT count(*) FROM audit_events WHERE action='device.enrolled' AND resource_id=$1", deviceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStoreEnrollmentPersistsCredentialStatusAndAudit(t *testing.T) {
	db := newEnrollmentTestDB(t)
	deviceID, userID := seedDeviceAndUser(t, db)
	if _, err := storeEnrollment(context.Background(), db, deviceID, userID, "root", []byte("cipher-1"), "SHA256:one", `{"username":"root"}`); err != nil {
		t.Fatal(err)
	}
	user, cipher, fp, found := credentialRow(t, db, deviceID)
	if !found || user != "root" || string(cipher) != "cipher-1" || fp != "SHA256:one" {
		t.Fatalf("unexpected credential row: found=%v user=%q fp=%q", found, user, fp)
	}
	var status string
	if err := db.QueryRow(context.Background(), "SELECT status FROM devices WHERE id=$1", deviceID).Scan(&status); err != nil || status != "online" {
		t.Fatalf("device status = %q, err %v; want online", status, err)
	}
	if n := auditCount(t, db, deviceID); n != 1 {
		t.Fatalf("expected one audit event, got %d", n)
	}
}

func TestStoreEnrollmentReenrollReplacesCredentialOnSuccess(t *testing.T) {
	db := newEnrollmentTestDB(t)
	deviceID, userID := seedDeviceAndUser(t, db)
	ctx := context.Background()
	if _, err := storeEnrollment(ctx, db, deviceID, userID, "root", []byte("cipher-1"), "SHA256:one", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := storeEnrollment(ctx, db, deviceID, userID, "admin2", []byte("cipher-2"), "SHA256:two", `{}`); err != nil {
		t.Fatal(err)
	}
	user, cipher, fp, _ := credentialRow(t, db, deviceID)
	if user != "admin2" || string(cipher) != "cipher-2" || fp != "SHA256:two" {
		t.Fatalf("re-enroll did not replace credential: %q %q %q", user, cipher, fp)
	}
	if n := auditCount(t, db, deviceID); n != 2 {
		t.Fatalf("expected two audit events, got %d", n)
	}
}

// A failed re-enrollment (here, the audit insert violating its user foreign key)
// must roll back the credential upsert and keep the previously stored credential.
func TestStoreEnrollmentAuditFailureKeepsOldCredentials(t *testing.T) {
	db := newEnrollmentTestDB(t)
	deviceID, userID := seedDeviceAndUser(t, db)
	ctx := context.Background()
	if _, err := storeEnrollment(ctx, db, deviceID, userID, "root", []byte("cipher-1"), "SHA256:one", `{}`); err != nil {
		t.Fatal(err)
	}
	missingUser := "00000000-0000-4000-8000-000000000000"
	if _, err := storeEnrollment(ctx, db, deviceID, missingUser, "attacker", []byte("cipher-2"), "SHA256:two", `{}`); err == nil {
		t.Fatal("audit insert failure must fail the enrollment")
	}
	user, cipher, fp, found := credentialRow(t, db, deviceID)
	if !found || user != "root" || string(cipher) != "cipher-1" || fp != "SHA256:one" {
		t.Fatalf("old credential not preserved: found=%v user=%q fp=%q", found, user, fp)
	}
	if n := auditCount(t, db, deviceID); n != 1 {
		t.Fatalf("failed enrollment must not add an audit event, got %d", n)
	}
}

func TestStoreEnrollmentFirstEnrollAuditFailureStoresNothing(t *testing.T) {
	db := newEnrollmentTestDB(t)
	deviceID, _ := seedDeviceAndUser(t, db)
	missingUser := "00000000-0000-4000-8000-000000000000"
	if _, err := storeEnrollment(context.Background(), db, deviceID, missingUser, "root", []byte("cipher"), "SHA256:one", `{}`); err == nil {
		t.Fatal("audit insert failure must fail the enrollment")
	}
	if _, _, _, found := credentialRow(t, db, deviceID); found {
		t.Fatal("credential must not persist without its audit event")
	}
	var status string
	if err := db.QueryRow(context.Background(), "SELECT status FROM devices WHERE id=$1", deviceID).Scan(&status); err != nil || status == "online" {
		t.Fatalf("device status must not change on failed enrollment, got %q (err %v)", status, err)
	}
}
