package typstpdfworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/scheduler"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstpdfexecutor"
)

type fakeScheduler struct {
	cancelRequested bool
	confirmed       int
	failedCode      string
	failedRetryable bool
	failureArtifact *jobpostgres.Artifact
	successArtifact *jobpostgres.Artifact
	evidence        jobpostgres.CompletionResultEvidence
}

func (*fakeScheduler) AcquireLeadership(context.Context) (*jobpostgres.Leadership, error) {
	return nil, errors.New("not used")
}
func (*fakeScheduler) Claim(context.Context, *jobpostgres.Leadership, string) (scheduler.Lease, error) {
	return scheduler.Lease{}, jobpostgres.ErrNoEligibleJob
}
func (*fakeScheduler) Heartbeat(_ context.Context, lease scheduler.Lease) (scheduler.Lease, error) {
	return lease, nil
}
func (*fakeScheduler) Recover(context.Context, *jobpostgres.Leadership) (int, error) { return 0, nil }
func (fake *fakeScheduler) Fail(_ context.Context, lease scheduler.Lease, retryable bool, code string) (jobpostgres.Job, error) {
	fake.failedCode, fake.failedRetryable = code, retryable
	lease.Job.State = "failed"
	return lease.Job, nil
}
func (fake *fakeScheduler) FailWithResult(_ context.Context, lease scheduler.Lease, retryable bool, code string, artifact jobpostgres.Artifact) (jobpostgres.Job, error) {
	fake.failedCode, fake.failedRetryable, fake.failureArtifact = code, retryable, &artifact
	if retryable {
		lease.Job.State = "queued"
	} else {
		lease.Job.State = "failed"
	}
	return lease.Job, nil
}
func (fake *fakeScheduler) SucceedWithResult(_ context.Context, lease scheduler.Lease, artifact jobpostgres.Artifact, evidence jobpostgres.CompletionResultEvidence) (jobpostgres.Job, error) {
	fake.successArtifact, fake.evidence = &artifact, evidence
	lease.Job.State = "succeeded"
	return lease.Job, nil
}
func (fake *fakeScheduler) CancellationRequested(context.Context, scheduler.Lease) (bool, error) {
	return fake.cancelRequested, nil
}
func (fake *fakeScheduler) ConfirmCancellation(_ context.Context, lease scheduler.Lease) (jobpostgres.Job, error) {
	fake.confirmed++
	lease.Job.State = "canceled"
	return lease.Job, nil
}

type fakeStore struct{ body []byte }

func (store *fakeStore) PutPrivate(_ context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	store.body = append([]byte(nil), write.Body...)
	digest := sha256.Sum256(write.Body)
	return contracts.ArtifactReference{
		ArtifactID: write.ArtifactID, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(write.Body)),
		MediaType: write.MediaType, Visibility: "private", ExpiresAt: write.ExpiresAt.UTC().Format(time.RFC3339),
	}, nil
}

type fakeExecutor struct{ outcome typstpdfexecutor.Outcome }

func (executor fakeExecutor) Execute(context.Context, jobpostgres.Job) typstpdfexecutor.Outcome {
	return executor.outcome
}

type fakeLifecycle struct{}

