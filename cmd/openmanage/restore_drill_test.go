package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
	"testing"
)

// These helpers are driven by scripts/restore-drill.sh and are skipped in normal
// test runs. They exercise the production encryptSecret/decryptSecret/encryptionKey
// so the drill cannot drift from the real credential format.
const drillPlaintext = "restore-drill-fake-private-key-material"

func TestRestoreDrillEncrypt(t *testing.T) {
	out := os.Getenv("OPENMANAGE_DRILL_ENCRYPT_OUT")
	if out == "" {
		t.Skip("only runs under scripts/restore-drill.sh")
	}
	key, err := encryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryptSecret(key, []byte(drillPlaintext))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte(hex.EncodeToString(ct)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreDrillDecrypt(t *testing.T) {
	ctHex := os.Getenv("OPENMANAGE_DRILL_CIPHERTEXT_HEX")
	if ctHex == "" {
		t.Skip("only runs under scripts/restore-drill.sh")
	}
	ct, err := hex.DecodeString(ctHex)
	if err != nil {
		t.Fatal(err)
	}
	key, err := encryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	pt, err := decryptSecret(key, ct)
	if err != nil {
		t.Fatalf("restored credential did not decrypt with the supplied key: %v", err)
	}
	if !bytes.Equal(pt, []byte(drillPlaintext)) {
		t.Fatal("decrypted credential does not match the enrolled plaintext")
	}
	wrong := bytes.Repeat([]byte{0x01}, 32)
	if os.Getenv("OPENMANAGE_ENCRYPTION_KEY") == base64.StdEncoding.EncodeToString(wrong) {
		t.Fatal("negative-control key collided with the real key")
	}
	if _, err := decryptSecret(wrong, ct); err == nil {
		t.Fatal("credential decrypted with the wrong key")
	}
}
