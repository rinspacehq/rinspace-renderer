package jobpostgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int64
	name     string
	checksum string
	sql      string
}

// Migrate applies the embedded, additive rin_renderer schema migrations. It serializes migrators
// with a transaction-scoped advisory lock and rejects changed history by checksum.
func Migrate(ctx context.Context, db *sql.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin renderer migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('rin_renderer_schema_migrations'))`); err != nil {
		return fmt.Errorf("lock renderer migrations: %w", err)
	}
	if err := ensureRendererSchema(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rin_renderer.schema_migrations (
			version bigint PRIMARY KEY,
			name text NOT NULL UNIQUE,
			checksum character(64) NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
			applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
		)`); err != nil {
		return fmt.Errorf("create renderer migration ledger: %w", err)
	}

	for _, item := range migrations {
		var existingName, existingChecksum string
		err := tx.QueryRowContext(ctx, `
			SELECT name, checksum
			FROM rin_renderer.schema_migrations
			WHERE version = $1`, item.version).Scan(&existingName, &existingChecksum)
		switch {
		case err == nil:
			if existingName != item.name || existingChecksum != item.checksum {
				return fmt.Errorf("renderer migration %d history mismatch", item.version)
			}
			continue
		case err != sql.ErrNoRows:
			return fmt.Errorf("read renderer migration %d: %w", item.version, err)
		}

		if _, err := tx.ExecContext(ctx, item.sql); err != nil {
			return fmt.Errorf("apply renderer migration %d (%s): %w", item.version, item.name, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rin_renderer.schema_migrations (version, name, checksum)
			VALUES ($1, $2, $3)`, item.version, item.name, item.checksum); err != nil {
			return fmt.Errorf("record renderer migration %d: %w", item.version, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit renderer migrations: %w", err)
	}
	return nil
}

// ensureRendererSchema supports both ordinary PostgreSQL, where the dedicated migrator creates
// its schema, and managed PostgreSQL platforms that provision the schema through their control
// plane before handing ownership to the migrator. An existing schema owned by any other role is
// rejected instead of silently migrating across a credential boundary.
func ensureRendererSchema(ctx context.Context, tx *sql.Tx) error {
	var currentUser string
	if err := tx.QueryRowContext(ctx, `SELECT current_user`).Scan(&currentUser); err != nil {
		return fmt.Errorf("read renderer migration role: %w", err)
	}

	var owner string
	err := tx.QueryRowContext(ctx, `
		SELECT pg_get_userbyid(nspowner)
		FROM pg_namespace
		WHERE nspname = 'rin_renderer'`).Scan(&owner)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.ExecContext(ctx, `CREATE SCHEMA rin_renderer`); err != nil {
			return fmt.Errorf("create renderer schema: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read renderer schema owner: %w", err)
	case owner != currentUser:
		return fmt.Errorf("renderer schema owner %q does not match migration role %q", owner, currentUser)
	default:
		return nil
	}
}

// VerifyMigrations checks that the Renderer schema contains exactly the embedded migration
// history. It performs no schema writes and is intended for API/worker startup after a dedicated
// migration job has completed.
func VerifyMigrations(ctx context.Context, db *sql.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	var ledger *string
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('rin_renderer.schema_migrations')::text`).Scan(&ledger); err != nil {
		return fmt.Errorf("locate renderer migration ledger: %w", err)
	}
	if ledger == nil || *ledger == "" {
		return fmt.Errorf("renderer schema is not initialized; run rin-renderer-api migrate")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT version, name, checksum
		FROM rin_renderer.schema_migrations
		ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read renderer migration ledger: %w", err)
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		if index >= len(migrations) {
			return fmt.Errorf("renderer migration ledger has unexpected version beyond %d", migrations[len(migrations)-1].version)
		}
		var version int64
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return fmt.Errorf("scan renderer migration ledger: %w", err)
		}
		expected := migrations[index]
		if version != expected.version || name != expected.name || checksum != expected.checksum {
			return fmt.Errorf("renderer migration %d history mismatch", expected.version)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read renderer migration ledger: %w", err)
	}
	if index != len(migrations) {
		return fmt.Errorf("renderer schema is behind: applied %d of %d migrations; run rin-renderer-api migrate", index, len(migrations))
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded renderer migrations: %w", err)
	}
	items := make([]migration, 0, len(entries))
	seen := make(map[int64]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		parts := strings.SplitN(strings.TrimSuffix(entry.Name(), ".sql"), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid renderer migration filename %q", entry.Name())
		}
		version, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid renderer migration version in %q", entry.Name())
		}
		if previous, ok := seen[version]; ok {
			return nil, fmt.Errorf("duplicate renderer migration version %d in %q and %q", version, previous, entry.Name())
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read renderer migration %q: %w", entry.Name(), err)
		}
		digest := sha256.Sum256(body)
		items = append(items, migration{
			version:  version,
			name:     parts[1],
			checksum: hex.EncodeToString(digest[:]),
			sql:      string(body),
		})
		seen[version] = entry.Name()
	}
	sort.Slice(items, func(i, j int) bool { return items[i].version < items[j].version })
	if len(items) == 0 {
		return nil, fmt.Errorf("no embedded renderer migrations")
	}
	return items, nil
}
