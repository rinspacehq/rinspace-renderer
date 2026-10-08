package jobworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/scheduler"
)

type Scheduler interface {
	AcquireLeadership(context.Context) (*jobpostgres.Leadership, error)
	Claim(context.Context, *jobpostgres.Leadership, string) (scheduler.Lease, error)
	Heartbeat(context.Context, scheduler.Lease) (scheduler.Lease, error)
	RefineWorkload(context.Context, scheduler.Lease, jobpostgres.WorkloadRefinement) (jobpostgres.WorkloadRecord, error)
	Recover(context.Context, *jobpostgres.Leadership) (int, error)
	Fail(context.Context, scheduler.Lease, bool, string) (jobpostgres.Job, error)
	FailWithResult(context.Context, scheduler.Lease, bool, string, jobpostgres.Artifact) (jobpostgres.Job, error)
	SucceedWithResult(context.Context, scheduler.Lease, jobpostgres.Artifact, jobpostgres.CompletionResultEvidence) (jobpostgres.Job, error)
	CancellationRequested(context.Context, scheduler.Lease) (bool, error)
	ConfirmCancellation(context.Context, scheduler.Lease) (jobpostgres.Job, error)
}

type Repository interface {
	Artifact(context.Context, string) (jobpostgres.Artifact, error)
}

type Store interface {
	GetPrivate(context.Context, contracts.ArtifactReference) ([]byte, error)
	PutPrivate(context.Context, orchestration.ArtifactWrite) (contracts.ArtifactReference, error)
	DeletePrivate(context.Context, contracts.ArtifactReference) error
}

type Executor interface {
	Execute(context.Context, renderapi.ProjectExecutionRequest) renderapi.ProjectExecutionResult
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
}

type Runner struct {
	scheduler  Scheduler
	repository Repository
	store      Store
	executor   Executor
	lifecycle  Lifecycle
	config     Config
	ready      atomic.Bool
	runCount   atomic.Uint64
}

type requestMetadata struct {
	RequestID          string                             `json:"requestId"`
	SourceName         string                             `json:"sourceName"`
	Entrypoint         string                             `json:"entrypoint"`
	Title              string                             `json:"title"`
	MetadataMainFile   string                             `json:"metadataMainFile"`
	ActiveFile         string                             `json:"activeFile"`
	ProjectStatus      string                             `json:"projectStatus"`
	MathPolicy         string                             `json:"mathPolicy"`
	Renderer           string                             `json:"renderer"`
	Options            string                             `json:"options"`
	DocumentMode       string                             `json:"documentMode"`
	MarkdownBookPages  []projectidentity.MarkdownBookPage `json:"bookPages,omitempty"`
	SourceCommit       string                             `json:"sourceCommit,omitempty"`
	ControlProjectHash string                             `json:"controlProjectHash,omitempty"`
	ControlProjectID   string                             `json:"controlProjectId,omitempty"`
}

type renderFailure struct {
	Code        string
	Status      int
	Message     string
	Diagnostics []renderapi.Diagnostic
	Retryable   bool
}

var latexBookClassPattern = regexp.MustCompile(`(?i)\\documentclass\s*(?:\[[^\]]*\]\s*)?\{(?:book|report|memoir)\}`)

const leaseMaintenanceDrainLimit = 2 * time.Second

func New(schedulerService Scheduler, repository Repository, store Store, executor Executor, lifecycle Lifecycle, config Config) (*Runner, error) {
	if schedulerService == nil || repository == nil || store == nil || executor == nil || lifecycle == nil ||
		strings.TrimSpace(config.WorkerID) == "" || config.PollInterval <= 0 || config.HeartbeatInterval <= 0 ||
		config.RecoveryTimeout <= 0 || config.MaxConcurrent <= 0 {
		return nil, errors.New("renderer job worker configuration is invalid")
	}
	return &Runner{scheduler: schedulerService, repository: repository, store: store, executor: executor, lifecycle: lifecycle, config: config}, nil
}

