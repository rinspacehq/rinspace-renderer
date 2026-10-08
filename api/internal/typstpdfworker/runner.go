// Package typstpdfworker runs the serial Typst PDF preview worker loop: it
// claims typst-pdf jobs, executes them through the pinned Typst executor and
// records the terminal state. It mirrors the LaTeX PDF worker but never shares
// its resource class, result contract or failure vocabulary.
package typstpdfworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/scheduler"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstpdfexecutor"
)

const (
	leaseMaintenanceDrainLimit = 2 * time.Second
	resourceClass              = "typst-pdf"
	contentKind                = "typst"
)

type Scheduler interface {
	AcquireLeadership(context.Context) (*jobpostgres.Leadership, error)
	Claim(context.Context, *jobpostgres.Leadership, string) (scheduler.Lease, error)
	Heartbeat(context.Context, scheduler.Lease) (scheduler.Lease, error)
	Recover(context.Context, *jobpostgres.Leadership) (int, error)
	Fail(context.Context, scheduler.Lease, bool, string) (jobpostgres.Job, error)
	FailWithResult(context.Context, scheduler.Lease, bool, string, jobpostgres.Artifact) (jobpostgres.Job, error)
	SucceedWithResult(context.Context, scheduler.Lease, jobpostgres.Artifact, jobpostgres.CompletionResultEvidence) (jobpostgres.Job, error)
	CancellationRequested(context.Context, scheduler.Lease) (bool, error)
	ConfirmCancellation(context.Context, scheduler.Lease) (jobpostgres.Job, error)
}

type Store interface {
	PutPrivate(context.Context, orchestration.ArtifactWrite) (contracts.ArtifactReference, error)
}

type Executor interface {
	Execute(context.Context, jobpostgres.Job) typstpdfexecutor.Outcome
}

type Lifecycle interface {
	BeginWorker(context.Context) (context.Context, func(), error)
}

type Config struct {
	WorkerID          string
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	RecoveryTimeout   time.Duration
	MaxConcurrent     int
	Now               func() time.Time
}

type Runner struct {
	scheduler Scheduler
	store     Store
	executor  Executor
	lifecycle Lifecycle
	config    Config
	ready     atomic.Bool
	runCount  atomic.Uint64
}

