package main

import (
	"context"
	"slices"
	"testing"
)

func TestDiffAgainstStock(t *testing.T) {
	m := firmwareManifest{
		// luci-theme-fluent and wpad-wolfssl were added; dnsmasq and wpad-basic-mbedtls were
		// replaced; libubox is installed under a dated name; odhcpd is still a stock package.
		Packages: []firmwarePackage{
			{Name: "base-files"}, {Name: "libubox20240329"}, {Name: "odhcpd"}, {Name: "dnsmasq-full"},
			{Name: "wpad-wolfssl"}, {Name: "luci-theme-fluent"}, {Name: "kmod-extra-dep"},
		},
		UserInstalled: []string{"dnsmasq-full", "wpad-wolfssl", "luci-theme-fluent", "odhcpd", "libubox20240329"},
	}
	stock := []string{"base-files", "dnsmasq", "libubox", "odhcpd", "wpad-basic-mbedtls", "uhttpd"}
	added, removed := diffAgainstStock(m, stock)
	if want := []string{"dnsmasq-full", "wpad-wolfssl", "luci-theme-fluent"}; !slices.Equal(added, want) {
		t.Errorf("added = %v, want %v (a stock package the owner reinstalled is not added)", added, want)
	}
	if want := []string{"dnsmasq", "wpad-basic-mbedtls", "uhttpd"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	// Lists are never nil, so the API answers [] and not null.
	a2, r2 := diffAgainstStock(firmwareManifest{}, nil)
	if a2 == nil || r2 == nil {
		t.Error("empty diffs must be empty lists")
	}
}

func TestStockDiffAgainstFakeASU(t *testing.T) {
	srv, _ := fakeASU(t)
	m := firmwareManifest{
		Release: "25.12.2", Target: "mediatek/filogic", BoardName: "cudy,wr3000-v1", PackageManager: "apk", ROMKnown: true,
		Packages: []firmwarePackage{
			{Name: "busybox"}, {Name: "luci"}, {Name: "kmod-mt7915e"}, {Name: "libc"}, // libc: a dependency, in no list
			{Name: "dnsmasq-full"}, {Name: "wpad-wolfssl"}, {Name: "luci-theme-fluent"},
		},
		UserInstalled: []string{"dnsmasq-full", "wpad-wolfssl", "luci-theme-fluent"},
		// What the old image comparison reported: it also lists dnsmasq, which this
		// profile does not ask for, and any package installed and later removed.
		RemovedDefaults: []string{"dnsmasq", "wpad-basic-mbedtls", "luci-app-opkg"},
	}
	diff, err := stockDiffFor(context.Background(), srv.URL, firmwareInventory{ID: 7, Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	if diff.Profile != "cudy_wr3000-v1" || diff.StockCount != 4 || diff.InventoryID != 7 {
		t.Fatalf("diff identity = %+v", diff)
	}
	if !slices.Equal(diff.Added, []string{"dnsmasq-full", "wpad-wolfssl", "luci-theme-fluent"}) {
		t.Errorf("added = %v", diff.Added)
	}
	if !slices.Equal(diff.Removed, []string{"wpad-basic-mbedtls"}) {
		t.Errorf("removed = %v (only the profile's own defaults count)", diff.Removed)
	}
	if diff.Note != "" {
		t.Errorf("unexpected note %q", diff.Note)
	}
	m.Release = "21.02.7"
	if _, err := stockDiffFor(context.Background(), srv.URL, firmwareInventory{Manifest: m}); err == nil {
		t.Error("a release the build server does not offer must fail")
	}
}
