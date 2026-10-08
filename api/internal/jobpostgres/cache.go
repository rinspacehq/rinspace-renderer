package jobpostgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

var ErrCacheConflict = errors.New("renderer cache entry conflicts with immutable metadata")

func (repository *Repository) RegisterCacheArtifact(ctx context.Context, reference contracts.ArtifactReference, schemaVersion string) error {
	if err := reference.Validate(); err != nil || reference.Visibility != "private" ||
		!strings.HasPrefix(reference.ArtifactID, "render-cache/v1/") || strings.TrimSpace(schemaVersion) == "" {
		return errors.New("renderer cache artifact registration is invalid")
	}
	expiresAt, err := time.Parse(time.RFC3339, reference.ExpiresAt)
	if err != nil {
		return errors.New("renderer cache artifact expiry is invalid")
	}
	expected := Artifact{ID: reference.ArtifactID, SHA256: reference.SHA256, Kind: "cache", Visibility: reference.Visibility,
		StorageKey: reference.ArtifactID, ByteSize: reference.Bytes, MediaType: reference.MediaType,
		SchemaVersion: schemaVersion, ExpiresAt: &expiresAt}
	if _, err := repository.CreateArtifact(ctx, expected); err == nil {
		return nil
	}
	existing, err := repository.Artifact(ctx, reference.ArtifactID)
	if err != nil || existing.ID != expected.ID || existing.SHA256 != expected.SHA256 || existing.Kind != expected.Kind ||
		existing.Visibility != expected.Visibility || existing.StorageKey != expected.StorageKey || existing.ByteSize != expected.ByteSize ||
		existing.MediaType != expected.MediaType || existing.SchemaVersion != expected.SchemaVersion || existing.ExpiresAt == nil ||
		!existing.ExpiresAt.UTC().Truncate(time.Second).Equal(expiresAt.UTC().Truncate(time.Second)) {
		return ErrCacheConflict
	}
	return nil
}

func (repository *Repository) CacheEntry(ctx context.Context, key orchestration.CacheKey, now time.Time) (orchestration.CacheRecord, bool, error) {
	if err := key.Validate(); err != nil || now.IsZero() {
		return orchestration.CacheRecord{}, false, errors.New("renderer cache lookup input is invalid")
	}
	entry, err := readCacheEntry(ctx, repository.db, key, now, true)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.CacheRecord{}, false, nil
	}
	if err != nil {
		return orchestration.CacheRecord{}, false, fmt.Errorf("read renderer cache entry: %w", err)
	}
	return entry, true, nil
}

func (repository *Repository) PutCacheEntry(ctx context.Context, entry orchestration.CacheRecord) error {
	if err := validateCacheEntry(entry); err != nil {
		return err
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin renderer cache entry write: %w", err)
	}
	defer tx.Rollback()
	artifact, err := artifactByID(ctx, tx, entry.Artifact.ArtifactID, true)
	if err != nil {
		return err
	}
	if err := validateCacheArtifact(entry, artifact); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_cache_entries (
			cache_key, stage, artifact_id, schema_version, renderer_version, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (cache_key) DO NOTHING`,
		entry.Key.String(), string(entry.Key.Stage), entry.Artifact.ArtifactID,
		entry.SchemaVersion, entry.Key.Version, entry.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert renderer cache entry: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read renderer cache entry insert count: %w", err)
	}
	if inserted == 0 {
		existing, err := readCacheEntry(ctx, tx, entry.Key, time.Time{}, false)
		if err != nil {
			return fmt.Errorf("read immutable renderer cache entry: %w", err)
		}
		if !sameCacheEntry(existing, entry) {
			return ErrCacheConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit renderer cache entry: %w", err)
	}
	return nil
}

func (repository *Repository) TouchCacheEntry(ctx context.Context, key orchestration.CacheKey, now time.Time) error {
	if err := key.Validate(); err != nil || now.IsZero() {
		return errors.New("renderer cache touch input is invalid")
	}
	result, err := repository.db.ExecContext(ctx, `
		UPDATE rin_renderer.render_cache_entries
		SET last_hit_at = GREATEST(last_hit_at, $2)
		WHERE cache_key = $1 AND expires_at > $2`, key.String(), now)
	if err != nil {
		return fmt.Errorf("touch renderer cache entry: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read renderer cache touch count: %w", err)
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

func (repository *Repository) DeleteCacheEntry(ctx context.Context, key orchestration.CacheKey) error {
	if err := key.Validate(); err != nil {
		return errors.New("renderer cache deletion key is invalid")
	}
	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_cache_entries WHERE cache_key = $1`, key.String()); err != nil {
		return fmt.Errorf("delete renderer cache entry: %w", err)
	}
	return nil
}

type cacheEntryQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readCacheEntry(ctx context.Context, querier cacheEntryQuerier, key orchestration.CacheKey, now time.Time, enforceExpiry bool) (orchestration.CacheRecord, error) {
	query := `
		SELECT cache.stage, cache.schema_version, COALESCE(artifacts.schema_version, ''), cache.renderer_version,
			cache.created_at, cache.last_hit_at, cache.expires_at,
			artifacts.id, artifacts.sha256, artifacts.byte_size, artifacts.media_type,
			artifacts.visibility, artifacts.expires_at
		FROM rin_renderer.render_cache_entries AS cache
		JOIN rin_renderer.render_artifacts AS artifacts ON artifacts.id = cache.artifact_id
		WHERE cache.cache_key = $1`
	arguments := []any{key.String()}
	if enforceExpiry {
		query += ` AND cache.expires_at > $2 AND (artifacts.expires_at IS NULL OR artifacts.expires_at > $2)`
		arguments = append(arguments, now)
	}
	var entry orchestration.CacheRecord
	var stage, version string
	var artifactExpiry sql.NullTime
	err := querier.QueryRowContext(ctx, query, arguments...).Scan(
		&stage, &entry.SchemaVersion, &entry.ArtifactSchemaVersion, &version, &entry.CreatedAt, &entry.LastHitAt, &entry.ExpiresAt,
		&entry.Artifact.ArtifactID, &entry.Artifact.SHA256, &entry.Artifact.Bytes,
		&entry.Artifact.MediaType, &entry.Artifact.Visibility, &artifactExpiry,
	)
	if err != nil {
		return orchestration.CacheRecord{}, err
	}
	entry.Key = orchestration.CacheKey{Stage: orchestration.CacheStage(stage), Version: version, Digest: key.Digest}
	if artifactExpiry.Valid {
		entry.Artifact.ExpiresAt = artifactExpiry.Time.UTC().Format(time.RFC3339)
	}
	return entry, nil
}

func validateCacheEntry(entry orchestration.CacheRecord) error {
	if err := entry.Key.Validate(); err != nil {
		return fmt.Errorf("renderer cache key: %w", err)
	}
	if err := entry.Artifact.Validate(); err != nil {
		return fmt.Errorf("renderer cache artifact: %w", err)
	}
	if strings.TrimSpace(entry.SchemaVersion) == "" || entry.ExpiresAt.IsZero() {
		return errors.New("renderer cache entry requires schema and expiry")
	}
	return nil
}

func validateCacheArtifact(entry orchestration.CacheRecord, artifact Artifact) error {
	if artifact.Kind != "cache" && artifact.Kind != "public-diagram" {
		return errors.New("renderer cache entry requires a cache or public diagram artifact")
	}
	if artifact.ID != entry.Artifact.ArtifactID || artifact.SHA256 != entry.Artifact.SHA256 ||
		artifact.ByteSize != entry.Artifact.Bytes || artifact.MediaType != entry.Artifact.MediaType ||
		artifact.Visibility != entry.Artifact.Visibility || artifact.SchemaVersion != entry.SchemaVersion {
		return ErrCacheConflict
	}
	if artifact.Visibility == "private" {
		if artifact.Kind != "cache" || artifact.ExpiresAt == nil || artifact.ExpiresAt.Before(entry.ExpiresAt) {
			return ErrCacheConflict
		}
		referenceExpiry, err := time.Parse(time.RFC3339, entry.Artifact.ExpiresAt)
		if err != nil || !referenceExpiry.Equal(artifact.ExpiresAt.UTC().Truncate(time.Second)) {
			return ErrCacheConflict
		}
	} else if artifact.Kind != "public-diagram" || artifact.ExpiresAt != nil || entry.Artifact.ExpiresAt != "" {
		return ErrCacheConflict
	}
	return nil
}

func sameCacheEntry(left, right orchestration.CacheRecord) bool {
	return left.Key == right.Key && left.Artifact == right.Artifact && left.SchemaVersion == right.SchemaVersion &&
		left.ExpiresAt.Equal(right.ExpiresAt)
}

type artifactQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func artifactByID(ctx context.Context, querier artifactQuerier, artifactID string, lock bool) (Artifact, error) {
	query := `SELECT id, sha256, kind, visibility, storage_key, byte_size, media_type,
		COALESCE(schema_version, ''), expires_at, created_at
		FROM rin_renderer.render_artifacts WHERE id = $1`
	if lock {
		query += ` FOR SHARE`
	}
	var artifact Artifact
	var expiresAt sql.NullTime
	if err := querier.QueryRowContext(ctx, query, artifactID).Scan(
		&artifact.ID, &artifact.SHA256, &artifact.Kind, &artifact.Visibility, &artifact.StorageKey,
		&artifact.ByteSize, &artifact.MediaType, &artifact.SchemaVersion, &expiresAt, &artifact.CreatedAt,
	); errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	} else if err != nil {
		return Artifact{}, fmt.Errorf("read renderer cache artifact: %w", err)
	}
	if expiresAt.Valid {
		artifact.ExpiresAt = &expiresAt.Time
	}
	return artifact, nil
}
