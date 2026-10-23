package main

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

var testRADIUSSecret = []byte("generated-test-secret")

// signedAccessRequest encodes a PAP Access-Request, adding a Message-Authenticator
// the way a hostapd client does unless withMA is false.
func signedAccessRequest(t *testing.T, secret []byte, withMA bool) []byte {
	t.Helper()
	p := radius.New(radius.CodeAccessRequest, secret)
	if withMA {
		p.Add(rfc2869.MessageAuthenticator_Type, make([]byte, 16))
	}
	if err := rfc2865.UserName_SetString(p, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := rfc2865.UserPassword_SetString(p, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	raw, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if withMA {
		mac := hmac.New(md5.New, secret)
		mac.Write(raw)
		copy(raw[22:38], mac.Sum(nil))
	}
	return raw
}

func TestValidMessageAuthenticator(t *testing.T) {
	good := signedAccessRequest(t, testRADIUSSecret, true)
	if !validMessageAuthenticator(good, testRADIUSSecret) {
		t.Fatal("valid Message-Authenticator rejected")
	}
	if validMessageAuthenticator(good, []byte("other-secret")) {
		t.Fatal("Message-Authenticator accepted with the wrong secret")
	}
	if validMessageAuthenticator(signedAccessRequest(t, testRADIUSSecret, false), testRADIUSSecret) {
		t.Fatal("request without Message-Authenticator accepted")
	}
	tampered := append([]byte(nil), good...)
	tampered[len(tampered)-1] ^= 1
	if validMessageAuthenticator(tampered, testRADIUSSecret) {
		t.Fatal("tampered request accepted")
	}
	// A second Message-Authenticator, even a correct-looking one, is refused.
	doubled := append(append([]byte(nil), good...), good[20:38]...)
	doubled[3] += 18
	if validMessageAuthenticator(doubled, testRADIUSSecret) {
		t.Fatal("request with two Message-Authenticators accepted")
	}
	short := append([]byte(nil), good...)
	short[21] = 10
	if validMessageAuthenticator(short, testRADIUSSecret) {
		t.Fatal("malformed Message-Authenticator accepted")
	}
}

func TestEncodeRADIUSReplyIsSigned(t *testing.T) {
	raw := signedAccessRequest(t, testRADIUSSecret, true)
	request, err := radius.Parse(raw, testRADIUSSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []radius.Code{radius.CodeAccessAccept, radius.CodeAccessReject} {
		reply, err := encodeRADIUSReply(request, code, nil)
		if err != nil {
			t.Fatal(err)
		}
		if radius.Code(reply[0]) != code || reply[1] != raw[1] {
			t.Fatalf("reply header = %d/%d, want %d/%d", reply[0], reply[1], code, raw[1])
		}
		if !radius.IsAuthenticResponse(reply, raw, testRADIUSSecret) {
			t.Fatal("reply authenticator does not verify")
		}
		if radius.Type(reply[20]) != rfc2869.MessageAuthenticator_Type || reply[21] != 18 {
			t.Fatal("Message-Authenticator is not the first reply attribute")
		}
		check := append([]byte(nil), reply...)
		copy(check[4:20], raw[4:20])
		clear(check[22:38])
		mac := hmac.New(md5.New, testRADIUSSecret)
		mac.Write(check)
		if !hmac.Equal(mac.Sum(nil), reply[22:38]) {
			t.Fatal("reply Message-Authenticator does not verify")
		}
	}
}

func TestRADIUSHandleDropsNonAccessRequests(t *testing.T) {
	s := newRADIUSServer(nil, nil, "", tls.Certificate{})
	addr := &net.UDPAddr{IP: net.ParseIP("192.168.1.2"), Port: 4000}
	raw := signedAccessRequest(t, testRADIUSSecret, true)
	raw[0] = byte(radius.CodeAccountingRequest)
	for _, packet := range [][]byte{raw, raw[:19], nil} {
		if reply := s.handle(context.Background(), packet, addr); reply != nil {
			t.Fatal("expected packet to be dropped")
		}
	}
	if got := s.dropped.Load(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
}

func TestRADIUSReplayCache(t *testing.T) {
	s := newRADIUSServer(nil, nil, "", tls.Certificate{})
	if _, seen := s.replayLookup("k"); seen {
		t.Fatal("first lookup reported a duplicate")
	}
	if reply, seen := s.replayLookup("k"); !seen || reply != nil {
		t.Fatal("in-progress request was not treated as a duplicate")
	}
	s.replayStore("k", []byte("reply"))
	if reply, seen := s.replayLookup("k"); !seen || string(reply) != "reply" {
		t.Fatal("retransmission did not get the cached reply")
	}
	s.replayForget("k")
	if _, seen := s.replayLookup("k"); seen {
		t.Fatal("forgotten request still cached")
	}
}

func TestRADIUSSecretAADBindsDevice(t *testing.T) {
	key := make([]byte, 32)
	sealed, err := sealSecret(key, testRADIUSSecret, radiusSecretAAD("device-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSecret(key, sealed, radiusSecretAAD("device-b")); err == nil {
		t.Fatal("secret sealed for one device opened for another")
	}
}

func TestRADIUSEndpointsRequireAdmin(t *testing.T) {
	a := &app{}
	for _, tc := range []struct {
		method, path, body string
		handler            http.HandlerFunc
	}{
		{http.MethodGet, "/api/network-users", "", a.listNetworkUsers},
		{http.MethodPost, "/api/network-users", `{"username":"bob","password":"long enough password"}`, a.createNetworkUser},
		{http.MethodPatch, "/api/network-users/x", `{"disabled":true}`, a.updateNetworkUser},
		{http.MethodPost, "/api/network-users/x/password", `{"password":"long enough password"}`, a.resetNetworkUserPassword},
		{http.MethodGet, "/api/radius", "", a.getRADIUSStatus},
		{http.MethodGet, "/api/radius/ca.pem", "", a.downloadRADIUSCA},
		{http.MethodGet, "/api/radius/apple-profile?ssid=x", "", a.downloadAppleWiFiProfile},
		{http.MethodPost, "/api/devices/x/radius-secret", "", a.rotateRADIUSSecret},
		{http.MethodDelete, "/api/devices/x/radius-secret", "", a.deleteRADIUSSecret},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "operator-id", Role: "operator"}))
		rec := httptest.NewRecorder()
		tc.handler(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: expected 403 for operator, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestRADIUSClearUserFailures(t *testing.T) {
	var none *radiusServer
	none.clearUserFailures("alice")
	s := newRADIUSServer(nil, nil, "", tls.Certificate{})
	now := time.Now()
	for i := 0; i < radiusUserFailureLimit; i++ {
		s.userAttempts.recordFailure("alice", now)
	}
	if blocked, _ := s.userAttempts.isBlocked("alice", now); !blocked {
		t.Fatal("user not blocked after the failure limit")
	}
	s.clearUserFailures("alice")
	if blocked, _ := s.userAttempts.isBlocked("alice", now); blocked {
		t.Fatal("user still blocked after clearUserFailures")
	}
}
