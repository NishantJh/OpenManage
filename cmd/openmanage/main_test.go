package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
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

func TestValidUUID(t *testing.T) {
	valid := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"550E8400-E29B-41D4-A716-446655440000",
	}
	for _, value := range valid {
		if !validUUID(value) {
			t.Errorf("expected UUID %q to be valid", value)
		}
	}
	invalid := []string{
		"", "not-a-uuid", "550e8400e29b41d4a716446655440000",
		"550e8400-e29b-41d4-a716-44665544000z",
		"550e8400_e29b-41d4-a716-446655440000",
	}
	for _, value := range invalid {
		if validUUID(value) {
			t.Errorf("expected %q to be rejected", value)
		}
	}
}

func TestRequireAnyRole(t *testing.T) {
	for _, tc := range []struct {
		name    string
		user    authUser
		allowed []string
		want    bool
		status  int
	}{
		{name: "admin allowed", user: authUser{Role: "admin"}, allowed: []string{"admin"}, want: true, status: http.StatusOK},
		{name: "operator allowed for inventory writes", user: authUser{Role: "operator"}, allowed: []string{"admin", "operator"}, want: true, status: http.StatusOK},
		{name: "readonly denied for writes", user: authUser{Role: "readonly"}, allowed: []string{"admin", "operator"}, want: false, status: http.StatusForbidden},
		{name: "operator denied audit listing", user: authUser{Role: "operator"}, allowed: []string{"admin"}, want: false, status: http.StatusForbidden},
		{name: "missing actor denied", user: authUser{}, allowed: []string{"admin"}, want: true, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.name != "missing actor denied" {
				req = req.WithContext(context.WithValue(req.Context(), userContextKey, tc.user))
			}
			rec := httptest.NewRecorder()
			got := requireAnyRole(rec, req, tc.allowed...)
			if tc.name == "missing actor denied" {
				if got || rec.Code != http.StatusUnauthorized {
					t.Fatalf("missing actor should be denied with 401, got allowed=%v status=%d", got, rec.Code)
				}
				return
			}
			if got != tc.want || rec.Code != tc.status {
				t.Fatalf("got allowed=%v status=%d, want allowed=%v status=%d", got, rec.Code, tc.want, tc.status)
			}
		})
	}
}

func TestReadonlyCannotCreateOrDeleteDevices(t *testing.T) {
	ctx := context.WithValue(context.Background(), userContextKey, authUser{ID: "user-id", Role: "readonly"})
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
		body    string
	}{
		{name: "create", handler: (&app{}).createDevice, method: http.MethodPost, path: "/api/devices", body: `{"name":"router","address":"192.168.1.1"}`},
		{name: "delete", handler: (&app{}).deleteDevice, method: http.MethodDelete, path: "/api/devices/550e8400-e29b-41d4-a716-446655440000"},
		{name: "audit listing", handler: (&app{}).listAudit, method: http.MethodGet, path: "/api/audit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)).WithContext(ctx)
			if strings.Contains(tc.path, "/api/devices/") {
				req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
			}
			rec := httptest.NewRecorder()
			tc.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 before database access, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestDatabaseConfigAcceptsReservedCharactersInPGPassword(t *testing.T) {
	want := "slash/at@colon:question?hash#plus+equals="
	t.Setenv("PGPASSWORD", want)

	cfg, err := pgxpool.ParseConfig("postgres://openmanage@db:5432/openmanage?sslmode=disable")
	if err != nil {
		t.Fatalf("parse database config: %v", err)
	}
	if got := cfg.ConnConfig.Password; got != want {
		t.Fatalf("password mismatch: got %q, want %q", got, want)
	}
}
