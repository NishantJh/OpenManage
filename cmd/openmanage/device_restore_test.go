package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestArchiveAuthorizesKey(t *testing.T) {
	ours, other := testSigner(t), testSigner(t)
	line := func(s ssh.Signer, comment string) string {
		return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + " " + comment + "\n"
	}
	both := []byte("# comment\n" + line(other, "other") + line(ours, "openmanage"))
	if !archiveAuthorizesKey(both, ours.PublicKey()) {
		t.Fatal("key listed second must be found")
	}
	if archiveAuthorizesKey([]byte(line(other, "other")), ours.PublicKey()) {
		t.Fatal("another key must not count")
	}
	for _, in := range []string{"", "garbage\n", "ssh-ed25519 notbase64 x\n"} {
		if archiveAuthorizesKey([]byte(in), ours.PublicKey()) {
			t.Fatalf("%q must not authorize", in)
		}
	}
}

func TestRestorePreflight(t *testing.T) {
	signer := testSigner(t)
	target := deviceTarget{Fingerprint: "SHA256:pinned", signer: signer}
	keys := ssh.MarshalAuthorizedKey(signer.PublicKey())
	b := deviceBackup{BoardName: "tplink,c7", Release: "24.10.4", HardwareID: "aa:bb:cc:dd:ee:ff"}
	good := archiveInfo{AuthorizedKeys: keys, HostKeyFingerprint: "SHA256:pinned"}
	live := backupBoard{Name: "tplink,c7", Release: "24.10.4"}
	for name, tc := range map[string]struct {
		info     archiveInfo
		board    backupBoard
		hw       string
		accept   bool
		wantCode string
	}{
		"ok":                  {good, live, "aa:bb:cc:dd:ee:ff", false, ""},
		"ok without hardware": {good, live, "", false, ""},
		"board":               {good, backupBoard{Name: "other", Release: "24.10.4"}, "", false, "board_mismatch"},
		"hardware":            {good, live, "11:22:33:44:55:66", false, "hardware_mismatch"},
		"release":             {good, backupBoard{Name: "tplink,c7", Release: "23.05.5"}, "", false, "release_mismatch"},
		"release accepted":    {good, backupBoard{Name: "tplink,c7", Release: "23.05.5"}, "", true, ""},
		"key removed":         {archiveInfo{AuthorizedKeys: []byte("\n"), HostKeyFingerprint: "SHA256:pinned"}, live, "", false, "lockout_key"},
		"no authorized_keys":  {archiveInfo{HostKeyFingerprint: "SHA256:pinned"}, live, "", false, "lockout_key"},
		"host key differs":    {archiveInfo{AuthorizedKeys: keys, HostKeyFingerprint: "SHA256:other"}, live, "", false, "lockout_host_key"},
		"no host key":         {archiveInfo{AuthorizedKeys: keys}, live, "", false, "lockout_host_key"},
	} {
		t.Run(name, func(t *testing.T) {
			err := restorePreflight(b, tc.info, target, tc.board, tc.hw, tc.accept)
			switch {
			case tc.wantCode == "" && err != nil:
				t.Fatalf("unexpected refusal: %v", err)
			case tc.wantCode != "" && (err == nil || err.code != tc.wantCode):
				t.Fatalf("got %v, want code %s", err, tc.wantCode)
			}
		})
	}
}

func TestRestoreCommandsAreFixedAdminOnlyWrites(t *testing.T) {
	for _, op := range restoreOperations {
		if op.Class != operationWrite || op.allows("operator") || op.allows("readonly") || !op.allows("admin") {
			t.Fatalf("%s must be an admin-only write", op.Name)
		}
		if !strings.Contains(op.command, restoreArchivePath) || strings.Contains(op.command, "$") {
			t.Fatalf("%s must use the fixed path and no variables: %s", op.Name, op.command)
		}
	}
	if !strings.HasPrefix(opRestoreUpload.command, "umask 077;") {
		t.Fatal("upload must create a private file")
	}
}

func TestRestoreEndpointRequiresAdminAndConfirmation(t *testing.T) {
	dev, backup := "550e8400-e29b-41d4-a716-446655440000", "650e8400-e29b-41d4-a716-446655440000"
	call := func(role, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/devices/"+dev+"/backups/"+backup+"/restore", strings.NewReader(body))
		req.SetPathValue("id", dev)
		req.SetPathValue("backup", backup)
		if role != "" {
			req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: role}))
		}
		rec := httptest.NewRecorder()
		(&app{}).restoreDeviceBackup(rec, req)
		return rec.Code
	}
	if got := call("", `{}`); got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", got)
	}
	for _, role := range []string{"operator", "readonly"} {
		if got := call(role, `{"confirm_name":"x"}`); got != http.StatusForbidden {
			t.Fatalf("%s: %d", role, got)
		}
	}
	if got := call("admin", `{"unknown":1}`); got != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", got)
	}
}