func (runner *Runner) Run(ctx context.Context) error {
	if runner.runCount.Add(1) > 1 {
		operational.Default().Count("worker_restarts_total", "job")
		operational.Default().Event("worker_restarted", slog.String("resource_class", "document"))
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
		return fmt.Errorf("recover renderer worker leases: %w", err)
	}
	runner.ready.Store(true)
	semaphore := make(chan struct{}, runner.config.MaxConcurrent)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case semaphore <- struct{}{}:
		}
		lease, err := runner.scheduler.Claim(ctx, leadership, runner.config.WorkerID)
		if errors.Is(err, jobpostgres.ErrNoEligibleJob) {
			<-semaphore
			if _, recoverErr := runner.recover(ctx, leadership); recoverErr != nil {
				operational.Default().Count("worker_recovery_failures_total", "job")
				slog.Error("renderer lease recovery deferred", "error", recoverErr)
				if !wait(ctx, runner.config.PollInterval) {
					return nil
				}
				continue
			}
			if !wait(ctx, runner.config.PollInterval) {
				return nil
			}
			continue
		}
		if err != nil {
			<-semaphore
			return fmt.Errorf("claim renderer job: %w", err)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-semaphore }()
			runner.execute(ctx, lease)
		}()
	}
}

func (runner *Runner) recover(ctx context.Context, leadership *jobpostgres.Leadership) (int, error) {
	recoveryCtx, cancel := context.WithTimeout(ctx, runner.config.RecoveryTimeout)
	defer cancel()
	return runner.scheduler.Recover(recoveryCtx, leadership)
}

func (runner *Runner) Ready(context.Context) error {
	if !runner.ready.Load() {
		return errors.New("worker_unavailable")
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
			return nil, fmt.Errorf("acquire renderer worker leadership: %w", err)
		}
		if !wait(ctx, runner.config.PollInterval) {
			return nil, ctx.Err()
		}
	}
}

