package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPublicAddress(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.9": false,
		"169.254.1.1": false, "100.64.0.1": false, "0.0.0.0": false, "0.1.2.3": false, "198.18.0.1": false, "255.255.255.255": false, "224.0.0.1": false, "fe80::1": false, "::ffff:192.168.1.5": false,
	} {
		if got := publicAddress(net.ParseIP(ip)); got != want {
			t.Errorf("publicAddress(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestParsePackageURL(t *testing.T) {
	_, origin, err := parsePackageURL("https://github.com/o/r/releases/download/v1/x.apk?token=secret#frag")
	if err != nil || origin != "https://github.com/o/r/releases/download/v1/x.apk" {
		t.Fatalf("origin %q err %v", origin, err)
	}
	for _, bad := range []string{
		"http://github.com/o/r/releases/download/v1/x.apk", "ftp://x/y", "https://user:pw@github.com/o/r/releases/download/v1/x.apk",
		"https:///x", "", "not a url", "https://github.com/o/r/releases/download/v1/" + strings.Repeat("a", 600),
		"https://example.com/o/r/releases/download/v1/x.apk",         // not GitHub
		"https://github.com.evil.example/o/r/releases/download/v1/x", // look-alike host
		"https://github.com:8443/o/r/releases/download/v1/x.apk",     // unexpected port
		"https://github.com/o/r/archive/refs/heads/main.zip",         // not a release asset
		"https://github.com/o/r/releases/download/v1/../../x",        // path tricks
		"https://github.com/o/r/releases/latest/download/x.apk",
	} {
		if _, _, err := parsePackageURL(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// rewriteTransport sends every request to a local TLS test server, whatever its host.
type rewriteTransport struct {
	base http.RoundTripper
	host string
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Host = r.host
	return r.base.RoundTrip(req)
}

// useLocalPackageServer lets github.com package downloads reach a local TLS test server.
func useLocalPackageServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	u, _ := url.Parse(srv.URL)
	oldClient := packageFetchClient
	packageFetchClient = &http.Client{
		Transport:     rewriteTransport{base: srv.Client().Transport, host: u.Host},
		CheckRedirect: newPackageFetchClient().CheckRedirect,
	}
	t.Cleanup(func() {
		srv.Close()
		packageFetchClient = oldClient
	})
	return srv
}

func TestPackageDialControlRefusesLocalAddresses(t *testing.T) {
	if err := packageDialControl("tcp", "127.0.0.1:443", nil); err == nil {
		t.Fatal("a download from a loopback address must be refused")
	}
	if err := packageDialControl("tcp", "192.168.1.1:443", nil); err == nil {
		t.Fatal("a download from a private address must be refused")
	}
	if err := packageDialControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Fatalf("a public address is fine: %v", err)
	}
}

func TestFetchPackageFile(t *testing.T) {
	ipk := fixture(t, "ca-bundle.ipk")
	useLocalPackageServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/r/releases/download/v1/ok.ipk":
			w.Write(ipk)
		case "/o/r/releases/download/v1/big.ipk":
			w.Header().Set("Content-Length", fmt.Sprint(packageMaxFileBytes+1))
			w.Write([]byte("x"))
		case "/o/r/releases/download/v1/redirect":
			http.Redirect(w, r, "http://example.invalid/x", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	get := func(name string) ([]byte, error) {
		return fetchPackageFile(context.Background(), "/o/r/releases/download/v1/"+name)
	}
	if data, err := get("ok.ipk"); err != nil || !bytes.Equal(data, ipk) {
		t.Fatalf("download: %v", err)
	}
	for _, name := range []string{"big.ipk", "redirect", "missing"} {
		if _, err := get(name); err == nil {
			t.Errorf("%s must fail", name)
		}
	}
}

func multipartBody(t *testing.T, field string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile(field, "pkg.ipk")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(data)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestPackageSourceLifecycle(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	_, userID := seedDeviceAndUser(t, db)
	ipk := fixture(t, "ca-bundle.ipk")
	sum := sha256.Sum256(ipk)
	hash := hex.EncodeToString(sum[:])

	call := func(role, method, path, contentType string, body *bytes.Buffer, handler http.HandlerFunc, id string) *httptest.ResponseRecorder {
		var req *http.Request
		if body == nil {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, body)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if id != "" {
			req.SetPathValue("id", id)
		}
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: role}))
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	upload := func(role string, data []byte) *httptest.ResponseRecorder {
		body, ct := multipartBody(t, "file", data)
		return call(role, http.MethodPost, "/api/package-sources", ct, body, a.createPackageSource, "")
	}
	list := func(role string) []packageSource {
		rec := call(role, http.MethodGet, "/api/package-sources", "", nil, a.listPackageSources, "")
		var out []packageSource
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list as %s: %d %s", role, rec.Code, rec.Body)
		}
		return out
	}
	confirm := func(role, id, sha string) int {
		return call(role, http.MethodPost, "/x", "application/json", bytes.NewBufferString(`{"sha256":"`+sha+`"}`), a.confirmPackageSource, id).Code
	}

	for _, role := range []string{"operator", "readonly"} {
		if c := upload(role, ipk).Code; c != http.StatusForbidden {
			t.Fatalf("%s upload: %d", role, c)
		}
	}
	if c := upload("admin", []byte("not a package")).Code; c != http.StatusUnprocessableEntity {
		t.Fatalf("a bad file: %d", c)
	}
	rec := upload("admin", ipk)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var src packageSource
	json.Unmarshal(rec.Body.Bytes(), &src)
	if src.Status != "pending" || src.SHA256 != hash || src.Name != "ca-bundle" || src.Origin != "upload" || strings.Contains(rec.Body.String(), "\"file\"") {
		t.Fatalf("unexpected pending source: %s", rec.Body)
	}
	id := fmt.Sprint(src.ID)
	if got := list("operator"); len(got) != 0 {
		t.Fatalf("operators must not see pending sources: %+v", got)
	}
	if got := list("admin"); len(got) != 1 {
		t.Fatalf("admins see the pending source: %+v", got)
	}
	if c := confirm("operator", id, hash); c != http.StatusForbidden {
		t.Fatalf("operator confirm: %d", c)
	}
	if c := confirm("admin", id, strings.Repeat("0", 64)); c != http.StatusBadRequest {
		t.Fatalf("wrong hash: %d", c)
	}
	if c := confirm("admin", id, strings.ToUpper(hash)); c != http.StatusOK {
		t.Fatalf("confirm: %d", c)
	}
	if c := confirm("admin", id, hash); c != http.StatusConflict {
		t.Fatalf("confirming twice: %d", c)
	}
	if rec := upload("admin", ipk); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already_registered") {
		t.Fatalf("the same file twice: %d %s", rec.Code, rec.Body)
	}
	if got := list("operator"); len(got) != 1 || got[0].Status != "confirmed" {
		t.Fatalf("operators see confirmed sources: %+v", got)
	}

	// A pending review expires.
	apk := fixture(t, "ca-bundle.apk")
	rec = upload("admin", apk)
	if rec.Code != http.StatusCreated {
		t.Fatalf("apk upload: %d %s", rec.Code, rec.Body)
	}
	var apkSrc packageSource
	json.Unmarshal(rec.Body.Bytes(), &apkSrc)
	if _, err := db.Exec(ctx, `UPDATE package_sources SET created_at=NOW()-INTERVAL '2 hours' WHERE id=$1`, apkSrc.ID); err != nil {
		t.Fatal(err)
	}
	if c := confirm("admin", fmt.Sprint(apkSrc.ID), apkSrc.SHA256); c != http.StatusConflict {
		t.Fatalf("an expired review: %d", c)
	}

	if c := call("operator", http.MethodDelete, "/x", "", nil, a.deletePackageSource, id).Code; c != http.StatusForbidden {
		t.Fatalf("operator delete: %d", c)
	}
	if c := call("admin", http.MethodDelete, "/x", "", nil, a.deletePackageSource, id).Code; c != http.StatusOK {
		t.Fatalf("delete: %d", c)
	}
	if c := call("admin", http.MethodDelete, "/x", "", nil, a.deletePackageSource, id).Code; c != http.StatusNotFound {
		t.Fatalf("deleting twice: %d", c)
	}
	var audited int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'package_source.%'`).Scan(&audited)
	if audited != 4 { // create ipk, confirm ipk, create apk, delete ipk
		t.Fatalf("audit events = %d, want 4", audited)
	}
}

func TestCreatePackageSourceFromURL(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	_, userID := seedDeviceAndUser(t, db)
	apk := fixture(t, "ca-bundle.apk")
	useLocalPackageServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(apk) }))
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/package-sources", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: "admin"}))
		rec := httptest.NewRecorder()
		a.createPackageSource(rec, req)
		return rec
	}
	if rec := post(`{"url":"http://example.com/x.apk"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("plain http: %d", rec.Code)
	}
	if rec := post(`{"url":"https://example.com/x.apk"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a link that is not a GitHub release: %d", rec.Code)
	}
	rec := post(`{"url":"https://github.com/o/r/releases/download/v1/theme.apk?sig=abc"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create from link: %d %s", rec.Code, rec.Body)
	}
	var src packageSource
	json.Unmarshal(rec.Body.Bytes(), &src)
	if src.Format != "apk" || src.Origin != "https://github.com/o/r/releases/download/v1/theme.apk" || strings.Contains(rec.Body.String(), "sig=abc") {
		t.Fatalf("unexpected source: %s", rec.Body)
	}
}

func TestPackageRedirectsStayOnGitHub(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://github.com/o/r/releases/download/v1/x.apk":       true,
		"https://objects.githubusercontent.com/github-production": true,
		"https://release-assets.githubusercontent.com/x":          true,
		"http://github.com/x":                                     false,
		"https://example.com/x":                                   false,
		"https://githubusercontent.com.evil.example/x":            false,
		"https://evilgithubusercontent.com/x":                     false,
	} {
		u, _ := url.Parse(raw)
		if got := packageRedirectAllowed(u); got != want {
			t.Errorf("packageRedirectAllowed(%s) = %v, want %v", raw, got, want)
		}
	}
}
