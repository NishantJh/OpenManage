package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestValidBuildServerURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://sysupgrade.openwrt.org/":  "https://sysupgrade.openwrt.org",
		"https://asu.example.com/sub":      "https://asu.example.com/sub",
		"http://192.168.1.20:8000":         "http://192.168.1.20:8000",
		"http://asu.lan":                   "",
		"http://8.8.8.8":                   "",
		"https://user:pw@asu.example.com":  "",
		"https://asu.example.com/?token=x": "",
		"ftp://asu.example.com":            "",
		"sysupgrade.openwrt.org":           "",
	} {
		got, ok := validBuildServerURL(raw)
		if (want == "") == ok || got != want {
			t.Errorf("%q: got %q %v, want %q", raw, got, ok, want)
		}
	}
}

func TestCompareReleases(t *testing.T) {
	ordered := []string{"23.05.5", "24.10.0-rc1", "24.10.0", "24.10.4", "24.10-SNAPSHOT", "25.12.0-rc2", "25.12.0", "25.12.10", "SNAPSHOT"}
	for i := 1; i < len(ordered); i++ {
		if compareReleases(ordered[i-1], ordered[i]) >= 0 {
			t.Errorf("%s should sort before %s", ordered[i-1], ordered[i])
		}
	}
	if releaseBranch("25.12.2") != "25.12" || releaseBranch("24.10-SNAPSHOT") != "24.10" || releaseBranch("SNAPSHOT") != "SNAPSHOT" {
		t.Fatal("branch of a release")
	}
	for _, bad := range []string{"", "25.12", "../x", "25.12.2/../../", "25.12.2 "} {
		if releaseVersionPattern.MatchString(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestResolveProfile(t *testing.T) {
	p := asuProfiles{Profiles: map[string]asuProfile{"cudy_wr3000-v1": {SupportedDevices: []string{"cudy,wr3000-v1", "cudy,wr3000-v1-alt"}}}}
	if got, ok := resolveProfile("cudy,wr3000-v1", p); !ok || got != "cudy_wr3000-v1" {
		t.Fatalf("direct: %q %v", got, ok)
	}
	if got, ok := resolveProfile("cudy,wr3000-v1-alt", p); !ok || got != "cudy_wr3000-v1" {
		t.Fatalf("alias: %q %v", got, ok)
	}
	if _, ok := resolveProfile("other,board", p); ok {
		t.Fatal("unknown board must not resolve")
	}
}

func TestPlanPackages(t *testing.T) {
	m := firmwareManifest{
		UserInstalled:   []string{"luci-theme-fluent", "wpad-openssl", "libfoo20240329", "old-dropped", "old-name"},
		RemovedDefaults: []string{"wpad-basic-mbedtls", "gone-default"},
	}
	defaults := map[string]bool{"wpad-basic-mbedtls": true, "busybox": true}
	available := map[string]bool{"wpad-openssl": true, "libfoo": true, "new-name": true}
	changes := []asuPackageChange{
		{Source: "old-name", Target: "new-name", Revision: 100},
		{Source: "old-dropped", Revision: 100},
		{Source: "wpad-openssl", Target: "future", Revision: 999},
	}
	var plan firmwarePlan
	planPackages(m, defaults, available, changes, 500, &plan)
	if !slices.Equal(plan.Packages, []string{"libfoo20240329", "new-name", "wpad-openssl", "-wpad-basic-mbedtls"}) {
		t.Fatalf("packages = %v", plan.Packages)
	}
	if !slices.Equal(plan.Unavailable, []string{"luci-theme-fluent"}) || !slices.Equal(plan.Dropped, []string{"old-dropped"}) ||
		len(plan.Renamed) != 1 || plan.Renamed[0] != (packageRename{"old-name", "new-name"}) {
		t.Fatalf("plan = %+v", plan)
	}
	if !slices.Equal(plan.RemovedDefaults, []string{"wpad-basic-mbedtls"}) {
		t.Fatalf("removed defaults = %v (a removal that is no longer a default is not requested)", plan.RemovedDefaults)
	}
}

// fakeASU serves the ASU and upstream JSON the plan reads, in the shapes the ASU
// server (asu/main.py, asu/util.py) and owut use.
func fakeASU(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var srv *httptest.Server
	files := map[string]string{
		"/json/v1/overview.json": `{"latest":["25.12.2","24.10.4"],"upstream_url":"UPSTREAM","branches":{
			"25.12":{"enabled":true,"path":"releases/{version}","versions":["25.12.2","25.12.1","25.12-SNAPSHOT"],"targets":{"mediatek/filogic":"aarch64_cortex-a53","x86/64":"x86_64"},
				"package_changes":[{"source":"luci-app-oldname","target":"luci-app-newname","revision":30000}]},
			"24.10":{"enabled":true,"path":"releases/{version}","versions":["24.10.4","24.10.0"],"targets":{"mediatek/filogic":"aarch64_cortex-a53"}},
			"21.02":{"enabled":false,"path":"releases/{version}","versions":["21.02.7"]}}}`,
		"/releases/25.12.2/targets/mediatek/filogic/profiles.json": `{"version_code":"r32400-abc","arch_packages":"aarch64_cortex-a53",
			"default_packages":["busybox","dnsmasq","wpad-basic-mbedtls","luci"],"linux_kernel":{"version":"6.12.74"},
			"profiles":{"cudy_wr3000-v1":{"device_packages":["kmod-mt7915e","-dnsmasq"],"supported_devices":["cudy,wr3000-v1"],"titles":[{"vendor":"Cudy","model":"WR3000","variant":"v1"}]}}}`,
		"/json/v1/releases/25.12.2/targets/mediatek/filogic/index.json":    `{"architecture":"aarch64_cortex-a53","packages":{"kmod-mt7915e":"6.12"}}`,
		"/json/v1/releases/25.12.2/packages/aarch64_cortex-a53-index.json": `{"luci-app-newname":"1.0","tcpdump":"4.99","wpad-openssl":"2025"}`,
	}
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.ReplaceAll(body, "UPSTREAM", srv.URL)))
	}))
	t.Cleanup(srv.Close)
	old := buildServer
	buildServer = newBuildServerClient()
	buildServer.http.Transport = srv.Client().Transport
	t.Cleanup(func() { buildServer = old })
	return srv, &hits
}

