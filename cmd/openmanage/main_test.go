package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenHashIsStableAndNotRawToken(t *testing.T) {
	token := "example-session-token"
	first := tokenHash(token)
	if first != tokenHash(token) {
		t.Fatal("same token must hash deterministically")
	}
	if first == token {
		t.Fatal("stored session value must not be the raw token")
	}
	if len(first) != 64 {
		t.Fatalf("expected 64-character SHA-256 hex hash, got %d", len(first))
	}
}

func TestRandomTokenHasExpectedHexLength(t *testing.T) {
	token, err := randomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("expected 64 hex characters, got %d", len(token))
	}
	if token == "" {
		t.Fatal("token must not be empty")
	}
}

func TestAuthMiddlewareRequiresSessionForAPI(t *testing.T) {
	a := &app{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := a.authMiddleware(next)
	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated API request, got %d", res.Code)
	}
}

func TestAuthMiddlewareAllowsLoginEndpoint(t *testing.T) {
	a := &app{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := a.authMiddleware(next)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNoContent {
		t.Fatalf("expected login endpoint to bypass session check, got %d", res.Code)
	}
}

func TestSecurityHeadersAreApplied(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing X-Content-Type-Options")
	}
	if res.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing Content-Security-Policy")
	}
}