func (runner *Runner) execute(parent context.Context, lease scheduler.Lease) {
	started := time.Now()
	queueWait := time.Duration(0)
	if lease.Job.QueuedAt != nil && started.After(*lease.Job.QueuedAt) {
		queueWait = started.Sub(*lease.Job.QueuedAt)
	}
	metrics := operational.Default()
	metrics.AddGauge("resource_in_use", 1, lease.Job.ResourceClass)
	defer metrics.AddGauge("resource_in_use", -1, lease.Job.ResourceClass)
	metrics.Count("jobs_started_total", lease.Job.ContentKind, lease.Job.ResourceClass)
	metrics.Observe("queue_wait_seconds", queueWait, lease.Job.ContentKind, lease.Job.ResourceClass)
	metrics.Event("job_started",
		slog.String("request_id", lease.Job.ID), slog.String("job_id", lease.Job.ID), slog.String("attempt_id", lease.AttemptID),
		slog.String("content_kind", lease.Job.ContentKind), slog.String("engine", lease.Job.DocumentEngine),
		slog.String("resource_class", lease.Job.ResourceClass), slog.String("renderer_version", lease.Job.RendererVersion),
		slog.Int64("queue_wait_ms", queueWait.Milliseconds()))
	ctx, done, err := runner.lifecycle.BeginWorker(parent)
	if err != nil {
		_, _ = runner.scheduler.Fail(context.WithoutCancel(parent), lease, true, "worker_shutting_down")
		return
	}
	defer done()
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatsDone := make(chan struct{})
	go runner.maintainLease(workCtx, cancel, lease, heartbeatsDone)

	renderStarted := time.Now()
	execution, failure := runner.render(workCtx, lease.Job)
	renderDuration := time.Since(renderStarted)
	metrics.Observe("stage_duration_seconds", renderDuration, "document_compile", lease.Job.ContentKind)
	metrics.Event("stage_finished", slog.String("request_id", lease.Job.ID), slog.String("job_id", lease.Job.ID),
		slog.String("attempt_id", lease.AttemptID), slog.String("content_kind", lease.Job.ContentKind),
		slog.String("stage", "document_compile"), slog.Int64("duration_ms", renderDuration.Milliseconds()),
		slog.String("renderer_version", lease.Job.RendererVersion))
	metrics.Observe("render_duration_seconds", time.Since(started), lease.Job.ContentKind, lease.Job.ResourceClass)
	cancel()
	if !waitForLeaseMaintenance(heartbeatsDone, leaseMaintenanceDrainLimit) {
		metrics.Count("worker_lease_maintenance_drain_timeouts_total", "job")
		slog.Error("renderer lease maintenance drain deferred", slog.String("resource_class", lease.Job.ResourceClass))
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer finishCancel()
	requested, checkErr := runner.scheduler.CancellationRequested(finishCtx, lease)
	if checkErr != nil {
		metrics.Count("worker_cancellation_check_failures_total", "job")
		slog.Error("renderer cancellation check deferred", "error", checkErr)
	}
	if checkErr == nil && requested {
		metrics.Event("cancellation_confirmation_started",
			slog.String("request_id", lease.Job.ID), slog.String("job_id", lease.Job.ID),
			slog.String("attempt_id", lease.AttemptID), slog.String("resource_class", lease.Job.ResourceClass))
		confirmedJob, confirmErr := runner.scheduler.ConfirmCancellation(finishCtx, lease)
		if confirmErr != nil {
			operational.Default().Count("worker_cancellation_confirmation_failures_total", "job")
			slog.Error("renderer cancellation confirmation deferred", "error", confirmErr)
		} else {
			metrics.Event("cancellation_confirmation_finished",
				slog.String("request_id", lease.Job.ID), slog.String("job_id", lease.Job.ID),
				slog.String("attempt_id", lease.AttemptID), slog.String("resource_class", lease.Job.ResourceClass),
				slog.String("state", confirmedJob.State))
		}
		return
	}
	if failure != nil {
		job, finishErr := runner.storeFailure(finishCtx, lease, *failure)
		state := job.State
		if state == "" {
			state = "unknown"
		}
		if state == "queued" {
			metrics.Count("retries_total", lease.Job.ResourceClass, metricFailureClass(failure.Code))
		}
		metrics.Count("jobs_finished_total", lease.Job.ContentKind, state, metricFailureClass(failure.Code))
		metrics.Warn("job_finished", slog.String("request_id", lease.Job.ID), slog.String("job_id", lease.Job.ID), slog.String("attempt_id", lease.AttemptID),
			slog.String("content_kind", lease.Job.ContentKind), slog.String("engine", lease.Job.DocumentEngine),
			slog.String("state", state), slog.String("failure_code", failure.Code),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()), slog.Bool("retryable", failure.Retryable))
		_ = finishErr
		return
	}
	analyzeStarted := time.Now()
	_, _ = runner.scheduler.RefineWorkload(finishCtx, lease, workloadRefinement(lease.Job, execution.Response))
	metrics.Observe("stage_duration_seconds", time.Since(analyzeStarted), "analyze", lease.Job.ContentKind)
	storeStarted := time.Now()
	job, failureCode, _, _ := runner.storeResult(finishCtx, lease, execution)
	storeDuration := time.Since(storeStarted)
	metrics.Observe("stage_duration_seconds", storeDuration, "store", lease.Job.ContentKind)
	metrics.Event("stage_finished", slog.String("request_id", lease.Job.ID), slog.String("job_id", lease.Job.ID),
		slog.String("attempt_id", lease.AttemptID), slog.String("content_kind", lease.Job.ContentKind),
		slog.String("stage", "store"), slog.Int64("duration_ms", storeDuration.Milliseconds()),
		slog.String("renderer_version", lease.Job.RendererVersion))
	state := job.State
	if state == "" {
		state = "unknown"
	}
	degradedCodes := degradedDiagnosticCodes(execution)
	if len(degradedCodes) > 0 {
		metrics.Count("jobs_degraded_total", lease.Job.ContentKind, lease.Job.ResourceClass)
		metrics.Warn("job_degraded", slog.String("request_id", lease.Job.ID), slog.String("job_id", lease.Job.ID), slog.String("attempt_id", lease.AttemptID),
			slog.String("content_kind", lease.Job.ContentKind), slog.String("engine", execution.Response.Engine),
			slog.String("state", state), slog.String("failure_code", failureCode),
			slog.Int("diagnostic_count", len(degradedCodes)), slog.String("codes", degradedCodeList(degradedCodes)))
	}
	metrics.Count("jobs_finished_total", lease.Job.ContentKind, state, metricFailureClass(failureCode))
	metrics.Event("job_finished", slog.String("request_id", lease.Job.ID), slog.String("job_id", lease.Job.ID), slog.String("attempt_id", lease.AttemptID),
		slog.String("content_kind", lease.Job.ContentKind), slog.String("engine", execution.Response.Engine),
		slog.String("state", state), slog.String("failure_code", failureCode),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()), slog.Int64("output_bytes", int64(len(execution.Response.HTML))),
		slog.String("renderer_version", lease.Job.RendererVersion))
}

