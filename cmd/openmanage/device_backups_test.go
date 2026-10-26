package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	link     string
	mtime    time.Time
}

func buildArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: flag, Mode: 0o600, Linkname: e.link, ModTime: e.mtime}
		if flag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInspectBackupArchiveDigestCoversPermissions(t *testing.T) {
	a := buildArchive(t, tarEntry{name: "etc/shadow", body: "x"})
	modeChanged := func() []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		x := tar.NewWriter(w)
		_ = x.WriteHeader(&tar.Header{Name: "etc/shadow", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
		_, _ = x.Write([]byte("x"))
		_ = x.Close()
		_ = w.Close()
		return b.Bytes()
	}()
	ia, _ := inspectBackupArchive(a)
	ib, _ := inspectBackupArchive(modeChanged)
	if ia.ContentDigest == ib.ContentDigest {
		t.Fatal("a permission change must change the digest")
	}
}

func dropbearKeyFile(pub ed25519.PublicKey, priv ed25519.PrivateKey) string {
	var b []byte
	for _, f := range [][]byte{[]byte("ssh-ed25519"), append(append([]byte{}, priv.Seed()...), pub...)} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return string(b)
}

func TestInspectBackupArchiveReadsHostKeyAndAuthorizedKeys(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	archive := buildArchive(t,
		tarEntry{name: "etc/", typeflag: tar.TypeDir},
		tarEntry{name: "./etc/config/network", body: "config interface lan\n"},
		tarEntry{name: "etc/dropbear/dropbear_ed25519_host_key", body: dropbearKeyFile(pub, priv)},
		tarEntry{name: "etc/dropbear/authorized_keys", body: "ssh-ed25519 AAAA test\n"},
	)
	info, err := inspectBackupArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	if info.HostKeyFingerprint != ssh.FingerprintSHA256(sshPub) {
		t.Fatalf("host key fingerprint = %q", info.HostKeyFingerprint)
	}
	if string(info.AuthorizedKeys) != "ssh-ed25519 AAAA test\n" || info.Files != 4 {
		t.Fatalf("unexpected info: files=%d keys=%q", info.Files, info.AuthorizedKeys)
	}
}

func TestInspectBackupArchiveDigestIgnoresTimestamps(t *testing.T) {
	a := buildArchive(t, tarEntry{name: "etc/config/system", body: "x", mtime: time.Unix(1000, 0)})
	b := buildArchive(t, tarEntry{name: "etc/config/system", body: "x", mtime: time.Unix(2000, 0)})
	c := buildArchive(t, tarEntry{name: "etc/config/system", body: "y", mtime: time.Unix(1000, 0)})
	ia, _ := inspectBackupArchive(a)
	ib, _ := inspectBackupArchive(b)
	ic, _ := inspectBackupArchive(c)
	if bytes.Equal(a, b) || ia.ContentDigest == "" {
		t.Fatal("test archives should differ byte-wise")
	}
	if ia.ContentDigest != ib.ContentDigest {
		t.Fatal("digest must not depend on timestamps")
	}
	if ia.ContentDigest == ic.ContentDigest {
		t.Fatal("digest must depend on content")
	}
}

func TestInspectBackupArchiveRejectsUnsafeInput(t *testing.T) {
	many := make([]tarEntry, archiveMaxEntries+1)
	for i := range many {
		many[i] = tarEntry{name: "etc/f" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)), body: "x"}
	}
	for name, archive := range map[string][]byte{
		"empty":         nil,
		"not gzip":      []byte("plain text"),
		"no entries":    buildArchive(t),
		"absolute":      buildArchive(t, tarEntry{name: "/etc/passwd", body: "x"}),
		"parent":        buildArchive(t, tarEntry{name: "etc/../../x", body: "x"}),
		"symlink":       buildArchive(t, tarEntry{name: "etc/l", typeflag: tar.TypeSymlink, link: "/etc/shadow"}),
		"hardlink":      buildArchive(t, tarEntry{name: "etc/h", typeflag: tar.TypeLink, link: "etc/x"}),
		"device":        buildArchive(t, tarEntry{name: "dev/x", typeflag: tar.TypeChar}),
		"too many":      buildArchive(t, many...),
		"entry too big": buildArchive(t, tarEntry{name: "etc/big", body: strings.Repeat("a", archiveMaxEntrySize+1)}),
		"bomb": buildArchive(t,
			tarEntry{name: "a", body: strings.Repeat("\x00", archiveMaxEntrySize)},
			tarEntry{name: "b", body: strings.Repeat("\x00", archiveMaxEntrySize)},
			tarEntry{name: "c", body: strings.Repeat("\x00", archiveMaxEntrySize)},
			tarEntry{name: "d", body: strings.Repeat("\x00", archiveMaxEntrySize)},
			tarEntry{name: "e", body: "x"}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := inspectBackupArchive(archive); err == nil {
				t.Fatal("archive must be rejected")
			}
		})
	}
}