func (fakeLifecycle) BeginWorker(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func TestNewRequiresOneTypstPDFCompiler(t *testing.T) {
	for _, concurrency := range []int{0, 2, 8} {
		_, err := New(&fakeScheduler{}, &fakeStore{}, fakeExecutor{}, fakeLifecycle{}, Config{
			WorkerID: "typst-pdf-worker", PollInterval: time.Millisecond, HeartbeatInterval: time.Hour,
			RecoveryTimeout: time.Second, MaxConcurrent: concurrency,
		})
		if err == nil {
			t.Fatalf("New() accepted concurrency %d", concurrency)
		}
	}
	if _, err := New(&fakeScheduler{}, &fakeStore{}, fakeExecutor{}, fakeLifecycle{}, Config{
		WorkerID: "typst-pdf-worker", PollInterval: time.Millisecond, HeartbeatInterval: time.Hour,
		RecoveryTimeout: time.Second, MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("New() rejected a valid Typst PDF worker: %v", err)
	}
}

func TestRunnerFinishesSuccessAndFailure(t *testing.T) {
	now := time.Date(2026, 9, 16, 4, 5, 6, 0, time.UTC)
	job := typstJob()
	lease := scheduler.Lease{Job: job, AttemptID: "attempt", Token: "token"}
	t.Run("success", func(t *testing.T) {
		schedulerService := &fakeScheduler{}
		artifactExpires := now.Add(24 * time.Hour)
		resultArtifact := jobpostgres.Artifact{
			ID:     "render-jobs/v1/result/" + job.ID + "/" + strings.Repeat("a", 64) + ".json",
			SHA256: strings.Repeat("a", 64), Kind: "result", Visibility: "private", ExpiresAt: &artifactExpires,
		}
		executor := fakeExecutor{outcome: typstpdfexecutor.Outcome{
			ResultReference: contracts.ArtifactReference{ArtifactID: resultArtifact.ID}, ResultArtifact: resultArtifact,
			Evidence: jobpostgres.CompletionResultEvidence{RendererProjectHash: strings.Repeat("b", 64), ResultHash: strings.Repeat("c", 64)},
		}}
		runner := mustRunner(t, schedulerService, &fakeStore{}, executor, now)
		runner.execute(context.Background(), lease)
		if schedulerService.successArtifact == nil || schedulerService.evidence != executor.outcome.Evidence {
			t.Fatalf("success finish = %#v, %#v", schedulerService.successArtifact, schedulerService.evidence)
		}
		if err := runner.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "typst_pdf_worker_unavailable") {
			t.Fatalf("an idle Typst PDF worker reported ready: %v", err)
		}
	})
	t.Run("compile-failure", func(t *testing.T) {
		schedulerService := &fakeScheduler{}
		store := &fakeStore{}
		expiresAt := now.Add(24 * time.Hour).Format(time.RFC3339)
		executor := fakeExecutor{outcome: typstpdfexecutor.Outcome{Failure: &typstpdfexecutor.Failure{
			Code: "typst_compile_failed", Status: 422, Message: "Typst compilation failed", ExitCode: intPointer(12),
			Diagnostics: []renderapi.Diagnostic{{Severity: "error", Code: "typst.compile_error", Message: "unknown variable: foo", Engine: "typst"}},
			Artifacts: []contracts.ArtifactReference{{
				ArtifactID: "render-jobs/v1/typst/" + job.ID + "/log-" + strings.Repeat("d", 64) + ".log", SHA256: strings.Repeat("d", 64),
				Bytes: 4, MediaType: "text/plain; charset=utf-8", Visibility: "private", ExpiresAt: expiresAt,
			}},
		}}}
		runner := mustRunner(t, schedulerService, store, executor, now)
		runner.execute(context.Background(), lease)
		if schedulerService.failureArtifact == nil || schedulerService.failedCode != "typst_compile_failed" || schedulerService.failedRetryable {
			t.Fatalf("failure finish = %#v, %q, %v", schedulerService.failureArtifact, schedulerService.failedCode, schedulerService.failedRetryable)
		}
		var stored jobresult.StoredFailure
		if err := json.Unmarshal(store.body, &stored); err != nil || stored.Validate() != nil || len(stored.Artifacts) != 1 ||
			stored.ExitCode == nil || *stored.ExitCode != 12 || len(stored.Diagnostics) != 1 {
			t.Fatalf("stored failure = %#v, %v", stored, err)
		}
	})
}

func TestRunnerRejectsNonTypstJob(t *testing.T) {
	schedulerService := &fakeScheduler{}
	runner := mustRunner(t, schedulerService, &fakeStore{}, fakeExecutor{}, time.Now())
	runner.execute(context.Background(), scheduler.Lease{Job: jobpostgres.Job{
		ID: "018fcafe-1234-4abc-8def-1234567890ab", ContentKind: "latex", DocumentEngine: "latexmk", ResourceClass: "latex-pdf",
	}, AttemptID: "attempt", Token: "token"})
	if schedulerService.failedCode != "invalid_typst_pdf_job" || schedulerService.failedRetryable {
		t.Fatalf("non-Typst job was not rejected: %q retryable=%v", schedulerService.failedCode, schedulerService.failedRetryable)
	}
}

func TestRunnerRejectsInvalidFailurePayload(t *testing.T) {
	for _, failure := range []typstpdfexecutor.Failure{
		{Code: "", Status: 422, Message: "missing code"},
		{Code: "typst_compile_failed", Status: 200, Message: "bad status"},
		{Code: "typst_compile_failed", Status: 422, Message: ""},
	} {
		schedulerService := &fakeScheduler{}
		runner := mustRunner(t, schedulerService, &fakeStore{}, fakeExecutor{outcome: typstpdfexecutor.Outcome{Failure: &failure}}, time.Now())
		runner.execute(context.Background(), scheduler.Lease{Job: typstJob(), AttemptID: "attempt", Token: "token"})
		if schedulerService.failedCode != "invalid_typst_pdf_failure" || !schedulerService.failedRetryable || schedulerService.failureArtifact != nil {
			t.Fatalf("invalid failure payload = %#v -> %q", failure, schedulerService.failedCode)
		}
	}
}

func TestRunnerRejectsIncompleteSuccessOutcome(t *testing.T) {
	schedulerService := &fakeScheduler{}
	runner := mustRunner(t, schedulerService, &fakeStore{}, fakeExecutor{outcome: typstpdfexecutor.Outcome{}}, time.Now())
	runner.execute(context.Background(), scheduler.Lease{Job: typstJob(), AttemptID: "attempt", Token: "token"})
	if schedulerService.failedCode != "invalid_typst_pdf_result" || !schedulerService.failedRetryable || schedulerService.successArtifact != nil {
		t.Fatalf("incomplete outcome = %q retryable=%v", schedulerService.failedCode, schedulerService.failedRetryable)
	}
}

func TestRunnerConfirmsCancellationWithoutPublishingResult(t *testing.T) {
	now := time.Date(2026, 9, 16, 4, 5, 6, 0, time.UTC)
	schedulerService := &fakeScheduler{cancelRequested: true}
	executor := fakeExecutor{outcome: typstpdfexecutor.Outcome{Failure: &typstpdfexecutor.Failure{
		Code: "typst_worker_interrupted", Status: 503, Message: "interrupted", Retryable: true,
	}}}
	runner := mustRunner(t, schedulerService, &fakeStore{}, executor, now)
	runner.execute(context.Background(), scheduler.Lease{Job: typstJob(), AttemptID: "attempt", Token: "token"})
	if schedulerService.confirmed != 1 || schedulerService.failureArtifact != nil || schedulerService.successArtifact != nil {
		t.Fatalf("cancellation finish = confirmed %d, failure %#v, success %#v", schedulerService.confirmed, schedulerService.failureArtifact, schedulerService.successArtifact)
	}
}

func mustRunner(t *testing.T, schedulerService *fakeScheduler, store *fakeStore, executor fakeExecutor, now time.Time) *Runner {
	t.Helper()
	runner, err := New(schedulerService, store, executor, fakeLifecycle{}, Config{
		WorkerID: "typst-pdf-worker", PollInterval: time.Millisecond, HeartbeatInterval: time.Hour,
		RecoveryTimeout: time.Second, MaxConcurrent: 1, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func intPointer(value int) *int { return &value }

func typstJob() jobpostgres.Job {
	return jobpostgres.Job{
		ID: "018fcafe-1234-4abc-8def-1234567890ab", ContentKind: "typst", DocumentEngine: "typst",
		ResourceClass: "typst-pdf", PriorityClass: "preview", RendererVersion: "test",
	}
}
