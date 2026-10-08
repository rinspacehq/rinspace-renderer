package jobpostgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
)

var (
	ErrLeadershipUnavailable = errors.New("renderer scheduler leadership unavailable")
	ErrNotLeader             = errors.New("renderer scheduler is not leader")
	ErrNoEligibleJob         = errors.New("no eligible renderer job")
	ErrLeaseLost             = errors.New("renderer job lease is no longer active")
)

type Leadership struct {
	mu            sync.Mutex
	conn          *sql.Conn
	lockName      string
	resourceClass string
	released      bool
}

// AcquireLeadership holds a session advisory lock on a dedicated SQL connection. Closing or
// losing that connection releases leadership, allowing a replacement scheduler to recover leases.
func (repository *Repository) AcquireLeadership(ctx context.Context) (*Leadership, error) {
	return repository.acquireLeadership(ctx, "", "rin_renderer_scheduler")
}

// AcquireResourceLeadership gives an independent worker pool leadership for one
// explicitly isolated class. The ordinary lock name is unchanged for mixed-version safety.
func (repository *Repository) AcquireResourceLeadership(ctx context.Context, resourceClass string) (*Leadership, error) {
	if !IsDedicatedWorkerResourceClass(resourceClass) {
		return nil, errors.New("renderer resource leadership class is invalid")
	}
	return repository.acquireLeadership(ctx, resourceClass, "rin_renderer_scheduler:"+resourceClass)
}

func (repository *Repository) acquireLeadership(ctx context.Context, resourceClass, lockName string) (*Leadership, error) {
	conn, err := repository.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve scheduler connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, lockName).Scan(&acquired); err != nil {
		conn.Close()
		return nil, fmt.Errorf("acquire renderer scheduler leadership: %w", err)
	}
	if !acquired {
		conn.Close()
		return nil, ErrLeadershipUnavailable
	}
	return &Leadership{conn: conn, lockName: lockName, resourceClass: resourceClass}, nil
}

func (leadership *Leadership) Close() error {
	leadership.mu.Lock()
	defer leadership.mu.Unlock()
	if leadership.released {
		return nil
	}
	leadership.released = true
	var unlocked bool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := leadership.conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, leadership.lockName).Scan(&unlocked)
	closeErr := leadership.conn.Close()
	if err != nil {
		return fmt.Errorf("release renderer scheduler leadership: %w", err)
	}
	if !unlocked {
		return ErrNotLeader
	}
	if closeErr != nil {
		return fmt.Errorf("close renderer scheduler connection: %w", closeErr)
	}
	return nil
}

type ClaimInput struct {
	AttemptID      string
	WorkerID       string
	LeaseTokenHash string
	Now            time.Time
	LeaseExpiresAt time.Time
	Policy         SchedulingPolicy
	// Empty selects the ordinary worker pool, which deliberately excludes
	// dedicated PDF preview/export classes. Dedicated workers select one exact resource class.
	ResourceClass string
}

type ResourceCapacities struct {
	DocumentLight   int64
	DocumentLaTeXML int64
	MathNode        int64
	TeXSVG          int64
	BatchMigration  int64
	LatexPDF        int64
	DocumentTypst   int64
	TypstPDF        int64
	Heavy           int64
}

type PriorityWeights struct {
	Publish   int64
	Preview   int64
	Rebuild   int64
	Migration int64
}

type SchedulingPolicy struct {
	Resources        ResourceCapacities
	Weights          PriorityWeights
	AgingInterval    time.Duration
	MaxAgingSteps    int64
	PrincipalRunning int64
}

