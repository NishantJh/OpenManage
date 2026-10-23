package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

// EAP-TTLS with inner PAP (RFC 5281), as chosen in docs/radius-design.md. The AP
// relays EAP inside RADIUS; each conversation is keyed by the RADIUS State
// attribute and runs a TLS 1.2 server over an in-memory connection.

const (
	eapCodeRequest  = 1
	eapCodeResponse = 2
	eapCodeSuccess  = 3
	eapCodeFailure  = 4

	eapTypeIdentity = 1
	eapTypeNak      = 3
	eapTypeTTLS     = 21

	ttlsFlagLength = 0x80
	ttlsFlagMore   = 0x40
	ttlsFlagStart  = 0x20

	// TLS data per EAP request, small enough for 802.1X frames on any AP.
	ttlsFragmentSize = 1000
	// A TLS 1.2 handshake from a client is a few kilobytes; refuse anything larger.
	ttlsMaxMessage = 16 * 1024

	eapMaxSessions          = 64
	eapMaxSessionsPerClient = 8
	eapIdleTimeout          = 30 * time.Second
	eapSessionLifetime      = 60 * time.Second

	// Diameter AVP codes reused from RADIUS inside the TTLS tunnel.
	avpUserName     = 1
	avpUserPassword = 2
	avpFlagVendor   = 0x80
	avpFlagMandator = 0x40

	microsoftVendorID = 311
	msMPPESendKey     = 16
	msMPPERecvKey     = 17
)

var errEAPConversationDone = errors.New("eap: conversation finished")

type eapPacket struct {
	code byte
	id   byte
	typ  byte
	data []byte
}

func parseEAP(b []byte) (eapPacket, error) {
	if len(b) < 4 {
		return eapPacket{}, errors.New("eap: short packet")
	}
	length := int(binary.BigEndian.Uint16(b[2:4]))
	if length < 4 || length > len(b) {
		return eapPacket{}, errors.New("eap: bad length")
	}
	p := eapPacket{code: b[0], id: b[1]}
	if length > 4 {
		p.typ, p.data = b[4], b[5:length]
	}
	return p, nil
}

func (p eapPacket) encode() []byte {
	if p.code == eapCodeSuccess || p.code == eapCodeFailure {
		return []byte{p.code, p.id, 0, 4}
	}
	b := make([]byte, 5+len(p.data))
	b[0], b[1], b[4] = p.code, p.id, p.typ
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
	copy(b[5:], p.data)
	return b
}

// eapMessageAttrs splits an EAP packet into EAP-Message attributes of at most 253 bytes.
func eapMessageAttrs(b []byte) radius.Attributes {
	var attrs radius.Attributes
	for len(b) > 0 {
		n := min(len(b), 253)
		attrs = append(attrs, &radius.AVP{Type: rfc2869.EAPMessage_Type, Attribute: append([]byte(nil), b[:n]...)})
		b = b[n:]
	}
	return attrs
}

// eapConn is the in-memory transport under crypto/tls. Read blocks until the
// RADIUS side feeds the next client message; feed returns what TLS wrote once the
// TLS goroutine is waiting for input again or has finished.
type eapConn struct {
	mu      sync.Mutex
	cond    *sync.Cond
	in, out []byte
	waiting bool
	closed  bool
	done    bool
}

func newEAPConn() *eapConn {
	c := &eapConn{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *eapConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.in) == 0 && !c.closed {
		c.waiting = true
		c.cond.Broadcast()
		c.cond.Wait()
	}
	c.waiting = false
	if len(c.in) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.in)
	c.in = c.in[n:]
	return n, nil
}

func (c *eapConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	c.out = append(c.out, p...)
	return len(p), nil
}

// feed hands the TLS goroutine one reassembled client message and waits for it to
// consume it.
func (c *eapConn) feed(data []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.in = append(c.in, data...)
	c.cond.Broadcast()
	for !c.done && !(c.waiting && len(c.in) == 0) {
		c.cond.Wait()
	}
	out := c.out
	c.out = nil
	return out
}

