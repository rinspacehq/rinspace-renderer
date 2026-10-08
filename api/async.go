package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/admission"
	"github.com/rinspacehq/rinspace-renderer/api/internal/artifactstore"
	"github.com/rinspacehq/rinspace-renderer/api/internal/cacheindex"
	"github.com/rinspacehq/rinspace-renderer/api/internal/completiondispatch"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobcompat"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobworker"
	"github.com/rinspacehq/rinspace-renderer/api/internal/lifecycle"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfexecutor"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfworker"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/scheduler"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstpdfexecutor"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstpdfworker"
)

type asyncRuntime struct {
	role             string
	handler          http.Handler
	projects         http.Handler
	coordinator      *lifecycle.Coordinator
	repository       *jobpostgres.Repository
	executor         *renderapi.ProjectExecutor
	workerCancel     context.CancelFunc
	workerDone       <-chan error
	dispatcherCancel context.CancelFunc
	dispatcherDone   <-chan error
}

func buildAsyncRuntime(ctx context.Context, config renderapi.Config) (*asyncRuntime, error) {
	role, err := configuredRuntimeRole()
	if err != nil {
		return nil, err
	}
	completionMode, err := configuredCompletionMode()
	if err != nil {
		return nil, err
	}
	if completionMode == "local" && config.StorageProvider != "local" {
		return nil, errors.New("local completion mode requires local public asset storage")
	}
	if completionMode == "control-plane" && config.StorageProvider == "local" {
		return nil, errors.New("Control Plane completion mode cannot publish local asset URLs")
	}
	databaseURL := strings.TrimSpace(os.Getenv("RIN_RENDERER_DATABASE_URL"))
	if databaseURL == "" {
		if completionMode == "local" {
			return nil, errors.New("RIN_RENDERER_DATABASE_URL is required for local completion mode")
		}
		if role != "all" {
			return nil, fmt.Errorf("RIN_RENDERER_DATABASE_URL is required for %s role", role)
		}
		return nil, nil
	}
	artifactRoot := strings.TrimSpace(os.Getenv("RIN_RENDERER_ARTIFACT_ROOT"))
	if artifactRoot == "" {
		return nil, errors.New("RIN_RENDERER_ARTIFACT_ROOT is required with RIN_RENDERER_DATABASE_URL")
	}
	serviceToken := strings.TrimSpace(config.ServiceToken)
	if serviceToken == "" {
		return nil, errors.New("RIN_RENDERER_SERVICE_TOKEN is required for the canonical job API")
	}
	repository, err := jobpostgres.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*asyncRuntime, error) {
		repository.Close()
		return nil, err
	}
	switch schemaMode := strings.TrimSpace(os.Getenv("RIN_RENDERER_SCHEMA_MODE")); schemaMode {
	case "", "auto":
		err = repository.Migrate(ctx)
	case "require-current":
		err = repository.VerifyMigrations(ctx)
	default:
		err = fmt.Errorf("RIN_RENDERER_SCHEMA_MODE must be auto or require-current, got %q", schemaMode)
	}
	if err != nil {
		return fail(err)
	}
	if completionMode == "local" {
		health, healthErr := repository.CompletionHealth(ctx, time.Now().UTC())
		if healthErr != nil {
			return fail(healthErr)
		}
		if health.Pending != 0 || health.Failed != 0 || health.Delivering != 0 || health.Dead != 0 {
			return fail(errors.New("local completion mode cannot start with Control Plane completion events in the outbox"))
		}
	}
	store, err := artifactstore.NewFileStore(artifactRoot, artifactstore.DefaultLimits())
	if err != nil {
		return fail(err)
	}
	principal := strings.TrimSpace(os.Getenv("RIN_RENDERER_SERVICE_PRINCIPAL"))
	if principal == "" {
		principal = "rinspace-service"
	}
	tokens := map[string]string{serviceToken: principal}
	grants := map[string][]string{principal: {"publish", "preview", "rebuild"}}
	if migrationToken := strings.TrimSpace(os.Getenv("RIN_RENDERER_MIGRATION_TOKEN")); migrationToken != "" {
		migrationPrincipal := strings.TrimSpace(os.Getenv("RIN_RENDERER_MIGRATION_PRINCIPAL"))
		if migrationPrincipal == "" {
			migrationPrincipal = "rinspace-migration"
		}
		tokens[migrationToken] = migrationPrincipal
		grants[migrationPrincipal] = []string{"migration"}
	}
	priorities, err := admission.NewTrustedPriorityPolicy("rebuild", grants)
	if err != nil {
		return fail(err)
	}
	coordinator := lifecycle.NewCoordinator()
	operations := operational.Default()
	if err := operations.AddProbe("database", repository.Ready); err != nil {
		return fail(err)
	}
	if err := operations.AddProbe("artifact_store", store.Ready); err != nil {
		return fail(err)
	}
	if err := operations.AddProbe("lifecycle", coordinator.Ready); err != nil {
		return fail(err)
	}
	if err := operations.AddCollector("queue", func(collectorContext context.Context) error {
		now := time.Now().UTC()
		summary, err := repository.Queue(collectorContext, now)
		if err != nil {
			return err
		}
		operations.Gauge("queue_projects", float64(summary.QueuedProjects), "queued")
		operations.Gauge("queue_projects", float64(summary.ActiveProjects), "active")
		operations.Gauge("queue_oldest_wait_seconds", summary.OldestWait.Seconds())
		usage, err := repository.ResourceUsage(collectorContext, now)
		if err != nil {
			return err
		}
		var heavyUsage int64
		for _, resourceClass := range jobpostgres.ResourceClasses() {
			operations.Gauge("resource_in_use", float64(usage[resourceClass]), resourceClass)
			if jobpostgres.IsHeavyResourceClass(resourceClass) {
				heavyUsage += usage[resourceClass]
			}
		}
		operations.Gauge("resource_in_use", float64(heavyUsage), "heavy")
		return nil
	}); err != nil {
		return fail(err)
	}
	completionHealth := func(probeContext context.Context) (jobpostgres.CompletionHealth, error) {
		return repository.CompletionHealth(probeContext, time.Now().UTC())
	}
	if err := operations.AddProbe("completion_path", func(probeContext context.Context) error {
		health, healthErr := completionHealth(probeContext)
		if healthErr != nil {
			return healthErr
		}
		return health.Ready()
	}); err != nil {
		return fail(err)
	}
	if err := operations.AddCollector("completion_path", func(collectorContext context.Context) error {
		health, healthErr := completionHealth(collectorContext)
		if healthErr != nil {
			return healthErr
		}
		operations.Gauge("completion_outbox_pending", float64(health.Pending))
		operations.Gauge("completion_outbox_failed", float64(health.Failed))
		operations.Gauge("completion_outbox_delivering", float64(health.Delivering))
		operations.Gauge("completion_outbox_dead", float64(health.Dead))
		operations.Gauge("completion_outbox_oldest_age_seconds", float64(health.OldestAgeSeconds))
		if health.LastAcceptedAt != nil {
			operations.Gauge("completion_last_accepted_unixtime", float64(health.LastAcceptedAt.Unix()))
		} else {
			operations.Gauge("completion_last_accepted_unixtime", 0)
		}
		return nil
	}); err != nil {
		return fail(err)
	}
	admissionService, err := admission.New(repository, store, operations, priorities, admission.Config{
		Limits: jobpostgres.AdmissionLimits{
			GlobalNonTerminal: 32, PrincipalQueued: 8, PreviewQueued: 20, PrivateSourceBytes: 2 << 30,
		},
		MaxSourceBytes: config.ProjectArchiveMaxBytes, ReservationTTL: 5 * time.Minute,
		SourceTTL: 24 * time.Hour, JobTTL: 8 * 24 * time.Hour,
		IdempotencyTTL: 24 * time.Hour, RetryAfter: 15 * time.Second,
	})
	if err != nil {
		return fail(err)
	}
	authenticator := jobapi.StaticTokenAuthenticator{Principals: tokens}
	schedulingPolicy := jobpostgres.SchedulingPolicy{
		Resources: jobpostgres.ResourceCapacities{
			DocumentLight: 2, DocumentLaTeXML: 1, MathNode: 2, TeXSVG: 2, BatchMigration: 1, LatexPDF: 1,
			DocumentTypst: 1, TypstPDF: 1, Heavy: 1,
		},
		Weights:       jobpostgres.PriorityWeights{Publish: 8, Preview: 5, Rebuild: 3, Migration: 1},
		AgingInterval: 5 * time.Minute, MaxAgingSteps: 12, PrincipalRunning: 2,
	}
	operations.Gauge("resource_capacity", float64(schedulingPolicy.Resources.DocumentLight), "document-light")
	operations.Gauge("resource_capacity", float64(schedulingPolicy.Resources.DocumentLaTeXML), "document-latexml")
	operations.Gauge("resource_capacity", float64(schedulingPolicy.Resources.MathNode), "math-node")
	operations.Gauge("resource_capacity", float64(schedulingPolicy.Resources.TeXSVG), "texsvg")
	operations.Gauge("resource_capacity", float64(schedulingPolicy.Resources.BatchMigration), "batch-migration")
	operations.Gauge("resource_capacity", float64(schedulingPolicy.Resources.LatexPDF), "latex-pdf")
	operations.Gauge("resource_capacity", float64(schedulingPolicy.Resources.DocumentTypst), "document-typst")
	operations.Gauge("resource_capacity", float64(schedulingPolicy.Resources.TypstPDF), "typst-pdf")
	operations.Gauge("resource_capacity", float64(schedulingPolicy.Resources.Heavy), "heavy")
	for _, resourceClass := range jobpostgres.ResourceClasses() {
		operations.Gauge("resource_in_use", 0, resourceClass)
	}
	handler, err := jobapi.New(authenticator, admissionService, repository, store, jobapi.Config{
		RendererVersion: config.RendererVersion, MaxSourceBytes: config.ProjectArchiveMaxBytes,
		RejectControlPlaneIdentity: completionMode == "local",
		ProjectFileMaxCount:        config.ProjectFileMaxCount, ProjectFileMaxBytes: config.ProjectFileMaxBytes,
		TypstProfileID: renderapi.TypstHTMLProfileID(config),
		EventPoll:      time.Second,
		Estimation:     jobpostgres.EstimationPolicy{MinimumSamples: 20, Scheduling: schedulingPolicy},
	})
	if err != nil {
		return fail(fmt.Errorf("configure renderer job API: %w", err))
	}
	projects, err := jobcompat.New(authenticator, admissionService, repository, store, jobcompat.Config{
		RendererVersion: config.RendererVersion, DefaultEngine: config.DefaultDocumentEngine,
		DefaultOwnerScope: "legacy-sync", MaxSourceBytes: config.ProjectArchiveMaxBytes,
		WaitLimit: config.RenderTimeout, PollInterval: 250 * time.Millisecond,
	})
	if err != nil {
		return fail(fmt.Errorf("configure renderer compatibility API: %w", err))
	}
	runtime := &asyncRuntime{
		role: role, handler: handler, projects: projects, coordinator: coordinator, repository: repository,
	}
	if role == "api" {
		return runtime, nil
	}
	if role == "pdf-worker" {
		broker, err := pdfexecutor.NewBrokerClient(
			strings.TrimSpace(os.Getenv("RINSPACE_WORKLOAD_BROKER_URL")),
			os.Getenv("RINSPACE_WORKLOAD_BROKER_KEY"), nil,
		)
		if err != nil {
			return fail(err)
		}
		pdfScheduler, err := scheduler.New(repository, scheduler.Config{
			LeaseDuration: time.Minute, RetryBackoff: 15 * time.Second, RecoveryLimit: 64,
			Policy: schedulingPolicy, ClaimResourceClass: "latex-pdf",
		})
		if err != nil {
			return fail(fmt.Errorf("configure PDF scheduler: %w", err))
		}
		spoolRoot := strings.TrimSpace(os.Getenv("RIN_RENDERER_PDF_SPOOL_ROOT"))
		if spoolRoot == "" {
			spoolRoot = "/var/lib/rinspace/renderer/jobs"
		}
		pdfExecutor, err := pdfexecutor.New(repository, store, broker, pdfexecutor.Config{
			SpoolRoot: spoolRoot, RendererVersion: config.RendererVersion,
			PollInterval: 250 * time.Millisecond, WallTimeout: 90 * time.Second,
		})
		if err != nil {
			return fail(fmt.Errorf("configure PDF executor: %w", err))
		}
		workerID := strings.TrimSpace(os.Getenv("RIN_RENDERER_PDF_WORKER_ID"))
		if workerID == "" {
			workerID = "rin-renderer-pdf-worker"
		}
		workerConcurrency, err := positiveEnvironmentInt("RIN_RENDERER_PDF_WORKER_CONCURRENCY", 1)
		if err != nil {
			return fail(err)
		}
		worker, err := pdfworker.New(pdfScheduler, store, pdfExecutor, coordinator, pdfworker.Config{
			WorkerID: workerID, PollInterval: 500 * time.Millisecond, HeartbeatInterval: 20 * time.Second,
			RecoveryTimeout: 5 * time.Second, MaxConcurrent: workerConcurrency,
		})
		if err != nil {
			return fail(fmt.Errorf("configure PDF worker: %w", err))
		}
		if err := operations.AddProbe("pdf_scheduler_worker", worker.Ready); err != nil {
			return fail(err)
		}
		workerContext, workerCancel := context.WithCancel(context.Background())
		workerDone := make(chan error, 1)
		go func() {
			workerErr := worker.Run(workerContext)
			reportWorkerStop(operations, "latex-pdf", workerErr)
			workerDone <- workerErr
		}()
		runtime.workerCancel = workerCancel
		runtime.workerDone = workerDone
		return runtime, nil
	}
	if role == "typst-pdf-worker" {
		broker, err := typstpdfexecutor.NewBrokerClient(
			strings.TrimSpace(os.Getenv("RINSPACE_WORKLOAD_BROKER_URL")),
			os.Getenv("RINSPACE_WORKLOAD_BROKER_KEY"), nil,
		)
		if err != nil {
			return fail(err)
		}
		typstScheduler, err := scheduler.New(repository, scheduler.Config{
			LeaseDuration: time.Minute, RetryBackoff: 15 * time.Second, RecoveryLimit: 64,
			Policy: schedulingPolicy, ClaimResourceClass: "typst-pdf",
		})
		if err != nil {
			return fail(fmt.Errorf("configure Typst PDF scheduler: %w", err))
		}
		spoolRoot := strings.TrimSpace(os.Getenv("RIN_RENDERER_TYPST_PDF_SPOOL_ROOT"))
		if spoolRoot == "" {
			spoolRoot = "/var/lib/rinspace/renderer/typst-jobs"
		}
		typstExecutor, err := typstpdfexecutor.New(repository, store, broker, typstpdfexecutor.Config{
			SpoolRoot: spoolRoot, RendererVersion: config.RendererVersion,
			PollInterval: 250 * time.Millisecond, WallTimeout: 30 * time.Second,
		})
		if err != nil {
			return fail(fmt.Errorf("configure Typst PDF executor: %w", err))
		}
		workerID := strings.TrimSpace(os.Getenv("RIN_RENDERER_TYPST_PDF_WORKER_ID"))
		if workerID == "" {
			workerID = "rin-renderer-typst-pdf-worker"
		}
		workerConcurrency, err := positiveEnvironmentInt("RIN_RENDERER_TYPST_PDF_WORKER_CONCURRENCY", 1)
		if err != nil {
			return fail(err)
		}
		worker, err := typstpdfworker.New(typstScheduler, store, typstExecutor, coordinator, typstpdfworker.Config{
			WorkerID: workerID, PollInterval: 500 * time.Millisecond, HeartbeatInterval: 20 * time.Second,
			RecoveryTimeout: 5 * time.Second, MaxConcurrent: workerConcurrency,
		})
		if err != nil {
			return fail(fmt.Errorf("configure Typst PDF worker: %w", err))
		}
		if err := operations.AddProbe("typst_pdf_scheduler_worker", worker.Ready); err != nil {
			return fail(err)
		}
		workerContext, workerCancel := context.WithCancel(context.Background())
		workerDone := make(chan error, 1)
		go func() {
			workerErr := worker.Run(workerContext)
			reportWorkerStop(operations, "typst-pdf", workerErr)
			workerDone <- workerErr
		}()
		runtime.workerCancel = workerCancel
		runtime.workerDone = workerDone
		return runtime, nil
	}
	var completionDispatcher *completiondispatch.Dispatcher
	if completionMode == "control-plane" {
		completionEndpoint := strings.TrimSpace(os.Getenv("RIN_RENDERER_CONTROL_PLANE_EVENT_URL"))
		completionKey := []byte(os.Getenv("RIN_RENDERER_CONTROL_PLANE_EVENT_HMAC_KEY"))
		completionKeyID := strings.TrimSpace(os.Getenv("RIN_RENDERER_CONTROL_PLANE_EVENT_HMAC_KEY_ID"))
		if completionKeyID == "" {
			completionKeyID = "renderer-current"
		}
		if completionEndpoint == "" || len(completionKey) < 32 || string(completionKey) == serviceToken {
			return fail(errors.New("Renderer completion dispatcher requires a Control Plane endpoint and purpose-specific 32-byte HMAC key"))
		}
		completionMaxAttempts, err := positiveEnvironmentInt("RIN_RENDERER_COMPLETION_MAX_ATTEMPTS", 10)
		if err != nil || completionMaxAttempts > 100 {
			if err == nil {
				err = errors.New("RIN_RENDERER_COMPLETION_MAX_ATTEMPTS must not exceed 100")
			}
			return fail(err)
		}
		completionSender, err := completiondispatch.NewHTTPSender(completiondispatch.HTTPSenderConfig{
			Endpoint: completionEndpoint, KeyID: completionKeyID, Key: completionKey,
			Client: &http.Client{Timeout: 10 * time.Second},
		})
		if err != nil {
			return fail(err)
		}
		completionDispatcher, err = completiondispatch.New(repository, completionSender, completiondispatch.Config{
			PollInterval: 250 * time.Millisecond, LeaseDuration: 30 * time.Second,
			RequestTimeout: 10 * time.Second, MaxAttempts: completionMaxAttempts, Operations: operations,
		})
		if err != nil {
			return fail(err)
		}
	}
	// The Typst HTML channel never shares the ordinary document pool. Either the
	// worker holds leadership for exactly document-typst, or it holds the ordinary
	// leadership whose claim list deliberately excludes every Typst class.
	workerResourceClass := ""
	if role == "typst-html-worker" {
		workerResourceClass = jobpostgres.TypstHTMLResourceClass
		if renderapi.TypstHTMLProfileID(config) == "" {
			return fail(errors.New("Typst HTML worker requires a complete pinned Typst profile; set every RIN_RENDERER_TYPST_* value"))
		}
	}
	schedulerService, err := scheduler.New(repository, scheduler.Config{
		LeaseDuration: time.Minute, RetryBackoff: 15 * time.Second, RecoveryLimit: 64,
		Policy: schedulingPolicy, ClaimResourceClass: workerResourceClass,
	})
	if err != nil {
		return fail(fmt.Errorf("configure renderer scheduler: %w", err))
	}
	cache := cacheindex.New(repository, store)
	executor, err := renderapi.NewProjectExecutorWithMarkdown(config, cache, cache)
	if err != nil {
		return fail(fmt.Errorf("configure Markdown project executor: %w", err))
	}
	workerID := strings.TrimSpace(os.Getenv("RIN_RENDERER_WORKER_ID"))
	if workerID == "" {
		workerID = "rin-renderer-worker"
		if workerResourceClass != "" {
			workerID = "rin-renderer-typst-worker"
		}
	}
	workerConcurrency, err := positiveEnvironmentInt("RIN_RENDERER_WORKER_CONCURRENCY", 1)
	if err != nil {
		_ = executor.Close()
		return fail(err)
	}
	worker, err := jobworker.New(
		schedulerService, repository, store, executor, coordinator,
		jobworker.Config{
			WorkerID: workerID, PollInterval: 500 * time.Millisecond, HeartbeatInterval: 20 * time.Second,
			RecoveryTimeout: 5 * time.Second, MaxConcurrent: workerConcurrency,
		},
	)
	if err != nil {
		_ = executor.Close()
		return fail(fmt.Errorf("configure renderer job worker: %w", err))
	}
	if err := operations.AddProbe("scheduler_worker", worker.Ready); err != nil {
		return fail(err)
	}
	if completionDispatcher != nil {
		if err := operations.AddProbe("completion_dispatcher", completionDispatcher.Ready); err != nil {
			return fail(err)
		}
	}
	workerContext, workerCancel := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	workerLabel := "document"
	if workerResourceClass != "" {
		workerLabel = workerResourceClass
	}
	go func() {
		workerErr := worker.Run(workerContext)
		reportWorkerStop(operations, workerLabel, workerErr)
		workerDone <- workerErr
	}()
	runtime.executor = executor
	runtime.workerCancel = workerCancel
	runtime.workerDone = workerDone
	if completionDispatcher != nil {
		dispatcherContext, dispatcherCancel := context.WithCancel(context.Background())
		dispatcherDone := make(chan error, 1)
		go func() {
			dispatcherErr := completionDispatcher.Run(dispatcherContext)
			reportWorkerStop(operations, "completion", dispatcherErr)
			dispatcherDone <- dispatcherErr
		}()
		runtime.dispatcherCancel = dispatcherCancel
		runtime.dispatcherDone = dispatcherDone
	}
	return runtime, nil
}