func TestDropbearFingerprintRejectsMalformedKeys(t *testing.T) {
	for _, in := range []string{"", "abc", dropbearKeyFile(make([]byte, 31), make(ed25519.PrivateKey, 64)), strings.Repeat("\xff", 40)} {
		if dropbearEd25519Fingerprint([]byte(in)) != "" {
			t.Fatalf("malformed key %q must yield no fingerprint", in)
		}
	}
}

func TestBackupCiphertextIsBoundToDeviceAndBackup(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	dev1, dev2 := "550e8400-e29b-41d4-a716-446655440000", "550e8400-e29b-41d4-a716-446655440001"
	b1, b2 := "650e8400-e29b-41d4-a716-446655440000", "650e8400-e29b-41d4-a716-446655440001"
	ct, err := sealSecret(key, []byte("archive"), backupAAD(dev1, b1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSecret(key, ct, backupAAD(dev1, b1)); err != nil {
		t.Fatalf("own device and backup must decrypt: %v", err)
	}
	for name, aad := range map[string]string{
		"other device": backupAAD(dev2, b1), "other backup": backupAAD(dev1, b2),
		"snapshot": configSnapshotAAD(dev1), "ssh key": sshKeyAAD,
	} {
		if _, err := openSecret(key, ct, aad); err == nil {
			t.Fatalf("backup must not decrypt as %s", name)
		}
	}
}

func TestBackupDigestIsKeyedAndSeparateFromConfigDigest(t *testing.T) {
	k1, k2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	d1, d2 := backupDigest(k1, "x"), backupDigest(k1, "x")
	if d1 != d2 || d1 == backupDigest(k2, "x") {
		t.Fatal("digest must be stable and keyed")
	}
	if d1 == configDigest(k1, "x") {
		t.Fatal("backup and config digests must be unrelated")
	}
}

func TestNewBackupIDIsValidUUID(t *testing.T) {
	a, err := newBackupID()
	if err != nil || !validUUID(a) {
		t.Fatalf("id %q err %v", a, err)
	}
	if b, _ := newBackupID(); a == b {
		t.Fatal("ids must be unique")
	}
}

func TestParseBackupBoard(t *testing.T) {
	got := parseBackupBoard(`{"board_name":"tplink,archer-c7-v2","release":{"version":"24.10.4","target":"ath79/generic"}}`)
	if got != (backupBoard{Name: "tplink,archer-c7-v2", Target: "ath79/generic", Release: "24.10.4"}) {
		t.Fatalf("got %+v", got)
	}
	if parseBackupBoard("not json") != (backupBoard{}) {
		t.Fatal("bad output must yield an empty board")
	}
}

func TestDeviceJobGuardAllowsOneJobPerDevice(t *testing.T) {
	var g deviceJobs
	if !g.tryStart("a") || g.tryStart("a") || !g.tryStart("b") {
		t.Fatal("one job per device, independent across devices")
	}
	g.finish("a")
	if !g.tryStart("a") {
		t.Fatal("device must be free after finish")
	}
}

func TestBackupEndpointRoles(t *testing.T) {
	dev, backup := "550e8400-e29b-41d4-a716-446655440000", "650e8400-e29b-41d4-a716-446655440000"
	base := "/api/devices/" + dev + "/backups"
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
		denied  []string
	}{
		{"create", (&app{}).createDeviceBackup, http.MethodPost, base, []string{"readonly"}},
		{"download", (&app{}).downloadDeviceBackup, http.MethodPost, base + "/" + backup + "/download", []string{"operator", "readonly"}},
		{"delete", (&app{}).deleteDeviceBackup, http.MethodDelete, base + "/" + backup, []string{"operator", "readonly"}},
	} {
		for _, role := range tc.denied {
			t.Run(tc.name+"/"+role, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, tc.path, nil)
				req.SetPathValue("id", dev)
				req.SetPathValue("backup", backup)
				req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: role}))
				rec := httptest.NewRecorder()
				tc.handler(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", rec.Code)
				}
			})
		}
	}
	t.Run("unauthenticated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, base+"/"+backup+"/download", nil)
		req.SetPathValue("id", dev)
		req.SetPathValue("backup", backup)
		rec := httptest.NewRecorder()
		(&app{}).downloadDeviceBackup(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("download needs a password", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, base+"/"+backup+"/download", strings.NewReader(`{}`))
		req.SetPathValue("id", dev)
		req.SetPathValue("backup", backup)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: "admin"}))
		rec := httptest.NewRecorder()
		(&app{}).downloadDeviceBackup(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestBackupOperationIsFixedAndReadOnly(t *testing.T) {
	if backupOperation.Class != operationRead || backupOperation.OutputLimit != 4<<20 {
		t.Fatal("backup is a read operation capped at 4 MiB")
	}
	if backupOperation.allows("readonly") || !backupOperation.allows("operator") || !backupOperation.allows("admin") {
		t.Fatal("create is admin and operator only")
	}
	if !strings.Contains(backupOperation.command, "2>/dev/null") || !strings.Contains(backupOperation.command, "umask 077") {
		t.Fatal("stderr must be dropped and files private")
	}
}
