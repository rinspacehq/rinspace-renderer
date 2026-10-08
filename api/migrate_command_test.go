package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunMigrationsRequiresDatabaseURL(t *testing.T) {
	t.Setenv("RIN_RENDERER_DATABASE_URL", "")
	err := runMigrations(context.Background())
	if err == nil || !strings.Contains(err.Error(), "RIN_RENDERER_DATABASE_URL is required") {
		t.Fatalf("runMigrations() error = %v", err)
	}
}