func New(schedulerService Scheduler, store Store, executor Executor, lifecycle Lifecycle, config Config) (*Runner, error) {
	if schedulerService == nil || store == nil || executor == nil || lifecycle == nil ||
		strings.TrimSpace(config.WorkerID) == "" || config.PollInterval <= 0 || config.HeartbeatInterval <= 0 ||
		config.RecoveryTimeout <= 0 || config.MaxConcurrent != 1 {
		return nil, errors.New("Typst PDF worker configuration is invalid; concurrency must equal one")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Runner{scheduler: schedulerService, store: store, executor: executor, lifecycle: lifecycle, config: config}, nil
}

func (runner *Runner) Run(ctx context.Context) error {
	if runner.runCount.Add(1) > 1 {
		operational.Default().Count("worker_restarts_total", resourceClass)
	}
	leadership, err := runner.acquireLeadership(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer leadership.Close()
	defer runner.ready.Store(false)
	if _, err := runner.recover(ctx, leadership); err != nil {
		return fmt.Errorf("recover Typst PDF worker leases: %w", err)
	}
	runner.ready.Store(true)
	for {
		if ctx.Err() != nil {
			return nil
		}
		lease, err := runner.scheduler.Claim(ctx, leadership, runner.config.WorkerID)
		if errors.Is(err, jobpostgres.ErrNoEligibleJob) {
			if _, recoverErr := runner.recover(ctx, leadership); recoverErr != nil {
				slog.Error("Typst PDF lease recovery deferred", "error", recoverErr)
			}
			if !wait(ctx, runner.config.PollInterval) {
				return nil
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("claim Typst PDF job: %w", err)
		}
		runner.execute(ctx, lease)
	}
}

func (runner *Runner) Ready(context.Context) error {
	if !runner.ready.Load() {
		return errors.New("typst_pdf_worker_unavailable")
	}
	return nil
}

func (runner *Runner) acquireLeadership(ctx context.Context) (*jobpostgres.Leadership, error) {
	for {
		leadership, err := runner.scheduler.AcquireLeadership(ctx)
		if err == nil {
			return leadership, nil
		}
		if !errors.Is(err, jobpostgres.ErrLeadershipUnavailable) {
			return nil, fmt.Errorf("acquire Typst PDF worker leadership: %w", err)
		}
		if !wait(ctx, runner.config.PollInterval) {
			return nil, ctx.Err()
		}
	}
}

func (runner *Runner) recover(ctx context.Context, leadership *jobpostgres.Leadership) (int, error) {
	recoveryCtx, cancel := context.WithTimeout(ctx, runner.config.RecoveryTimeout)
	defer cancel()
	return runner.scheduler.Recover(recoveryCtx, leadership)
}

func (runner *Runner) execute(parent context.Context, lease scheduler.Lease) {
	if lease.Job.ResourceClass != resourceClass || lease.Job.ContentKind != contentKind || lease.Job.DocumentEngine != contentKind {
		_, _ = runner.scheduler.Fail(context.WithoutCancel(parent), lease, false, "invalid_typst_pdf_job")
		return
	}
	started := time.Now()
	operational.Default().AddGauge("resource_in_use", 1, "typst-pdf-worker")
	defer operational.Default().AddGauge("resource_in_use", -1, "typst-pdf-worker")
	ctx, done, err := runner.lifecycle.BeginWorker(parent)
	if err != nil {
		_, _ = runner.scheduler.Fail(context.WithoutCancel(parent), lease, true, "typst_pdf_worker_shutting_down")
		return
	}
	defer done()
	workCtx, cancel := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	go runner.maintainLease(workCtx, cancel, lease, maintenanceDone)
	outcome := runner.executor.Execute(workCtx, lease.Job)
	cancel()
	if !waitForMaintenance(maintenanceDone) {
		operational.Default().Count("worker_lease_maintenance_drain_timeouts_total", resourceClass)
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer finishCancel()
	requested, checkErr := runner.scheduler.CancellationRequested(finishCtx, lease)
	if checkErr == nil && requested {
		_, _ = runner.scheduler.ConfirmCancellation(finishCtx, lease)
		operational.Default().Count("jobs_finished_total", contentKind, "canceled", "canceled")
		return
	}
	if outcome.Failure != nil {
		job, err := runner.finishFailure(finishCtx, lease, *outcome.Failure)
		state := firstNonEmpty(job.State, "unknown")
		operational.Default().Count("jobs_finished_total", contentKind, state, failureClass(outcome.Failure.Code))
		if err != nil {
			slog.Error("finish Typst PDF job failure deferred", "job_id", lease.Job.ID, "error", err)
		}
		return
	}
	if outcome.ResultArtifact.ID == "" || outcome.ResultReference.ArtifactID == "" {
		_, _ = runner.scheduler.Fail(finishCtx, lease, true, "invalid_typst_pdf_result")
		return
	}
	job, err := runner.scheduler.SucceedWithResult(finishCtx, lease, outcome.ResultArtifact, outcome.Evidence)
	state := firstNonEmpty(job.State, "unknown")
	operational.Default().Count("jobs_finished_total", contentKind, state, "none")
	operational.Default().Observe("render_duration_seconds", time.Since(started), contentKind, resourceClass)
	if err != nil {
		slog.Error("finish Typst PDF job success deferred", "job_id", lease.Job.ID, "error", err)
	}
}

func (runner *Runner) finishFailure(ctx context.Context, lease scheduler.Lease, failure typstpdfexecutor.Failure) (jobpostgres.Job, error) {
	if failure.Status < 400 || failure.Status > 599 || failure.Code == "" || failure.Message == "" {
		return runner.scheduler.Fail(ctx, lease, true, "invalid_typst_pdf_failure")
	}
	if failure.Diagnostics == nil {
		failure.Diagnostics = []renderapi.Diagnostic{}
	}
	stored := jobresult.StoredFailure{
		SchemaVersion: jobresult.StoredFailureSchemaVersion, Status: failure.Status,
		Code: failure.Code, Error: failure.Message, Retryable: failure.Retryable, ExitCode: failure.ExitCode,
		Diagnostics: failure.Diagnostics, Artifacts: failure.Artifacts,
	}
	if err := stored.Validate(); err != nil {
		return runner.scheduler.Fail(ctx, lease, true, "invalid_typst_pdf_failure")
	}
	body, err := json.Marshal(stored)
	if err != nil {
		return runner.scheduler.Fail(ctx, lease, failure.Retryable, failure.Code)
	}
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	expiresAt := runner.config.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	reference, err := runner.store.PutPrivate(ctx, orchestration.ArtifactWrite{
		ArtifactID: "render-jobs/v1/result/" + lease.Job.ID + "/" + hash + ".json", Body: body,
		MediaType: "application/json", SchemaVersion: jobresult.StoredFailureSchemaVersion, ExpiresAt: &expiresAt,
	})
	if err != nil {
		return runner.scheduler.Fail(ctx, lease, failure.Retryable, failure.Code)
	}
	artifact := jobpostgres.Artifact{
		ID: reference.ArtifactID, SHA256: reference.SHA256, Kind: "result", Visibility: reference.Visibility,
		StorageKey: reference.ArtifactID, ByteSize: reference.Bytes, MediaType: reference.MediaType,
		SchemaVersion: jobresult.StoredFailureSchemaVersion, ExpiresAt: &expiresAt,
	}
	return runner.scheduler.FailWithResult(ctx, lease, failure.Retryable, failure.Code, artifact)
}

func (runner *Runner) maintainLease(ctx context.Context, cancel context.CancelFunc, lease scheduler.Lease, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(runner.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			requested, err := runner.scheduler.CancellationRequested(ctx, lease)
			if err != nil || requested {
				cancel()
				return
			}
			if _, err := runner.scheduler.Heartbeat(ctx, lease); err != nil {
				cancel()
				return
			}
		}
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func waitForMaintenance(done <-chan struct{}) bool {
	timer := time.NewTimer(leaseMaintenanceDrainLimit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func failureClass(code string) string {
	switch {
	case strings.Contains(code, "timeout"):
		return "timeout"
	case strings.Contains(code, "oom"):
		return "oom"
	case strings.Contains(code, "invalid") || strings.Contains(code, "unsafe"):
		return "invalid"
	case strings.Contains(code, "compile"):
		return "compile"
	default:
		return "unavailable"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