func (runner *Runner) storeFailure(ctx context.Context, lease scheduler.Lease, failure renderFailure) (jobpostgres.Job, error) {
	storedFailure := jobresult.StoredFailure{
		SchemaVersion: jobresult.StoredFailureSchemaVersion, Status: failure.Status,
		Code: failure.Code, Error: failure.Message, Diagnostics: failure.Diagnostics,
	}
	body, err := json.Marshal(storedFailure)
	if err != nil {
		return runner.scheduler.Fail(ctx, lease, failure.Retryable, failure.Code)
	}
	reference, artifact, err := runner.putOutput(ctx, lease.Job, body, jobresult.StoredFailureSchemaVersion)
	if err != nil {
		return runner.scheduler.Fail(ctx, lease, failure.Retryable, failure.Code)
	}
	job, err := runner.scheduler.FailWithResult(ctx, lease, failure.Retryable, failure.Code, artifact)
	if err != nil || job.State != "failed" {
		_ = runner.store.DeletePrivate(context.WithoutCancel(ctx), reference)
	}
	return job, err
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

func (runner *Runner) render(ctx context.Context, job jobpostgres.Job) (renderapi.ProjectExecutionResult, *renderFailure) {
	artifact, err := runner.repository.Artifact(ctx, job.SourceArtifactID)
	if err != nil || artifact.Visibility != "private" || artifact.ExpiresAt == nil {
		return renderapi.ProjectExecutionResult{}, &renderFailure{Code: "source_artifact_unavailable", Status: 502, Message: "source artifact is unavailable", Retryable: true}
	}
	reference := contracts.ArtifactReference{
		ArtifactID: artifact.StorageKey, SHA256: artifact.SHA256, Bytes: artifact.ByteSize,
		MediaType: artifact.MediaType, Visibility: artifact.Visibility, ExpiresAt: artifact.ExpiresAt.UTC().Format(time.RFC3339),
	}
	body, err := runner.store.GetPrivate(ctx, reference)
	if err != nil {
		return renderapi.ProjectExecutionResult{}, &renderFailure{Code: "source_artifact_unavailable", Status: 502, Message: "source artifact is unavailable", Retryable: true}
	}
	var metadata requestMetadata
	if len(job.RequestMetadata) > 0 && json.Unmarshal(job.RequestMetadata, &metadata) != nil {
		return renderapi.ProjectExecutionResult{}, &renderFailure{Code: "invalid_request_metadata", Status: 400, Message: "job request metadata is invalid"}
	}
	disableMarkdownBookReuse := false
	if strings.TrimSpace(metadata.DocumentMode) == "book" && strings.TrimSpace(metadata.Options) != "" {
		var options struct {
			IncrementalReuse *bool `json:"incrementalReuse"`
		}
		if json.Unmarshal([]byte(metadata.Options), &options) != nil {
			return renderapi.ProjectExecutionResult{}, &renderFailure{Code: "invalid_request_metadata", Status: 400, Message: "job render options are invalid"}
		}
		disableMarkdownBookReuse = options.IncrementalReuse != nil && !*options.IncrementalReuse
	}
	result := runner.executor.Execute(ctx, renderapi.ProjectExecutionRequest{
		ContentKind: job.ContentKind,
		JobID:       job.ID, RequestID: firstNonEmpty(metadata.RequestID, job.ID),
		ArchiveName: firstNonEmpty(metadata.SourceName, "project.zip"),
		Archive:     body, Engine: job.DocumentEngine, Title: metadata.Title, ExplicitMainFile: metadata.Entrypoint,
		MetadataMainFile: metadata.MetadataMainFile, ActiveFile: metadata.ActiveFile,
		ProjectStatus: metadata.ProjectStatus, MathPolicy: metadata.MathPolicy, Renderer: metadata.Renderer,
		DocumentMode: metadata.DocumentMode, DisableMarkdownBookReuse: disableMarkdownBookReuse,
		MarkdownBookPages: metadata.MarkdownBookPages,
		ProjectID:         metadata.ControlProjectID, SourceCommit: metadata.SourceCommit, ControlProjectHash: metadata.ControlProjectHash,
	})
	if result.Err != nil {
		status := result.Status
		if status < 400 || status > 599 {
			status = 502
		}
		message := result.Message
		if message == "" {
			message = result.Err.Error()
		}
		code := classifyExecutionFailure(status, result.Diagnostics)
		// A render timeout is a property of the document, not of the attempt:
		// retrying it only re-occupies a serial worker for another full render
		// timeout and starves every job queued behind it. Fail it terminally so
		// the Control Plane can report the document instead of looping on it.
		return renderapi.ProjectExecutionResult{}, &renderFailure{
			Code: code, Status: status, Message: message,
			Diagnostics: result.Diagnostics, Retryable: status >= 500 && code != "document_render_timeout" && code != "document_render_incomplete",
		}
	}
	return result, nil
}

func (runner *Runner) storeResult(ctx context.Context, lease scheduler.Lease, execution renderapi.ProjectExecutionResult) (jobpostgres.Job, string, *contracts.RenderResult, error) {
	canonical := contracts.RenderResult{}
	expectedRequestID := resultRequestID(lease.Job)
	var err error
	if execution.Canonical != nil {
		canonical = *execution.Canonical
	} else {
		canonical, err = renderapi.CanonicalResult(lease.Job.ID, canonicalProjectHash(lease.Job), lease.Job.ContentKind, execution.Response)
	}
	if err == nil {
		if canonical.JobID != lease.Job.ID || canonical.RequestID != expectedRequestID ||
			canonical.ContentKind != contracts.ContentKind(lease.Job.ContentKind) {
			err = errors.New("canonical render result identity does not match the leased job")
		} else {
			err = canonical.Validate()
		}
	}
	if err == nil && canonical.Inline != nil {
		for _, page := range canonical.Inline.Pages {
			if err = finaloutput.ValidateRenderingErrors(page.Fragment); err != nil {
				break
			}
		}
	}
	if err != nil {
		if finaloutput.Code(err) == "final_output.render_error" {
			job, finishErr := runner.storeFailure(ctx, lease, renderFailure{
				Code: "document_render_incomplete", Status: 422, Message: err.Error(), Retryable: false,
				Diagnostics: []renderapi.Diagnostic{{Severity: "error", Code: "final_output.render_error", Message: err.Error(), Engine: canonical.Engine}},
			})
			return job, "document_render_incomplete", nil, finishErr
		}
		slog.Error("canonical render result rejected", "job_id", lease.Job.ID, "error", err,
			"job_id_match", canonical.JobID == lease.Job.ID, "request_id_match", canonical.RequestID == expectedRequestID,
			"content_kind_match", canonical.ContentKind == contracts.ContentKind(lease.Job.ContentKind))
		job, finishErr := runner.scheduler.Fail(ctx, lease, false, "invalid_render_result")
		return job, "invalid_render_result", nil, finishErr
	}
	compatibility, err := json.Marshal(execution.Response)
	if err != nil {
		job, finishErr := runner.scheduler.Fail(ctx, lease, false, "invalid_compatibility_result")
		return job, "invalid_compatibility_result", nil, finishErr
	}
	output := jobresult.StoredOutput{SchemaVersion: jobresult.StoredOutputSchemaVersion, Result: canonical, Compatibility: compatibility}
	body, err := json.Marshal(output)
	if err != nil {
		job, finishErr := runner.scheduler.Fail(ctx, lease, false, "invalid_render_result")
		return job, "invalid_render_result", nil, finishErr
	}
	reference, artifact, err := runner.putOutput(ctx, lease.Job, body, jobresult.StoredOutputSchemaVersion)
	if err != nil {
		job, finishErr := runner.scheduler.Fail(ctx, lease, true, "result_storage_failed")
		return job, "result_storage_failed", nil, finishErr
	}
	job, err := runner.scheduler.SucceedWithResult(ctx, lease, artifact, jobpostgres.CompletionResultEvidence{
		RendererProjectHash: canonical.ProjectHash,
		ResultHash:          canonical.ResultHash,
	})
	if err != nil || job.State != "succeeded" {
		_ = runner.store.DeletePrivate(context.WithoutCancel(ctx), reference)
	}
	return job, "", &canonical, err
}

func resultRequestID(job jobpostgres.Job) string {
	var metadata requestMetadata
	if len(job.RequestMetadata) > 0 && json.Unmarshal(job.RequestMetadata, &metadata) == nil {
		return firstNonEmpty(metadata.RequestID, job.ID)
	}
	return job.ID
}

func canonicalProjectHash(job jobpostgres.Job) string {
	var metadata requestMetadata
	if len(job.RequestMetadata) > 0 && json.Unmarshal(job.RequestMetadata, &metadata) == nil {
		if projectHash := strings.TrimSpace(metadata.ControlProjectHash); projectHash != "" {
			return projectHash
		}
	}
	return job.ProjectHash
}

func metricFailureClass(code string) string {
	switch {
	case code == "":
		return "none"
	case strings.Contains(code, "timeout"):
		return "timeout"
	case strings.Contains(code, "invalid"):
		return "invalid"
	case strings.Contains(code, "unavailable") || strings.Contains(code, "storage") || strings.Contains(code, "worker"):
		return "unavailable"
	default:
		return "internal"
	}
}

func (runner *Runner) putOutput(ctx context.Context, job jobpostgres.Job, body []byte, schemaVersion string) (contracts.ArtifactReference, jobpostgres.Artifact, error) {
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	artifactID := "render-jobs/v1/result/" + job.ID + "/" + hash + ".json"
	expiresAt := job.ExpiresAt
	reference, err := runner.store.PutPrivate(ctx, orchestration.ArtifactWrite{
		ArtifactID: artifactID, Body: body, MediaType: "application/json",
		SchemaVersion: schemaVersion, ExpiresAt: &expiresAt,
	})
	if err != nil {
		return contracts.ArtifactReference{}, jobpostgres.Artifact{}, err
	}
	artifact := jobpostgres.Artifact{
		ID: reference.ArtifactID, SHA256: reference.SHA256, Kind: "result", Visibility: reference.Visibility,
		StorageKey: reference.ArtifactID, ByteSize: reference.Bytes, MediaType: reference.MediaType,
		SchemaVersion: schemaVersion, ExpiresAt: &expiresAt,
	}
	return reference, artifact, nil
}

func classifyExecutionFailure(status int, diagnostics []renderapi.Diagnostic) string {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "final_output.render_error" {
			return "document_render_incomplete"
		}
	}
	for _, diagnostic := range diagnostics {
		if strings.Contains(strings.ToLower(diagnostic.Code), "timeout") {
			return "document_render_timeout"
		}
	}
	if status >= 400 && status < 500 {
		return "invalid_project_source"
	}
	return "document_render_failed"
}

