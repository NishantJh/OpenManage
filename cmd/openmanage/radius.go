package main

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

// The built-in RADIUS listener (docs/radius-design.md, milestone 2). It answers
// PAP Access-Requests from enrolled APs only, and is off unless
// OPENMANAGE_RADIUS_LISTEN is set. EAP is rejected until EAP-TTLS lands.

const (
	radiusMaxInFlight     = 16
	radiusRequestTimeout  = 5 * time.Second
	radiusReplayCacheTTL  = 30 * time.Second
	radiusReplayCacheSize = 1024
	// A Message-Authenticator attribute is its 2-byte header plus an HMAC-MD5.
	radiusMessageAuthenticatorLen = 18
)

const (
	radiusUserFailureLimit   = 10
	radiusClientFailureLimit = 50
	radiusFailureWindow      = 5 * time.Minute
	radiusBlockDuration      = 5 * time.Minute
)

func radiusSecretAAD(deviceID string) string {
	return "openmanage:radius-secret:v1:" + deviceID
}

type radiusServer struct {
	db  *pgxpool.Pool
	key []byte
	// addr is the configured listen address, shown to administrators.
	addr string
	// cert is the server's TLS identity for EAP-TTLS.
	cert tls.Certificate

	sem            chan struct{}
	wg             sync.WaitGroup
	userAttempts   *loginRateLimiter
	clientAttempts *loginRateLimiter
	// dropped counts packets discarded without a reply: unknown source, bad or
	// missing Message-Authenticator, malformed, or over the in-flight limit.
	dropped atomic.Int64

	eap eapSessions

	replayMu sync.Mutex
	replay   map[string]replayEntry

	// clients caches the RADIUS client list so packets from unknown sources do not
	// each cost a database query. Secret changes call invalidateClients.
	clientsMu     sync.Mutex
	clients       []radiusClient
	clientsLoaded time.Time
}

type radiusClient struct {
	deviceID   string
	ip         net.IP
	ciphertext []byte
}

const radiusClientCacheTTL = 30 * time.Second

// replayEntry remembers the reply to a request so a retransmission gets the same
// answer without a second password check. A nil reply means still in progress.
type replayEntry struct {
	reply   []byte
	expires time.Time
}

func newRADIUSServer(db *pgxpool.Pool, key []byte, addr string, cert tls.Certificate) *radiusServer {
	return &radiusServer{
		db: db, key: key, addr: addr, cert: cert,
		sem: make(chan struct{}, radiusMaxInFlight),
		userAttempts: newLoginRateLimiter(radiusUserFailureLimit, radiusFailureWindow,
			radiusBlockDuration, loginLimiterMaxEntries),
		clientAttempts: newLoginRateLimiter(radiusClientFailureLimit, radiusFailureWindow,
			radiusBlockDuration, loginLimiterMaxEntries),
		replay: make(map[string]replayEntry),
	}
}

// serve reads packets until ctx is cancelled, then waits for in-flight requests.
func (s *radiusServer) serve(ctx context.Context, conn net.PacketConn) {
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	defer s.wg.Wait()
	buf := make([]byte, radius.MaxPacketLength)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("radius: read: %v", err)
			continue
		}
		select {
		case s.sem <- struct{}{}:
		default:
			s.dropped.Add(1)
			continue
		}
		raw := append([]byte(nil), buf[:n]...)
		s.wg.Add(1)
		go func() {
			defer func() { <-s.sem; s.wg.Done() }()
			reqCtx, cancel := context.WithTimeout(ctx, radiusRequestTimeout)
			defer cancel()
			if reply := s.handle(reqCtx, raw, addr); reply != nil {
				if _, err := conn.WriteTo(reply, addr); err != nil {
					log.Printf("radius: write: %v", err)
				}
			}
		}()
	}
}

// handle returns the encoded reply, or nil to drop the packet silently, as RFC 2865
// requires for packets that cannot be authenticated.
func (s *radiusServer) handle(ctx context.Context, raw []byte, addr net.Addr) []byte {
	udp, ok := addr.(*net.UDPAddr)
	if !ok || len(raw) < 20 || radius.Code(raw[0]) != radius.CodeAccessRequest {
		s.dropped.Add(1)
		return nil
	}
	deviceID, secret, err := s.lookupClient(ctx, udp.IP)
	if err != nil {
		s.dropped.Add(1)
		return nil
	}
	packet, err := radius.Parse(raw, secret)
	if err != nil || !validMessageAuthenticator(raw, secret) {
		s.dropped.Add(1)
		return nil
	}
	replayKey := deviceID + string(raw[1:2]) + string(raw[4:20])
	if cached, seen := s.replayLookup(replayKey); seen {
		return cached
	}
	var reply []byte
	if _, eap := packet.Attributes.Lookup(rfc2869.EAPMessage_Type); eap {
		code, attrs := s.handleEAP(ctx, deviceID, packet)
		reply, err = encodeRADIUSReply(packet, code, attrs)
	} else {
		code := radius.CodeAccessReject
		if s.authenticate(ctx, deviceID, packet) {
			code = radius.CodeAccessAccept
		}
		reply, err = encodeRADIUSReply(packet, code, nil)
	}
	if err != nil {
		log.Printf("radius: encode reply: %v", err)
		s.replayForget(replayKey)
		return nil
	}
	s.replayStore(replayKey, reply)
	return reply
}

