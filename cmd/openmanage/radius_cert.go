package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// radiusServerName is the name in the server certificate. Clients that validate the
// server are told to expect it together with the CA (docs/radius-design.md).
const radiusServerName = "radius.openmanage.home.arpa"

const (
	radiusCALifetime     = 10 * 365 * 24 * time.Hour
	radiusServerLifetime = 730 * 24 * time.Hour
	// The server certificate is reissued at startup once it is this close to expiry.
	radiusRenewBefore = 30 * 24 * time.Hour
)

const (
	radiusCAKeyAAD     = "openmanage:radius-ca-key:v1"
	radiusServerKeyAAD = "openmanage:radius-server-key:v1"
)

// radiusCertInfo is what administrators see; it carries no key material.
type radiusCertInfo struct {
	CAFingerprint  string    `json:"ca_fingerprint_sha256"`
	CANotAfter     time.Time `json:"ca_not_after"`
	ServerName     string    `json:"server_name"`
	ServerNotAfter time.Time `json:"server_not_after"`
}

func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	digits := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(sum))
	for i := 0; i < len(digits); i += 2 {
		parts = append(parts, digits[i:i+2])
	}
	return strings.Join(parts, ":")
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func newRADIUSCA(now time.Time) (der []byte, key *ecdsa.PrivateKey, err error) {
	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "OpenManage RADIUS CA " + serial.Text(16)[:8], Organization: []string{"OpenManage"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(radiusCALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	return der, key, err
}

func newRADIUSServerCert(now time.Time, ca *x509.Certificate, caKey *ecdsa.PrivateKey) (der []byte, key *ecdsa.PrivateKey, err error) {
	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	notAfter := now.Add(radiusServerLifetime)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: radiusServerName, Organization: []string{"OpenManage"}},
		DNSNames:     []string{radiusServerName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	return der, key, err
}

// ensureRADIUSCertificate loads the RADIUS TLS identity, creating the CA and server
// certificate the first time and reissuing the server certificate near expiry.
func ensureRADIUSCertificate(ctx context.Context, db *pgxpool.Pool, key []byte) (tls.Certificate, radiusCertInfo, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return tls.Certificate{}, radiusCertInfo{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE radius_certificate IN EXCLUSIVE MODE`); err != nil {
		return tls.Certificate{}, radiusCertInfo{}, err
	}
	now := time.Now()
	var caDER, caSealed, serverDER, serverSealed []byte
	err = tx.QueryRow(ctx, `SELECT ca_cert, ca_key_ciphertext, server_cert, server_key_ciphertext
		FROM radius_certificate WHERE id=1`).Scan(&caDER, &caSealed, &serverDER, &serverSealed)
	action := ""
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		var caKey *ecdsa.PrivateKey
		var ca *x509.Certificate
		if caDER, caKey, err = newRADIUSCA(now); err == nil {
			ca, err = x509.ParseCertificate(caDER)
		}
		if err == nil {
			serverDER, serverSealed, err = issueRADIUSServerCert(now, ca, caKey, key)
		}
		if err == nil {
			caSealed, err = sealECKey(key, caKey, radiusCAKeyAAD)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO radius_certificate(id, ca_cert, ca_key_ciphertext, server_cert, server_key_ciphertext)
				VALUES(1,$1,$2,$3,$4)`, caDER, caSealed, serverDER, serverSealed)
		}
		action = "radius.certificate_create"
	case err == nil:
		var server *x509.Certificate
		if server, err = x509.ParseCertificate(serverDER); err == nil && server.NotAfter.Sub(now) < radiusRenewBefore {
			var ca *x509.Certificate
			var caKey *ecdsa.PrivateKey
			if ca, err = x509.ParseCertificate(caDER); err == nil {
				caKey, err = openECKey(key, caSealed, radiusCAKeyAAD)
			}
			if err == nil {
				serverDER, serverSealed, err = issueRADIUSServerCert(now, ca, caKey, key)
			}
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE radius_certificate SET server_cert=$1, server_key_ciphertext=$2, renewed_at=NOW() WHERE id=1`,
					serverDER, serverSealed)
			}
			action = "radius.certificate_renew"
		}
	}
	if err != nil {
		return tls.Certificate{}, radiusCertInfo{}, err
	}
	info, err := certInfo(caDER, serverDER)
	if err != nil {
		return tls.Certificate{}, radiusCertInfo{}, err
	}
	if action != "" {
		err = insertAuditEvent(ctx, tx, "", action, "radius", "certificate", map[string]string{
			"ca_fingerprint_sha256": info.CAFingerprint, "server_not_after": info.ServerNotAfter.UTC().Format(time.RFC3339),
		})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return tls.Certificate{}, radiusCertInfo{}, err
	}
	serverKey, err := openECKey(key, serverSealed, radiusServerKeyAAD)
	if err != nil {
		return tls.Certificate{}, radiusCertInfo{}, err
	}
	return tls.Certificate{Certificate: [][]byte{serverDER, caDER}, PrivateKey: serverKey}, info, nil
}

func issueRADIUSServerCert(now time.Time, ca *x509.Certificate, caKey *ecdsa.PrivateKey, key []byte) ([]byte, []byte, error) {
	der, serverKey, err := newRADIUSServerCert(now, ca, caKey)
	if err != nil {
		return nil, nil, err
	}
	sealed, err := sealECKey(key, serverKey, radiusServerKeyAAD)
	return der, sealed, err
}

func sealECKey(key []byte, k *ecdsa.PrivateKey, aad string) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return sealSecret(key, der, aad)
}

func openECKey(key, sealed []byte, aad string) (*ecdsa.PrivateKey, error) {
	der, err := openSecret(key, sealed, aad)
	if err != nil {
		return nil, err
	}
	return x509.ParseECPrivateKey(der)
}

func certInfo(caDER, serverDER []byte) (radiusCertInfo, error) {
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return radiusCertInfo{}, err
	}
	server, err := x509.ParseCertificate(serverDER)
	if err != nil {
		return radiusCertInfo{}, err
	}
	return radiusCertInfo{CAFingerprint: certFingerprint(caDER), CANotAfter: ca.NotAfter,
		ServerName: radiusServerName, ServerNotAfter: server.NotAfter}, nil
}

// loadRADIUSCertInfo returns the stored certificate details, or nil if RADIUS has
// never been enabled.
func loadRADIUSCertInfo(ctx context.Context, db *pgxpool.Pool) (*radiusCertInfo, []byte, error) {
	var caDER, serverDER []byte
	err := db.QueryRow(ctx, `SELECT ca_cert, server_cert FROM radius_certificate WHERE id=1`).Scan(&caDER, &serverDER)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	info, err := certInfo(caDER, serverDER)
	if err != nil {
		return nil, nil, err
	}
	return &info, caDER, nil
}

// downloadRADIUSCA serves the CA certificate so it can be installed on clients.
func (a *app) downloadRADIUSCA(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	info, caDER, err := loadRADIUSCertInfo(r.Context(), a.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the RADIUS certificate")
		return
	}
	if info == nil {
		writeError(w, http.StatusNotFound, "RADIUS has not been enabled yet, so there is no certificate")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="openmanage-radius-ca.pem"`)
	pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}
