package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

func avp(code uint32, flags byte, vendor uint32, value []byte) []byte {
	header := 8
	if flags&avpFlagVendor != 0 {
		header = 12
	}
	b := make([]byte, header, header+len(value)+3)
	binary.BigEndian.PutUint32(b[:4], code)
	length := header + len(value)
	b[4], b[5], b[6], b[7] = flags, byte(length>>16), byte(length>>8), byte(length)
	if header == 12 {
		binary.BigEndian.PutUint32(b[8:12], vendor)
	}
	b = append(b, value...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func TestParseInnerPAP(t *testing.T) {
	good := append(avp(avpUserName, avpFlagMandator, 0, []byte("alice")),
		avp(avpUserPassword, avpFlagMandator, 0, []byte("correct horse\x00\x00\x00"))...)
	user, pass, err := parseInnerPAP(good)
	if err != nil || user != "alice" || pass != "correct horse" {
		t.Fatalf("got %q %q %v", user, pass, err)
	}
	// An optional unknown AVP is ignored; a mandatory one (MS-CHAP2-Response) is refused.
	optional := append(good, avp(99, 0, 0, []byte("x"))...)
	if _, _, err := parseInnerPAP(optional); err != nil {
		t.Fatalf("optional AVP refused: %v", err)
	}
	mschap := append(avp(avpUserName, avpFlagMandator, 0, []byte("alice")),
		avp(25, avpFlagVendor|avpFlagMandator, microsoftVendorID, make([]byte, 50))...)
	if _, _, err := parseInnerPAP(mschap); err == nil {
		t.Fatal("MS-CHAP-V2 inner method accepted")
	}
	for _, bad := range [][]byte{nil, good[:7], avp(avpUserName, 0, 0, []byte("alice")), {0, 0, 0, 1, 0, 0, 0, 200}} {
		if _, _, err := parseInnerPAP(bad); err == nil {
			t.Fatalf("malformed inner data accepted: %x", bad)
		}
	}
}

func TestEAPPacketRoundTrip(t *testing.T) {
	p := eapPacket{code: eapCodeRequest, id: 7, typ: eapTypeTTLS, data: []byte{ttlsFlagStart}}
	got, err := parseEAP(p.encode())
	if err != nil || got.code != p.code || got.id != 7 || got.typ != eapTypeTTLS || !bytes.Equal(got.data, p.data) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if b := (eapPacket{code: eapCodeSuccess, id: 9}).encode(); !bytes.Equal(b, []byte{3, 9, 0, 4}) {
		t.Fatalf("success encoding %x", b)
	}
	if _, err := parseEAP([]byte{2, 1, 0, 50, 1}); err == nil {
		t.Fatal("length beyond the data accepted")
	}
	attrs := eapMessageAttrs(make([]byte, 600))
	if len(attrs) != 3 || len(attrs[0].Attribute) != 253 || len(attrs[2].Attribute) != 94 {
		t.Fatal("EAP-Message split incorrectly")
	}
}

func TestTTLSFragmentation(t *testing.T) {
	e := &eapSession{pending: bytes.Repeat([]byte{1}, 2*ttlsFragmentSize+10), lastReqID: 5}
	first := e.firstFragment()
	if first.id != 6 || first.data[0] != ttlsFlagLength|ttlsFlagMore || binary.BigEndian.Uint32(first.data[1:5]) != uint32(2*ttlsFragmentSize+10) {
		t.Fatalf("first fragment header %x", first.data[:5])
	}
	second, third := e.nextFragment(), e.nextFragment()
	if second.data[0] != ttlsFlagMore || len(second.data) != 1+ttlsFragmentSize || third.data[0] != 0 || len(third.data) != 11 || len(e.pending) != 0 {
		t.Fatal("later fragments wrong")
	}
	small := &eapSession{pending: []byte{1, 2, 3}}
	if f := small.firstFragment(); f.data[0] != 0 || len(f.data) != 4 {
		t.Fatal("unfragmented message should carry no length")
	}
}

func TestMPPEKeyAttrsDecrypt(t *testing.T) {
	request := radius.New(radius.CodeAccessRequest, testRADIUSSecret)
	msk := bytes.Repeat([]byte{0xab}, 32)
	msk = append(msk, bytes.Repeat([]byte{0xcd}, 32)...)
	attrs, err := mppeKeyAttrs(request, msk)
	if err != nil || len(attrs) != 2 {
		t.Fatal(err)
	}
	salts := map[string]bool{}
	for i, a := range attrs {
		if a.Type != rfc2865.VendorSpecific_Type || binary.BigEndian.Uint32(a.Attribute[:4]) != microsoftVendorID {
			t.Fatal("not a Microsoft VSA")
		}
		value := a.Attribute[6:]
		salts[string(value[:2])] = true
		key, _, err := radius.TunnelPassword(value, testRADIUSSecret, request.Authenticator[:])
		if err != nil || !bytes.Equal(key, msk[32*i:32*i+32]) {
			t.Fatalf("key %d does not decrypt: %v", i, err)
		}
	}
	if len(salts) != 2 {
		t.Fatal("keys share a salt")
	}
}

// TestTTLSSessionHandshake drives the in-memory TLS server with a real TLS client
// and checks both sides export the same MSK and the inner data arrives.
func TestTTLSSessionHandshake(t *testing.T) {
	now := time.Now()
	caDER, caKey, err := newRADIUSCA(now)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	serverDER, serverKey, err := newRADIUSServerCert(now, ca, caKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &radiusServer{cert: tls.Certificate{Certificate: [][]byte{serverDER, caDER}, PrivateKey: serverKey}}
	e := &eapSession{conn: newEAPConn()}
	go e.run(s.ttlsConfig())

	roots := x509.NewCertPool()
	roots.AddCert(ca)
	client, clientSide := net.Pipe()
	tc := tls.Client(client, &tls.Config{RootCAs: roots, ServerName: radiusServerName, MaxVersion: tls.VersionTLS12})
	var clientMSK []byte
	var clientErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if clientErr = tc.Handshake(); clientErr != nil {
			return
		}
		st := tc.ConnectionState()
		clientMSK, clientErr = st.ExportKeyingMaterial("ttls keying material", nil, 64)
		if clientErr == nil {
			_, clientErr = tc.Write([]byte("inner"))
		}
	}()
	// Relay: whatever the client writes is fed to the session, and the session's
	// output goes back to the client, one flight at a time.
	buf := make([]byte, 16384)
	for i := 0; i < 10; i++ {
		clientSide.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := clientSide.Read(buf)
		if err != nil {
			break
		}
		out := e.conn.feed(append([]byte(nil), buf[:n]...))
		if done, _, _, _ := e.result(); done {
			break
		}
		if len(out) > 0 {
			if _, err := clientSide.Write(out); err != nil && err != io.EOF {
				t.Fatal(err)
			}
		}
	}
	wg.Wait()
	if clientErr != nil {
		t.Fatalf("client: %v", clientErr)
	}
	done, tlsErr, inner, msk := e.result()
	if !done || tlsErr != nil || string(inner) != "inner" {
		t.Fatalf("session done=%v err=%v inner=%q", done, tlsErr, inner)
	}
	if !bytes.Equal(msk, clientMSK) {
		t.Fatal("server and client MSK differ")
	}
}

func FuzzParseEAP(f *testing.F) {
	f.Add((eapPacket{code: eapCodeResponse, id: 1, typ: eapTypeTTLS, data: []byte{ttlsFlagLength, 0, 0, 0, 9}}).encode())
	f.Fuzz(func(t *testing.T, b []byte) {
		if p, err := parseEAP(b); err == nil && len(p.data) > len(b) {
			t.Fatal("data longer than input")
		}
	})
}

func FuzzParseInnerPAP(f *testing.F) {
	f.Add(append(avp(avpUserName, avpFlagMandator, 0, []byte("alice")), avp(avpUserPassword, avpFlagMandator, 0, []byte("pw"))...))
	f.Add(avp(25, avpFlagVendor|avpFlagMandator, microsoftVendorID, make([]byte, 50)))
	f.Fuzz(func(t *testing.T, b []byte) {
		user, pass, err := parseInnerPAP(b)
		if err == nil && (len(user) > len(b) || len(pass) > len(b)) {
			t.Fatal("values longer than input")
		}
	})
}