// finish marks the TLS goroutine as gone so feed stops waiting for it.
func (c *eapConn) finish() {
	c.mu.Lock()
	c.done = true
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *eapConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

func (c *eapConn) LocalAddr() net.Addr              { return eapAddr{} }
func (c *eapConn) RemoteAddr() net.Addr             { return eapAddr{} }
func (c *eapConn) SetDeadline(time.Time) error      { return nil }
func (c *eapConn) SetReadDeadline(time.Time) error  { return nil }
func (c *eapConn) SetWriteDeadline(time.Time) error { return nil }

type eapAddr struct{}

func (eapAddr) Network() string { return "eap" }
func (eapAddr) String() string  { return "eap" }

// eapSession is one EAP-TTLS conversation with one supplicant through one AP.
type eapSession struct {
	mu        sync.Mutex
	deviceID  string
	identity  string
	created   time.Time
	lastSeen  time.Time
	lastReqID byte
	// lastChallenge is resent if the supplicant repeats its previous response.
	lastChallenge radius.Attributes

	conn    *eapConn
	started bool
	// in reassembles a fragmented client message; pending holds server TLS data
	// still to be sent in later fragments.
	in      []byte
	inTotal int
	pending []byte

	// Set by the TLS goroutine when it ends.
	tlsErr error
	inner  []byte
	msk    []byte
}

func (s *radiusServer) ttlsConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{s.cert},
		// EAP-TTLS key derivation here follows RFC 5281 for TLS 1.2. TLS 1.3 for
		// EAP (RFC 9427) changes the handshake ending and keys; it is not enabled.
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		},
		// No resumption: every conversation is a full handshake, which also rules
		// out the triple-handshake attack on the exported keys.
		SessionTicketsDisabled: true,
		Renegotiation:          tls.RenegotiateNever,
	}
}

// run performs the TLS handshake, exports the MSK, and reads the inner AVPs.
func (e *eapSession) run(cfg *tls.Config) {
	defer e.conn.finish()
	// Bound the goroutine even if the supplicant goes quiet mid-conversation.
	timer := time.AfterFunc(eapSessionLifetime, func() { e.conn.Close() })
	defer timer.Stop()
	tc := tls.Server(e.conn, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), eapSessionLifetime)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		e.setResult(err, nil, nil)
		return
	}
	state := tc.ConnectionState()
	msk, err := state.ExportKeyingMaterial("ttls keying material", nil, 64)
	if err != nil {
		e.setResult(err, nil, nil)
		return
	}
	buf := make([]byte, 4096)
	n, err := tc.Read(buf)
	if err != nil {
		e.setResult(err, nil, nil)
		return
	}
	e.setResult(nil, buf[:n], msk)
}

func (e *eapSession) setResult(err error, inner, msk []byte) {
	e.conn.mu.Lock()
	defer e.conn.mu.Unlock()
	e.tlsErr, e.inner, e.msk = err, inner, msk
}

func (e *eapSession) result() (bool, error, []byte, []byte) {
	e.conn.mu.Lock()
	defer e.conn.mu.Unlock()
	return e.conn.done, e.tlsErr, e.inner, e.msk
}

func (e *eapSession) close() {
	e.conn.Close()
}

// eapSessions holds conversations by RADIUS State.
type eapSessions struct {
	mu        sync.Mutex
	byState   map[string]*eapSession
	perClient map[string]int
}

func (t *eapSessions) prune(now time.Time) {
	for state, e := range t.byState {
		if now.Sub(e.lastSeen) > eapIdleTimeout || now.Sub(e.created) > eapSessionLifetime {
			t.removeLocked(state, e)
		}
	}
}

func (t *eapSessions) removeLocked(state string, e *eapSession) {
	if _, ok := t.byState[state]; !ok {
		return
	}
	delete(t.byState, state)
	t.perClient[e.deviceID]--
	if t.perClient[e.deviceID] <= 0 {
		delete(t.perClient, e.deviceID)
	}
	e.close()
}

