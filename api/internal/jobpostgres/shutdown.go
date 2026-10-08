package jobpostgres

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// InterruptActiveLeases durably fences a bounded set of workers during shutdown. Recovery then
// applies the ordinary retry budget, but records shutdown_interrupted instead of pretending the
// worker completed or failed deterministically.
func (repository *Repository) InterruptActiveLeases(ctx context.Context, leadership *Leadership, now time.Time, limit int) (int, error) {
	if limit <= 0 || now.IsZero() {
		return 0, errors.New("renderer interruption input is invalid")
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
		return 0, fmt.Errorf("begin renderer interruption: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT attempts.id::text, attempts.job_id::text, attempts.attempt_no
		FROM rin_renderer.render_attempts AS attempts
		JOIN rin_renderer.render_jobs AS jobs ON jobs.id = attempts.job_id
		WHERE attempts.state = 'running' AND jobs.state = 'running'
			AND (($2 = '' AND jobs.resource_class IN `+documentWorkerResourceClassListSQL+`) OR jobs.resource_class = $2)
		ORDER BY attempts.started_at, attempts.id
		FOR UPDATE OF attempts, jobs SKIP LOCKED
		LIMIT $1`, limit, leadership.resourceClass)
	if err != nil {
		return 0, fmt.Errorf("select renderer interruption leases: %w", err)
	}
	type interrupted struct {
		attemptID string
		jobID     string
		attemptNo int16
	}
	var items []interrupted
	for rows.Next() {
		var item interrupted
		if err := rows.Scan(&item.attemptID, &item.jobID, &item.attemptNo); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan renderer interruption lease: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close renderer interruption leases: %w", err)
	}
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `
			UPDATE rin_renderer.render_attempts
			SET error_code = 'shutdown_interrupted', lease_expires_at = $2
			WHERE id = $1::uuid`, item.attemptID, now); err != nil {
			return 0, fmt.Errorf("fence interrupted renderer attempt: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rin_renderer.resource_allocations
			SET lease_expires_at = $2
			WHERE attempt_id = $1::uuid`, item.attemptID, now); err != nil {
			return 0, fmt.Errorf("expire interrupted renderer allocation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
			VALUES ($1::uuid, 'interrupted', 'shutdown', jsonb_build_object(
				'attemptNo', $2::smallint, 'errorCode', 'shutdown_interrupted'))`, item.jobID, item.attemptNo); err != nil {
			return 0, fmt.Errorf("record renderer interruption: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit renderer interruption: %w", err)
	}
	return len(items), nil
}
