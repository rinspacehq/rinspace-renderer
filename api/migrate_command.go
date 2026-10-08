package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
)

func runMigrations(ctx context.Context) error {
	databaseURL := strings.TrimSpace(os.Getenv("RIN_RENDERER_DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("RIN_RENDERER_DATABASE_URL is required for migrate")
	}
	repository, err := jobpostgres.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer repository.Close()
	if err := repository.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate renderer schema: %w", err)
	}
	if err := repository.VerifyMigrations(ctx); err != nil {
		return fmt.Errorf("verify renderer schema after migration: %w", err)
	}
	return nil
}
