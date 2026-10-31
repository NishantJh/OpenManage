package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every JSON write endpoint must reject malformed, unknown-field, trailing, oversized,
// and over-length input before touching the database (the app here has no pool).
func TestWriteHandlersRejectBadBodiesBeforeDatabase(t *testing.T) {
	admin := context.WithValue(context.Background(), userContextKey, authUser{ID: "user-id", Role: "admin"})
	handlers := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
		valid   string
		limit   int
	}{
		{name: "login", handler: (&app{}).login, method: http.MethodPost, path: "/api/auth/login", valid: `{"username":"admin","password":"pw"}`, limit: loginBodyLimit},
		{name: "create device", handler: (&app{}).createDevice, method: http.MethodPost, path: "/api/devices", valid: `{"name":"router","address":"192.168.1.1"}`, limit: deviceBodyLimit},
		{name: "monitoring settings", handler: (&app{}).updateMonitoringSettings, method: http.MethodPut, path: "/api/monitoring/settings", valid: `{"enabled":false,"interval_seconds":300}`, limit: monitoringBodyLimit},
		{name: "settings batch", handler: (&app{}).previewConfigChangeBatch, method: http.MethodPost, path: "/api/devices/550e8400-e29b-41d4-a716-446655440000/config-change-batches", valid: `{"base_versions":{"system":"opaque"},"changes":[{"setting":"system.hostname","value":"router"}]}`, limit: configBatchBodyLimit},
	}
	for _, h := range handlers {
		bodies := map[string]string{
			"truncated":     "{",
			"unknown field": strings.TrimSuffix(h.valid, "}") + `,"unexpected":true}`,
			"trailing JSON": h.valid + ` {}`,
			"trailing text": h.valid + ` x`,
			"oversized":     `{"name":"` + strings.Repeat("x", h.limit) + `"}`,
		}
		for label, body := range bodies {
			t.Run(h.name+"/"+label, func(t *testing.T) {
				req := httptest.NewRequest(h.method, h.path, strings.NewReader(body)).WithContext(admin)
				req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
				rec := httptest.NewRecorder()
				h.handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), "xxxxxxxx") {
					t.Fatal("request content must not be echoed")
				}
			})
		}
	}
}

func TestFieldLengthLimitsRejectBeforeDatabase(t *testing.T) {
	admin := context.WithValue(context.Background(), userContextKey, authUser{ID: "user-id", Role: "admin"})
	long := strings.Repeat("a", maxDeviceFieldLength+1)
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		path    string
		body    string
	}{
		{name: "long username", handler: (&app{}).login, path: "/api/auth/login", body: `{"username":"` + strings.Repeat("u", maxUsernameLength+1) + `","password":"pw"}`},
		{name: "long password", handler: (&app{}).login, path: "/api/auth/login", body: `{"username":"admin","password":"` + strings.Repeat("p", maxPasswordLength+1) + `"}`},
		{name: "long device name", handler: (&app{}).createDevice, path: "/api/devices", body: `{"name":"` + long + `","address":"192.168.1.1"}`},
		{name: "long address", handler: (&app{}).createDevice, path: "/api/devices", body: `{"name":"r","address":"` + strings.Repeat("1", maxDeviceAddressLength+1) + `"}`},
		{name: "long model", handler: (&app{}).createDevice, path: "/api/devices", body: `{"name":"r","address":"192.168.1.1","model":"` + long + `"}`},
		{name: "long firmware", handler: (&app{}).createDevice, path: "/api/devices", body: `{"name":"r","address":"192.168.1.1","firmware":"` + long + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(admin)
			rec := httptest.NewRecorder()
			tc.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAPIDeadlineAppliesOnlyToAPIRoutes(t *testing.T) {
	for _, tc := range []struct {
		path         string
		wantDeadline bool
	}{
		{path: "/api/devices", wantDeadline: true},
		{path: "/api/healthz", wantDeadline: true},
		{path: "/", wantDeadline: false},
	} {
		var deadline time.Time
		var ok bool
		h := apiDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			deadline, ok = r.Context().Deadline()
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tc.path, nil))
		if ok != tc.wantDeadline {
			t.Fatalf("%s: deadline present=%v, want %v", tc.path, ok, tc.wantDeadline)
		}
		if ok && time.Until(deadline) > apiRequestTimeout {
			t.Fatalf("%s: deadline %v exceeds apiRequestTimeout", tc.path, time.Until(deadline))
		}
	}
}

func TestBackupCreateGetsLongerDeadline(t *testing.T) {
	var got time.Duration
	h := apiDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, _ := r.Context().Deadline()
		got = time.Until(d)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/devices/x/backups", nil))
	if got <= apiRequestTimeout {
		t.Fatalf("backup create deadline %v should exceed apiRequestTimeout", got)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/devices/x/backups", nil))
	if got > apiRequestTimeout {
		t.Fatalf("listing backups deadline %v should not exceed apiRequestTimeout", got)
	}
}

// Device operations and the API deadline must leave room for the server to write a response.
func TestTimeoutsStayBelowServerWriteTimeout(t *testing.T) {
	if apiRequestTimeout >= serverWriteTimeout {
		t.Fatalf("apiRequestTimeout %v must be below WriteTimeout %v", apiRequestTimeout, serverWriteTimeout)
	}
	if healthCheckTimeout >= apiRequestTimeout {
		t.Fatalf("healthCheckTimeout %v must be below apiRequestTimeout %v", healthCheckTimeout, apiRequestTimeout)
	}
	if enrollmentTimeout >= apiRequestTimeout {
		t.Fatalf("enrollmentTimeout %v must be below apiRequestTimeout %v", enrollmentTimeout, apiRequestTimeout)
	}
}
