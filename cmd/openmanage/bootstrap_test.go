package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// startPasswordSSHServer accepts one password and records the stdin of each exec.
func startPasswordSSHServer(t *testing.T, password string) (addr, fingerprint string, received chan string) {
	t.Helper()
	signer, _ := generatedSigner(t)
	config := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if string(p) == password {
				return nil, nil
			}
			return nil, io.ErrUnexpectedEOF
		},
	}
	config.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	received = make(chan string, 4)
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
					c, creqs, err := ch.Accept()
					if err != nil {
						continue
					}
					go func() {
						for rq := range creqs {
							if rq.Type == "exec" {
								_ = rq.Reply(true, nil)
								data, _ := io.ReadAll(c)
								received <- string(data)
								_, _ = c.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
								c.Close()
								return
							}
							_ = rq.Reply(false, nil)
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey()), received
}

func TestInstallManagementKeyDeliversKeyOverStdin(t *testing.T) {
	addr, fp, received := startPasswordSSHServer(t, "hunter2")
	_, line, err := generateManagementKey()
	if err != nil {
		t.Fatal(err)
	}
	if status, code, msg := installManagementKey(context.Background(), addr, "root", []byte("hunter2"), fp, line); code != "" {
		t.Fatalf("install failed: %d %s %s", status, code, msg)
	}
	if got := strings.TrimSpace(<-received); got != line || !strings.HasPrefix(got, "ssh-ed25519 ") {
		t.Fatalf("server received %q", got)
	}
}

func TestInstallManagementKeyFailures(t *testing.T) {
	addr, fp, _ := startPasswordSSHServer(t, "right")
	_, line, _ := generateManagementKey()
	if _, code, _ := installManagementKey(context.Background(), addr, "root", []byte("wrong"), fp, line); code != bootstrapCodePasswordBad {
		t.Fatalf("wrong password code = %q", code)
	}
	if _, code, _ := installManagementKey(context.Background(), addr, "root", []byte("right"), "SHA256:other", line); code != enrollCodeHostKeyMismatch {
		t.Fatalf("pin mismatch code = %q", code)
	}
}

func TestProbeHostKeyReturnsFingerprint(t *testing.T) {
	addr, fp, _ := startPasswordSSHServer(t, "x")
	got, algo, err := probeHostKey(context.Background(), addr)
	if err != nil || got != fp || algo != ssh.KeyAlgoED25519 {
		t.Fatalf("got %q %q %v want %q", got, algo, err, fp)
	}
}

func TestGenerateManagementKeyParses(t *testing.T) {
	pemBytes, line, err := generateManagementKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || string(pub.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Fatalf("public key does not match private key: %v", err)
	}
}

func TestBootstrapRequiresAdminAndValidInput(t *testing.T) {
	do := func(role, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/device-bootstrap", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: role}))
		rec := httptest.NewRecorder()
		(&app{}).bootstrapDevice(rec, req)
		return rec.Code
	}
	if c := do("operator", "{}"); c != http.StatusForbidden {
		t.Fatalf("operator got %d", c)
	}
	t.Setenv("OPENMANAGE_ENCRYPTION_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if c := do("admin", `{"address":"192.168.1.2","password":"","expected_fingerprint":"SHA256:x"}`); c != http.StatusBadRequest {
		t.Fatalf("empty password got %d", c)
	}
	if c := do("admin", `{"address":"8.8.8.8","password":"p","expected_fingerprint":"SHA256:x"}`); c != http.StatusUnprocessableEntity {
		t.Fatalf("public address got %d", c)
	}
}

func TestStoreBootstrapEnrollsExistingDevice(t *testing.T) {
	db := newEnrollmentTestDB(t)
	deviceID, userID := seedDeviceAndUser(t, db)
	a := &app{db: db}
	d, err := a.storeBootstrap(context.Background(), userID, bootstrapRequest{DeviceID: deviceID, Name: "test-router", Address: "192.168.1.1", Role: "router", Username: "root", ExpectedFingerprint: "SHA256:x"}, []byte("sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != deviceID || d.Role != "router" || !d.Enrollable || !d.HasKey {
		t.Fatalf("got %+v", d)
	}
	var count int
	if err := db.QueryRow(context.Background(), "SELECT count(*) FROM devices").Scan(&count); err != nil || count != 1 {
		t.Fatalf("device rows = %d, %v; want the existing row updated, not a second one", count, err)
	}
	if user, _, fp, found := credentialRow(t, db, deviceID); !found || user != "root" || fp != "SHA256:x" {
		t.Fatalf("credential = %q %q %v", user, fp, found)
	}
	// A second bootstrap must not replace an existing credential.
	if _, err := a.storeBootstrap(context.Background(), userID, bootstrapRequest{DeviceID: deviceID, Address: "192.168.1.1", Role: "router", Username: "root", ExpectedFingerprint: "SHA256:y"}, []byte("other")); err == nil {
		t.Fatal("second bootstrap replaced the credential")
	}
	if _, _, fp, _ := credentialRow(t, db, deviceID); fp != "SHA256:x" {
		t.Fatalf("fingerprint changed to %q", fp)
	}
}
