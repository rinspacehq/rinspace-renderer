package pdfworker

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
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfexecutor"
	"github.com/rinspacehq/rinspace-renderer/api/internal/scheduler"
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

type fakeExecutor struct{ outcome pdfexecutor.Outcome }

func (executor fakeExecutor) Execute(context.Context, jobpostgres.Job) pdfexecutor.Outcome {
	return executor.outcome
}

type fakeLifecycle struct{}

func (fakeLifecycle) BeginWorker(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func TestNewRequiresOnePDFCompiler(t *testing.T) {
	for _, concurrency := range []int{0, 2, 8} {
		_, err := New(&fakeScheduler{}, &fakeStore{}, fakeExecutor{}, fakeLifecycle{}, Config{
			WorkerID: "pdf-worker", PollInterval: time.Millisecond, HeartbeatInterval: time.Hour,
			RecoveryTimeout: time.Second, MaxConcurrent: concurrency,
		})
		if err == nil {
			t.Fatalf("New() accepted concurrency %d", concurrency)
		}
	}
}

func TestRunnerFinishesSuccessAndFailure(t *testing.T) {
	now := time.Date(2026, 8, 29, 4, 5, 6, 0, time.UTC)
	job := pdfJob()
	lease := scheduler.Lease{Job: job, AttemptID: "attempt", Token: "token"}
	t.Run("success", func(t *testing.T) {
		schedulerService := &fakeScheduler{}
		artifactExpires := now.Add(24 * time.Hour)
		resultArtifact := jobpostgres.Artifact{
			ID:     "render-jobs/v1/result/" + job.ID + "/" + strings.Repeat("a", 64) + ".json",
			SHA256: strings.Repeat("a", 64), Kind: "result", Visibility: "private", ExpiresAt: &artifactExpires,
		}
		executor := fakeExecutor{outcome: pdfexecutor.Outcome{
			ResultReference: contracts.ArtifactReference{ArtifactID: resultArtifact.ID}, ResultArtifact: resultArtifact,
			Evidence: jobpostgres.CompletionResultEvidence{RendererProjectHash: strings.Repeat("b", 64), ResultHash: strings.Repeat("c", 64)},
		}}
		runner := mustRunner(t, schedulerService, &fakeStore{}, executor, now)
		runner.execute(context.Background(), lease)
		if schedulerService.successArtifact == nil || schedulerService.evidence != executor.outcome.Evidence {
			t.Fatalf("success finish = %#v, %#v", schedulerService.successArtifact, schedulerService.evidence)
		}
	})
	t.Run("compile-failure", func(t *testing.T) {
		schedulerService := &fakeScheduler{}
		store := &fakeStore{}
		expiresAt := now.Add(24 * time.Hour).Format(time.RFC3339)
		executor := fakeExecutor{outcome: pdfexecutor.Outcome{Failure: &pdfexecutor.Failure{
			Code: "pdf_compile_failed", Status: 422, Message: "LaTeX compilation failed", ExitCode: intPointer(12),
			Artifacts: []contracts.ArtifactReference{{
				ArtifactID: "render-jobs/v1/debug/" + job.ID + "/log.txt", SHA256: strings.Repeat("d", 64),
				Bytes: 4, MediaType: "text/plain", Visibility: "private", ExpiresAt: expiresAt,
			}},
		}}}
		runner := mustRunner(t, schedulerService, store, executor, now)
		runner.execute(context.Background(), lease)
		if schedulerService.failureArtifact == nil || schedulerService.failedCode != "pdf_compile_failed" || schedulerService.failedRetryable {
			t.Fatalf("failure finish = %#v, %q, %v", schedulerService.failureArtifact, schedulerService.failedCode, schedulerService.failedRetryable)
		}
		var stored jobresult.StoredFailure
		if err := json.Unmarshal(store.body, &stored); err != nil || stored.Validate() != nil || len(stored.Artifacts) != 1 || stored.ExitCode == nil || *stored.ExitCode != 12 {
			t.Fatalf("stored failure = %#v, %v", stored, err)
		}
	})
}

func intPointer(value int) *int { return &value }

func TestRunnerConfirmsCancellationWithoutPublishingResult(t *testing.T) {
	now := time.Date(2026, 8, 29, 4, 5, 6, 0, time.UTC)
	schedulerService := &fakeScheduler{cancelRequested: true}
	executor := fakeExecutor{outcome: pdfexecutor.Outcome{Failure: &pdfexecutor.Failure{
		Code: "pdf_worker_interrupted", Status: 503, Message: "interrupted", Retryable: true,
	}}}
	runner := mustRunner(t, schedulerService, &fakeStore{}, executor, now)
	runner.execute(context.Background(), scheduler.Lease{Job: pdfJob(), AttemptID: "attempt", Token: "token"})
	if schedulerService.confirmed != 1 || schedulerService.failureArtifact != nil || schedulerService.successArtifact != nil {
		t.Fatalf("cancellation finish = confirmed %d, failure %#v, success %#v", schedulerService.confirmed, schedulerService.failureArtifact, schedulerService.successArtifact)
	}
}

func mustRunner(t *testing.T, schedulerService *fakeScheduler, store *fakeStore, executor fakeExecutor, now time.Time) *Runner {
	t.Helper()
	runner, err := New(schedulerService, store, executor, fakeLifecycle{}, Config{
		WorkerID: "pdf-worker", PollInterval: time.Millisecond, HeartbeatInterval: time.Hour,
		RecoveryTimeout: time.Second, MaxConcurrent: 1, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func pdfJob() jobpostgres.Job {
	return jobpostgres.Job{
		ID: "018fcafe-1234-4abc-8def-1234567890ab", ContentKind: "latex", DocumentEngine: "latexmk",
		ResourceClass: "latex-pdf", PriorityClass: "preview", RendererVersion: "test",
	}
}
