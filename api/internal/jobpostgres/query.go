package jobpostgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
)

type QueueSummary struct {
	QueuedProjects int64         `json:"queuedProjects"`
	ActiveProjects int64         `json:"activeProjects"`
	Estimate       *WaitEstimate `json:"estimate"`
	Scope          string        `json:"scope"`
	CalculatedAt   time.Time     `json:"calculatedAt"`
	OldestWait     time.Duration `json:"-"`
}

type JobQueueSummary struct {
	JobsAheadEstimate int64         `json:"jobsAheadEstimate"`
	QueuedProjects    int64         `json:"queuedProjects"`
	ActiveProjects    int64         `json:"activeProjects"`
	Estimate          *WaitEstimate `json:"estimate"`
	Scope             string        `json:"scope"`
	CalculatedAt      time.Time     `json:"calculatedAt"`
}

type JobEvent struct {
	ID        int64
	JobID     string
	Type      string
	Stage     string
	Payload   json.RawMessage
	CreatedAt time.Time
}

func (repository *Repository) AuthorizedJob(ctx context.Context, jobID string, principalID string, ownerScope string) (Job, error) {
	var job Job
	var sourceArtifactID, resultArtifactID sql.NullString
	err := repository.db.QueryRowContext(ctx, `
		SELECT id::text, principal_id, owner_scope, content_kind, document_engine, resource_class,
			priority_class, state, source_artifact_id, result_artifact_id, project_hash,
			options_hash, renderer_version, request_metadata, max_attempts, queued_at, available_at, started_at,
			finished_at, expires_at, cancel_requested, declared_source_bytes,
			reservation_expires_at, created_at, updated_at
		FROM rin_renderer.render_jobs
		WHERE id = $1::uuid AND principal_id = $2 AND owner_scope = $3`, jobID, principalID, ownerScope).Scan(
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
		return Job{}, fmt.Errorf("read authorized renderer job: %w", err)
	}
	job.SourceArtifactID, job.ResultArtifactID = sourceArtifactID.String, resultArtifactID.String
	return job, nil
}

