package jobpostgres

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (repository *Repository) ExpireTerminalJobs(ctx context.Context, now time.Time, limit int) (int, error) {
	if now.IsZero() || limit <= 0 {
		return 0, errors.New("renderer job expiry input is invalid")
	}
	result, err := repository.db.ExecContext(ctx, `
		WITH candidates AS (
			SELECT id FROM rin_renderer.render_jobs
			WHERE state IN ('succeeded', 'failed', 'canceled') AND expires_at <= $1
			ORDER BY expires_at, id FOR UPDATE SKIP LOCKED LIMIT $2
		), updated AS (
			UPDATE rin_renderer.render_jobs AS jobs
			SET state = 'expired', updated_at = clock_timestamp()
			FROM candidates WHERE jobs.id = candidates.id RETURNING jobs.id
		)
		INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
		SELECT id, 'expired', 'cleanup', '{}'::jsonb FROM updated`, now, limit)
	if err != nil {
		return 0, fmt.Errorf("expire renderer terminal jobs: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count expired renderer jobs: %w", err)
	}
	return int(count), nil
}

// StageExpiredPrivateArtifacts atomically removes database references and creates durable deletion
// tombstones. Public objects, active-job inputs/results, and cache-referenced objects are excluded.
func (repository *Repository) StageExpiredPrivateArtifacts(ctx context.Context, now time.Time, limit int) (int, error) {
	if now.IsZero() || limit <= 0 {
		return 0, errors.New("renderer artifact cleanup input is invalid")
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin renderer artifact cleanup staging: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT artifacts.id
		FROM rin_renderer.render_artifacts AS artifacts
		WHERE artifacts.visibility = 'private' AND artifacts.kind IN ('source', 'result', 'debug')
			AND artifacts.expires_at <= $1
			AND NOT EXISTS (
				SELECT 1 FROM rin_renderer.render_jobs AS jobs
				WHERE (jobs.source_artifact_id = artifacts.id OR jobs.result_artifact_id = artifacts.id)
					AND jobs.state IN ('uploading', 'queued', 'running'))
			AND NOT EXISTS (
				SELECT 1 FROM rin_renderer.render_cache_entries AS cache
				WHERE cache.artifact_id = artifacts.id)
		ORDER BY artifacts.expires_at, artifacts.id
		FOR UPDATE OF artifacts SKIP LOCKED LIMIT $2`, now, limit)
	if err != nil {
		return 0, fmt.Errorf("select renderer artifacts for cleanup: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan renderer artifact cleanup: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close renderer artifact cleanup selection: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rin_renderer.artifact_cleanup_queue (
				id, sha256, kind, storage_key, byte_size, media_type, schema_version, expires_at
			)
			SELECT id, sha256, kind, storage_key, byte_size, media_type, schema_version, expires_at
			FROM rin_renderer.render_artifacts WHERE id = $1`, id); err != nil {
			return 0, fmt.Errorf("stage renderer artifact tombstone: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rin_renderer.render_jobs SET
				source_artifact_id = CASE WHEN source_artifact_id = $1 THEN NULL ELSE source_artifact_id END,
				result_artifact_id = CASE WHEN result_artifact_id = $1 THEN NULL ELSE result_artifact_id END,
				updated_at = clock_timestamp()
			WHERE state IN ('succeeded', 'failed', 'canceled', 'expired')
				AND (source_artifact_id = $1 OR result_artifact_id = $1)`, id); err != nil {
			return 0, fmt.Errorf("detach expired renderer artifact: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rin_renderer.render_artifacts WHERE id = $1`, id); err != nil {
			return 0, fmt.Errorf("remove staged renderer artifact: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit renderer artifact cleanup staging: %w", err)
	}
	return len(ids), nil
}

func (repository *Repository) PendingArtifactDeletions(ctx context.Context, limit int) ([]Artifact, error) {
	if limit <= 0 {
		return nil, errors.New("renderer deletion limit must be positive")
	}
	rows, err := repository.db.QueryContext(ctx, `
		SELECT id, sha256, kind, storage_key, byte_size, media_type,
			COALESCE(schema_version, ''), expires_at, staged_at
		FROM rin_renderer.artifact_cleanup_queue ORDER BY staged_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("read renderer artifact tombstones: %w", err)
	}
	defer rows.Close()
	var artifacts []Artifact
	for rows.Next() {
		var artifact Artifact
		var expiresAt time.Time
		if err := rows.Scan(&artifact.ID, &artifact.SHA256, &artifact.Kind, &artifact.StorageKey,
			&artifact.ByteSize, &artifact.MediaType, &artifact.SchemaVersion, &expiresAt, &artifact.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan renderer artifact tombstone: %w", err)
		}
		artifact.Visibility = "private"
		artifact.ExpiresAt = &expiresAt
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func (repository *Repository) AckArtifactDeletion(ctx context.Context, artifactID string) error {
	result, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.artifact_cleanup_queue WHERE id = $1`, artifactID)
	if err != nil {
		return fmt.Errorf("ack renderer artifact deletion: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return ErrNotFound
	}
	return nil
}

type MetadataCleanup struct{ Idempotency, Cache, Events int }

func (repository *Repository) CleanupExpiredMetadata(ctx context.Context, now time.Time, eventBefore time.Time, limit int) (MetadataCleanup, error) {
	if now.IsZero() || eventBefore.IsZero() || limit <= 0 {
		return MetadataCleanup{}, errors.New("renderer metadata cleanup input is invalid")
	}
	cleanup := MetadataCleanup{}
	queries := []struct {
		target *int
		query  string
		at     time.Time
	}{
		{&cleanup.Idempotency, `DELETE FROM rin_renderer.idempotency_keys WHERE (principal_id, owner_scope, idempotency_key) IN (SELECT principal_id, owner_scope, idempotency_key FROM rin_renderer.idempotency_keys WHERE expires_at <= $1 ORDER BY expires_at, principal_id, owner_scope, idempotency_key LIMIT $2)`, now},
		{&cleanup.Cache, `DELETE FROM rin_renderer.render_cache_entries WHERE cache_key IN (SELECT cache_key FROM rin_renderer.render_cache_entries WHERE expires_at <= $1 ORDER BY expires_at, cache_key LIMIT $2)`, now},
		{&cleanup.Events, `DELETE FROM rin_renderer.render_job_events WHERE id IN (SELECT events.id FROM rin_renderer.render_job_events AS events JOIN rin_renderer.render_jobs AS jobs ON jobs.id = events.job_id WHERE events.created_at <= $1 AND jobs.state IN ('succeeded', 'failed', 'canceled', 'expired') ORDER BY events.created_at, events.id LIMIT $2)`, eventBefore},
	}
	for _, item := range queries {
		result, err := repository.db.ExecContext(ctx, item.query, item.at, limit)
		if err != nil {
			return cleanup, fmt.Errorf("cleanup renderer metadata: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return cleanup, fmt.Errorf("count renderer metadata cleanup: %w", err)
		}
		*item.target = int(count)
	}
	return cleanup, nil
}