func (policy SchedulingPolicy) Validate() error {
	if policy.Resources.DocumentLight <= 0 || policy.Resources.DocumentLaTeXML <= 0 ||
		policy.Resources.MathNode <= 0 || policy.Resources.TeXSVG <= 0 ||
		policy.Resources.BatchMigration <= 0 || policy.Resources.LatexPDF <= 0 ||
		policy.Resources.DocumentTypst <= 0 || policy.Resources.TypstPDF <= 0 ||
		policy.Resources.Heavy <= 0 || policy.Weights.Publish <= 0 ||
		policy.Weights.Preview <= 0 || policy.Weights.Rebuild <= 0 || policy.Weights.Migration <= 0 ||
		policy.Weights.Publish <= policy.Weights.Preview || policy.Weights.Preview <= policy.Weights.Rebuild ||
		policy.Weights.Rebuild <= policy.Weights.Migration ||
		policy.AgingInterval <= 0 || policy.MaxAgingSteps <= 0 || policy.PrincipalRunning <= 0 {
		return errors.New("renderer scheduling policy must be positive")
	}
	return nil
}

type Lease struct {
	Job            Job
	AttemptID      string
	AttemptNo      int16
	WorkerID       string
	LeaseExpiresAt time.Time
}

func (repository *Repository) ClaimNext(ctx context.Context, leadership *Leadership, input ClaimInput) (Lease, error) {
	if leadership == nil {
		return Lease{}, ErrNotLeader
	}
	leadership.mu.Lock()
	defer leadership.mu.Unlock()
	if leadership.released || leadership.conn == nil {
		return Lease{}, ErrNotLeader
	}
	if input.WorkerID == "" || input.AttemptID == "" || input.LeaseTokenHash == "" ||
		input.Now.IsZero() || !input.LeaseExpiresAt.After(input.Now) {
		return Lease{}, errors.New("renderer claim input is invalid")
	}
	if err := input.Policy.Validate(); err != nil {
		return Lease{}, err
	}
	if input.ResourceClass != "" && resourceCapacity(input.Policy.Resources, input.ResourceClass) <= 0 {
		return Lease{}, errors.New("renderer claim resource class is invalid")
	}
	if input.ResourceClass != leadership.resourceClass {
		return Lease{}, errors.New("renderer claim does not match scheduler leadership class")
	}
	tx, err := leadership.conn.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("begin renderer job claim: %w", err)
	}
	defer tx.Rollback()
	// Top-level claims and direct formula/diagram subwork share the same capacity
	// pools. Serialize their count-and-insert sections so neither path can
	// over-allocate a resource class under concurrent load.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, resourceCapacityLock); err != nil {
		return Lease{}, fmt.Errorf("lock renderer resource capacity: %w", err)
	}
	weightSQL := `CASE priority_class WHEN 'publish' THEN $2::numeric WHEN 'preview' THEN $3::numeric WHEN 'rebuild' THEN $4::numeric ELSE $5::numeric END`
	if _, err := tx.ExecContext(ctx, `
		WITH maximum_service AS (
			SELECT max(dispatch_count::numeric / (`+weightSQL+`)) AS ratio
			FROM rin_renderer.scheduler_priority_state
		)
		UPDATE rin_renderer.scheduler_priority_state AS state
		SET dispatch_count = GREATEST(
			state.dispatch_count,
			CEIL(COALESCE((SELECT ratio FROM maximum_service), 0) * (`+weightSQL+`))::bigint
		), updated_at = clock_timestamp()
		WHERE NOT EXISTS (
			SELECT 1 FROM rin_renderer.render_jobs AS queued
			WHERE queued.state = 'queued' AND queued.cancel_requested = false
				AND queued.priority_class = state.priority_class
				AND (queued.available_at IS NULL OR queued.available_at <= $1)
				AND (($6 = '' AND queued.resource_class IN `+documentWorkerResourceClassListSQL+`) OR queued.resource_class = $6)
		)`, input.Now, input.Policy.Weights.Publish, input.Policy.Weights.Preview,
		input.Policy.Weights.Rebuild, input.Policy.Weights.Migration, input.ResourceClass,
	); err != nil {
		return Lease{}, fmt.Errorf("normalize renderer priority fairness: %w", err)
	}
	var jobID, priorityClass string
	err = tx.QueryRowContext(ctx, `
		SELECT jobs.id::text, jobs.priority_class
		FROM rin_renderer.render_jobs AS jobs
		JOIN rin_renderer.scheduler_priority_state AS priority
			ON priority.priority_class = jobs.priority_class
		WHERE jobs.state = 'queued' AND jobs.cancel_requested = false
			AND (jobs.available_at IS NULL OR jobs.available_at <= $1)
			AND (($15 = '' AND jobs.resource_class IN `+documentWorkerResourceClassListSQL+`) OR jobs.resource_class = $15)
			AND NOT (
				jobs.resource_class IN `+previewResourceClassListSQL+` AND EXISTS (
					SELECT 1 FROM rin_renderer.render_jobs AS context_running
					WHERE context_running.resource_class = jobs.resource_class
						AND context_running.state = 'running'
						AND context_running.principal_id = jobs.principal_id
						AND context_running.owner_scope = jobs.owner_scope
				)
			)
			AND (
				SELECT count(*) FROM rin_renderer.resource_allocations AS allocations
				WHERE allocations.resource_class = jobs.resource_class
					AND allocations.lease_expires_at > $1
			) < CASE jobs.resource_class
				WHEN 'document-light' THEN $2::bigint
				WHEN 'document-latexml' THEN $3::bigint
				WHEN 'math-node' THEN $4::bigint
				WHEN 'texsvg' THEN $5::bigint
				WHEN 'batch-migration' THEN $6::bigint
				WHEN 'latex-pdf' THEN $7::bigint
				WHEN 'document-typst' THEN $17::bigint
				WHEN 'typst-pdf' THEN $18::bigint
				ELSE 0::bigint
			END
			AND (
				SELECT count(*) FROM rin_renderer.render_jobs AS running
				WHERE running.state = 'running'
					AND running.principal_id = jobs.principal_id
					AND running.owner_scope = jobs.owner_scope
			) < $8::bigint
			AND (
				jobs.resource_class NOT IN `+heavyResourceClassListSQL+`
				OR (
					SELECT count(*) FROM rin_renderer.resource_allocations AS heavy_allocations
					WHERE heavy_allocations.resource_class IN `+heavyResourceClassListSQL+`
						AND heavy_allocations.lease_expires_at > $1
				) < $16::bigint
			)
		ORDER BY (
			priority.dispatch_count::numeric /
				(CASE jobs.priority_class WHEN 'publish' THEN $9::numeric WHEN 'preview' THEN $10::numeric WHEN 'rebuild' THEN $11::numeric ELSE $12::numeric END)
			- LEAST(
				$14::numeric,
				FLOOR(GREATEST(0, EXTRACT(EPOCH FROM ($1 - jobs.queued_at))) / $13::numeric)
			)
		), CASE jobs.priority_class
			WHEN 'publish' THEN 0 WHEN 'preview' THEN 1 WHEN 'rebuild' THEN 2 ELSE 3
		END, jobs.queued_at, jobs.id
		FOR UPDATE OF jobs SKIP LOCKED
		LIMIT 1`, input.Now,
		input.Policy.Resources.DocumentLight, input.Policy.Resources.DocumentLaTeXML,
		input.Policy.Resources.MathNode, input.Policy.Resources.TeXSVG,
		input.Policy.Resources.BatchMigration, input.Policy.Resources.LatexPDF, input.Policy.PrincipalRunning,
		input.Policy.Weights.Publish, input.Policy.Weights.Preview, input.Policy.Weights.Rebuild, input.Policy.Weights.Migration,
		input.Policy.AgingInterval.Seconds(), input.Policy.MaxAgingSteps, input.ResourceClass,
		input.Policy.Resources.Heavy, input.Policy.Resources.DocumentTypst, input.Policy.Resources.TypstPDF,
	).Scan(&jobID, &priorityClass)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNoEligibleJob
	}
	if err != nil {
		return Lease{}, fmt.Errorf("select renderer queued job: %w", err)
	}
	job, err := jobByID(ctx, tx, jobID, false)
	if err != nil {
		return Lease{}, err
	}
	var attemptNo int16
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(max(attempt_no), 0) + 1
		FROM rin_renderer.render_attempts
		WHERE job_id = $1::uuid`, job.ID).Scan(&attemptNo); err != nil {
		return Lease{}, fmt.Errorf("allocate renderer attempt number: %w", err)
	}
	if attemptNo > job.MaxAttempts {
		return Lease{}, errors.New("renderer queued job exhausted attempts")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_attempts (
			id, job_id, attempt_no, worker_id, lease_token_hash, lease_expires_at,
			heartbeat_at, state, started_at
		) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, 'running', $7)`,
		input.AttemptID, job.ID, attemptNo, input.WorkerID, input.LeaseTokenHash,
		input.LeaseExpiresAt, input.Now); err != nil {
		return Lease{}, fmt.Errorf("create renderer attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.resource_allocations (
			id, resource_class, job_id, attempt_id, allocation_kind, owner_id,
			lease_token_hash, lease_expires_at, heartbeat_at
		) VALUES ($1::uuid, $2, $3::uuid, $1::uuid, 'top-level', $4, $5, $6, $7)`,
		input.AttemptID, job.ResourceClass, job.ID, input.WorkerID, input.LeaseTokenHash,
		input.LeaseExpiresAt, input.Now); err != nil {
		return Lease{}, fmt.Errorf("allocate renderer resource slot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.scheduler_priority_state
		SET dispatch_count = dispatch_count + 1, updated_at = clock_timestamp()
		WHERE priority_class = $1`, priorityClass); err != nil {
		return Lease{}, fmt.Errorf("advance renderer priority fairness: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_jobs
		SET state = 'running', started_at = COALESCE(started_at, $2), updated_at = clock_timestamp()
		WHERE id = $1::uuid AND state = 'queued'`, job.ID, input.Now)
	if err != nil {
		return Lease{}, fmt.Errorf("start renderer job: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return Lease{}, ErrLeaseLost
	}
	var estimateError sql.NullInt64
	var intervalCovered sql.NullBool
	err = tx.QueryRowContext(ctx, `
		UPDATE rin_renderer.render_job_wait_estimates
		SET actual_started_at = $2,
			central_error_ms = round(EXTRACT(EPOCH FROM ($2 - estimated_start_at)) * 1000)::bigint,
			interval_covered = $2 BETWEEN earliest_start_at AND latest_start_at,
			updated_at = clock_timestamp()
		WHERE job_id = $1::uuid AND actual_started_at IS NULL
		RETURNING abs(central_error_ms), interval_covered`, job.ID, input.Now).Scan(&estimateError, &intervalCovered)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Lease{}, fmt.Errorf("record renderer wait estimate calibration: %w", err)
	}
	if estimateError.Valid {
		operational.Default().Observe("eta_error_seconds", time.Duration(estimateError.Int64)*time.Millisecond, job.ResourceClass)
		coverage := "miss"
		if intervalCovered.Valid && intervalCovered.Bool {
			coverage = "covered"
		}
		operational.Default().Count("eta_interval_total", coverage, job.ResourceClass)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
		VALUES ($1::uuid, 'running', 'claimed', jsonb_build_object('attemptNo', $2::smallint))`,
		job.ID, attemptNo); err != nil {
		return Lease{}, fmt.Errorf("record renderer claim event: %w", err)
	}
	job, err = jobByID(ctx, tx, job.ID, false)
	if err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, fmt.Errorf("commit renderer job claim: %w", err)
	}
	return Lease{
		Job: job, AttemptID: input.AttemptID, AttemptNo: attemptNo,
		WorkerID: input.WorkerID, LeaseExpiresAt: input.LeaseExpiresAt,
	}, nil
}