func (repository *Repository) Queue(ctx context.Context, now time.Time) (QueueSummary, error) {
	summary := QueueSummary{Scope: "instance", CalculatedAt: now}
	var oldestWaitSeconds sql.NullFloat64
	err := repository.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE state = 'queued'), count(*) FILTER (WHERE state = 'running'),
			EXTRACT(EPOCH FROM ($1::timestamptz - min(queued_at) FILTER (WHERE state = 'queued')))
		FROM rin_renderer.render_jobs WHERE state IN ('queued', 'running')`, now).Scan(
		&summary.QueuedProjects, &summary.ActiveProjects, &oldestWaitSeconds,
	)
	if err != nil {
		return QueueSummary{}, fmt.Errorf("read renderer queue summary: %w", err)
	}
	if oldestWaitSeconds.Valid && oldestWaitSeconds.Float64 > 0 {
		summary.OldestWait = time.Duration(oldestWaitSeconds.Float64 * float64(time.Second))
	}
	return summary, nil
}

func (repository *Repository) ResourceUsage(ctx context.Context, now time.Time) (map[string]int64, error) {
	rows, err := repository.db.QueryContext(ctx, `
		SELECT resource_class, count(*)
		FROM rin_renderer.resource_allocations
		WHERE lease_expires_at > $1
		GROUP BY resource_class`, now)
	if err != nil {
		return nil, fmt.Errorf("read renderer resource usage: %w", err)
	}
	defer rows.Close()
	usage := map[string]int64{}
	for rows.Next() {
		var resourceClass string
		var count int64
		if err := rows.Scan(&resourceClass, &count); err != nil {
			return nil, fmt.Errorf("scan renderer resource usage: %w", err)
		}
		usage[resourceClass] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate renderer resource usage: %w", err)
	}
	return usage, nil
}

func (repository *Repository) AuthorizedJobQueue(ctx context.Context, jobID string, principalID string, ownerScope string, now time.Time, policy EstimationPolicy) (JobQueueSummary, error) {
	if err := policy.Validate(); err != nil {
		return JobQueueSummary{}, err
	}
	summary := JobQueueSummary{Scope: "instance", CalculatedAt: now}
	err := repository.db.QueryRowContext(ctx, `
		WITH target AS (
			SELECT id, queued_at FROM rin_renderer.render_jobs
			WHERE id = $1::uuid AND principal_id = $2 AND owner_scope = $3
		)
		SELECT
			(SELECT count(*) FROM rin_renderer.render_jobs AS queued, target
			 WHERE queued.state = 'queued' AND queued.cancel_requested = false
				AND (queued.queued_at, queued.id) < (target.queued_at, target.id)),
			(SELECT count(*) FROM rin_renderer.render_jobs WHERE state = 'queued'),
			(SELECT count(*) FROM rin_renderer.render_jobs WHERE state = 'running')
		FROM target`, jobID, principalID, ownerScope).Scan(
		&summary.JobsAheadEstimate, &summary.QueuedProjects, &summary.ActiveProjects,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return JobQueueSummary{}, ErrNotFound
	}
	if err != nil {
		return JobQueueSummary{}, fmt.Errorf("read authorized renderer job queue: %w", err)
	}
	target, err := repository.AuthorizedJob(ctx, jobID, principalID, ownerScope)
	if err != nil {
		return JobQueueSummary{}, err
	}
	if target.State != "queued" || target.CancelRequested {
		return summary, nil
	}
	estimate, jobsAhead, known, err := repository.estimateJobStart(ctx, target, now, policy)
	if err != nil {
		return JobQueueSummary{}, err
	}
	if known {
		summary.Estimate = &estimate
		summary.JobsAheadEstimate = jobsAhead
		wait := estimate.EstimatedStartAt.Sub(now)
		if wait < 0 {
			wait = 0
		}
		operational.Default().Count("eta_calculations_total", "known", target.ResourceClass)
		operational.Default().Observe("eta_wait_seconds", wait, target.ResourceClass)
		operational.Default().Event("eta_calculated", slog.String("request_id", target.ID), slog.String("job_id", target.ID),
			slog.String("content_kind", target.ContentKind), slog.String("engine", target.DocumentEngine),
			slog.String("resource_class", target.ResourceClass), slog.String("estimator_version", estimate.EstimatorVersion),
			slog.Int64("sample_count", estimate.SampleCount), slog.Int64("duration_ms", wait.Milliseconds()))
	} else {
		operational.Default().Count("eta_calculations_total", "unknown", target.ResourceClass)
	}
	return summary, nil
}

func (repository *Repository) estimateJobStart(ctx context.Context, target Job, now time.Time, policy EstimationPolicy) (WaitEstimate, int64, bool, error) {
	rows, err := repository.db.QueryContext(ctx, `
		SELECT jobs.id::text, jobs.principal_id, jobs.owner_scope, jobs.priority_class,
			jobs.state, jobs.queued_at, COALESCE(jobs.available_at, jobs.queued_at), jobs.started_at,
			COALESCE(profiles.sample_count, 0), profiles.p50_ms, profiles.p90_ms
		FROM rin_renderer.render_jobs AS jobs
		LEFT JOIN rin_renderer.render_job_workloads AS workloads ON workloads.job_id = jobs.id
		LEFT JOIN rin_renderer.workload_profiles AS profiles
			ON profiles.profile_key = workloads.profile_key
			AND profiles.renderer_version = jobs.renderer_version
		WHERE (
			(jobs.resource_class = $1)
			OR ($1 IN `+heavyResourceClassListSQL+`
				AND jobs.resource_class IN `+heavyResourceClassListSQL+`)
		) AND jobs.state IN ('queued', 'running')
			AND jobs.cancel_requested = false
		ORDER BY jobs.queued_at, jobs.id`, target.ResourceClass)
	if err != nil {
		return WaitEstimate{}, 0, false, fmt.Errorf("read renderer estimation jobs: %w", err)
	}
	defer rows.Close()
	jobs := []estimateJob{}
	for rows.Next() {
		var job estimateJob
		var p50, p90 sql.NullInt64
		if err := rows.Scan(&job.ID, &job.PrincipalID, &job.OwnerScope, &job.PriorityClass,
			&job.State, &job.QueuedAt, &job.AvailableAt, &job.StartedAt,
			&job.SampleCount, &p50, &p90); err != nil {
			return WaitEstimate{}, 0, false, fmt.Errorf("scan renderer estimation job: %w", err)
		}
		if p50.Valid {
			value := p50.Int64
			job.P50MS = &value
		}
		if p90.Valid {
			value := p90.Int64
			job.P90MS = &value
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return WaitEstimate{}, 0, false, fmt.Errorf("iterate renderer estimation jobs: %w", err)
	}
	dispatch := map[string]int64{"publish": 0, "preview": 0, "rebuild": 0, "migration": 0}
	priorityRows, err := repository.db.QueryContext(ctx, `
		SELECT priority_class, dispatch_count FROM rin_renderer.scheduler_priority_state`)
	if err != nil {
		return WaitEstimate{}, 0, false, fmt.Errorf("read renderer estimation fairness: %w", err)
	}
	for priorityRows.Next() {
		var priority string
		var count int64
		if err := priorityRows.Scan(&priority, &count); err != nil {
			priorityRows.Close()
			return WaitEstimate{}, 0, false, fmt.Errorf("scan renderer estimation fairness: %w", err)
		}
		dispatch[priority] = count
	}
	if err := priorityRows.Err(); err != nil {
		priorityRows.Close()
		return WaitEstimate{}, 0, false, fmt.Errorf("iterate renderer estimation fairness: %w", err)
	}
	if err := priorityRows.Close(); err != nil {
		return WaitEstimate{}, 0, false, err
	}
	simulationPolicy := policy
	capacity := estimateResourceCapacity(policy.Scheduling.Resources, target.ResourceClass)
	if heavyResourceClass(target.ResourceClass) {
		capacity = policy.Scheduling.Resources.Heavy
	}
	simulationPolicy.Scheduling.Resources.DocumentLight = capacity
	if previewResourceClass(target.ResourceClass) {
		simulationPolicy.Scheduling.PrincipalRunning = 1
	}
	p50, p50Known := simulateEstimatedStart(target.ID, jobs, cloneDispatch(dispatch), simulationPolicy, now, 50)
	p90, p90Known := simulateEstimatedStart(target.ID, jobs, cloneDispatch(dispatch), simulationPolicy, now, 90)
	if !p50Known || !p90Known {
		return WaitEstimate{}, 0, false, nil
	}
	minimumSamples := p50.samples
	if p90.samples < minimumSamples {
		minimumSamples = p90.samples
	}
	latest := p90.start
	if latest.Before(p50.start) {
		latest = p50.start
	}
	estimate := WaitEstimate{
		EstimatedStartAt:    p50.start,
		EstimatedStartRange: EstimatedStartRange{Earliest: p50.start, Latest: latest},
		Confidence:          estimateConfidence(minimumSamples), SampleCount: minimumSamples,
		EstimatorVersion: WaitEstimatorVersion, Scope: "instance", CalculatedAt: now,
	}
	if err := repository.storeWaitEstimate(ctx, target.ID, estimate); err != nil {
		return WaitEstimate{}, 0, false, err
	}
	return estimate, p50.jobsAhead, true, nil
}

func estimateResourceCapacity(capacities ResourceCapacities, resourceClass string) int64 {
	switch resourceClass {
	case "document-light":
		return capacities.DocumentLight
	case "document-latexml":
		return capacities.DocumentLaTeXML
	case "math-node":
		return capacities.MathNode
	case "texsvg":
		return capacities.TeXSVG
	case "latex-pdf":
		return capacities.LatexPDF
	case "document-typst":
		return capacities.DocumentTypst
	case "typst-pdf":
		return capacities.TypstPDF
	default:
		return capacities.BatchMigration
	}
}

func cloneDispatch(input map[string]int64) map[string]int64 {
	return map[string]int64{"publish": input["publish"], "preview": input["preview"], "rebuild": input["rebuild"], "migration": input["migration"]}
}

func (repository *Repository) storeWaitEstimate(ctx context.Context, jobID string, estimate WaitEstimate) error {
	_, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_wait_estimates (
			job_id, estimator_version, estimated_start_at, earliest_start_at, latest_start_at,
			confidence, sample_count, scope, calculated_at
		) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (job_id) DO UPDATE SET
			estimator_version = EXCLUDED.estimator_version,
			estimated_start_at = EXCLUDED.estimated_start_at,
			earliest_start_at = EXCLUDED.earliest_start_at,
			latest_start_at = EXCLUDED.latest_start_at,
			confidence = EXCLUDED.confidence,
			sample_count = EXCLUDED.sample_count,
			scope = EXCLUDED.scope,
			calculated_at = EXCLUDED.calculated_at,
			updated_at = clock_timestamp()
		WHERE rin_renderer.render_job_wait_estimates.actual_started_at IS NULL`,
		jobID, estimate.EstimatorVersion, estimate.EstimatedStartAt,
		estimate.EstimatedStartRange.Earliest, estimate.EstimatedStartRange.Latest,
		estimate.Confidence, estimate.SampleCount, estimate.Scope, estimate.CalculatedAt)
	if err != nil {
		return fmt.Errorf("store renderer wait estimate: %w", err)
	}
	return nil
}

