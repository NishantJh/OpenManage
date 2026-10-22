package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostnameValidation(t *testing.T) {
	validate := writableSettings["system.hostname"].Validate
	for _, ok := range []string{"a", "router-1", "AP2", strings.Repeat("a", 63)} {
		if msg := validate(ok); msg != "" {
			t.Errorf("%q rejected: %s", ok, msg)
		}
	}
	for _, bad := range []string{"", "-lead", "trail-", "has space", "dot.name", strings.Repeat("a", 64), "x'y", "a;reboot", "a\nb", "ünï"} {
		if validate(bad) == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestChangeOperationsAreAdminOnlyWritesWithFixedCommands(t *testing.T) {
	for _, op := range changeOperations {
		if op.Class != operationWrite || len(op.Roles) != 1 || op.Roles[0] != "admin" || op.Audit != auditTransactional {
			t.Errorf("%s: class %s roles %v audit %s", op.Name, op.Class, op.Roles, op.Audit)
		}
		// Parameters come from stdin only; the command must be the fixed read-and-call form.
		if !strings.HasPrefix(op.command, `read -r p && ubus call `) || !strings.HasSuffix(op.command, ` "$p"`) || strings.Count(op.command, "$") != 1 {
			t.Errorf("%s: unexpected command %q", op.Name, op.command)
		}
	}
}

func TestRunWriteRefusesReadOperations(t *testing.T) {
	res := (&deviceSession{}).runWrite(healthOperations[0], nil)
	if res.Status != "error" || res.Error != "not a write operation" {
		t.Fatalf("got %+v", res)
	}
}

func TestConfigChangeEndpointsRequireAdmin(t *testing.T) {
	id := "550e8400-e29b-41d4-a716-446655440000"
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
		body    string
	}{
		{"settings", (&app{}).listWritableSettings, http.MethodGet, ""},
		{"list", (&app{}).listConfigChanges, http.MethodGet, ""},
		{"preview", (&app{}).previewConfigChange, http.MethodPost, `{"setting":"system.hostname","value":"x"}`},
		{"apply", (&app{}).applyConfigChange, http.MethodPost, ""},
	} {
		for _, role := range []string{"operator", "readonly"} {
			t.Run(tc.name+"/"+role, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, "/api/x", strings.NewReader(tc.body))
				req.SetPathValue("id", id)
				req.SetPathValue("change", "1")
				req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: role}))
				rec := httptest.NewRecorder()
				tc.handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status %d, want 403", rec.Code)
				}
			})
		}
	}
}

func TestPreviewRejectsUnknownSettingsAndBadValues(t *testing.T) {
	for _, body := range []string{
		`{"setting":"network.lan.ipaddr","value":"10.0.0.1"}`,
		`{"setting":"system.hostname","value":"bad name"}`,
		`{"setting":"system.hostname","value":"ok","extra":1}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(body))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: "admin"}))
		rec := httptest.NewRecorder()
		(&app{}).previewConfigChange(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
}