func workloadRefinement(job jobpostgres.Job, response renderapi.ProjectRenderResponse) jobpostgres.WorkloadRefinement {
	pageCount := collectionLength(response.Reader, "pages")
	fileCount := collectionLength(response.Project, "files")
	documentClass := "article"
	if job.ContentKind == "latex" {
		if latexBookClassPattern.MatchString(response.AnalysisSource) {
			documentClass = "book"
		}
	} else if pageCount > 1 {
		documentClass = "book"
	}
	return jobpostgres.WorkloadRefinement{
		DocumentEngine: response.Engine, DocumentClass: documentClass,
		FileCount: fileCount, PageCount: pageCount, MathCount: int64(response.Math.Count),
		DiagramCount: int64(len(response.Diagrams)), CodeBlockCount: int64(strings.Count(strings.ToLower(response.HTML), "<pre")),
	}
}

func collectionLength(value any, key string) int64 {
	object, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	collection := reflect.ValueOf(object[key])
	if !collection.IsValid() || (collection.Kind() != reflect.Array && collection.Kind() != reflect.Slice) {
		return 0
	}
	return int64(collection.Len())
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

func waitForLeaseMaintenance(done <-chan struct{}, limit time.Duration) bool {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
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

// degradedDiagnosticCodes lists the distinct warning/error diagnostic codes of
// a render that still returned success. Formula, diagram or reference loss that
// reaches the reader through an HTTP 200 render must be loud in the operator
// log, not only discoverable by inspecting the stored artifact.
func degradedDiagnosticCodes(execution renderapi.ProjectExecutionResult) []string {
	diagnostics := make([]renderapi.Diagnostic, 0, len(execution.Diagnostics)+len(execution.Response.Diagnostics))
	diagnostics = append(diagnostics, execution.Diagnostics...)
	diagnostics = append(diagnostics, execution.Response.Diagnostics...)
	seen := map[string]bool{}
	codes := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		switch strings.ToLower(strings.TrimSpace(diagnostic.Severity)) {
		case "warning", "error":
		default:
			continue
		}
		code := strings.TrimSpace(diagnostic.Code)
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// degradedCodeList joins diagnostic codes, dropping whole trailing codes so the
// attribute stays inside the operational log value limit.
func degradedCodeList(codes []string) string {
	var builder strings.Builder
	for _, code := range codes {
		if builder.Len()+len(code)+1 > 128 {
			break
		}
		if builder.Len() > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(code)
	}
	return builder.String()
}
