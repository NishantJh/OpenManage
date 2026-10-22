package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		t.Run(strconv.Itoa(interval), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/monitoring/settings",
				strings.NewReader(`{"enabled":true,"interval_seconds":`+strconv.Itoa(interval)+`}`))
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

func TestMonitoringWorkerCountIsBounded(t *testing.T) {
	for _, tc := range []struct {
		batchSize int
		want      int
	}{
		{batchSize: -1, want: 0},
		{batchSize: 0, want: 0},
		{batchSize: 1, want: 1},
		{batchSize: 2, want: 2},
		{batchSize: 3, want: 3},
		{batchSize: 4, want: 3},
		{batchSize: 5, want: 3},
		{batchSize: 100, want: 3},
	} {
		if got := monitoringWorkerCount(tc.batchSize); got != tc.want {
			t.Errorf("monitoringWorkerCount(%d) = %d, want %d", tc.batchSize, got, tc.want)
		}
	}
}

func TestMonitoringSettingsRejectOutOfRangeSnapshotInterval(t *testing.T) {
	for _, interval := range []int{0, 60, 899, 86401} {
		t.Run(strconv.Itoa(interval), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/monitoring/settings",
				strings.NewReader(`{"enabled":false,"interval_seconds":300,"snapshots_enabled":true,"snapshot_interval_seconds":`+strconv.Itoa(interval)+`}`))
			req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "admin-id", Role: "admin"}))
			rec := httptest.NewRecorder()
			(&app{}).updateMonitoringSettings(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("snapshot interval %d: expected 400, got %d: %s", interval, rec.Code, rec.Body.String())
			}
		})
	}
}
