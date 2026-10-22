package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// hostKeyAlgorithmFamilies are offered one family at a time until the device presents
// the pinned key. A device usually has several host keys (OpenWrt's dropbear has RSA
// and Ed25519), and a client offering every algorithm at once gets whichever key the
// negotiation prefers, which may not be the one whose fingerprint the operator verified.
// errKeyNotUsed means the device let the session in without public-key authentication.
var errKeyNotUsed = errors.New("device accepted SSH login without public-key authentication")

var hostKeyAlgorithmFamilies = [][]string{
	{ssh.KeyAlgoED25519},
	{ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521},
	{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
}

// dialPinnedSSH opens an SSH client to hostPort only if the device presents a host key
// whose SHA-256 fingerprint equals fingerprint. Network failures return *sshDialError;
// a device that never presents the pinned key returns an error wrapping errHostKeyMismatch.
// The connection deadline is set to deadline and left in place for the caller's session.
func dialPinnedSSH(ctx context.Context, hostPort, username string, signer ssh.Signer, fingerprint string, deadline time.Time) (*ssh.Client, error) {
	var lastErr error
	sawMismatch := false
	for _, algorithms := range hostKeyAlgorithmFamilies {
		var observed string
		// The client always tries the "none" method first, and a router with a blank
		// root password (OpenWrt's first-boot state) accepts it. Record whether the
		// public-key method was reached so such a login is not mistaken for proof
		// that the stored key works.
		keyOffered := false
		config := &ssh.ClientConfig{
			User: username,
			Auth: []ssh.AuthMethod{ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
				keyOffered = true
				return []ssh.Signer{signer}, nil
			})},
			HostKeyCallback:   pinnedHostKeyCallback(fingerprint, &observed),
			HostKeyAlgorithms: algorithms,
			Timeout:           time.Until(deadline),
		}
		dialer := net.Dialer{Deadline: deadline}
		conn, err := dialer.DialContext(ctx, "tcp", hostPort)
		if err != nil {
			return nil, &sshDialError{err}
		}
		_ = conn.SetDeadline(deadline)
		sshConn, channels, requests, err := ssh.NewClientConn(conn, hostPort, config)
		if err == nil {
			client := ssh.NewClient(sshConn, channels, requests)
			if !keyOffered {
				_ = client.Close()
				return nil, errKeyNotUsed
			}
			return client, nil
		}
		conn.Close()
		lastErr = err
		switch {
		case errors.Is(err, errHostKeyMismatch):
			sawMismatch = true
		case strings.Contains(err.Error(), "no common algorithm"):
			// The device has no host key in this family; try the next one.
		default:
			// Authentication and other handshake failures happen after the host key
			// was accepted, so another family cannot help.
			return nil, err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if sawMismatch {
		return nil, errHostKeyMismatch
	}
	return nil, lastErr
}