func (t *eapSessions) add(e *eapSession, now time.Time) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byState == nil {
		t.byState, t.perClient = map[string]*eapSession{}, map[string]int{}
	}
	t.prune(now)
	if len(t.byState) >= eapMaxSessions || t.perClient[e.deviceID] >= eapMaxSessionsPerClient {
		return "", false
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", false
	}
	state := string(b)
	t.byState[state] = e
	t.perClient[e.deviceID]++
	return state, true
}

func (t *eapSessions) get(state, deviceID string, now time.Time) *eapSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(now)
	e := t.byState[state]
	if e == nil || e.deviceID != deviceID {
		return nil
	}
	return e
}

func (t *eapSessions) remove(state string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.byState[state]; e != nil {
		t.removeLocked(state, e)
	}
}

// handleEAP answers one Access-Request carrying EAP. It returns the RADIUS code and
// the attributes to send (EAP-Message, State, and on success the MPPE keys).
func (s *radiusServer) handleEAP(ctx context.Context, deviceID string, packet *radius.Packet) (radius.Code, radius.Attributes) {
	now := time.Now()
	var raw []byte
	for _, avp := range packet.Attributes {
		if avp.Type == rfc2869.EAPMessage_Type {
			raw = append(raw, avp.Attribute...)
		}
	}
	msg, err := parseEAP(raw)
	if err != nil || msg.code != eapCodeResponse {
		return radius.CodeAccessReject, eapMessageAttrs(eapPacket{code: eapCodeFailure, id: msg.id}.encode())
	}
	if blocked, _ := s.clientAttempts.isBlocked(deviceID, now); blocked {
		return eapFailure(msg.id)
	}
	stateAttr, hasState := packet.Attributes.Lookup(rfc2865.State_Type)
	if !hasState {
		return s.startEAP(ctx, deviceID, msg, now)
	}
	state := string(stateAttr)
	e := s.eap.get(state, deviceID, now)
	if e == nil {
		return eapFailure(msg.id)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if msg.id == e.lastReqID-1 && e.lastChallenge != nil {
		// The supplicant repeated its previous response; repeat our request.
		return radius.CodeAccessChallenge, e.lastChallenge
	}
	if msg.id != e.lastReqID {
		return eapFailure(msg.id)
	}
	e.lastSeen = now
	code, attrs, finished := s.continueEAP(ctx, e, packet, msg, stateAttr)
	if finished {
		s.eap.remove(state)
	} else {
		e.lastChallenge = attrs
	}
	return code, attrs
}

func eapFailure(id byte) (radius.Code, radius.Attributes) {
	return radius.CodeAccessReject, eapMessageAttrs(eapPacket{code: eapCodeFailure, id: id}.encode())
}

// startEAP handles EAP-Response/Identity by opening a session and sending the
// EAP-TTLS start request.
func (s *radiusServer) startEAP(ctx context.Context, deviceID string, msg eapPacket, now time.Time) (radius.Code, radius.Attributes) {
	if msg.typ != eapTypeIdentity {
		return eapFailure(msg.id)
	}
	identity := ""
	if validUsername.MatchString(string(msg.data)) {
		identity = string(msg.data)
	}
	e := &eapSession{deviceID: deviceID, identity: identity, created: now, lastSeen: now, conn: newEAPConn(), lastReqID: msg.id + 1}
	state, ok := s.eap.add(e, now)
	if !ok {
		s.reject(ctx, deviceID, "", radiusMethodTTLS, radiusReasonEAPLimit, now)
		return eapFailure(msg.id)
	}
	start := eapPacket{code: eapCodeRequest, id: e.lastReqID, typ: eapTypeTTLS, data: []byte{ttlsFlagStart}}
	return radius.CodeAccessChallenge, challengeAttrs(start, []byte(state))
}

func challengeAttrs(p eapPacket, state []byte) radius.Attributes {
	return append(eapMessageAttrs(p.encode()), &radius.AVP{Type: rfc2865.State_Type, Attribute: state})
}

// continueEAP processes one EAP-TTLS response within a session. finished reports
// that the session should be removed.
func (s *radiusServer) continueEAP(ctx context.Context, e *eapSession, packet *radius.Packet, msg eapPacket, state []byte) (radius.Code, radius.Attributes, bool) {
	now := time.Now()
	fail := func(reason string) (radius.Code, radius.Attributes, bool) {
		s.reject(ctx, e.deviceID, e.identity, radiusMethodTTLS, reason, now)
		code, attrs := eapFailure(msg.id)
		return code, attrs, true
	}
	if msg.typ == eapTypeNak {
		return fail(radiusReasonEAPMethod)
	}
	if msg.typ != eapTypeTTLS || len(msg.data) < 1 {
		return fail(radiusReasonInvalid)
	}
	flags, data := msg.data[0], msg.data[1:]
	if flags&ttlsFlagLength != 0 {
		if len(data) < 4 {
			return fail(radiusReasonInvalid)
		}
		total := int(binary.BigEndian.Uint32(data[:4]))
		if total > ttlsMaxMessage || (e.inTotal != 0 && total != e.inTotal) {
			return fail(radiusReasonInvalid)
		}
		e.inTotal, data = total, data[4:]
	}
	// An empty response acknowledges our last fragment.
	if len(data) == 0 && flags&ttlsFlagMore == 0 {
		if len(e.pending) > 0 {
			return radius.CodeAccessChallenge, challengeAttrs(e.nextFragment(), state), false
		}
		return fail(radiusReasonInvalid)
	}
	if len(e.pending) > 0 || len(e.in)+len(data) > ttlsMaxMessage {
		return fail(radiusReasonInvalid)
	}
	e.in = append(e.in, data...)
	if flags&ttlsFlagMore != 0 {
		e.lastReqID++
		ack := eapPacket{code: eapCodeRequest, id: e.lastReqID, typ: eapTypeTTLS, data: []byte{0}}
		return radius.CodeAccessChallenge, challengeAttrs(ack, state), false
	}
	if e.inTotal != 0 && len(e.in) != e.inTotal {
		return fail(radiusReasonInvalid)
	}
	input := e.in
	e.in, e.inTotal = nil, 0
	if !e.started {
		e.started = true
		go e.run(s.ttlsConfig())
	}
	out := e.conn.feed(input)
	done, tlsErr, inner, msk := e.result()
	if done {
		if tlsErr != nil || inner == nil {
			return fail(radiusReasonTLS)
		}
		return s.finishTTLS(ctx, e, packet, msg.id, inner, msk)
	}
	if len(out) == 0 {
		return fail(radiusReasonTLS)
	}
	e.pending = out
	return radius.CodeAccessChallenge, challengeAttrs(e.firstFragment(), state), false
}

func (e *eapSession) firstFragment() eapPacket {
	e.lastReqID++
	total := len(e.pending)
	if total <= ttlsFragmentSize {
		data := append([]byte{0}, e.pending...)
		e.pending = nil
		return eapPacket{code: eapCodeRequest, id: e.lastReqID, typ: eapTypeTTLS, data: data}
	}
	data := make([]byte, 5, 5+ttlsFragmentSize)
	data[0] = ttlsFlagLength | ttlsFlagMore
	binary.BigEndian.PutUint32(data[1:5], uint32(total))
	data = append(data, e.pending[:ttlsFragmentSize]...)
	e.pending = e.pending[ttlsFragmentSize:]
	return eapPacket{code: eapCodeRequest, id: e.lastReqID, typ: eapTypeTTLS, data: data}
}

func (e *eapSession) nextFragment() eapPacket {
	e.lastReqID++
	n := min(len(e.pending), ttlsFragmentSize)
	flags := byte(0)
	if n < len(e.pending) {
		flags = ttlsFlagMore
	}
	data := append([]byte{flags}, e.pending[:n]...)
	e.pending = e.pending[n:]
	return eapPacket{code: eapCodeRequest, id: e.lastReqID, typ: eapTypeTTLS, data: data}
}

// finishTTLS checks the inner PAP credentials and builds the final reply.
func (s *radiusServer) finishTTLS(ctx context.Context, e *eapSession, packet *radius.Packet, id byte, inner, msk []byte) (radius.Code, radius.Attributes, bool) {
	username, password, err := parseInnerPAP(inner)
	if err != nil {
		s.reject(ctx, e.deviceID, e.identity, radiusMethodTTLS, radiusReasonInnerMethod, time.Now())
		code, attrs := eapFailure(id)
		return code, attrs, true
	}
	if !s.checkPassword(ctx, e.deviceID, username, password, radiusMethodTTLS) {
		code, attrs := eapFailure(id)
		return code, attrs, true
	}
	attrs := eapMessageAttrs(eapPacket{code: eapCodeSuccess, id: id}.encode())
	attrs = append(attrs, &radius.AVP{Type: rfc2865.UserName_Type, Attribute: []byte(username)})
	keys, err := mppeKeyAttrs(packet, msk)
	if err != nil {
		code, attrs := eapFailure(id)
		return code, attrs, true
	}
	return radius.CodeAccessAccept, append(attrs, keys...), true
}

// parseInnerPAP reads the Diameter AVPs a TTLS client sends after the handshake.
// Only User-Name and User-Password are accepted; any other mandatory AVP (for
// example MS-CHAP-V2 from a client set up for another inner method) is refused.
func parseInnerPAP(b []byte) (string, string, error) {
	var username, password []byte
	for len(b) > 0 {
		if len(b) < 8 {
			return "", "", errors.New("ttls: short AVP")
		}
		code := binary.BigEndian.Uint32(b[0:4])
		flags := b[4]
		length := int(b[5])<<16 | int(b[6])<<8 | int(b[7])
		header := 8
		if flags&avpFlagVendor != 0 {
			header = 12
		}
		if length < header || length > len(b) {
			return "", "", errors.New("ttls: bad AVP length")
		}
		value := b[header:length]
		switch {
		case flags&avpFlagVendor == 0 && code == avpUserName && username == nil:
			username = value
		case flags&avpFlagVendor == 0 && code == avpUserPassword && password == nil:
			password = bytes.TrimRight(value, "\x00")
		case flags&avpFlagMandator != 0:
			return "", "", errors.New("ttls: unsupported mandatory AVP")
		}
		padded := (length + 3) &^ 3
		if padded > len(b) {
			padded = len(b)
		}
		b = b[padded:]
	}
	if username == nil || password == nil {
		return "", "", errors.New("ttls: inner PAP needs User-Name and User-Password")
	}
	return string(username), string(password), nil
}

// mppeKeyAttrs carries the MSK to the AP as MS-MPPE-Recv-Key and MS-MPPE-Send-Key
// (RFC 2548), encrypted with the AP's secret and this request's authenticator.
// Each key gets its own salt, as the RFC requires.
func mppeKeyAttrs(request *radius.Packet, msk []byte) (radius.Attributes, error) {
	if len(msk) != 64 {
		return nil, errors.New("eap: MSK must be 64 bytes")
	}
	var salts [4]byte
	for salts[0]&0x7f == salts[2]&0x7f && salts[1] == salts[3] {
		if _, err := rand.Read(salts[:]); err != nil {
			return nil, err
		}
	}
	var attrs radius.Attributes
	for i, k := range []struct {
		typ byte
		key []byte
	}{{msMPPERecvKey, msk[:32]}, {msMPPESendKey, msk[32:]}} {
		salt := []byte{salts[2*i] | 0x80, salts[2*i+1]}
		value, err := radius.NewTunnelPassword(k.key, salt, request.Secret, request.Authenticator[:])
		if err != nil {
			return nil, err
		}
		vsa := make([]byte, 6, 6+len(value))
		binary.BigEndian.PutUint32(vsa[:4], microsoftVendorID)
		vsa[4], vsa[5] = k.typ, byte(2+len(value))
		attrs = append(attrs, &radius.AVP{Type: rfc2865.VendorSpecific_Type, Attribute: append(vsa, value...)})
	}
	return attrs, nil
}