func (repository *Repository) Heartbeat(ctx context.Context, attemptID string, leaseTokenHash string, now time.Time, leaseExpiresAt time.Time) error {
	if !leaseExpiresAt.After(now) {
		return errors.New("renderer heartbeat expiry must be in the future")
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin renderer heartbeat: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_attempts AS attempts
		SET heartbeat_at = $3, lease_expires_at = $4
		FROM rin_renderer.render_jobs AS jobs
		WHERE attempts.id = $1::uuid AND attempts.lease_token_hash = $2
			AND attempts.state = 'running' AND attempts.lease_expires_at > $3
			AND attempts.error_code IS NULL
			AND jobs.id = attempts.job_id AND jobs.state = 'running'
			AND jobs.cancel_requested = false`,
		attemptID, leaseTokenHash, now, leaseExpiresAt)
	if err != nil {
		return fmt.Errorf("heartbeat renderer attempt: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read renderer heartbeat count: %w", err)
	}
	if count != 1 {
		return ErrLeaseLost
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE rin_renderer.resource_allocations
		SET heartbeat_at = $3, lease_expires_at = $4
		WHERE attempt_id = $1::uuid AND lease_token_hash = $2 AND lease_expires_at > $3`,
		attemptID, leaseTokenHash, now, leaseExpiresAt)
	if err != nil {
		return fmt.Errorf("heartbeat renderer resource allocation: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit renderer heartbeat: %w", err)
	}
	return nil
}

