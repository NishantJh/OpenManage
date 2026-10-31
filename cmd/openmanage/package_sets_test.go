package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPackageSetRevisionLifecycle(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	var sourceID int64
	if err := db.QueryRow(ctx, `INSERT INTO package_sources (name,version,arch,format,sha256,size,file,info,status)
		VALUES ('luci-theme-demo','1.0','all','apk',$1,1,'x','{"format":"apk","name":"luci-theme-demo","version":"1.0","arch":"all"}','confirmed')
		RETURNING id`, strings.Repeat("a", 64)).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}

	call := func(role, method, path, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.SetPathValue("id", deviceID)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: role}))
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	first, _ := json.Marshal(packageSetRequest{
		OfficialPackages: []string{"ca-bundle"},
		RemovedDefaults:  []string{"ppp"},
		Sources:          []packageSetSourceInput{{Name: "luci-theme-demo", Mode: "pinned", SourceID: &sourceID}},
	})
	rec := call("admin", http.MethodPut, "/x", string(first), a.putDevicePackageSet)
	if rec.Code != http.StatusOK {
		t.Fatalf("first revision: %d %s", rec.Code, rec.Body)
	}
	var got packageSetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CurrentRevision != 1 || got.Current == nil || got.Current.Sources[0].Version != "1.0" {
		t.Fatalf("first revision: %+v", got)
	}

	second, _ := json.Marshal(packageSetRequest{OfficialPackages: []string{"curl"}})
	rec = call("admin", http.MethodPut, "/x", string(second), a.putDevicePackageSet)
	if rec.Code != http.StatusOK {
		t.Fatalf("second revision: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CurrentRevision != 2 || len(got.Revisions) != 2 {
		t.Fatalf("second revision: %+v", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.SetPathValue("id", deviceID)
	req.SetPathValue("revision", "1")
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	rec = httptest.NewRecorder()
	a.restoreDevicePackageSetRevision(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CurrentRevision != 3 || got.Current == nil || got.Current.BasedOnRevision == nil || *got.Current.BasedOnRevision != 1 ||
		len(got.Current.OfficialPackages) != 1 || got.Current.OfficialPackages[0] != "ca-bundle" {
		t.Fatalf("restored revision: %+v", got)
	}
	var audited int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action IN ('device.package_set_revision','device.package_set_restore')`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 3 {
		t.Fatalf("audit events=%d, want 3", audited)
	}
	var installs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM package_installs WHERE device_id=$1`, deviceID).Scan(&installs); err != nil {
		t.Fatal(err)
	}
	if installs != 0 {
		t.Fatalf("saving a desired set must not install packages; installs=%d", installs)
	}
}

func TestPackageSetValidationAndAuthorization(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	deviceID, userID := seedDeviceAndUser(t, db)
	call := func(role, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(body))
		req.SetPathValue("id", deviceID)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: role}))
		rec := httptest.NewRecorder()
		a.putDevicePackageSet(rec, req)
		return rec
	}
	if rec := call("operator", `{"official_packages":["curl"]}`); rec.Code != http.StatusForbidden {
		t.Fatalf("operator write status=%d", rec.Code)
	}
	for _, body := range []string{
		`{"official_packages":["bad name"]}`,
		`{"official_packages":["curl","curl"]}`,
		`{"official_packages":["curl"],"removed_defaults":["curl"]}`,
		`{"sources":[{"name":"theme","mode":"latest"}]}`,
		`{"sources":[{"name":"theme","mode":"pinned"}]}`,
	} {
		if rec := call("admin", body); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid body %s: status=%d body=%s", body, rec.Code, rec.Body)
		}
	}
	if rec := call("admin", `{"official_packages":["curl"]}`); rec.Code != http.StatusOK {
		t.Fatalf("valid write: %d %s", rec.Code, rec.Body)
	}
	if rec := call("admin", `{"unexpected":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown fields must be rejected: %d", rec.Code)
	}
}

func TestPackageSetSourceDeletionIsRestricted(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	var sourceID int64
	if err := db.QueryRow(ctx, `INSERT INTO package_sources (name,version,arch,format,sha256,size,file,info,status)
		VALUES ('theme','1','all','ipk',$1,1,'x','{"format":"ipk","name":"theme","version":"1","arch":"all"}','confirmed')
		RETURNING id`, strings.Repeat("b", 64)).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(packageSetRequest{Sources: []packageSetSourceInput{{Name: "theme", Mode: "pinned", SourceID: &sourceID}}})
	req := httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader(body))
	req.SetPathValue("id", deviceID)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	rec := httptest.NewRecorder()
	a.putDevicePackageSet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	deleteReq := httptest.NewRequest(http.MethodDelete, "/x", nil)
	deleteReq.SetPathValue("id", fmt.Sprint(sourceID))
	deleteReq = deleteReq.WithContext(context.WithValue(deleteReq.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
	deleteRec := httptest.NewRecorder()
	a.deletePackageSource(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusConflict {
		t.Fatalf("deleting a pinned source: status=%d body=%s", deleteRec.Code, deleteRec.Body)
	}
	_, err := db.Exec(ctx, `DELETE FROM package_sources WHERE id=$1`, sourceID)
	if err == nil {
		t.Fatal("the database must also restrict deletion of a pinned source")
	}
}
