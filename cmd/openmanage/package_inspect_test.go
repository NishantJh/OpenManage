package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/packages/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func scriptNames(info packageInfo) string {
	var names []string
	for _, s := range info.Scripts {
		names = append(names, s.Name)
	}
	return strings.Join(names, ",")
}

// The fixtures are the metadata of the real ca-bundle package: its .ipk from the 24.10
// feed and its .apk from the 25.12 feed, with the package contents cut out.
func TestInspectIPK(t *testing.T) {
	info, err := inspectPackage(fixture(t, "ca-bundle.ipk"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "ipk" || info.Name != "ca-bundle" || info.Version != "20260816-r2" || info.Arch != "all" {
		t.Fatalf("unexpected info %+v", info)
	}
	if len(info.Depends) != 1 || info.Depends[0] != "libc" || info.InstalledSize != 194560 {
		t.Fatalf("unexpected depends or size %+v", info)
	}
	if scriptNames(info) != "postinst,prerm" || !strings.Contains(info.Scripts[0].Body, "#!/bin/sh") {
		t.Fatalf("unexpected scripts %+v", info.Scripts)
	}
	if info.Description != "System CA certificates as a bundle" {
		t.Fatalf("description %q", info.Description)
	}
}

func TestInspectIPKAsArArchive(t *testing.T) {
	control := fixture(t, "ca-bundle.control.tar.gz")
	var ar bytes.Buffer
	ar.WriteString("!<arch>\n")
	member := func(name string, body []byte) {
		fmt.Fprintf(&ar, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(body))
		ar.Write(body)
		if len(body)%2 == 1 {
			ar.WriteByte('\n')
		}
	}
	member("debian-binary/", []byte("2.0\n"))
	member("control.tar.gz/", control)
	member("data.tar.gz/", []byte("x"))
	info, err := inspectPackage(ar.Bytes())
	if err != nil || info.Name != "ca-bundle" || info.Arch != "all" {
		t.Fatalf("ar archive: %+v %v", info, err)
	}
}

func TestInspectAPK(t *testing.T) {
	info, err := inspectPackage(fixture(t, "ca-bundle.apk"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "apk" || info.Name != "ca-bundle" || info.Version != "20260816-r2" || info.Arch != "noarch" {
		t.Fatalf("unexpected info %+v", info)
	}
	if len(info.Depends) != 1 || info.Depends[0] != "libc" || info.InstalledSize != 180797 {
		t.Fatalf("unexpected depends or size %+v", info)
	}
	if scriptNames(info) != "postinst,prerm,post-upgrade" || !strings.Contains(info.Scripts[0].Body, "#!/bin/sh") {
		t.Fatalf("unexpected scripts %+v", info.Scripts)
	}
}

// Damaged and hostile input must give an error, never a panic or a huge read.
func TestInspectPackageRejectsBadInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":     {},
		"text":      []byte("not a package"),
		"gzip junk": append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte{7}, 50)...),
		"ar junk":   []byte("!<arch>\nnonsense"),
		"adb junk":  []byte("ADBdnot deflate"),
		"adb plain": append([]byte("ADB.pckg"), bytes.Repeat([]byte{0xff}, 40)...),
	}
	for name, data := range cases {
		if _, err := inspectPackage(data); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	for _, name := range []string{"ca-bundle.apk", "ca-bundle.ipk"} {
		full := fixture(t, name)
		for cut := 0; cut < len(full); cut += 7 {
			_, _ = inspectPackage(full[:cut])
		}
		for i := 0; i < len(full); i += 5 {
			mutated := append([]byte(nil), full...)
			mutated[i] ^= 0xff
			_, _ = inspectPackage(mutated)
		}
	}
	if _, err := inspectPackage(make([]byte, packageMaxFileBytes+1)); err == nil {
		t.Error("an oversized file must be refused")
	}
}

func TestInspectPackageRejectsBadNames(t *testing.T) {
	control := "Package: ../evil\nVersion: 1\nArchitecture: all\n"
	var info packageInfo
	parseIPKControl(control, &info)
	if packageNamePattern.MatchString(info.Name) {
		t.Fatal("a path-like package name must not be accepted")
	}
}

// Text that would read differently from what runs (a right-to-left override, a raw
// escape sequence, invalid UTF-8) is shown as visible escapes, never silently dropped.
func TestCleanPackageTextShowsHiddenCharacters(t *testing.T) {
	got := cleanPackageText("echo ok \u202e# \x1b[2Jrm\xff -rf\r\n\tdone", 1000)
	want := "echo ok \\u{202E}# \\u{001B}[2Jrm\\xff -rf\\u{000D}\n\tdone"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestScriptTooLargeToReviewIsRefused(t *testing.T) {
	if _, err := scriptFrom("postinst", bytes.Repeat([]byte("a"), packageMaxFieldBytes+1)); err == nil {
		t.Fatal("a script too large to show in full must be refused")
	}
	s, err := scriptFrom("postinst", bytes.Repeat([]byte("a"), packageMaxFieldBytes))
	if err != nil || len(s.Body) != packageMaxFieldBytes {
		t.Fatalf("a 64 KiB script must be kept whole: %v %d", err, len(s.Body))
	}
}
