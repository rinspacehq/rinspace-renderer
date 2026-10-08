package jobpostgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrCancellationNotAllowed = errors.New("renderer job cannot be canceled in its current state")

const runningCancellationLeaseGrace = 10 * time.Second

type Cancellation struct {
	Job            Job
	WorkerMustStop bool
}

func (repository *Repository) RequestCancellation(ctx context.Context, jobID string, now time.Time) (Cancellation, error) {
	if jobID == "" || now.IsZero() {
		return Cancellation{}, errors.New("renderer cancellation input is invalid")
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return Cancellation{}, fmt.Errorf("begin renderer cancellation: %w", err)
	}
	defer tx.Rollback()
	job, err := jobByID(ctx, tx, jobID, true)
	if err != nil {
		return Cancellation{}, err
	}
	workerMustStop := false
	terminal := false
	eventType := "canceled"
	stage := "queue"
	switch job.State {
	case "queued":
		terminal = true
		if _, err := tx.ExecContext(ctx, `
			UPDATE rin_renderer.render_jobs
			SET state = 'canceled', cancel_requested = true, available_at = NULL,
				finished_at = $2, updated_at = clock_timestamp()
			WHERE id = $1::uuid`, job.ID, now); err != nil {
			return Cancellation{}, fmt.Errorf("cancel renderer queued job: %w", err)
		}
	case "running":
		if job.CancelRequested {
			return Cancellation{Job: job, WorkerMustStop: true}, nil
		}
		workerMustStop, eventType, stage = true, "cancel_requested", "running"
		if _, err := tx.ExecContext(ctx, `
			UPDATE rin_renderer.render_jobs
			SET cancel_requested = true, updated_at = clock_timestamp()
			WHERE id = $1::uuid`, job.ID); err != nil {
			return Cancellation{}, fmt.Errorf("request renderer running cancellation: %w", err)
		}
		cancelLeaseExpiresAt := now.Add(runningCancellationLeaseGrace)
		if _, err := tx.ExecContext(ctx, `
			UPDATE rin_renderer.render_attempts
			SET lease_expires_at = LEAST(lease_expires_at, $2)
			WHERE job_id = $1::uuid AND state = 'running'`, job.ID, cancelLeaseExpiresAt); err != nil {
			return Cancellation{}, fmt.Errorf("bound renderer cancellation attempt lease: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rin_renderer.resource_allocations
			SET lease_expires_at = LEAST(lease_expires_at, $2)
			WHERE job_id = $1::uuid`, job.ID, cancelLeaseExpiresAt); err != nil {
			return Cancellation{}, fmt.Errorf("bound renderer cancellation resource lease: %w", err)
		}
	case "canceled":
		return Cancellation{Job: job}, nil
	default:
		return Cancellation{}, ErrCancellationNotAllowed
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
		VALUES ($1::uuid, $2, $3, '{}'::jsonb)`, job.ID, eventType, stage); err != nil {
		return Cancellation{}, fmt.Errorf("record renderer cancellation: %w", err)
	}
	job, err = jobByID(ctx, tx, job.ID, false)
	if err != nil {
		return Cancellation{}, err
	}
	if terminal {
		// A queued job never claimed an attempt, so its cancellation has no
		// attempt number to publish. Take the next ordinal after every terminal
		// outcome already recorded for the job instead.
		terminalVersion, err := nextCompletionTerminalVersionTx(ctx, tx, job.ID)
		if err != nil {
			return Cancellation{}, err
		}
		if err := insertTerminalCompletionTx(ctx, tx, job, "canceled", "", nil, "job_canceled", terminalVersion, now); err != nil {
			return Cancellation{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Cancellation{}, fmt.Errorf("commit renderer cancellation: %w", err)
	}
	return Cancellation{Job: job, WorkerMustStop: workerMustStop}, nil
}

func (repository *Repository) CancellationRequested(ctx context.Context, attemptID string, leaseTokenHash string, _ time.Time) (bool, error) {
	// A bounded cancellation lease may expire before a long-running child process
	// observes its next heartbeat. The still-running attempt and its secret token,
	// rather than wall-clock lease validity, fence this read from a replacement.
	var requested bool
	err := repository.db.QueryRowContext(ctx, `
		SELECT jobs.cancel_requested
		FROM rin_renderer.render_attempts AS attempts
		JOIN rin_renderer.render_jobs AS jobs ON jobs.id = attempts.job_id
		WHERE attempts.id = $1::uuid AND attempts.lease_token_hash = $2
			AND attempts.state = 'running' AND jobs.state = 'running'`, attemptID, leaseTokenHash).Scan(&requested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrLeaseLost
	}
	if err != nil {
		return false, fmt.Errorf("read renderer cancellation request: %w", err)
	}
	return requested, nil
}

func (repository *Repository) ConfirmCancellation(ctx context.Context, attemptID string, leaseTokenHash string, now time.Time) (Job, error) {
	if attemptID == "" || leaseTokenHash == "" || now.IsZero() {
		return Job{}, errors.New("renderer cancellation confirmation input is invalid")
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("begin renderer cancellation confirmation: %w", err)
	}
	defer tx.Rollback()
	var jobID string
	var attemptNo int16
	var startedAt time.Time
	// Permit the canceled worker to finish the handoff after its bounded lease has
	// expired. Recovery clears the token and changes the attempt state before a
	// replacement can run, so a stale worker still cannot confirm a newer lease.
	err = tx.QueryRowContext(ctx, `
		SELECT attempts.job_id::text, attempts.attempt_no, attempts.started_at
		FROM rin_renderer.render_attempts AS attempts
		JOIN rin_renderer.render_jobs AS jobs ON jobs.id = attempts.job_id
		WHERE attempts.id = $1::uuid AND attempts.lease_token_hash = $2
			AND attempts.state = 'running' AND jobs.state = 'running'
			AND jobs.cancel_requested = true
		FOR UPDATE OF attempts, jobs`, attemptID, leaseTokenHash).Scan(&jobID, &attemptNo, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrLeaseLost
	}
	if err != nil {
		return Job{}, fmt.Errorf("lock renderer cancellation confirmation: %w", err)
	}
	durationMS := now.Sub(startedAt).Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_attempts
		SET state = 'canceled', error_code = 'job_canceled', duration_ms = $2,
			finished_at = $3, lease_expires_at = NULL, lease_token_hash = NULL
		WHERE id = $1::uuid`, attemptID, durationMS, now); err != nil {
		return Job{}, fmt.Errorf("cancel renderer attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rin_renderer.resource_allocations WHERE job_id = $1::uuid`, jobID); err != nil {
		return Job{}, fmt.Errorf("release canceled renderer resources: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rin_renderer.render_jobs
		SET state = 'canceled', available_at = NULL, finished_at = $2, updated_at = clock_timestamp()
		WHERE id = $1::uuid`, jobID, now); err != nil {
		return Job{}, fmt.Errorf("finish renderer cancellation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
		VALUES ($1::uuid, 'canceled', 'attempt', jsonb_build_object('attemptNo', $2::smallint))`, jobID, attemptNo); err != nil {
		return Job{}, fmt.Errorf("record renderer cancellation confirmation: %w", err)
	}
	job, err := jobByID(ctx, tx, jobID, false)
	if err != nil {
		return Job{}, err
	}
	if err := insertTerminalCompletionTx(ctx, tx, job, "canceled", "", nil, "job_canceled", attemptNo, now); err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit renderer cancellation confirmation: %w", err)
	}
	return job, nil
}
