package main

import (
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

func TestRADIUSCertificateChain(t *testing.T) {
	now := time.Now()
	caDER, caKey, err := newRADIUSCA(now)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.MaxPathLenZero {
		t.Fatal("CA certificate is not a path-length-zero CA")
	}
	serverDER, _, err := newRADIUSServerCert(now, ca, caKey)
	if err != nil {
		t.Fatal(err)
	}
	server, err := x509.ParseCertificate(serverDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: radiusServerName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("server certificate does not verify: %v", err)
	}
	// Apple platforms refuse TLS server certificates valid for more than 825 days.
	if days := server.NotAfter.Sub(server.NotBefore).Hours() / 24; days > 825 {
		t.Fatalf("server certificate lifetime %.0f days exceeds 825", days)
	}
	if server.IsCA {
		t.Fatal("server certificate must not be a CA")
	}
}

func TestRADIUSKeySealingBindsPurpose(t *testing.T) {
	_, caKey, err := newRADIUSCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	sealed, err := sealECKey(key, caKey, radiusCAKeyAAD)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := openECKey(key, sealed, radiusCAKeyAAD)
	if err != nil || !opened.Equal(caKey) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := openECKey(key, sealed, radiusServerKeyAAD); err == nil {
		t.Fatal("CA key opened as the server key")
	}
}

func TestCertFingerprintFormat(t *testing.T) {
	fp := certFingerprint([]byte("x"))
	if len(fp) != 95 || strings.Count(fp, ":") != 31 || strings.ToUpper(fp) != fp {
		t.Fatalf("unexpected fingerprint format %q", fp)
	}
}