// reportWorkerStop records a background worker's exit. A failure that is not a
// deliberate shutdown is surfaced as an error log carrying the worker's
// resource class, so an operator sees why a channel died instead of a bare
// `worker_stopped` line with no cause.
func reportWorkerStop(operations *operational.Registry, resourceClass string, workerErr error) {
	state := "stopped"
	if workerErr != nil && !errors.Is(workerErr, context.Canceled) {
		state = "failed"
		operations.Count("worker_failures_total", resourceClass)
		slog.Error("renderer worker stopped", "resource_class", resourceClass, "error", workerErr)
	}
	operations.Event("worker_stopped", slog.String("resource_class", resourceClass), slog.String("state", state))
}

func (runtime *asyncRuntime) Close() error {
	if runtime == nil || runtime.repository == nil {
		return nil
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, shutdownErr := runtime.coordinator.Shutdown(shutdownContext, 20*time.Second)
	var workerErr, dispatcherErr, executorErr error
	if runtime.dispatcherCancel != nil {
		runtime.dispatcherCancel()
		dispatcherErr = <-runtime.dispatcherDone
	}
	if runtime.workerCancel != nil {
		runtime.workerCancel()
		workerErr = <-runtime.workerDone
	}
	if runtime.executor != nil {
		executorErr = runtime.executor.Close()
	}
	closeErr := runtime.repository.Close()
	if workerErr != nil && !errors.Is(workerErr, context.Canceled) {
		return workerErr
	}
	if dispatcherErr != nil && !errors.Is(dispatcherErr, context.Canceled) {
		return dispatcherErr
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	if executorErr != nil {
		return executorErr
	}
	return closeErr
}

func configuredRuntimeRole() (string, error) {
	role := strings.TrimSpace(os.Getenv("RIN_RENDERER_RUNTIME_ROLE"))
	if role == "" {
		role = "all"
	}
	switch role {
	case "all", "api", "worker", "typst-html-worker", "pdf-worker", "typst-pdf-worker":
		return role, nil
	default:
		return "", fmt.Errorf("RIN_RENDERER_RUNTIME_ROLE must be all, api, worker, typst-html-worker, pdf-worker, or typst-pdf-worker, got %q", role)
	}
}

func configuredCompletionMode() (string, error) {
	mode := strings.TrimSpace(os.Getenv("RIN_RENDERER_COMPLETION_MODE"))
	if mode == "" {
		mode = "control-plane"
	}
	switch mode {
	case "control-plane", "local":
		return mode, nil
	default:
		return "", fmt.Errorf("RIN_RENDERER_COMPLETION_MODE must be control-plane or local, got %q", mode)
	}
}

func positiveEnvironmentInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}
	return value, nil
}
