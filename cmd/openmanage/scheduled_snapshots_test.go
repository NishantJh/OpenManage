package main

import (
	"testing"
	"time"
)

func TestRetryTrackerHoldsBackRecentFailuresOnly(t *testing.T) {
	var tr retryTracker
	now := time.Now()
	if got := tr.waiting(now, time.Minute); len(got) != 0 {
		t.Fatalf("empty tracker returned %v", got)
	}
	tr.fail("recent", now.Add(-30*time.Second))
	tr.fail("old", now.Add(-2*time.Minute))
	tr.fail("fixed", now)
	tr.succeed("fixed")
	got := tr.waiting(now, time.Minute)
	if len(got) != 1 || got[0] != "recent" {
		t.Fatalf("waiting = %v, want [recent]", got)
	}
	if _, kept := tr.failed["old"]; kept {
		t.Fatal("expired failures should be forgotten")
	}
}
