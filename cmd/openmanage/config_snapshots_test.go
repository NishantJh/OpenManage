package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfigSnapshotCiphertextIsBoundToDevice(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	a, b := "550e8400-e29b-41d4-a716-446655440000", "550e8400-e29b-41d4-a716-446655440001"
	ct, err := sealSecret(key, []byte("package system"), configSnapshotAAD(a))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSecret(key, ct, configSnapshotAAD(a)); err != nil {
		t.Fatalf("own device must decrypt: %v", err)
	}
	if _, err := openSecret(key, ct, configSnapshotAAD(b)); err == nil {
		t.Fatal("a snapshot copied to another device must not decrypt")
	}
	if _, err := decryptSecret(key, ct); err == nil {
		t.Fatal("a snapshot must not decrypt as an SSH credential")
	}
}

func TestConfigDigestIsKeyedAndStable(t *testing.T) {
	k1, k2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	if configDigest(k1, "x") != configDigest(k1, "x") {
		t.Fatal("digest must be stable")
	}
	if configDigest(k1, "x") == configDigest(k2, "x") || configDigest(k1, "x") == configDigest(k1, "y") {
		t.Fatal("digest must depend on key and content")
	}
}

func TestConfigSnapshotEndpointsRequireAdmin(t *testing.T) {
	id := "550e8400-e29b-41d4-a716-446655440000"
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
		snap    string
	}{
		{name: "capture", handler: (&app{}).captureConfigSnapshot, method: http.MethodPost, path: "/api/devices/" + id + "/config-snapshots"},
		{name: "list", handler: (&app{}).listConfigSnapshots, method: http.MethodGet, path: "/api/devices/" + id + "/config-snapshots"},
		{name: "changes", handler: (&app{}).configSnapshotChanges, method: http.MethodGet, path: "/api/devices/" + id + "/config-snapshots/1/changes", snap: "1"},
	} {
		for _, role := range []string{"operator", "readonly"} {
			t.Run(tc.name+"/"+role, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, tc.path, nil)
				req.SetPathValue("id", id)
				if tc.snap != "" {
					req.SetPathValue("snapshot", tc.snap)
				}
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
