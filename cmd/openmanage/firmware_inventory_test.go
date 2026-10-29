package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func readInventoryFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The fixtures are the real command output from the OpenWrt rootfs containers the lab
// uses. Containers have no /rom and no board_name, so ROM comparisons are unknown.
func TestParseFirmwareInventoryOpkgFixture(t *testing.T) {
	m, err := parseFirmwareInventory(readInventoryFixture(t, "firmware-inventory-24.10-opkg.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Release != "24.10.4" || m.Target != "x86/64" || m.Arch != "x86_64" || m.PackageManager != "opkg" {
		t.Fatalf("identity = %+v", m)
	}
	if len(m.Packages) != 129 || m.ROMKnown || len(m.UserInstalled) != 0 || len(m.RemovedDefaults) != 0 {
		t.Fatalf("packages=%d rom=%v user=%v removed=%v", len(m.Packages), m.ROMKnown, m.UserInstalled, m.RemovedDefaults)
	}
	i := slices.IndexFunc(m.Packages, func(p firmwarePackage) bool { return p.Name == "libc" })
	if i < 0 || !m.Packages[i].Hold || m.Packages[i].Version == "" {
		t.Fatalf("libc should be held with a version: %+v", m.Packages)
	}
	if !slices.IsSortedFunc(m.Packages, func(a, b firmwarePackage) int { return strings.Compare(a.Name, b.Name) }) {
		t.Fatal("packages must be sorted for a stable digest")
	}
	if len(m.CustomFeeds) != 0 || len(m.KeepList) != 0 || m.Extroot {
		t.Fatalf("feeds=%v keep=%v extroot=%v", m.CustomFeeds, m.KeepList, m.Extroot)
	}
	if m.OverlayTotalKB == 0 || m.OverlayFreeKB == 0 || m.MemTotalKB == 0 || m.MemAvailableKB == 0 {
		t.Fatalf("space and memory missing: %+v", m)
	}
}

func TestParseFirmwareInventoryApkFixture(t *testing.T) {
	m, err := parseFirmwareInventory(readInventoryFixture(t, "firmware-inventory-25.12-apk.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Release != "25.12.2" || m.PackageManager != "apk" || m.ROMKnown {
		t.Fatalf("identity = %+v", m)
	}
	// Without /rom every world entry counts as asked for; constraints are stripped.
	if !slices.Contains(m.UserInstalled, "base-files") || !slices.Contains(m.UserInstalled, "kernel") || len(m.UserInstalled) < 100 {
		t.Fatalf("user installed = %v", m.UserInstalled)
	}
}

func TestParseFirmwareInventoryComparesWithROM(t *testing.T) {
	out := strings.Join([]string{
		"@@board", `{"board_name":"cudy,wr3000-v1","model":"Cudy WR3000 v1","release":{"version":"24.10.4","revision":"r1","target":"mediatek/filogic"}}`,
		"@@arch", "aarch64_cortex-a53",
		"@@manager", "opkg",
		"@@installed",
		"busybox\t1.36\tok",
		"wpad-openssl\t2024\tuser",
		"luci-theme-fluent\t1.0\tuser,hold",
		"luci\t25\tuser", // in the image, then reinstalled by hand: still a default
		"bad name\t1\tuser",
		"@@world",
		"@@rom",
		"@@rom_installed",
		"busybox\t1.36\tok",
		"wpad-basic-mbedtls\t2024\tok",
		"luci\t25\tok",
		"@@rom_world",
		"@@feeds", "src/gz mine https://user:token@example.com/feed?key=secret#x",
		"@@keep", "/etc/rc.local", "", "# only comments are filtered on the device",
		"@@fstab", "fstab.@mount[0]=mount", "fstab.@mount[0].target='/overlay'", "fstab.@mount[0].enabled='1'",
		"@@df", "Filesystem 1K-blocks Used Available Use% Mounted on", "/dev/ubi0_1 100 40 60 40% /overlay",
		"@@mem", "MemTotal: 500 kB", "MemAvailable: 300 kB",
	}, "\n")
	m, err := parseFirmwareInventory(out)
	if err != nil {
		t.Fatal(err)
	}
	if m.BoardName != "cudy,wr3000-v1" || !m.ROMKnown {
		t.Fatalf("identity = %+v", m)
	}
	if !slices.Equal(m.UserInstalled, []string{"luci-theme-fluent", "wpad-openssl"}) {
		t.Fatalf("user installed = %v", m.UserInstalled)
	}
	if !slices.Equal(m.RemovedDefaults, []string{"wpad-basic-mbedtls"}) {
		t.Fatalf("removed defaults = %v", m.RemovedDefaults)
	}
	if len(m.Packages) != 4 {
		t.Fatalf("invalid package names must be dropped: %+v", m.Packages)
	}
	feed := strings.Join(m.CustomFeeds, " ")
	if strings.Contains(feed, "token") || strings.Contains(feed, "secret") || !strings.Contains(feed, "example.com/feed") {
		t.Fatalf("feed not redacted: %q", feed)
	}
	if !m.Extroot || m.OverlayTotalKB != 100 || m.OverlayFreeKB != 60 || m.MemAvailableKB != 300 {
		t.Fatalf("extroot/space = %+v", m)
	}
	if !slices.Equal(m.KeepList, []string{"/etc/rc.local", "# only comments are filtered on the device"}) {
		t.Fatalf("keep list = %v", m.KeepList)
	}
}

func TestParseFirmwareInventoryApkWithROM(t *testing.T) {
	out := strings.Join([]string{
		"@@board", `{"board_name":"x","release":{"version":"25.12.2","target":"t/s"}}`,
		"@@manager", "apk",
		"@@installed", "busybox\t1\t-", "luci-theme-fluent\t2\t-", "dnsmasq-full\t2\t-",
		"@@world", "busybox", "luci-theme-fluent@custom", "dnsmasq-full>2", "not-installed",
		"@@rom", "@@rom_installed", "busybox\t1\t-", "dnsmasq\t2\t-",
		"@@rom_world", "busybox", "dnsmasq",
	}, "\n")
	m, err := parseFirmwareInventory(out)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.UserInstalled, []string{"dnsmasq-full", "luci-theme-fluent"}) || !slices.Equal(m.RemovedDefaults, []string{"dnsmasq"}) {
		t.Fatalf("user=%v removed=%v", m.UserInstalled, m.RemovedDefaults)
	}
}

func TestParseFirmwareInventoryRejectsNonOpenWrt(t *testing.T) {
	for name, out := range map[string]string{
		"empty":      "",
		"no release": "@@board\n{\"board_name\":\"x\"}\n@@manager\nopkg",
		"no manager": "@@board\n{\"release\":{\"version\":\"24.10.4\"}}\n@@manager\ndpkg",
	} {
		if _, err := parseFirmwareInventory(out); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestFstabExtrootIgnoresDisabledMounts(t *testing.T) {
	if fstabHasExtroot([]string{"fstab.a.target='/overlay'", "fstab.a.enabled='0'", "fstab.b.target='/mnt/usb'"}) {
		t.Fatal("a disabled /overlay mount is not extroot")
	}
}

func TestFirmwareInventoryOperationIsFixedAndReadOnly(t *testing.T) {
	op := firmwareInventoryOperation
	if op.Class != operationRead || op.allows("readonly") || !op.allows("operator") || !op.allows("admin") {
		t.Fatal("inventory is a read operation that admins and operators start")
	}
	if !strings.HasPrefix(op.command, "exec 2>/dev/null") {
		t.Fatal("stderr must be dropped")
	}
}

func TestFirmwareInventoryEndpointRoles(t *testing.T) {
	dev := "550e8400-e29b-41d4-a716-446655440000"
	req := httptest.NewRequest(http.MethodPost, "/api/devices/"+dev+"/firmware-inventory", nil)
	req.SetPathValue("id", dev)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, authUser{ID: "u", Role: "readonly"}))
	rec := httptest.NewRecorder()
	(&app{}).captureFirmwareInventory(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("readonly capture status = %d, want 403", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/devices/"+dev+"/firmware-inventory", nil)
	req.SetPathValue("id", dev)
	rec = httptest.NewRecorder()
	(&app{}).getFirmwareInventory(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read status = %d, want 401", rec.Code)
	}
}

func TestFirmwareManifestDigestIgnoresFreeSpaceAndMemory(t *testing.T) {
	a := firmwareManifest{Release: "24.10.4", OverlayFreeKB: 10, MemAvailableKB: 20, OverlayTotalKB: 100}
	b := a
	b.OverlayFreeKB, b.MemAvailableKB = 11, 21
	da, _ := firmwareManifestDigest(a)
	db, _ := firmwareManifestDigest(b)
	if da != db {
		t.Fatal("free space and memory must not change the digest")
	}
	b.UserInstalled = []string{"luci-theme-fluent"}
	if dc, _ := firmwareManifestDigest(b); dc == da {
		t.Fatal("a package change must change the digest")
	}
}

// The package listing runs on the device, so run its shell function here against a
// fake opkg status file that holds a removed package's leftover record.
func TestFirmwareInventoryListsOnlyInstalledOpkgPackages(t *testing.T) {
	start := strings.Index(firmwareInventoryCommand, "pkgs() {")
	end := strings.Index(firmwareInventoryCommand[start:], "\n")
	if start < 0 || end < 0 {
		t.Fatal("pkgs function not found in the inventory command")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/usr/lib/opkg", 0o755); err != nil {
		t.Fatal(err)
	}
	status := "Package: busybox\nVersion: 1.36\nStatus: install ok installed\n\n" +
		"Package: luci-theme-old\nVersion: 1.0\nStatus: deinstall user config-files\n\n" +
		"Package: gone\nVersion: 2\nStatus: deinstall ok not-installed\n\n" +
		"Package: luci-theme-fluent\nVersion: 3\nStatus: install user installed\n"
	if err := os.WriteFile(dir+"/usr/lib/opkg/status", []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", firmwareInventoryCommand[start:start+end]+"\npkgs \"$1\"", "sh", dir).Output()
	if err != nil {
		t.Skip("no POSIX shell and awk here:", err)
	}
	if got := string(out); got != "busybox\t1.36\tok\nluci-theme-fluent\t3\tuser\n" {
		t.Fatalf("listing = %q", got)
	}
}
