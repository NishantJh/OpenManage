package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginRateLimiterBlocksAfterFailureLimitAndExpires(t *testing.T) {
	now := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	limiter := newLoginRateLimiter(3, 10*time.Minute, 5*time.Minute, 10)
	for i := 0; i < 2; i++ {
		limiter.recordFailure("192.0.2.10", now.Add(time.Duration(i)*time.Second))
	}
	if blocked, _ := limiter.isBlocked("192.0.2.10", now.Add(2*time.Second)); blocked {
		t.Fatal("client should not be blocked before reaching the failure limit")
	}
	limiter.recordFailure("192.0.2.10", now.Add(2*time.Second))
	blocked, retry := limiter.isBlocked("192.0.2.10", now.Add(3*time.Second))
	if !blocked {
		t.Fatal("client should be blocked after reaching the failure limit")
	}
	if retry <= 0 || retry > 5*time.Minute {
		t.Fatalf("unexpected retry duration: %s", retry)
	}
	if blocked, _ := limiter.isBlocked("192.0.2.10", now.Add(5*time.Minute+time.Second)); blocked {
		t.Fatal("client should be unblocked after the block duration")
	}
}

func TestLoginRateLimiterResetsAfterSuccessfulLogin(t *testing.T) {
	now := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	limiter := newLoginRateLimiter(2, time.Minute, time.Minute, 10)
	limiter.recordFailure("192.0.2.11", now)
	limiter.recordFailure("192.0.2.11", now.Add(time.Second))
	if blocked, _ := limiter.isBlocked("192.0.2.11", now.Add(2*time.Second)); !blocked {
		t.Fatal("expected client to be blocked")
	}
	limiter.clear("192.0.2.11")
	if blocked, _ := limiter.isBlocked("192.0.2.11", now.Add(3*time.Second)); blocked {
		t.Fatal("successful login should clear the client's failure state")
	}
}

func TestLoginRateLimiterBoundsMemory(t *testing.T) {
	now := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	limiter := newLoginRateLimiter(3, time.Minute, time.Minute, 2)
	limiter.recordFailure("192.0.2.1", now)
	limiter.recordFailure("192.0.2.2", now.Add(time.Second))
	limiter.recordFailure("192.0.2.3", now.Add(2*time.Second))
	if got := len(limiter.entries); got > 2 {
		t.Fatalf("limiter retained %d entries; maximum is 2", got)
	}
}

func TestLoginClientIPUsesPeerAddress(t *testing.T) {
	for _, tc := range []struct {
		remote string
		want   string
	}{
		{remote: "192.0.2.5:54321", want: "192.0.2.5"},
		{remote: "[2001:db8::5]:443", want: "2001:db8::5"},
		{remote: "malformed", want: "unknown"},
	} {
		if got := loginClientIP(tc.remote); got != tc.want {
			t.Errorf("loginClientIP(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

func TestWriteLoginRateLimitedSetsRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLoginRateLimited(rec, 1500*time.Millisecond)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got HTTP %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
}