type FinishInput struct {
	AttemptID        string
	LeaseTokenHash   string
	Now              time.Time
	Succeeded        bool
	Retryable        bool
	ErrorCode        string
	RetryAvailableAt time.Time
	ResultArtifact   *Artifact
	CompletionResult *CompletionResultEvidence
}

func (repository *Repository) FinishAttempt(ctx context.Context, input FinishInput) (Job, error) {
	if input.AttemptID == "" || input.LeaseTokenHash == "" || input.Now.IsZero() {
		return Job{}, errors.New("renderer attempt finish input is invalid")
	}
	if input.Succeeded && (input.Retryable || input.ErrorCode != "") {
		return Job{}, errors.New("successful renderer attempt cannot include failure metadata")
	}
	if !input.Succeeded && input.ErrorCode == "" {
		return Job{}, errors.New("failed renderer attempt requires an error code")
	}
	if input.Retryable && !input.RetryAvailableAt.After(input.Now) {
		return Job{}, errors.New("retryable renderer attempt requires future availability")
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("begin renderer attempt finish: %w", err)
	}
	defer tx.Rollback()
	var finishResourceClass, finishPrincipalID, finishOwnerScope string
	err = tx.QueryRowContext(ctx, `
		SELECT jobs.resource_class, jobs.principal_id, jobs.owner_scope
		FROM rin_renderer.render_attempts AS attempts
		JOIN rin_renderer.render_jobs AS jobs ON jobs.id = attempts.job_id
		WHERE attempts.id = $1::uuid AND attempts.lease_token_hash = $2`,
		input.AttemptID, input.LeaseTokenHash).Scan(&finishResourceClass, &finishPrincipalID, &finishOwnerScope)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrLeaseLost
	}
	if err != nil {
		return Job{}, fmt.Errorf("read renderer attempt finish context: %w", err)
	}
	if previewResourceClass(finishResourceClass) {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
			"rin_renderer_preview:"+finishPrincipalID+"\x1f"+finishOwnerScope); err != nil {
			return Job{}, fmt.Errorf("lock renderer preview finish context: %w", err)
		}
	}
	var jobID string
	var attemptNo int16
	var startedAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT job_id::text, attempt_no, started_at
		FROM rin_renderer.render_attempts
		WHERE id = $1::uuid AND lease_token_hash = $2 AND state = 'running'
			AND lease_expires_at > $3 AND error_code IS NULL
		FOR UPDATE`, input.AttemptID, input.LeaseTokenHash, input.Now).Scan(&jobID, &attemptNo, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrLeaseLost
	}
	if err != nil {
		return Job{}, fmt.Errorf("lock renderer attempt finish: %w", err)
	}
	job, err := jobByID(ctx, tx, jobID, true)
	if err != nil {
		return Job{}, err
	}
	if job.State != "running" {
		return Job{}, ErrLeaseLost
	}
	durationMS := input.Now.Sub(startedAt).Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}
	attemptState := "failed"
	jobState := "failed"
	eventType := "failed"
	finishedAt := any(input.Now)
	availableAt := any(nil)
	supersededBy := any(nil)
	if job.CancelRequested {
		attemptState, jobState, eventType = "canceled", "canceled", "canceled"
		input.ErrorCode = "job_canceled"
	} else if input.Succeeded {
		attemptState, jobState, eventType = "succeeded", "succeeded", "succeeded"
	} else if input.Retryable && attemptNo < job.MaxAttempts {
		var successorID string
		hasSuccessor, err := queuedPreviewSuccessor(ctx, tx, job, &successorID)
		if err != nil {
			return Job{}, err
		}
		if hasSuccessor {
			attemptState, jobState, eventType = "canceled", "canceled", "superseded"
			input.ErrorCode, supersededBy = "preview_superseded", successorID
		} else {
			jobState, eventType = "queued", "retry_queued"
			finishedAt = nil
			availableAt = input.RetryAvailableAt
		}
	}
	if jobState != "canceled" {
		if err := recordWorkloadOutcome(ctx, tx, job, durationMS, input.Succeeded, input.ErrorCode); err != nil {
			return Job{}, err
		}
	}
	resultArtifactID := any(nil)
	if (jobState == "succeeded" || jobState == "failed") && input.ResultArtifact != nil {
		artifact := *input.ResultArtifact
		if artifact.Kind != "result" || artifact.Visibility != "private" || artifact.ExpiresAt == nil {
			return Job{}, errors.New("terminal renderer result artifact is invalid")
		}
		if err := createOrVerifyArtifact(ctx, tx, artifact); err != nil {
			return Job{}, err
		}
		resultArtifactID = artifact.ID
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_attempts
		SET state = $2, error_code = NULLIF($3, ''), duration_ms = $4,
			finished_at = $5, lease_expires_at = NULL, lease_token_hash = NULL
		WHERE id = $1::uuid`, input.AttemptID, attemptState, input.ErrorCode,
		durationMS, input.Now); err != nil {
		return Job{}, fmt.Errorf("finish renderer attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM rin_renderer.resource_allocations
		WHERE attempt_id = $1::uuid OR ($2 = 'canceled' AND job_id = $3::uuid)`, input.AttemptID, jobState, job.ID); err != nil {
		return Job{}, fmt.Errorf("release renderer resource allocation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_jobs
		SET state = $2, available_at = $3, finished_at = $4,
			result_artifact_id = COALESCE($5, result_artifact_id),
			superseded_by = COALESCE($6::uuid, superseded_by), updated_at = clock_timestamp()
		WHERE id = $1::uuid`, job.ID, jobState, availableAt, finishedAt, resultArtifactID, supersededBy); err != nil {
		return Job{}, fmt.Errorf("finish renderer job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
		VALUES ($1::uuid, $2, 'attempt', jsonb_build_object('attemptNo', $3::smallint, 'errorCode', NULLIF($4, '')))`,
		job.ID, eventType, attemptNo, input.ErrorCode); err != nil {
		return Job{}, fmt.Errorf("record renderer attempt finish event: %w", err)
	}
	if jobState != "queued" {
		resultReference := ""
		if jobState == "succeeded" && input.ResultArtifact != nil {
			resultReference = input.ResultArtifact.ID
		}
		if err := insertTerminalCompletionTx(
			ctx, tx, job, jobState, resultReference, input.CompletionResult, input.ErrorCode, attemptNo, input.Now,
		); err != nil {
			return Job{}, err
		}
	}
	job, err = jobByID(ctx, tx, job.ID, false)
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit renderer attempt finish: %w", err)
	}
	return job, nil
}

func createOrVerifyArtifact(ctx context.Context, tx *sql.Tx, artifact Artifact) error {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_artifacts (
			id, sha256, kind, visibility, storage_key, byte_size, media_type, schema_version, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9)
		ON CONFLICT (id) DO NOTHING`,
		artifact.ID, artifact.SHA256, artifact.Kind, artifact.Visibility, artifact.StorageKey,
		artifact.ByteSize, artifact.MediaType, artifact.SchemaVersion, artifact.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("record renderer result artifact: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read renderer result artifact insert: %w", err)
	}
	if inserted == 1 {
		return nil
	}
	var existing Artifact
	var schemaVersion sql.NullString
	var expiresAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT id, sha256, kind, visibility, storage_key, byte_size, media_type, schema_version, expires_at
		FROM rin_renderer.render_artifacts WHERE id = $1`, artifact.ID).Scan(
		&existing.ID, &existing.SHA256, &existing.Kind, &existing.Visibility, &existing.StorageKey,
		&existing.ByteSize, &existing.MediaType, &schemaVersion, &expiresAt,
	); err != nil {
		return fmt.Errorf("read existing renderer result artifact: %w", err)
	}
	existing.SchemaVersion = schemaVersion.String
	if expiresAt.Valid {
		existing.ExpiresAt = &expiresAt.Time
	}
	if !sameArtifact(existing, artifact) {
		return errors.New("renderer result artifact conflicts with existing metadata")
	}
	return nil
}

func (repository *Repository) RecoverExpiredLeases(ctx context.Context, leadership *Leadership, now time.Time, retryAvailableAt time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("renderer lease recovery limit must be positive")
	}
	if leadership == nil {
		return 0, ErrNotLeader
	}
	leadership.mu.Lock()
	defer leadership.mu.Unlock()
	if leadership.released || leadership.conn == nil {
		return 0, ErrNotLeader
	}
	tx, err := leadership.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin renderer lease recovery: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT attempts.id::text, attempts.job_id::text, attempts.attempt_no,
			attempts.started_at, jobs.max_attempts, jobs.cancel_requested,
			COALESCE(attempts.error_code, 'worker_lease_expired')
		FROM rin_renderer.render_attempts AS attempts
		JOIN rin_renderer.render_jobs AS jobs ON jobs.id = attempts.job_id
		WHERE attempts.state = 'running' AND attempts.lease_expires_at <= $1
			AND jobs.state = 'running'
			AND (($3 = '' AND jobs.resource_class IN `+documentWorkerResourceClassListSQL+`) OR jobs.resource_class = $3)
		ORDER BY attempts.lease_expires_at, attempts.id
		FOR UPDATE OF attempts, jobs SKIP LOCKED
		LIMIT $2`, now, limit, leadership.resourceClass)
	if err != nil {
		return 0, fmt.Errorf("select expired renderer leases: %w", err)
	}
	type expiredLease struct {
		attemptID       string
		jobID           string
		attemptNo       int16
		startedAt       time.Time
		maxAttempts     int16
		cancelRequested bool
		errorCode       string
	}
	var expired []expiredLease
	for rows.Next() {
		var item expiredLease
		if err := rows.Scan(&item.attemptID, &item.jobID, &item.attemptNo, &item.startedAt, &item.maxAttempts, &item.cancelRequested, &item.errorCode); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired renderer lease: %w", err)
		}
		expired = append(expired, item)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close expired renderer leases: %w", err)
	}
	for _, item := range expired {
		durationMS := now.Sub(item.startedAt).Milliseconds()
		if durationMS < 0 {
			durationMS = 0
		}
		attemptState := "expired"
		errorCode := item.errorCode
		if item.cancelRequested {
			attemptState, errorCode = "canceled", "job_canceled"
		}
		jobState := "failed"
		eventType := "failed"
		finishedAt := any(now)
		availableAt := any(nil)
		supersededBy := any(nil)
		if item.cancelRequested {
			jobState, eventType = "canceled", "canceled"
		}
		job, err := jobByID(ctx, tx, item.jobID, false)
		if err != nil {
			return 0, err
		}
		if previewResourceClass(job.ResourceClass) {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
				"rin_renderer_preview:"+job.PrincipalID+"\x1f"+job.OwnerScope); err != nil {
				return 0, fmt.Errorf("lock renderer preview recovery context: %w", err)
			}
		}
		if !item.cancelRequested && item.attemptNo < item.maxAttempts {
			var successorID string
			hasSuccessor, err := queuedPreviewSuccessor(ctx, tx, job, &successorID)
			if err != nil {
				return 0, err
			}
			if hasSuccessor {
				attemptState, jobState, eventType, errorCode = "canceled", "canceled", "superseded", "preview_superseded"
				supersededBy = successorID
			} else {
				jobState, eventType = "queued", "retry_queued"
				finishedAt = nil
				availableAt = retryAvailableAt
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rin_renderer.render_attempts
			SET state = $2, error_code = $3, duration_ms = $4,
				finished_at = $5, lease_expires_at = NULL, lease_token_hash = NULL
			WHERE id = $1::uuid`, item.attemptID, attemptState, errorCode, durationMS, now); err != nil {
			return 0, fmt.Errorf("expire renderer attempt: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM rin_renderer.resource_allocations
			WHERE attempt_id = $1::uuid OR ($2 = 'canceled' AND job_id = $3::uuid)`, item.attemptID, jobState, item.jobID); err != nil {
			return 0, fmt.Errorf("release expired renderer resource allocation: %w", err)
		}
		if !item.cancelRequested {
			if err := recordWorkloadOutcome(ctx, tx, job, durationMS, false, errorCode); err != nil {
				return 0, err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rin_renderer.render_jobs
			SET state = $2, available_at = $3, finished_at = $4,
				superseded_by = COALESCE($5::uuid, superseded_by), updated_at = clock_timestamp()
			WHERE id = $1::uuid`, item.jobID, jobState, availableAt, finishedAt, supersededBy); err != nil {
			return 0, fmt.Errorf("recover renderer job lease: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
			VALUES ($1::uuid, $2, 'lease_recovery', jsonb_build_object(
				'attemptNo', $3::smallint, 'errorCode', $4::text))`,
			item.jobID, eventType, item.attemptNo, errorCode); err != nil {
			return 0, fmt.Errorf("record renderer lease recovery event: %w", err)
		}
		if jobState != "queued" {
			if err := insertTerminalCompletionTx(ctx, tx, job, jobState, "", nil, errorCode, item.attemptNo, now); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit renderer lease recovery: %w", err)
	}
	return len(expired), nil
}

func queuedPreviewSuccessor(ctx context.Context, tx *sql.Tx, job Job, successorID *string) (bool, error) {
	if !previewResourceClass(job.ResourceClass) {
		return false, nil
	}
	err := tx.QueryRowContext(ctx, `
		SELECT id::text
		FROM rin_renderer.render_jobs
		WHERE principal_id = $1 AND owner_scope = $2
			AND resource_class = $4 AND state = 'queued'
			AND cancel_requested = false AND id <> $3::uuid
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, job.PrincipalID, job.OwnerScope, job.ID, job.ResourceClass).Scan(successorID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read queued renderer preview successor: %w", err)
	}
	return true, nil
}
