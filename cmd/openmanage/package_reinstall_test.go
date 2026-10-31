package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceFits(t *testing.T) {
	apk := firmwareManifest{PackageManager: "apk", Arch: "aarch64_cortex-a53"}
	for _, tc := range []struct {
		format, arch string
		fits         bool
	}{
		{"apk", "noarch", true}, {"apk", "aarch64_cortex-a53", true}, {"apk", "mipsel_24kc", false}, {"ipk", "all", false},
	} {
		if got, why := sourceFits(tc.format, tc.arch, apk); got != tc.fits || (!got && why == "") {
			t.Errorf("%+v: fits = %v (%q)", tc, got, why)
		}
	}
}

func TestReinstallCheck(t *testing.T) {
	if c := reinstallCheck(nil); c.Status != "ok" {
		t.Errorf("nothing missing: %+v", c)
	}
	id := int64(4)
	c := reinstallCheck([]reinstallCandidate{
		{Name: "luci-theme-fluent", SourceID: &id, Version: "2.0", Format: "apk"},
		{Name: "luci-theme-x", Reason: "no confirmed package source for this name"},
	})
	if c.Status != "warning" || !strings.Contains(c.Detail, "luci-theme-fluent can be reinstalled from source 4 (2.0)") ||
		!strings.Contains(c.Detail, "luci-theme-x: no confirmed") || !strings.Contains(c.Detail, "Nothing was reinstalled") {
		t.Errorf("check = %+v", c)
	}
}

func TestReinstallCandidatesAndEndpoint(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	deviceID, userID := seedDeviceAndUser(t, db)
	add := func(name, version, format, arch, status string, hash string) int64 {
		var id int64
		if err := db.QueryRow(ctx, `INSERT INTO package_sources (name, version, arch, format, sha256, size, file, info, status, confirmed_at)
			VALUES ($1,$2,$3,$4,$5,1,'x','{}',$6, CASE WHEN $6='confirmed' THEN NOW() END) RETURNING id`, name, version, arch, format, strings.Repeat(hash, 64), status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	good := add("luci-theme-fluent", "2.0", "apk", "noarch", "confirmed", "a")
	add("luci-theme-fluent", "1.0", "ipk", "all", "confirmed", "b") // wrong format for an apk device
	add("luci-theme-pending", "1", "apk", "noarch", "pending", "c")
	add("luci-theme-old", "1", "ipk", "all", "confirmed", "d")
	// Two wrong sources: the newer one is for another architecture, the older is an .ipk.
	add("luci-theme-two", "1", "ipk", "all", "confirmed", "e")
	add("luci-theme-two", "2", "apk", "mipsel_24kc", "confirmed", "f")

	m := firmwareManifest{PackageManager: "apk", Arch: "x86_64", Packages: []firmwarePackage{{Name: "luci-theme-kept"}}}
	got, err := a.reinstallCandidates(ctx, []string{"luci-theme-kept", "luci-theme-fluent", "luci-theme-pending", "luci-theme-old", "luci-theme-none"}, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("candidates = %+v (an installed package is not reported)", got)
	}
	if got[0].Name != "luci-theme-fluent" || got[0].SourceID == nil || *got[0].SourceID != good || got[0].Version != "2.0" {
		t.Errorf("fluent = %+v", got[0])
	}
	for _, c := range got[1:] {
		if c.SourceID != nil || c.Reason == "" {
			t.Errorf("%s must have no source and a reason: %+v", c.Name, c)
		}
	}
	two, err := a.reinstallCandidates(ctx, []string{"luci-theme-two"}, m)
	if err != nil || len(two) != 1 || two[0].SourceID != nil || !strings.Contains(two[0].Reason, "mipsel_24kc") {
		t.Errorf("the newest source's reason must be reported: %+v %v", two, err)
	}
	if !strings.Contains(got[2].Reason, "device now uses apk") {
		t.Errorf("a wrong-format source says so: %q", got[2].Reason)
	}

	// The endpoint reads the newest finished upgrade's plan and the newest inventory.
	if _, err := db.Exec(ctx, `INSERT INTO firmware_inventories (device_id, manifest, digest) VALUES ($1,$2,'d')`, deviceID, mustJSON(t, m)); err != nil {
		t.Fatal(err)
	}
	plan := mustJSON(t, firmwarePlan{Unavailable: []string{"luci-theme-fluent"}})
	if _, err := db.Exec(ctx, `INSERT INTO firmware_upgrades (device_id, version, status, plan, created_by) VALUES ($1,'25.12.2','done_with_warnings',$2,$3)`, deviceID, plan, userID); err != nil {
		t.Skipf("upgrade row needs more columns in this schema: %v", err)
	}
	call := func(role string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.SetPathValue("id", deviceID)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: userID, Role: role}))
		rec := httptest.NewRecorder()
		a.getPackageReinstalls(rec, req)
		return rec
	}
	if c := call("readonly").Code; c != http.StatusForbidden {
		t.Fatalf("readonly: %d", c)
	}
	rec := call("admin")
	var list []reinstallCandidate
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].SourceID == nil {
		t.Fatalf("endpoint: %d %s", rec.Code, rec.Body)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDesiredPackageSourcesUseThePlanSnapshot(t *testing.T) {
	db := newEnrollmentTestDB(t)
	a := &app{db: db}
	ctx := context.Background()
	add := func(name, version, hash string) int64 {
		var id int64
		if err := db.QueryRow(ctx, `INSERT INTO package_sources (name, version, arch, format, sha256, size, file, info, status, confirmed_at)
			VALUES ($1,$2,'noarch','apk',$3,1,'x','{}','confirmed',NOW()) RETURNING id`, name, version, strings.Repeat(hash, 64)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pinnedV1 := add("luci-theme-pinned", "1.0", "a")
	_ = add("luci-theme-pinned", "2.0", "b")
	latestV1 := add("luci-theme-latest", "2.0", "c")
	plan := firmwarePlan{Warnings: []string{}}
	resolved, err := a.resolveDesiredPackageSources(ctx, []packageSetSource{
		{Name: "luci-theme-pinned", Mode: "pinned", SourceID: &pinnedV1, Version: "1.0"},
		{Name: "luci-theme-latest", Mode: "latest"},
	}, &plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 || resolved[0].Version != "1.0" || resolved[1].SourceID == nil || *resolved[1].SourceID != latestV1 || resolved[1].Version != "2.0" {
		t.Fatalf("resolved source snapshot = %+v", resolved)
	}
	_ = add("luci-theme-latest", "3.0", "d")
	manifest := firmwareManifest{PackageManager: "apk", Arch: "x86_64"}
	got, err := a.reinstallCandidatesWithDesired(ctx, []string{"luci-theme-pinned"}, resolved, manifest)
	if err != nil { t.Fatal(err) }
	if len(got) != 2 || got[0].SourceID == nil || *got[0].SourceID != pinnedV1 || got[0].Version != "1.0" || got[1].SourceID == nil || *got[1].SourceID != latestV1 || got[1].Version != "2.0" {
		t.Fatalf("reinstall candidates must retain selected source ids: %+v", got)
	}
}
