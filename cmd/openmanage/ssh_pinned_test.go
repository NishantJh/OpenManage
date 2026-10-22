package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startMultiKeySSHServer mimics OpenWrt's dropbear, which serves both an RSA and an
// Ed25519 host key, and returns the address and both fingerprints.
func startMultiKeySSHServer(t *testing.T, authorizedKey ssh.PublicKey) (addr, rsaFP, ed25519FP string) {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	edSigner, _ := generatedSigner(t)
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorizedKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	config.AddHostKey(rsaSigner)
	config.AddHostKey(edSigner)
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
	return ln.Addr().String(), ssh.FingerprintSHA256(rsaSigner.PublicKey()), ssh.FingerprintSHA256(edSigner.PublicKey())
}

func TestDialPinnedSSHMatchesEitherHostKeyOfMultiKeyDevice(t *testing.T) {
	signer, _ := generatedSigner(t)
	addr, rsaFP, edFP := startMultiKeySSHServer(t, signer.PublicKey())
	for name, fp := range map[string]string{"ed25519": edFP, "rsa": rsaFP} {
		t.Run(name, func(t *testing.T) {
			client, err := dialPinnedSSH(context.Background(), addr, "root", signer, fp, time.Now().Add(5*time.Second))
			if err != nil {
				t.Fatalf("pinning the %s host key must succeed: %v", name, err)
			}
			client.Close()
		})
	}
}

func TestDialPinnedSSHReportsMismatchWhenNoHostKeyMatches(t *testing.T) {
	signer, _ := generatedSigner(t)
	addr, _, _ := startMultiKeySSHServer(t, signer.PublicKey())
	_, err := dialPinnedSSH(context.Background(), addr, "root", signer, "SHA256:"+strings.Repeat("A", 43), time.Now().Add(5*time.Second))
	if !errors.Is(err, errHostKeyMismatch) {
		t.Fatalf("expected host-key mismatch, got %v", err)
	}
}

func TestDialPinnedSSHStopsOnAuthFailure(t *testing.T) {
	authorized, _ := generatedSigner(t)
	other, _ := generatedSigner(t)
	addr, _, edFP := startMultiKeySSHServer(t, authorized.PublicKey())
	_, err := dialPinnedSSH(context.Background(), addr, "root", other, edFP, time.Now().Add(5*time.Second))
	if err == nil || errors.Is(err, errHostKeyMismatch) {
		t.Fatalf("expected an authentication failure, got %v", err)
	}
	if _, code, _ := classifyEnrollmentSSHError(err); code != enrollCodeAuthFailed {
		t.Fatalf("auth failure classified as %q", code)
	}
}

// A router with a blank root password accepts the "none" method before the key is
// ever offered; that must not count as a verified credential.
func TestDialPinnedSSHRejectsLoginWithoutKeyAuthentication(t *testing.T) {
	signer, _ := generatedSigner(t)
	hostSigner, _ := generatedSigner(t)
	config := &ssh.ServerConfig{NoClientAuth: true}
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
	_, err = dialPinnedSSH(context.Background(), ln.Addr().String(), "root", signer, ssh.FingerprintSHA256(hostSigner.PublicKey()), time.Now().Add(5*time.Second))
	if !errors.Is(err, errKeyNotUsed) {
		t.Fatalf("expected errKeyNotUsed, got %v", err)
	}
	if _, code, _ := classifyEnrollmentSSHError(err); code != enrollCodeKeyNotVerified {
		t.Fatalf("classified as %q", code)
	}
}
