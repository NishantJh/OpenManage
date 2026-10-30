package main

import (
	"context"
	"net/http"
	"testing"
)

func TestNewerReleaseFile(t *testing.T) {
	for _, c := range []struct{ file, oldTag, newTag, want string }{
		{"luci-theme-fluent-1.2.0.apk", "v1.2.0", "v1.3.0", "luci-theme-fluent-1.3.0.apk"},
		{"theme_v1.2.0_all.ipk", "v1.2.0", "v1.3.0", "theme_v1.3.0_all.ipk"},
		{"theme.apk", "v1.2.0", "v1.3.0", "theme.apk"},
		{"theme-2.0.apk", "2.0", "2.1", "theme-2.1.apk"},
	} {
		if got := newerReleaseFile(c.file, c.oldTag, c.newTag); got != c.want {
			t.Errorf("newerReleaseFile(%q,%q,%q) = %q, want %q", c.file, c.oldTag, c.newTag, got, c.want)
		}
	}
}

func TestReleaseParts(t *testing.T) {
	repo, tag, file, err := releaseParts("https://github.com/o/r/releases/download/v1/x.apk")
	if err != nil || repo != "o/r" || tag != "v1" || file != "x.apk" {
		t.Fatalf("got %q %q %q %v", repo, tag, file, err)
	}
	for _, bad := range []string{"upload", "https://example.com/o/r/releases/download/v1/x.apk", ""} {
		if _, _, _, err := releaseParts(bad); err == nil {
			t.Errorf("%q must not be checkable", bad)
		}
	}
}

func TestLatestReleaseTag(t *testing.T) {
	location := "/o/r/releases/tag/v2.0.0"
	useLocalPackageServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/o/r/releases/latest" {
			http.NotFound(w, r)
			return
		}
		if location == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, location, http.StatusFound)
	}))
	if tag, err := latestReleaseTag(context.Background(), "o/r"); err != nil || tag != "v2.0.0" {
		t.Fatalf("tag %q err %v", tag, err)
	}
	for _, bad := range []string{"", "https://evil.example/o/r/releases/tag/v2", "/other/repo/releases/tag/v2", "/o/r/releases/tag/a%2Fb"} {
		location = bad
		if tag, err := latestReleaseTag(context.Background(), "o/r"); err == nil {
			t.Errorf("location %q gave tag %q, want an error", bad, tag)
		}
	}
}
