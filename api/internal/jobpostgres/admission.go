package jobpostgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrGlobalCapacity       = errors.New("renderer global job capacity reached")
	ErrPrincipalQuota       = errors.New("renderer principal queue quota reached")
	ErrStorageCapacity      = errors.New("renderer private source capacity reached")
	ErrPreviewQueueCapacity = errors.New("renderer PDF preview queue capacity reached")
	ErrIdempotencyConflict  = errors.New("renderer idempotency key conflicts with another request")
	ErrReservationExpired   = errors.New("renderer admission reservation expired")
)

type AdmissionLimits struct {
	GlobalNonTerminal  int64
	PrincipalQueued    int64
	PreviewQueued      int64
	PrivateSourceBytes int64
}

type AdmissionReservationInput struct {
	Job                  Job
	Workload             WorkloadRecord
	DeclaredSourceBytes  int64
	AdmissionTokenHash   string
	IdempotencyKey       string
	RequestHash          string
	IdempotencyExpiresAt time.Time
	ReservationExpiresAt time.Time
	Now                  time.Time
}

type AdmissionReservation struct {
	Job       Job
	Reused    bool
	Uploading bool
}

func (repository *Repository) ReserveAdmission(ctx context.Context, input AdmissionReservationInput, limits AdmissionLimits) (AdmissionReservation, error) {
	if limits.GlobalNonTerminal <= 0 || limits.PrincipalQueued <= 0 || limits.PreviewQueued <= 0 || limits.PrivateSourceBytes <= 0 {
		return AdmissionReservation{}, errors.New("admission limits must be positive")
	}
	if input.DeclaredSourceBytes < 0 || input.DeclaredSourceBytes > limits.PrivateSourceBytes {
		return AdmissionReservation{}, ErrStorageCapacity
	}
	if input.Now.IsZero() || !input.ReservationExpiresAt.After(input.Now) || !input.IdempotencyExpiresAt.After(input.Now) {
		return AdmissionReservation{}, errors.New("admission reservation times are invalid")
	}
	if err := validateAdmissionWorkload(input.Workload, WorkloadAdmission{
		ContentKind: input.Job.ContentKind, DocumentEngine: input.Job.DocumentEngine,
		ResourceClass: input.Job.ResourceClass, PriorityClass: input.Job.PriorityClass,
		ProjectBytes: input.DeclaredSourceBytes,
	}); err != nil {
		return AdmissionReservation{}, err
	}

	tx, err := repository.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return AdmissionReservation{}, fmt.Errorf("begin renderer admission reservation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('rin_renderer_admission'))`); err != nil {
		return AdmissionReservation{}, fmt.Errorf("lock renderer admission: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		WITH abandoned AS (
			SELECT id FROM rin_renderer.render_jobs
			WHERE state = 'uploading' AND reservation_expires_at <= $1
			ORDER BY reservation_expires_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 64
		)
		DELETE FROM rin_renderer.render_jobs AS jobs
		USING abandoned WHERE jobs.id = abandoned.id`, input.Now); err != nil {
		return AdmissionReservation{}, fmt.Errorf("cleanup renderer admission reservations: %w", err)
	}

	if input.IdempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM rin_renderer.idempotency_keys
			WHERE principal_id = $1 AND owner_scope = $2 AND idempotency_key = $3
				AND expires_at <= $4`, input.Job.PrincipalID, input.Job.OwnerScope,
			input.IdempotencyKey, input.Now); err != nil {
			return AdmissionReservation{}, fmt.Errorf("expire renderer idempotency key: %w", err)
		}
		var existingRequestHash string
		var existingJobID string
		err := tx.QueryRowContext(ctx, `
			SELECT request_hash, job_id::text
			FROM rin_renderer.idempotency_keys
			WHERE principal_id = $1 AND owner_scope = $2 AND idempotency_key = $3`,
			input.Job.PrincipalID, input.Job.OwnerScope, input.IdempotencyKey,
		).Scan(&existingRequestHash, &existingJobID)
		switch {
		case err == nil:
			if existingRequestHash != input.RequestHash {
				return AdmissionReservation{}, ErrIdempotencyConflict
			}
			job, err := jobByID(ctx, tx, existingJobID, false)
			if err != nil {
				return AdmissionReservation{}, err
			}
			if job.State == "uploading" && job.ReservationExpiresAt != nil && !job.ReservationExpiresAt.After(input.Now) {
				if _, err := tx.ExecContext(ctx, `DELETE FROM rin_renderer.render_jobs WHERE id = $1::uuid`, job.ID); err != nil {
					return AdmissionReservation{}, fmt.Errorf("delete expired renderer admission reservation: %w", err)
				}
				break
			}
			if err := tx.Commit(); err != nil {
				return AdmissionReservation{}, fmt.Errorf("commit reused renderer admission: %w", err)
			}
			return AdmissionReservation{Job: job, Reused: true, Uploading: job.State == "uploading"}, nil
		case !errors.Is(err, sql.ErrNoRows):
			return AdmissionReservation{}, fmt.Errorf("read renderer idempotency key: %w", err)
		}
	}

	// Preview results are private and context-bound. A content cache hit is trusted
	// only when both the successful job and its result artifact are still retained.
	if previewResourceClass(input.Job.ResourceClass) {
		var cachedJobID string
		err := tx.QueryRowContext(ctx, `
			SELECT jobs.id::text
			FROM rin_renderer.render_jobs AS jobs
			JOIN rin_renderer.render_artifacts AS artifacts
				ON artifacts.id = jobs.result_artifact_id
			WHERE jobs.principal_id = $1 AND jobs.owner_scope = $2
				AND jobs.resource_class = $7 AND jobs.state = 'succeeded'
				AND jobs.project_hash = $3 AND jobs.options_hash = $4
				AND jobs.renderer_version = $5 AND jobs.expires_at > $6
				AND artifacts.kind = 'result' AND artifacts.visibility = 'private'
				AND (artifacts.expires_at IS NULL OR artifacts.expires_at > $6)
			ORDER BY jobs.finished_at DESC, jobs.id
			LIMIT 1`, input.Job.PrincipalID, input.Job.OwnerScope, input.Job.ProjectHash,
			input.Job.OptionsHash, input.Job.RendererVersion, input.Now,
			input.Job.ResourceClass).Scan(&cachedJobID)
		switch {
		case err == nil:
			if input.IdempotencyKey != "" {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO rin_renderer.idempotency_keys (
						principal_id, owner_scope, idempotency_key, request_hash, job_id, expires_at
					) VALUES ($1, $2, $3, $4, $5::uuid, $6)`, input.Job.PrincipalID,
					input.Job.OwnerScope, input.IdempotencyKey, input.RequestHash, cachedJobID,
					input.IdempotencyExpiresAt); err != nil {
					return AdmissionReservation{}, fmt.Errorf("cache renderer preview idempotency key: %w", err)
				}
			}
			job, err := jobByID(ctx, tx, cachedJobID, false)
			if err != nil {
				return AdmissionReservation{}, err
			}
			if err := tx.Commit(); err != nil {
				return AdmissionReservation{}, fmt.Errorf("commit cached renderer preview admission: %w", err)
			}
			return AdmissionReservation{Job: job, Reused: true}, nil
		case !errors.Is(err, sql.ErrNoRows):
			return AdmissionReservation{}, fmt.Errorf("read renderer preview content cache: %w", err)
		}
	}

	var globalCount, principalCount, storedSourceBytes, reservedSourceBytes int64
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM rin_renderer.render_jobs
		WHERE state IN ('uploading', 'queued', 'running')`).Scan(&globalCount); err != nil {
		return AdmissionReservation{}, fmt.Errorf("count renderer active jobs: %w", err)
	}
	if globalCount >= limits.GlobalNonTerminal {
		return AdmissionReservation{}, ErrGlobalCapacity
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM rin_renderer.render_jobs
		WHERE principal_id = $1 AND owner_scope = $2 AND state IN ('uploading', 'queued')`,
		input.Job.PrincipalID, input.Job.OwnerScope).Scan(&principalCount); err != nil {
		return AdmissionReservation{}, fmt.Errorf("count renderer principal jobs: %w", err)
	}
	if principalCount >= limits.PrincipalQueued {
		return AdmissionReservation{}, ErrPrincipalQuota
	}
	if previewResourceClass(input.Job.ResourceClass) {
		var previewQueued int64
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*)
			FROM rin_renderer.render_jobs
			WHERE resource_class = $3 AND state = 'queued'
				AND cancel_requested = false
				AND NOT (principal_id = $1 AND owner_scope = $2)`, input.Job.PrincipalID,
			input.Job.OwnerScope, input.Job.ResourceClass).Scan(&previewQueued); err != nil {
			return AdmissionReservation{}, fmt.Errorf("count renderer preview queue: %w", err)
		}
		if previewQueued >= limits.PreviewQueued {
			return AdmissionReservation{}, ErrPreviewQueueCapacity
		}
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(sum(byte_size), 0)
		FROM rin_renderer.render_artifacts
		WHERE kind = 'source' AND (expires_at IS NULL OR expires_at > $1)`, input.Now).Scan(&storedSourceBytes); err != nil {
		return AdmissionReservation{}, fmt.Errorf("sum renderer source artifacts: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(sum(declared_source_bytes), 0)
		FROM rin_renderer.render_jobs
		WHERE state = 'uploading'`).Scan(&reservedSourceBytes); err != nil {
		return AdmissionReservation{}, fmt.Errorf("sum renderer source reservations: %w", err)
	}
	if input.DeclaredSourceBytes > limits.PrivateSourceBytes-storedSourceBytes-reservedSourceBytes {
		return AdmissionReservation{}, ErrStorageCapacity
	}

	job := input.Job
	job.State = "uploading"
	job.SourceArtifactID = ""
	job.ResultArtifactID = ""
	job.QueuedAt = nil
	job.AvailableAt = nil
	job.StartedAt = nil
	job.FinishedAt = nil
	job.DeclaredSourceBytes = input.DeclaredSourceBytes
	job.ReservationExpiresAt = &input.ReservationExpiresAt
	err = tx.QueryRowContext(ctx, `
		INSERT INTO rin_renderer.render_jobs (
			id, principal_id, owner_scope, content_kind, document_engine, resource_class,
			priority_class, state, project_hash, options_hash, renderer_version, request_metadata, max_attempts,
			expires_at, declared_source_bytes, reservation_expires_at, admission_token_hash
		) VALUES (
			$1::uuid, $2, $3, $4, $5, $6, $7, 'uploading', $8, $9, $10,
			COALESCE($11::jsonb, '{}'::jsonb), $12, $13, $14, $15, $16
		)
		RETURNING created_at, updated_at`,
		job.ID, job.PrincipalID, job.OwnerScope, job.ContentKind, job.DocumentEngine,
		job.ResourceClass, job.PriorityClass, job.ProjectHash, job.OptionsHash,
		job.RendererVersion, jsonObject(job.RequestMetadata), job.MaxAttempts, job.ExpiresAt, job.DeclaredSourceBytes,
		input.ReservationExpiresAt, input.AdmissionTokenHash,
	).Scan(&job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return AdmissionReservation{}, fmt.Errorf("create renderer admission reservation: %w", err)
	}
	input.Workload.JobID = job.ID
	if err := upsertJobWorkload(ctx, tx, input.Workload); err != nil {
		return AdmissionReservation{}, err
	}
	if input.IdempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rin_renderer.idempotency_keys (
				principal_id, owner_scope, idempotency_key, request_hash, job_id, expires_at
			) VALUES ($1, $2, $3, $4, $5::uuid, $6)`,
			job.PrincipalID, job.OwnerScope, input.IdempotencyKey, input.RequestHash,
			job.ID, input.IdempotencyExpiresAt); err != nil {
			return AdmissionReservation{}, fmt.Errorf("create renderer idempotency key: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return AdmissionReservation{}, fmt.Errorf("commit renderer admission reservation: %w", err)
	}
	return AdmissionReservation{Job: job, Uploading: true}, nil
}

type CommitAdmissionInput struct {
	JobID              string
	AdmissionTokenHash string
	Artifact           Artifact
	Workload           WorkloadRecord
	QueuedAt           time.Time
	PreviewQueueLimit  int64
}

func (repository *Repository) CommitAdmission(ctx context.Context, input CommitAdmissionInput) (Job, error) {
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("begin renderer admission commit: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('rin_renderer_admission'))`); err != nil {
		return Job{}, fmt.Errorf("lock renderer admission commit: %w", err)
	}
	var resourceClass, principalID, ownerScope string
	if err := tx.QueryRowContext(ctx, `
		SELECT resource_class, principal_id, owner_scope
		FROM rin_renderer.render_jobs
		WHERE id = $1::uuid`, input.JobID).Scan(&resourceClass, &principalID, &ownerScope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNotFound
		}
		return Job{}, fmt.Errorf("read renderer admission context: %w", err)
	}
	if previewResourceClass(resourceClass) {
		if input.PreviewQueueLimit <= 0 {
			return Job{}, errors.New("renderer preview queue limit must be positive")
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
			"rin_renderer_preview:"+principalID+"\x1f"+ownerScope); err != nil {
			return Job{}, fmt.Errorf("lock renderer preview context: %w", err)
		}
	}
	job, err := jobByID(ctx, tx, input.JobID, true)
	if err != nil {
		return Job{}, err
	}
	if job.State != "uploading" {
		if job.SourceArtifactID == input.Artifact.ID && job.State != "" {
			if err := tx.Commit(); err != nil {
				return Job{}, fmt.Errorf("commit repeated renderer admission: %w", err)
			}
			return job, nil
		}
		return Job{}, ErrReservationExpired
	}
	if err := validateAdmissionWorkload(input.Workload, WorkloadAdmission{
		ContentKind: job.ContentKind, DocumentEngine: job.DocumentEngine,
		ResourceClass: job.ResourceClass, PriorityClass: job.PriorityClass,
		ProjectBytes: input.Artifact.ByteSize,
	}); err != nil {
		return Job{}, err
	}
	input.Workload.JobID = job.ID
	if err := upsertJobWorkload(ctx, tx, input.Workload); err != nil {
		return Job{}, err
	}
	var storedTokenHash string
	if err := tx.QueryRowContext(ctx, `
		SELECT admission_token_hash
		FROM rin_renderer.render_jobs
		WHERE id = $1::uuid`, input.JobID).Scan(&storedTokenHash); err != nil {
		return Job{}, fmt.Errorf("read renderer admission token: %w", err)
	}
	if storedTokenHash != input.AdmissionTokenHash || job.ReservationExpiresAt == nil || !job.ReservationExpiresAt.After(input.QueuedAt) {
		return Job{}, ErrReservationExpired
	}
	if input.Artifact.ByteSize > job.DeclaredSourceBytes || input.Artifact.Kind != "source" || input.Artifact.Visibility != "private" {
		return Job{}, errors.New("renderer admission artifact does not match reservation")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_artifacts (
			id, sha256, kind, visibility, storage_key, byte_size, media_type, schema_version, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9)
		ON CONFLICT (id) DO NOTHING`, input.Artifact.ID, input.Artifact.SHA256,
		input.Artifact.Kind, input.Artifact.Visibility, input.Artifact.StorageKey,
		input.Artifact.ByteSize, input.Artifact.MediaType, input.Artifact.SchemaVersion,
		input.Artifact.ExpiresAt); err != nil {
		return Job{}, fmt.Errorf("record renderer admission artifact: %w", err)
	}
	var artifact Artifact
	if err := tx.QueryRowContext(ctx, `
		SELECT id, sha256, kind, visibility, storage_key, byte_size, media_type,
			COALESCE(schema_version, ''), expires_at, created_at
		FROM rin_renderer.render_artifacts WHERE id = $1`, input.Artifact.ID).Scan(
		&artifact.ID, &artifact.SHA256, &artifact.Kind, &artifact.Visibility,
		&artifact.StorageKey, &artifact.ByteSize, &artifact.MediaType,
		&artifact.SchemaVersion, &artifact.ExpiresAt, &artifact.CreatedAt,
	); err != nil {
		return Job{}, fmt.Errorf("verify renderer admission artifact: %w", err)
	}
	if !sameArtifact(artifact, input.Artifact) {
		return Job{}, errors.New("renderer admission artifact metadata conflicts with existing record")
	}
	if previewResourceClass(job.ResourceClass) {
		var previewQueued int64
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*)
			FROM rin_renderer.render_jobs
			WHERE resource_class = $3 AND state = 'queued'
				AND cancel_requested = false
				AND NOT (principal_id = $1 AND owner_scope = $2)`, job.PrincipalID,
			job.OwnerScope, job.ResourceClass).Scan(&previewQueued); err != nil {
			return Job{}, fmt.Errorf("count renderer preview queue at commit: %w", err)
		}
		if previewQueued >= input.PreviewQueueLimit {
			return Job{}, ErrPreviewQueueCapacity
		}
		var newerQueuedID string
		err := tx.QueryRowContext(ctx, `
			SELECT id::text
			FROM rin_renderer.render_jobs
			WHERE principal_id = $1 AND owner_scope = $2
				AND resource_class = $5 AND state = 'queued'
				AND cancel_requested = false
				AND (created_at, id) > ($3, $4::uuid)
			ORDER BY created_at DESC, id DESC
			LIMIT 1
			FOR UPDATE`, job.PrincipalID, job.OwnerScope, job.CreatedAt, job.ID,
			job.ResourceClass).Scan(&newerQueuedID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Job{}, fmt.Errorf("read newer renderer preview: %w", err)
		}
		if err == nil {
			if _, err := tx.ExecContext(ctx, `
				UPDATE rin_renderer.render_jobs
				SET state = 'canceled', source_artifact_id = $2, declared_source_bytes = $3,
					finished_at = $4, reservation_expires_at = NULL, admission_token_hash = NULL,
					cancel_requested = true, superseded_by = $5::uuid, updated_at = clock_timestamp()
				WHERE id = $1::uuid`, job.ID, artifact.ID, artifact.ByteSize, input.QueuedAt,
				newerQueuedID); err != nil {
				return Job{}, fmt.Errorf("supersede stale renderer preview admission: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
				VALUES ($1::uuid, 'superseded', 'admission', jsonb_build_object('supersededBy', $2::text))`,
				job.ID, newerQueuedID); err != nil {
				return Job{}, fmt.Errorf("record stale renderer preview supersede: %w", err)
			}
			job, err = jobByID(ctx, tx, job.ID, false)
			if err != nil {
				return Job{}, err
			}
			if err := tx.Commit(); err != nil {
				return Job{}, fmt.Errorf("commit stale renderer preview supersede: %w", err)
			}
			return job, nil
		}
		if _, err := tx.ExecContext(ctx, `
			WITH superseded AS (
				UPDATE rin_renderer.render_jobs
				SET state = 'canceled', cancel_requested = true, available_at = NULL,
					finished_at = $3, superseded_by = $4::uuid, updated_at = clock_timestamp()
				WHERE principal_id = $1 AND owner_scope = $2
					AND resource_class = $5 AND state = 'queued'
					AND cancel_requested = false
				RETURNING id
			)
			INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
			SELECT id, 'superseded', 'queue', jsonb_build_object('supersededBy', $4::text)
			FROM superseded`, job.PrincipalID, job.OwnerScope, input.QueuedAt, job.ID,
			job.ResourceClass); err != nil {
			return Job{}, fmt.Errorf("supersede queued renderer preview: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_jobs
		SET state = 'queued', source_artifact_id = $2, declared_source_bytes = $3,
			queued_at = $4, available_at = $4, reservation_expires_at = NULL,
			admission_token_hash = NULL, updated_at = clock_timestamp()
		WHERE id = $1::uuid`, job.ID, artifact.ID, artifact.ByteSize, input.QueuedAt); err != nil {
		return Job{}, fmt.Errorf("queue renderer admission: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
		VALUES ($1::uuid, 'queued', 'admission', jsonb_build_object(
			'profileVersion', $2::text, 'profileKey', $3::text))`,
		job.ID, input.Workload.ProfileVersion, input.Workload.ProfileKey); err != nil {
		return Job{}, fmt.Errorf("record renderer admission event: %w", err)
	}
	job, err = jobByID(ctx, tx, job.ID, false)
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit renderer queued job: %w", err)
	}
	return job, nil
}

func (repository *Repository) AbortAdmission(ctx context.Context, jobID string, admissionTokenHash string) error {
	result, err := repository.db.ExecContext(ctx, `
		DELETE FROM rin_renderer.render_jobs
		WHERE id = $1::uuid AND state = 'uploading' AND admission_token_hash = $2`,
		jobID, admissionTokenHash)
	if err != nil {
		return fmt.Errorf("abort renderer admission: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read aborted renderer admission count: %w", err)
	}
	if rows == 0 {
		return ErrReservationExpired
	}
	return nil
}

func (repository *Repository) CleanupAbandonedAdmissions(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, errors.New("admission cleanup limit must be positive")
	}
	result, err := repository.db.ExecContext(ctx, `
		WITH abandoned AS (
			SELECT id
			FROM rin_renderer.render_jobs
			WHERE state = 'uploading' AND reservation_expires_at <= $1
			ORDER BY reservation_expires_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		DELETE FROM rin_renderer.render_jobs AS jobs
		USING abandoned
		WHERE jobs.id = abandoned.id`, now, limit)
	if err != nil {
		return 0, fmt.Errorf("cleanup abandoned renderer admissions: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read abandoned renderer admission count: %w", err)
	}
	return count, nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func jobByID(ctx context.Context, querier rowQuerier, id string, forUpdate bool) (Job, error) {
	query := `
		SELECT id::text, principal_id, owner_scope, content_kind, document_engine, resource_class,
			priority_class, state, source_artifact_id, result_artifact_id, project_hash,
			options_hash, renderer_version, request_metadata, max_attempts, queued_at, available_at, started_at,
			finished_at, expires_at, cancel_requested, declared_source_bytes,
			reservation_expires_at, created_at, updated_at
		FROM rin_renderer.render_jobs
		WHERE id = $1::uuid`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var job Job
	var sourceArtifactID, resultArtifactID sql.NullString
	err := querier.QueryRowContext(ctx, query, id).Scan(
		&job.ID, &job.PrincipalID, &job.OwnerScope, &job.ContentKind, &job.DocumentEngine,
		&job.ResourceClass, &job.PriorityClass, &job.State, &sourceArtifactID,
		&resultArtifactID, &job.ProjectHash, &job.OptionsHash, &job.RendererVersion, &job.RequestMetadata,
		&job.MaxAttempts, &job.QueuedAt, &job.AvailableAt, &job.StartedAt, &job.FinishedAt,
		&job.ExpiresAt, &job.CancelRequested, &job.DeclaredSourceBytes,
		&job.ReservationExpiresAt, &job.CreatedAt, &job.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("read renderer job: %w", err)
	}
	job.SourceArtifactID = sourceArtifactID.String
	job.ResultArtifactID = resultArtifactID.String
	return job, nil
}

func sameArtifact(left Artifact, right Artifact) bool {
	if left.ID != right.ID || left.SHA256 != right.SHA256 || left.Kind != right.Kind ||
		left.Visibility != right.Visibility || left.StorageKey != right.StorageKey ||
		left.ByteSize != right.ByteSize || left.MediaType != right.MediaType ||
		left.SchemaVersion != right.SchemaVersion {
		return false
	}
	if (left.ExpiresAt == nil) != (right.ExpiresAt == nil) {
		return false
	}
	return left.ExpiresAt == nil || left.ExpiresAt.Equal(*right.ExpiresAt)
}
