package main

import (
	"bytes"
	"encoding/base64"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestEncryptSecretRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	plain := []byte("private key material must not be stored as plaintext")
	ciphertext, err := encryptSecret(key, plain)
	if err != nil { t.Fatal(err) }
	if bytes.Contains(ciphertext, plain) { t.Fatal("ciphertext contains plaintext") }
	got, err := decryptSecret(key, ciphertext)
	if err != nil { t.Fatal(err) }
	if !bytes.Equal(got, plain) { t.Fatal("decrypted value differs from original") }
}

func TestEncryptSecretUsesFreshNonce(t *testing.T) {
	key := bytes.Repeat([]byte{0x24}, 32)
	plain := []byte("same secret")
	first, err := encryptSecret(key, plain)
	if err != nil { t.Fatal(err) }
	second, err := encryptSecret(key, plain)
	if err != nil { t.Fatal(err) }
	if bytes.Equal(first, second) { t.Fatal("encrypting same secret twice should use fresh nonces") }
}

func TestDecryptSecretRejectsTampering(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	ciphertext, err := encryptSecret(key, []byte("secret"))
	if err != nil { t.Fatal(err) }
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := decryptSecret(key, ciphertext); err == nil { t.Fatal("tampered ciphertext must fail authentication") }
}

func TestPrivateDeviceIP(t *testing.T) {
	for _, address := range []string{"192.168.1.1", "10.0.0.2", "172.16.0.1", "fd00::1"} {
		if !isPrivateDeviceIP(address) { t.Errorf("expected %q to be permitted", address) }
	}
	for _, address := range []string{"example.com", "8.8.8.8", "1.1.1.1", "2001:4860:4860::8888", "192.168.1.1:22", "169.254.1.1"} {
		if isPrivateDeviceIP(address) { t.Errorf("expected %q to be rejected", address) }
	}
}

func TestPinnedHostKeyCallback(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil { t.Fatal(err) }
	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil { t.Fatal(err) }
	fingerprint := ssh.FingerprintSHA256(publicKey)
	var observed string
	callback := pinnedHostKeyCallback(fingerprint, &observed)
	if err := callback("router", &net.TCPAddr{}, publicKey); err != nil { t.Fatalf("expected pinned key to pass: %v", err) }
	if observed != fingerprint { t.Fatalf("observed fingerprint %q, want %q", observed, fingerprint) }
	wrong := pinnedHostKeyCallback("SHA256:not-the-device-key", &observed)
	if err := wrong("router", &net.TCPAddr{}, publicKey); err == nil { t.Fatal("mismatched host key must be rejected") }
}

func TestEncryptionKeyConfiguration(t *testing.T) {
	t.Setenv("OPENMANAGE_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32)))
	got, err := encryptionKey()
	if err != nil || len(got) != 32 { t.Fatalf("expected configured 32-byte key, got %d bytes and error %v", len(got), err) }
	t.Setenv("OPENMANAGE_ENCRYPTION_KEY", "bad")
	if _, err := encryptionKey(); err == nil { t.Fatal("invalid encryption key must fail closed") }
	t.Setenv("OPENMANAGE_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte("too short")))
	if _, err := encryptionKey(); err == nil { t.Fatal("short encryption key must fail closed") }
}
