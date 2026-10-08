package jobpostgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrSupportActionNotAllowed = errors.New("renderer support action is not allowed")

type SupportJob struct {
	JobID           string     `json:"jobId"`
	ContentKind     string     `json:"contentKind"`
	DocumentEngine  string     `json:"documentEngine"`
	ResourceClass   string     `json:"resourceClass"`
	PriorityClass   string     `json:"priorityClass"`
	State           string     `json:"state"`
	RendererVersion string     `json:"rendererVersion"`
	MaxAttempts     int16      `json:"maxAttempts"`
	AttemptCount    int64      `json:"attemptCount"`
	QueuedAt        *time.Time `json:"queuedAt,omitempty"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	CancelRequested bool       `json:"cancelRequested"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type SupportAttempt struct {
	AttemptID  string     `json:"attemptId"`
	AttemptNo  int16      `json:"attemptNo"`
	State      string     `json:"state"`
	ErrorCode  string     `json:"errorCode,omitempty"`
	DurationMS *int64     `json:"durationMs,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type SupportEstimate struct {
	EstimatorVersion string     `json:"estimatorVersion"`
	EstimatedStartAt time.Time  `json:"estimatedStartAt"`
	EarliestStartAt  time.Time  `json:"earliestStartAt"`
	LatestStartAt    time.Time  `json:"latestStartAt"`
	Confidence       string     `json:"confidence"`
	SampleCount      int64      `json:"sampleCount"`
	Scope            string     `json:"scope"`
	CalculatedAt     time.Time  `json:"calculatedAt"`
	ActualStartedAt  *time.Time `json:"actualStartedAt,omitempty"`
	CentralErrorMS   *int64     `json:"centralErrorMs,omitempty"`
	IntervalCovered  *bool      `json:"intervalCovered,omitempty"`
}

type SupportArtifact struct {
	ArtifactID    string     `json:"artifactId"`
	Kind          string     `json:"kind"`
	Visibility    string     `json:"visibility"`
	SHA256        string     `json:"sha256"`
	ByteSize      int64      `json:"byteSize"`
	MediaType     string     `json:"mediaType"`
	SchemaVersion string     `json:"schemaVersion,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type SupportSnapshot struct {
	Job         SupportJob        `json:"job"`
	Attempts    []SupportAttempt  `json:"attempts"`
	Estimator   *SupportEstimate  `json:"estimator"`
	Artifacts   []SupportArtifact `json:"artifacts"`
	Scope       string            `json:"scope"`
	InspectedAt time.Time         `json:"inspectedAt"`
}

func (repository *Repository) AuthorizedSupportSnapshot(ctx context.Context, jobID, principalID, ownerScope string, now time.Time) (SupportSnapshot, error) {
	if now.IsZero() {
		return SupportSnapshot{}, errors.New("renderer support inspection time is required")
	}
	job, err := repository.AuthorizedJob(ctx, jobID, principalID, ownerScope)
	if err != nil {
		return SupportSnapshot{}, err
	}
	snapshot := SupportSnapshot{Scope: "instance", InspectedAt: now, Attempts: []SupportAttempt{}, Artifacts: []SupportArtifact{}}
	snapshot.Job = SupportJob{
		JobID: job.ID, ContentKind: job.ContentKind, DocumentEngine: job.DocumentEngine,
		ResourceClass: job.ResourceClass, PriorityClass: job.PriorityClass, State: job.State,
		RendererVersion: job.RendererVersion, MaxAttempts: job.MaxAttempts,
		QueuedAt: job.QueuedAt, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt,
		ExpiresAt: job.ExpiresAt, CancelRequested: job.CancelRequested,
		CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}
	rows, err := repository.db.QueryContext(ctx, `
		SELECT id::text, attempt_no, state, COALESCE(error_code, ''), duration_ms, started_at, finished_at
		FROM rin_renderer.render_attempts WHERE job_id = $1::uuid ORDER BY attempt_no, id`, job.ID)
	if err != nil {
		return SupportSnapshot{}, fmt.Errorf("read renderer support attempts: %w", err)
	}
	for rows.Next() {
		var attempt SupportAttempt
		var duration sql.NullInt64
		if err := rows.Scan(&attempt.AttemptID, &attempt.AttemptNo, &attempt.State, &attempt.ErrorCode,
			&duration, &attempt.StartedAt, &attempt.FinishedAt); err != nil {
			rows.Close()
			return SupportSnapshot{}, fmt.Errorf("scan renderer support attempt: %w", err)
		}
		if duration.Valid {
			value := duration.Int64
			attempt.DurationMS = &value
		}
		snapshot.Attempts = append(snapshot.Attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SupportSnapshot{}, fmt.Errorf("iterate renderer support attempts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return SupportSnapshot{}, fmt.Errorf("close renderer support attempts: %w", err)
	}
	snapshot.Job.AttemptCount = int64(len(snapshot.Attempts))
	var estimate SupportEstimate
	var actualStarted sql.NullTime
	var centralError sql.NullInt64
	var covered sql.NullBool
	err = repository.db.QueryRowContext(ctx, `
		SELECT estimator_version, estimated_start_at, earliest_start_at, latest_start_at,
			confidence, sample_count, scope, calculated_at, actual_started_at,
			central_error_ms, interval_covered
		FROM rin_renderer.render_job_wait_estimates WHERE job_id = $1::uuid`, job.ID).Scan(
		&estimate.EstimatorVersion, &estimate.EstimatedStartAt, &estimate.EarliestStartAt,
		&estimate.LatestStartAt, &estimate.Confidence, &estimate.SampleCount, &estimate.Scope,
		&estimate.CalculatedAt, &actualStarted, &centralError, &covered)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SupportSnapshot{}, fmt.Errorf("read renderer support estimate: %w", err)
	}
	if err == nil {
		if actualStarted.Valid {
			estimate.ActualStartedAt = &actualStarted.Time
		}
		if centralError.Valid {
			value := centralError.Int64
			estimate.CentralErrorMS = &value
		}
		if covered.Valid {
			value := covered.Bool
			estimate.IntervalCovered = &value
		}
		snapshot.Estimator = &estimate
	}
	artifactRows, err := repository.db.QueryContext(ctx, `
		SELECT artifacts.id, artifacts.kind, artifacts.visibility, artifacts.sha256,
			artifacts.byte_size, artifacts.media_type, COALESCE(artifacts.schema_version, ''),
			artifacts.expires_at, artifacts.created_at
		FROM rin_renderer.render_artifacts AS artifacts
		JOIN rin_renderer.render_jobs AS jobs
			ON artifacts.id = jobs.source_artifact_id OR artifacts.id = jobs.result_artifact_id
		WHERE jobs.id = $1::uuid AND artifacts.visibility = 'private'
		ORDER BY artifacts.kind, artifacts.id`, job.ID)
	if err != nil {
		return SupportSnapshot{}, fmt.Errorf("read renderer support artifacts: %w", err)
	}
	for artifactRows.Next() {
		var artifact SupportArtifact
		if err := artifactRows.Scan(&artifact.ArtifactID, &artifact.Kind, &artifact.Visibility,
			&artifact.SHA256, &artifact.ByteSize, &artifact.MediaType, &artifact.SchemaVersion,
			&artifact.ExpiresAt, &artifact.CreatedAt); err != nil {
			artifactRows.Close()
			return SupportSnapshot{}, fmt.Errorf("scan renderer support artifact: %w", err)
		}
		snapshot.Artifacts = append(snapshot.Artifacts, artifact)
	}
	if err := artifactRows.Err(); err != nil {
		artifactRows.Close()
		return SupportSnapshot{}, fmt.Errorf("iterate renderer support artifacts: %w", err)
	}
	if err := artifactRows.Close(); err != nil {
		return SupportSnapshot{}, fmt.Errorf("close renderer support artifacts: %w", err)
	}
	return snapshot, nil
}

func (repository *Repository) AuthorizedManualRetry(ctx context.Context, jobID, principalID, ownerScope string, now time.Time) (Job, error) {
	if now.IsZero() {
		return Job{}, errors.New("renderer manual retry time is required")
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("begin renderer manual retry: %w", err)
	}
	defer tx.Rollback()
	job, err := authorizedJobTx(ctx, tx, jobID, principalID, ownerScope, true)
	if err != nil {
		return Job{}, err
	}
	var attemptCount int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM rin_renderer.render_attempts WHERE job_id = $1::uuid`, job.ID).Scan(&attemptCount); err != nil {
		return Job{}, fmt.Errorf("count renderer manual retry attempts: %w", err)
	}
	var sourceAvailable bool
	if job.SourceArtifactID != "" {
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM rin_renderer.render_artifacts
				WHERE id = $1 AND visibility = 'private' AND (expires_at IS NULL OR expires_at > $2))`,
			job.SourceArtifactID, now).Scan(&sourceAvailable); err != nil {
			return Job{}, fmt.Errorf("check renderer manual retry source: %w", err)
		}
	}
	if job.State != "failed" || attemptCount >= int64(job.MaxAttempts) || !sourceAvailable || !job.ExpiresAt.After(now) {
		return Job{}, ErrSupportActionNotAllowed
	}
	if previewResourceClass(job.ResourceClass) {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
			"rin_renderer_preview:"+job.PrincipalID+"\x1f"+job.OwnerScope); err != nil {
			return Job{}, fmt.Errorf("lock renderer preview manual retry context: %w", err)
		}
		var queuedExists bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM rin_renderer.render_jobs
				WHERE principal_id = $1 AND owner_scope = $2
					AND resource_class = $4 AND state = 'queued'
					AND cancel_requested = false AND id <> $3::uuid
			)`, job.PrincipalID, job.OwnerScope, job.ID, job.ResourceClass).Scan(&queuedExists); err != nil {
			return Job{}, fmt.Errorf("check renderer preview manual retry slot: %w", err)
		}
		if queuedExists {
			return Job{}, ErrSupportActionNotAllowed
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_jobs SET
			state = 'queued', result_artifact_id = NULL, queued_at = $2, available_at = $2,
			started_at = NULL, finished_at = NULL, cancel_requested = false,
			updated_at = clock_timestamp()
		WHERE id = $1::uuid`, job.ID, now); err != nil {
		return Job{}, fmt.Errorf("queue renderer manual retry: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
		VALUES ($1::uuid, 'manual_retry_queued', 'support',
			jsonb_build_object('attemptCount', $2::bigint, 'maxAttempts', $3::smallint))`,
		job.ID, attemptCount, job.MaxAttempts); err != nil {
		return Job{}, fmt.Errorf("audit renderer manual retry: %w", err)
	}
	job, err = jobByID(ctx, tx, job.ID, false)
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit renderer manual retry: %w", err)
	}
	return job, nil
}

func (repository *Repository) AuthorizedManualExpire(ctx context.Context, jobID, principalID, ownerScope string, now time.Time) (Job, error) {
	if now.IsZero() {
		return Job{}, errors.New("renderer manual expiry time is required")
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("begin renderer manual expiry: %w", err)
	}
	defer tx.Rollback()
	job, err := authorizedJobTx(ctx, tx, jobID, principalID, ownerScope, true)
	if err != nil {
		return Job{}, err
	}
	if job.State != "succeeded" && job.State != "failed" && job.State != "canceled" {
		return Job{}, ErrSupportActionNotAllowed
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_artifacts SET expires_at = LEAST(COALESCE(expires_at, $2), $2)
		WHERE visibility = 'private' AND id IN (
			SELECT source_artifact_id FROM rin_renderer.render_jobs WHERE id = $1::uuid
			UNION SELECT result_artifact_id FROM rin_renderer.render_jobs WHERE id = $1::uuid
		)`, job.ID, now); err != nil {
		return Job{}, fmt.Errorf("expire renderer private artifacts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_jobs
		SET state = 'expired', expires_at = $2, updated_at = clock_timestamp()
		WHERE id = $1::uuid`, job.ID, now); err != nil {
		return Job{}, fmt.Errorf("expire renderer support job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
		VALUES ($1::uuid, 'manual_expired', 'support', '{}'::jsonb)`, job.ID); err != nil {
		return Job{}, fmt.Errorf("audit renderer manual expiry: %w", err)
	}
	job, err = jobByID(ctx, tx, job.ID, false)
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit renderer manual expiry: %w", err)
	}
	return job, nil
}

func authorizedJobTx(ctx context.Context, tx *sql.Tx, jobID, principalID, ownerScope string, lock bool) (Job, error) {
	query := `SELECT id::text FROM rin_renderer.render_jobs
		WHERE id = $1::uuid AND principal_id = $2 AND owner_scope = $3`
	if lock {
		query += ` FOR UPDATE`
	}
	var id string
	if err := tx.QueryRowContext(ctx, query, jobID, principalID, ownerScope).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	} else if err != nil {
		return Job{}, fmt.Errorf("authorize renderer support job: %w", err)
	}
	return jobByID(ctx, tx, id, false)
}
