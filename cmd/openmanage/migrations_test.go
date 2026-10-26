package main

import (
	"context"
	"testing"
)

// main re-applies every migration on each start, so each file must be idempotent.
func TestMigrationsAreRerunnable(t *testing.T) {
	db := newEnrollmentTestDB(t)
	if err := applyMigrations(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}