func (repository *Repository) AuthorizedEvents(ctx context.Context, jobID string, principalID string, ownerScope string, afterID int64, limit int) ([]JobEvent, error) {
	if limit <= 0 {
		return nil, errors.New("renderer event limit must be positive")
	}
	rows, err := repository.db.QueryContext(ctx, `
		SELECT events.id, events.job_id::text, events.event_type, COALESCE(events.stage, ''),
			events.payload, events.created_at
		FROM rin_renderer.render_job_events AS events
		JOIN rin_renderer.render_jobs AS jobs ON jobs.id = events.job_id
		WHERE events.job_id = $1::uuid AND jobs.principal_id = $2 AND jobs.owner_scope = $3
			AND events.id > $4
		ORDER BY events.id LIMIT $5`, jobID, principalID, ownerScope, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("read authorized renderer events: %w", err)
	}
	defer rows.Close()
	var events []JobEvent
	for rows.Next() {
		var event JobEvent
		if err := rows.Scan(&event.ID, &event.JobID, &event.Type, &event.Stage, &event.Payload, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan authorized renderer event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authorized renderer events: %w", err)
	}
	if len(events) == 0 {
		if _, err := repository.AuthorizedJob(ctx, jobID, principalID, ownerScope); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (repository *Repository) Artifact(ctx context.Context, artifactID string) (Artifact, error) {
	var artifact Artifact
	var expiresAt sql.NullTime
	err := repository.db.QueryRowContext(ctx, `
		SELECT id, sha256, kind, visibility, storage_key, byte_size, media_type,
			COALESCE(schema_version, ''), expires_at, created_at
		FROM rin_renderer.render_artifacts WHERE id = $1`, artifactID).Scan(
		&artifact.ID, &artifact.SHA256, &artifact.Kind, &artifact.Visibility,
		&artifact.StorageKey, &artifact.ByteSize, &artifact.MediaType,
		&artifact.SchemaVersion, &expiresAt, &artifact.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("read renderer artifact: %w", err)
	}
	if expiresAt.Valid {
		artifact.ExpiresAt = &expiresAt.Time
	}
	return artifact, nil
}