var errRADIUSUnknownClient = errors.New("radius: no single enrolled client for source address")

// lookupClient finds the one device with a RADIUS secret whose inventory address is
// the packet's source address, and decrypts that secret.
func (s *radiusServer) lookupClient(ctx context.Context, source net.IP) (string, []byte, error) {
	clients, err := s.loadClients(ctx)
	if err != nil {
		return "", nil, err
	}
	var match *radiusClient
	for i := range clients {
		if clients[i].ip.Equal(source) {
			if match != nil {
				return "", nil, errRADIUSUnknownClient
			}
			match = &clients[i]
		}
	}
	if match == nil {
		return "", nil, errRADIUSUnknownClient
	}
	secret, err := openSecret(s.key, match.ciphertext, radiusSecretAAD(match.deviceID))
	if err != nil {
		return "", nil, err
	}
	return match.deviceID, secret, nil
}

func (s *radiusServer) loadClients(ctx context.Context) ([]radiusClient, error) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	if !s.clientsLoaded.IsZero() && time.Since(s.clientsLoaded) < radiusClientCacheTTL {
		return s.clients, nil
	}
	rows, err := s.db.Query(ctx, `SELECT d.id::text, d.address, rc.secret_ciphertext
		FROM radius_clients rc JOIN devices d ON d.id=rc.device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	clients := []radiusClient{}
	for rows.Next() {
		var c radiusClient
		var address string
		if err := rows.Scan(&c.deviceID, &address, &c.ciphertext); err != nil {
			return nil, err
		}
		if c.ip = net.ParseIP(address); c.ip != nil {
			clients = append(clients, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.clients, s.clientsLoaded = clients, time.Now()
	return clients, nil
}

// invalidateClients makes the next packet reload the client list. Safe on nil.
func (s *radiusServer) invalidateClients() {
	if s == nil {
		return
	}
	s.clientsMu.Lock()
	s.clientsLoaded = time.Time{}
	s.clientsMu.Unlock()
}

// validMessageAuthenticator requires exactly one well-formed Message-Authenticator
// (RFC 3579 section 3.2) and checks it. Requiring it on every Access-Request blocks
// forged responses and the Blast-RADIUS attack (CVE-2024-3596).
func validMessageAuthenticator(raw, secret []byte) bool {
	offset := -1
	for i := 20; i < len(raw); {
		if i+2 > len(raw) || raw[i+1] < 2 || i+int(raw[i+1]) > len(raw) {
			return false
		}
		if radius.Type(raw[i]) == rfc2869.MessageAuthenticator_Type {
			if offset >= 0 || raw[i+1] != radiusMessageAuthenticatorLen {
				return false
			}
			offset = i + 2
		}
		i += int(raw[i+1])
	}
	if offset < 0 {
		return false
	}
	zeroed := append([]byte(nil), raw...)
	clear(zeroed[offset : offset+16])
	mac := hmac.New(md5.New, secret)
	mac.Write(zeroed)
	return hmac.Equal(mac.Sum(nil), raw[offset:offset+16])
}

// encodeRADIUSReply builds a reply carrying Message-Authenticator as its first
// attribute, computed over the reply with the request authenticator in place.
func encodeRADIUSReply(request *radius.Packet, code radius.Code, attrs radius.Attributes) ([]byte, error) {
	reply := request.Response(code)
	reply.Attributes = append(radius.Attributes{{Type: rfc2869.MessageAuthenticator_Type, Attribute: make([]byte, 16)}}, attrs...)
	unsigned, err := reply.MarshalBinary()
	if err != nil {
		return nil, err
	}
	mac := hmac.New(md5.New, request.Secret)
	mac.Write(unsigned)
	reply.Attributes[0].Attribute = mac.Sum(nil)
	return reply.Encode()
}

func (s *radiusServer) replayLookup(key string) ([]byte, bool) {
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	now := time.Now()
	if entry, ok := s.replay[key]; ok && now.Before(entry.expires) {
		return entry.reply, true
	}
	if len(s.replay) >= radiusReplayCacheSize {
		for k, entry := range s.replay {
			if !now.Before(entry.expires) {
				delete(s.replay, k)
			}
		}
	}
	if len(s.replay) >= radiusReplayCacheSize {
		// Still full of live entries: process without caching rather than evict.
		return nil, false
	}
	s.replay[key] = replayEntry{expires: now.Add(radiusReplayCacheTTL)}
	return nil, false
}

func (s *radiusServer) replayStore(key string, reply []byte) {
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	if _, ok := s.replay[key]; ok {
		s.replay[key] = replayEntry{reply: reply, expires: time.Now().Add(radiusReplayCacheTTL)}
	}
}

func (s *radiusServer) replayForget(key string) {
	s.replayMu.Lock()
	delete(s.replay, key)
	s.replayMu.Unlock()
}

// Reasons recorded in the audit log. The client only ever sees Access-Reject.
const (
	radiusReasonInvalid     = "invalid_request"
	radiusReasonCredentials = "bad_credentials"
	radiusReasonDisabled    = "disabled"
	radiusReasonEAPMethod   = "eap_method_rejected"
	radiusReasonInnerMethod = "inner_method_not_pap"
	radiusReasonTLS         = "tls_failed"
	radiusReasonEAPLimit    = "eap_session_limit"
)

const (
	radiusMethodPAP  = "pap"
	radiusMethodTTLS = "eap-ttls"
)

// authenticate checks a plain PAP request (no EAP).
func (s *radiusServer) authenticate(ctx context.Context, deviceID string, packet *radius.Packet) bool {
	now := time.Now()
	if blocked, _ := s.clientAttempts.isBlocked(deviceID, now); blocked {
		return false
	}
	username := ""
	if names, err := rfc2865.UserName_GetStrings(packet); err == nil && len(names) == 1 && validUsername.MatchString(names[0]) {
		username = names[0]
	}
	passwords, err := rfc2865.UserPassword_GetStrings(packet)
	if err != nil || username == "" || len(passwords) != 1 {
		s.reject(ctx, deviceID, username, radiusMethodPAP, radiusReasonInvalid, now)
		return false
	}
	return s.checkPassword(ctx, deviceID, username, passwords[0], radiusMethodPAP)
}

// checkPassword verifies a network user's password for PAP and for the inner PAP
// of EAP-TTLS. Unknown users, wrong passwords and disabled users all cost one
// bcrypt comparison and are rejected the same way.
func (s *radiusServer) checkPassword(ctx context.Context, deviceID, username, password, method string) bool {
	now := time.Now()
	if blocked, _ := s.clientAttempts.isBlocked(deviceID, now); blocked {
		return false
	}
	if !validUsername.MatchString(username) || password == "" || len(password) > maxNewPasswordLength {
		if !validUsername.MatchString(username) {
			username = ""
		}
		s.reject(ctx, deviceID, username, method, radiusReasonInvalid, now)
		return false
	}
	if blocked, _ := s.userAttempts.isBlocked(username, now); blocked {
		return false
	}
	var hash string
	var disabled bool
	err := s.db.QueryRow(ctx, `SELECT password_hash, disabled_at IS NOT NULL FROM network_users WHERE username=$1`,
		username).Scan(&hash, &disabled)
	if err != nil {
		hash, disabled = dummyPasswordHash(), false
	}
	matched := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil && err == nil
	switch {
	case matched && !disabled:
		s.userAttempts.clear(username)
		s.audit(ctx, deviceID, "radius.auth_accept", map[string]string{"username": username, "method": method})
		return true
	case matched:
		s.reject(ctx, deviceID, username, method, radiusReasonDisabled, now)
	default:
		s.reject(ctx, deviceID, username, method, radiusReasonCredentials, now)
	}
	return false
}

// reject counts a failure against the AP and user and audits it. Rejections while
// blocked are not audited, so the rate limits also bound audit volume.
func (s *radiusServer) reject(ctx context.Context, deviceID, username, method, reason string, now time.Time) {
	s.clientAttempts.recordFailure(deviceID, now)
	if username != "" {
		s.userAttempts.recordFailure(username, now)
	}
	s.audit(ctx, deviceID, "radius.auth_reject", map[string]string{"username": username, "method": method, "reason": reason})
}

func (s *radiusServer) audit(ctx context.Context, deviceID, action string, details map[string]string) {
	tx, err := s.db.Begin(ctx)
	if err == nil {
		defer tx.Rollback(ctx)
		if err = insertAuditEvent(ctx, tx, "", action, "device", deviceID, details); err == nil {
			err = tx.Commit(ctx)
		}
	}
	if err != nil {
		log.Printf("radius: audit %s: %v", action, err)
	}
}

var dummyHash struct {
	once sync.Once
	hash string
}

// dummyPasswordHash is compared against when the username is unknown, so the reply
// time does not reveal which usernames exist.
func dummyPasswordHash() string {
	dummyHash.once.Do(func() {
		b := make([]byte, 32)
		rand.Read(b)
		hash, err := bcrypt.GenerateFromPassword(b, bcrypt.DefaultCost)
		if err != nil {
			panic(err)
		}
		dummyHash.hash = string(hash)
	})
	return dummyHash.hash
}
