package jobpostgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrResourceCapacity = errors.New("renderer resource capacity reached")

const resourceCapacityLock = "rin_renderer_resource_capacity"

type SubworkAllocationInput struct {
	AllocationID   string
	ResourceClass  string
	JobID          string
	WorkKey        string
	OwnerID        string
	LeaseTokenHash string
	Now            time.Time
	LeaseExpiresAt time.Time
	Capacities     ResourceCapacities
}

type SubworkAllocation struct {
	ID             string
	ResourceClass  string
	JobID          string
	WorkKey        string
	OwnerID        string
	LeaseExpiresAt time.Time
}

func (repository *Repository) AcquireSubwork(ctx context.Context, input SubworkAllocationInput) (SubworkAllocation, error) {
	capacity := resourceCapacity(input.Capacities, input.ResourceClass)
	if capacity <= 0 || input.AllocationID == "" || input.JobID == "" || input.WorkKey == "" ||
		input.OwnerID == "" || input.LeaseTokenHash == "" || input.Now.IsZero() ||
		!input.LeaseExpiresAt.After(input.Now) {
		return SubworkAllocation{}, errors.New("renderer subwork allocation input is invalid")
	}
	tx, err := repository.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SubworkAllocation{}, fmt.Errorf("begin renderer subwork allocation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, resourceCapacityLock); err != nil {
		return SubworkAllocation{}, fmt.Errorf("lock renderer resource capacity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM rin_renderer.resource_allocations
		WHERE resource_class = $1 AND lease_expires_at <= $2`, input.ResourceClass, input.Now); err != nil {
		return SubworkAllocation{}, fmt.Errorf("cleanup expired renderer resources: %w", err)
	}
	var jobState string
	if err := tx.QueryRowContext(ctx, `
		SELECT state FROM rin_renderer.render_jobs WHERE id = $1::uuid FOR SHARE`, input.JobID).Scan(&jobState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SubworkAllocation{}, ErrNotFound
		}
		return SubworkAllocation{}, fmt.Errorf("read renderer subwork parent: %w", err)
	}
	if jobState != "running" {
		return SubworkAllocation{}, ErrLeaseLost
	}
	var active int64
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM rin_renderer.resource_allocations
		WHERE resource_class = $1 AND lease_expires_at > $2`, input.ResourceClass, input.Now).Scan(&active); err != nil {
		return SubworkAllocation{}, fmt.Errorf("count renderer resource allocations: %w", err)
	}
	if active >= capacity {
		return SubworkAllocation{}, ErrResourceCapacity
	}
	if heavyResourceClass(input.ResourceClass) {
		var heavyActive int64
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM rin_renderer.resource_allocations
			WHERE resource_class IN `+heavyResourceClassListSQL+`
				AND lease_expires_at > $1`, input.Now).Scan(&heavyActive); err != nil {
			return SubworkAllocation{}, fmt.Errorf("count renderer heavy allocations: %w", err)
		}
		if heavyActive >= input.Capacities.Heavy {
			return SubworkAllocation{}, ErrResourceCapacity
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.resource_allocations (
			id, resource_class, job_id, allocation_kind, work_key, owner_id,
			lease_token_hash, lease_expires_at, heartbeat_at
		) VALUES ($1::uuid, $2, $3::uuid, 'subwork', $4, $5, $6, $7, $8)`,
		input.AllocationID, input.ResourceClass, input.JobID, input.WorkKey, input.OwnerID,
		input.LeaseTokenHash, input.LeaseExpiresAt, input.Now); err != nil {
		return SubworkAllocation{}, fmt.Errorf("create renderer subwork allocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SubworkAllocation{}, fmt.Errorf("commit renderer subwork allocation: %w", err)
	}
	return SubworkAllocation{
		ID: input.AllocationID, ResourceClass: input.ResourceClass, JobID: input.JobID,
		WorkKey: input.WorkKey, OwnerID: input.OwnerID, LeaseExpiresAt: input.LeaseExpiresAt,
	}, nil
}

func (repository *Repository) HeartbeatSubwork(ctx context.Context, allocationID string, leaseTokenHash string, now time.Time, leaseExpiresAt time.Time) error {
	if !leaseExpiresAt.After(now) {
		return errors.New("renderer subwork heartbeat expiry must be in the future")
	}
	result, err := repository.db.ExecContext(ctx, `
		UPDATE rin_renderer.resource_allocations
		SET heartbeat_at = $3, lease_expires_at = $4
		WHERE id = $1::uuid AND allocation_kind = 'subwork' AND lease_token_hash = $2
			AND lease_expires_at > $3`, allocationID, leaseTokenHash, now, leaseExpiresAt)
	if err != nil {
		return fmt.Errorf("heartbeat renderer subwork: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (repository *Repository) ReleaseSubwork(ctx context.Context, allocationID string, leaseTokenHash string) error {
	result, err := repository.db.ExecContext(ctx, `
		DELETE FROM rin_renderer.resource_allocations
		WHERE id = $1::uuid AND allocation_kind = 'subwork' AND lease_token_hash = $2`,
		allocationID, leaseTokenHash)
	if err != nil {
		return fmt.Errorf("release renderer subwork: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return ErrLeaseLost
	}
	return nil
}

func resourceCapacity(capacities ResourceCapacities, resourceClass string) int64 {
	switch resourceClass {
	case "document-light":
		return capacities.DocumentLight
	case "document-latexml":
		return capacities.DocumentLaTeXML
	case "math-node":
		return capacities.MathNode
	case "texsvg":
		return capacities.TeXSVG
	case "batch-migration":
		return capacities.BatchMigration
	case "latex-pdf":
		return capacities.LatexPDF
	case "document-typst":
		return capacities.DocumentTypst
	case "typst-pdf":
		return capacities.TypstPDF
	default:
		return 0
	}
}

func heavyResourceClass(resourceClass string) bool {
	return IsHeavyResourceClass(resourceClass)
}