func TestFirmwarePlanAgainstFakeASU(t *testing.T) {
	srv, hits := fakeASU(t)
	ctx := context.Background()
	ov, err := fetchOverview(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, r := range overviewReleases(ov) {
		versions = append(versions, r.Version)
	}
	if !slices.Equal(versions, []string{"25.12-SNAPSHOT", "25.12.2", "25.12.1", "24.10.4", "24.10.0"}) {
		t.Fatalf("releases = %v (disabled branches are hidden, newest first)", versions)
	}

	m := firmwareManifest{
		Release: "25.12.1", Target: "mediatek/filogic", BoardName: "cudy,wr3000-v1", PackageManager: "apk", ROMKnown: true,
		UserInstalled:   []string{"luci-theme-fluent", "luci-app-oldname", "tcpdump"},
		RemovedDefaults: []string{"wpad-basic-mbedtls", "dnsmasq"},
	}
	plan, err := firmwarePlanFor(ctx, srv.URL, m, "25.12.2")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Profile != "cudy_wr3000-v1" || plan.ProfileTitle != "Cudy WR3000 v1" || plan.VersionCode != "r32400-abc" || plan.Kernel != "6.12.74" || plan.PackageManager != "apk" {
		t.Fatalf("plan identity = %+v", plan)
	}
	if !slices.Equal(plan.Packages, []string{"luci-app-newname", "tcpdump", "-wpad-basic-mbedtls"}) {
		t.Fatalf("packages = %v (dnsmasq is not a default for this profile, so it is not removed again)", plan.Packages)
	}
	if !slices.Equal(plan.Unavailable, []string{"luci-theme-fluent"}) || len(plan.Blockers) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	before := hits.Load()
	if _, err := firmwarePlanFor(ctx, srv.URL, m, "25.12.2"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != before {
		t.Fatal("release metadata should come from the cache the second time")
	}

	m.BoardName = "unknown,board"
	if plan, err = firmwarePlanFor(ctx, srv.URL, m, "25.12.2"); err != nil || len(plan.Blockers) != 1 {
		t.Fatalf("unknown board: %v %+v", err, plan.Blockers)
	}
	m.BoardName = "cudy,wr3000-v1"
	if plan, err = firmwarePlanFor(ctx, srv.URL, m, "24.10.4"); err != nil || len(plan.Blockers) == 0 || plan.PackageManager != "opkg" {
		t.Fatalf("apk to opkg must be blocked: %v %+v", err, plan)
	}
	if _, err = firmwarePlanFor(ctx, srv.URL, m, "21.02.7"); err == nil {
		t.Fatal("a disabled branch is not offered")
	}
}

func TestBuildServerRefusesCrossHostRedirect(t *testing.T) {
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer srv.Close()
	c := newBuildServerClient()
	c.http.Transport = srv.Client().Transport
	var out map[string]any
	if err := c.getJSON(context.Background(), srv.URL+"/json/v1/overview.json", &out); err == nil {
		t.Fatal("a redirect to another host must be refused")
	}
}

func TestFirmwareEndpointRoles(t *testing.T) {
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		role    string
	}{
		"settings update needs admin": {(&app{}).updateFirmwareSettings, "operator"},
		"releases need operator":      {(&app{}).listFirmwareReleases, "readonly"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", strings.NewReader(`{}`))
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: tc.role}))
		rec := httptest.NewRecorder()
		tc.handler(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
	}
}
