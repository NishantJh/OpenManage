package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testUserID = "550e8400-e29b-41d4-a716-446655440000"

func userRequest(method, path, body, role string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.SetPathValue("id", testUserID)
	return req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: role}))
}

func TestUserAdministrationRequiresAdmin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
		body    string
	}{
		{"list", (&app{}).listUsers, http.MethodGet, ""},
		{"create", (&app{}).createUser, http.MethodPost, `{"username":"x","role":"readonly","password":"long-enough-pass"}`},
		{"update", (&app{}).updateUser, http.MethodPatch, `{"role":"admin"}`},
		{"reset password", (&app{}).resetUserPassword, http.MethodPost, `{"password":"long-enough-pass"}`},
	} {
		for _, role := range []string{"operator", "readonly"} {
			t.Run(tc.name+"/"+role, func(t *testing.T) {
				rec := httptest.NewRecorder()
				tc.handler.ServeHTTP(rec, userRequest(tc.method, "/api/users", tc.body, role))
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status %d, want 403", rec.Code)
				}
			})
		}
	}
}

func TestCreateUserValidatesInput(t *testing.T) {
	for _, body := range []string{
		`{"username":"","role":"readonly","password":"long-enough-pass"}`,
		`{"username":"-dash","role":"readonly","password":"long-enough-pass"}`,
		`{"username":"has space","role":"readonly","password":"long-enough-pass"}`,
		`{"username":"` + strings.Repeat("a", 65) + `","role":"readonly","password":"long-enough-pass"}`,
		`{"username":"ok","role":"root","password":"long-enough-pass"}`,
		`{"username":"ok","role":"readonly","password":"short"}`,
		`{"username":"ok","role":"readonly","password":"` + strings.Repeat("p", 73) + `"}`,
		`{"username":"ok","role":"readonly","password":"long-enough-pass","extra":1}`,
	} {
		rec := httptest.NewRecorder()
		(&app{}).createUser(rec, userRequest(http.MethodPost, "/api/users", body, "admin"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
}

func TestUpdateUserValidatesInput(t *testing.T) {
	for _, body := range []string{`{}`, `{"role":"root"}`, `{"username":"rename"}`} {
		rec := httptest.NewRecorder()
		(&app{}).updateUser(rec, userRequest(http.MethodPatch, "/api/users/"+testUserID, body, "admin"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
}

func TestPasswordChangesRejectWeakPasswords(t *testing.T) {
	rec := httptest.NewRecorder()
	(&app{}).changeOwnPassword(rec, userRequest(http.MethodPost, "/api/auth/password",
		`{"current_password":"whatever","new_password":"short"}`, "readonly"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("own change: status %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	(&app{}).resetUserPassword(rec, userRequest(http.MethodPost, "/api/users/"+testUserID+"/password",
		`{"password":"`+strings.Repeat("p", 73)+`"}`, "admin"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reset: status %d, want 400", rec.Code)
	}
}
