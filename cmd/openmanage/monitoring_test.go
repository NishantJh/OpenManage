package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMonitoringSettingsRequireAdmin(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/api/monitoring/settings", strings.NewReader(`{"enabled":true,"interval_seconds":300}`))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "operator-id", Role: "operator"}))
	rec := httptest.NewRecorder()
	(&app{}).updateMonitoringSettings(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected operator to be denied with 403, got %d", rec.Code)
	}
}

func TestMonitoringSettingsRejectOutOfRangeInterval(t *testing.T) {
	for _, interval := range []int{0, 1, 59, 3601, 86400} {
		t.Run(strings.TrimSpace(string(rune(interval))), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/monitoring/settings",
				strings.NewReader(`{"enabled":true,"interval_seconds":`+itoa(interval)+`}`))
			req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "admin-id", Role: "admin"}))
			rec := httptest.NewRecorder()
			(&app{}).updateMonitoringSettings(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("interval %d: expected 400, got %d: %s", interval, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMonitoringSettingsRejectUnknownFieldsAndTrailingJSON(t *testing.T) {
	for _, body := range []string{
		`{"enabled":true,"interval_seconds":300,"unexpected":true}`,
		`{"enabled":true,"interval_seconds":300} {}`,
	} {
		req := httptest.NewRequest(http.MethodPut, "/api/monitoring/settings", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "admin-id", Role: "admin"}))
		rec := httptest.NewRecorder()
		(&app{}).updateMonitoringSettings(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid body %q, got %d", body, rec.Code)
		}
	}
}
