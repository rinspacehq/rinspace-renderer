package scheduler

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
)

func TestServiceGeneratesAndHashesLeaseToken(t *testing.T) {
	repository := &fakeRepository{}
	service := newTestService(t, repository)
	lease, err := service.Claim(context.Background(), nil, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(lease.Token) != 64 || lease.Token == repository.claimInput.LeaseTokenHash {
		t.Fatalf("lease token/hash = %q / %q", lease.Token, repository.claimInput.LeaseTokenHash)
	}
	if repository.claimInput.LeaseTokenHash != hashToken(lease.Token) {
		t.Fatal("repository did not receive the lease-token hash")
	}
	if lease.AttemptID != "00000000-0000-4000-8000-000000000000" || lease.AttemptNo != 1 {
		t.Fatalf("lease = %#v", lease)
	}

	heartbeat, err := service.Heartbeat(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if repository.heartbeatTokenHash != hashToken(lease.Token) || !heartbeat.LeaseExpiresAt.After(lease.LeaseExpiresAt) {
		t.Fatalf("heartbeat = %#v, hash %q", heartbeat, repository.heartbeatTokenHash)
	}
	refinement := jobpostgres.WorkloadRefinement{DocumentEngine: "latexml", DocumentClass: "book", FileCount: 3, PageCount: 10}
	if _, err := service.RefineWorkload(context.Background(), heartbeat, refinement); err != nil || repository.refineInput.LeaseTokenHash != hashToken(lease.Token) || repository.refineInput.Refinement.DocumentClass != "book" {
		t.Fatalf("RefineWorkload() input = %#v, error %v", repository.refineInput, err)
	}

	job, err := service.Fail(context.Background(), heartbeat, true, "worker_transient")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" || repository.finishInput.LeaseTokenHash != hashToken(lease.Token) ||
		!repository.finishInput.Retryable || repository.finishInput.ErrorCode != "worker_transient" {
		t.Fatalf("failure input/job = %#v / %#v", repository.finishInput, job)
	}
}

func TestServiceSuccessAndRecoveryUseConfiguredBounds(t *testing.T) {
	repository := &fakeRepository{}
	service := newTestService(t, repository)
	lease, err := service.Claim(context.Background(), nil, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Succeed(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "succeeded" || !repository.finishInput.Succeeded {
		t.Fatalf("success = %#v / %#v", job, repository.finishInput)
	}
	expiresAt := time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC)
	artifact := jobpostgres.Artifact{ID: "result-id", Kind: "result", Visibility: "private", ExpiresAt: &expiresAt}
	evidence := jobpostgres.CompletionResultEvidence{RendererProjectHash: strings.Repeat("a", 64), ResultHash: strings.Repeat("b", 64)}
	if _, err := service.SucceedWithResult(context.Background(), lease, artifact, evidence); err != nil || repository.finishInput.ResultArtifact == nil || repository.finishInput.ResultArtifact.ID != artifact.ID || repository.finishInput.CompletionResult == nil || *repository.finishInput.CompletionResult != evidence {
		t.Fatalf("SucceedWithResult() input = %#v, error %v", repository.finishInput, err)
	}
	if _, err := service.FailWithResult(context.Background(), lease, false, "document_invalid", artifact); err != nil || repository.finishInput.ResultArtifact == nil || repository.finishInput.Succeeded || repository.finishInput.ErrorCode != "document_invalid" {
		t.Fatalf("FailWithResult() input = %#v, error %v", repository.finishInput, err)
	}
	recovered, err := service.Recover(context.Background(), nil)
	if err != nil || recovered != 2 {
		t.Fatalf("Recover() = %d, %v", recovered, err)
	}
	if repository.recoveryLimit != 10 || repository.recoveryRetryAt.Sub(repository.recoveryNow) != 15*time.Second {
		t.Fatalf("recovery bounds = %d, %v", repository.recoveryLimit, repository.recoveryRetryAt.Sub(repository.recoveryNow))
	}
	interrupted, err := service.InterruptAndRecover(context.Background(), nil)
	if err != nil || interrupted != 2 || repository.interruptLimit != 10 || repository.recoveryLimit != 10 {
		t.Fatalf("InterruptAndRecover() = %d, %v; limits %d/%d", interrupted, err, repository.interruptLimit, repository.recoveryLimit)
	}
	if !repository.recoveryNow.Equal(repository.interruptNow) || repository.recoveryRetryAt.Sub(repository.recoveryNow) != 15*time.Second {
		t.Fatalf("interruption recovery timing = %v / %v", repository.interruptNow, repository.recoveryRetryAt)
	}
}

func TestServiceRejectsMissingWorkerOrFailureCode(t *testing.T) {
	service := newTestService(t, &fakeRepository{})
	if _, err := service.Claim(context.Background(), nil, ""); err == nil {
		t.Fatal("Claim() accepted empty worker ID")
	}
	if _, err := service.Fail(context.Background(), Lease{}, true, ""); err == nil {
		t.Fatal("Fail() accepted empty error code")
	}
}

func TestServicePassesDedicatedPDFClaimFilter(t *testing.T) {
	repository := &fakeRepository{}
	service := newTestService(t, repository)
	service.config.ClaimResourceClass = "latex-pdf"
	if _, err := service.Claim(context.Background(), nil, "pdf-worker"); err != nil {
		t.Fatal(err)
	}
	if repository.claimInput.ResourceClass != "latex-pdf" {
		t.Fatalf("PDF claim resource class = %q", repository.claimInput.ResourceClass)
	}
}

func TestSchedulerAcceptsOnlyDedicatedClaimFilters(t *testing.T) {
	repository := &fakeRepository{}
	for _, resourceClass := range []string{"latex-pdf", "typst-pdf", "document-typst"} {
		if _, err := New(repository, Config{
			LeaseDuration: 60 * time.Second, RetryBackoff: 15 * time.Second, RecoveryLimit: 10,
			Policy: jobpostgres.SchedulingPolicy{
				Resources: jobpostgres.ResourceCapacities{
					DocumentLight: 2, DocumentLaTeXML: 1, MathNode: 2, TeXSVG: 2, BatchMigration: 1,
					LatexPDF: 1, DocumentTypst: 1, TypstPDF: 1, Heavy: 5,
				},
				Weights:          jobpostgres.PriorityWeights{Publish: 8, Preview: 5, Rebuild: 3, Migration: 1},
				AgingInterval:    5 * time.Minute,
				MaxAgingSteps:    12,
				PrincipalRunning: 2,
			},
			ClaimResourceClass: resourceClass,
		}); err != nil {
			t.Fatalf("scheduler rejected dedicated claim filter %q: %v", resourceClass, err)
		}
	}
	for _, resourceClass := range []string{"document-latexml", "document-light", "math-node", "texsvg", "batch-migration"} {
		if _, err := New(repository, Config{
			LeaseDuration: 60 * time.Second, RetryBackoff: 15 * time.Second, RecoveryLimit: 10,
			Policy: jobpostgres.SchedulingPolicy{
				Resources: jobpostgres.ResourceCapacities{
					DocumentLight: 2, DocumentLaTeXML: 1, MathNode: 2, TeXSVG: 2, BatchMigration: 1,
					LatexPDF: 1, DocumentTypst: 1, TypstPDF: 1, Heavy: 5,
				},
				Weights:          jobpostgres.PriorityWeights{Publish: 8, Preview: 5, Rebuild: 3, Migration: 1},
				AgingInterval:    5 * time.Minute,
				MaxAgingSteps:    12,
				PrincipalRunning: 2,
			},
			ClaimResourceClass: resourceClass,
		}); err == nil {
			t.Fatalf("scheduler accepted non-dedicated claim filter %q", resourceClass)
		}
	}
}

func TestServiceSubworkUsesSharedHashedResourceLease(t *testing.T) {
	repository := &fakeRepository{}
	service := newTestService(t, repository)
	lease, err := service.AcquireSubwork(context.Background(), "texsvg", "job-a", "diagram-1", "worker-diagram")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Allocation.ResourceClass != "texsvg" || repository.subworkInput.LeaseTokenHash != hashToken(lease.Token) ||
		repository.subworkInput.Capacities.TeXSVG != 2 {
		t.Fatalf("subwork lease/input = %#v / %#v", lease, repository.subworkInput)
	}
	lease, err = service.HeartbeatSubwork(context.Background(), lease)
	if err != nil || repository.subworkHeartbeatHash != hashToken(lease.Token) {
		t.Fatalf("HeartbeatSubwork() = %#v, %v", lease, err)
	}
	if err := service.ReleaseSubwork(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if repository.subworkReleaseHash != hashToken(lease.Token) {
		t.Fatal("ReleaseSubwork() did not hash the token")
	}
}

func TestServiceCancellationUsesLeaseFence(t *testing.T) {
	repository := &fakeRepository{}
	service := newTestService(t, repository)
	lease, err := service.Claim(context.Background(), nil, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	cancellation, err := service.RequestCancellation(context.Background(), lease.Job.ID)
	if err != nil || !cancellation.WorkerMustStop || repository.cancelJobID != lease.Job.ID {
		t.Fatalf("RequestCancellation() = %#v, %v", cancellation, err)
	}
	requested, err := service.CancellationRequested(context.Background(), lease)
	if err != nil || !requested || repository.cancelCheckHash != hashToken(lease.Token) {
		t.Fatalf("CancellationRequested() = %v, %v", requested, err)
	}
	job, err := service.ConfirmCancellation(context.Background(), lease)
	if err != nil || job.State != "canceled" || repository.cancelConfirmHash != hashToken(lease.Token) {
		t.Fatalf("ConfirmCancellation() = %#v, %v", job, err)
	}
}

type fakeRepository struct {
	claimInput           jobpostgres.ClaimInput
	heartbeatTokenHash   string
	refineInput          jobpostgres.RefineWorkloadInput
	finishInput          jobpostgres.FinishInput
	recoveryNow          time.Time
	recoveryRetryAt      time.Time
	recoveryLimit        int
	subworkInput         jobpostgres.SubworkAllocationInput
	subworkHeartbeatHash string
	subworkReleaseHash   string
	cancelJobID          string
	cancelNow            time.Time
	cancelCheckHash      string
	cancelConfirmHash    string
	interruptNow         time.Time
	interruptLimit       int
}

func (repository *fakeRepository) AcquireLeadership(context.Context) (*jobpostgres.Leadership, error) {
	return nil, errors.New("unexpected leadership acquisition")
}

func (repository *fakeRepository) AcquireResourceLeadership(context.Context, string) (*jobpostgres.Leadership, error) {
	return nil, errors.New("unexpected resource leadership acquisition")
}

func (repository *fakeRepository) ClaimNext(_ context.Context, _ *jobpostgres.Leadership, input jobpostgres.ClaimInput) (jobpostgres.Lease, error) {
	repository.claimInput = input
	return jobpostgres.Lease{
		Job: jobpostgres.Job{ID: "job-a", State: "running"}, AttemptID: input.AttemptID,
		AttemptNo: 1, WorkerID: input.WorkerID, LeaseExpiresAt: input.LeaseExpiresAt,
	}, nil
}

func (repository *fakeRepository) Heartbeat(_ context.Context, _ string, tokenHash string, _ time.Time, _ time.Time) error {
	repository.heartbeatTokenHash = tokenHash
	return nil
}

func (repository *fakeRepository) RefineWorkload(_ context.Context, input jobpostgres.RefineWorkloadInput) (jobpostgres.WorkloadRecord, error) {
	repository.refineInput = input
	return jobpostgres.WorkloadRecord{ProfileKey: "test"}, nil
}

func (repository *fakeRepository) FinishAttempt(_ context.Context, input jobpostgres.FinishInput) (jobpostgres.Job, error) {
	repository.finishInput = input
	state := "queued"
	if input.Succeeded {
		state = "succeeded"
	}
	return jobpostgres.Job{ID: "job-a", State: state}, nil
}

func (repository *fakeRepository) RecoverExpiredLeases(_ context.Context, _ *jobpostgres.Leadership, now time.Time, retryAt time.Time, limit int) (int, error) {
	repository.recoveryNow = now
	repository.recoveryRetryAt = retryAt
	repository.recoveryLimit = limit
	return 2, nil
}

func (repository *fakeRepository) AcquireSubwork(_ context.Context, input jobpostgres.SubworkAllocationInput) (jobpostgres.SubworkAllocation, error) {
	repository.subworkInput = input
	return jobpostgres.SubworkAllocation{
		ID: input.AllocationID, ResourceClass: input.ResourceClass, JobID: input.JobID,
		WorkKey: input.WorkKey, OwnerID: input.OwnerID, LeaseExpiresAt: input.LeaseExpiresAt,
	}, nil
}

func (repository *fakeRepository) HeartbeatSubwork(_ context.Context, _ string, tokenHash string, _ time.Time, _ time.Time) error {
	repository.subworkHeartbeatHash = tokenHash
	return nil
}

func (repository *fakeRepository) ReleaseSubwork(_ context.Context, _ string, tokenHash string) error {
	repository.subworkReleaseHash = tokenHash
	return nil
}

func (repository *fakeRepository) RequestCancellation(_ context.Context, jobID string, now time.Time) (jobpostgres.Cancellation, error) {
	repository.cancelJobID, repository.cancelNow = jobID, now
	return jobpostgres.Cancellation{Job: jobpostgres.Job{ID: jobID, State: "running"}, WorkerMustStop: true}, nil
}

func (repository *fakeRepository) CancellationRequested(_ context.Context, _ string, tokenHash string, _ time.Time) (bool, error) {
	repository.cancelCheckHash = tokenHash
	return true, nil
}

func (repository *fakeRepository) ConfirmCancellation(_ context.Context, _ string, tokenHash string, _ time.Time) (jobpostgres.Job, error) {
	repository.cancelConfirmHash = tokenHash
	return jobpostgres.Job{ID: "job-a", State: "canceled"}, nil
}

func (repository *fakeRepository) InterruptActiveLeases(_ context.Context, _ *jobpostgres.Leadership, now time.Time, limit int) (int, error) {
	repository.interruptNow, repository.interruptLimit = now, limit
	return 2, nil
}

func newTestService(t *testing.T, repository Repository) *Service {
	t.Helper()
	nowCalls := 0
	service, err := New(repository, Config{
		LeaseDuration: 60 * time.Second,
		RetryBackoff:  15 * time.Second,
		RecoveryLimit: 10,
		Policy: jobpostgres.SchedulingPolicy{
			Resources: jobpostgres.ResourceCapacities{
				DocumentLight: 2, DocumentLaTeXML: 1, MathNode: 2, TeXSVG: 2, BatchMigration: 1, LatexPDF: 1,
				DocumentTypst: 1, TypstPDF: 1, Heavy: 5,
			},
			Weights:          jobpostgres.PriorityWeights{Publish: 8, Preview: 5, Rebuild: 3, Migration: 1},
			AgingInterval:    5 * time.Minute,
			MaxAgingSteps:    12,
			PrincipalRunning: 2,
		},
		Now: func() time.Time {
			value := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC).Add(time.Duration(nowCalls) * time.Second)
			nowCalls++
			return value
		},
		Random: bytes.NewReader(make([]byte, 256)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestHashTokenIsStableAndNonIdentity(t *testing.T) {
	token := strings.Repeat("a", 64)
	if hashToken(token) == token || hashToken(token) != hashToken(token) {
		t.Fatal("lease-token hashing is not stable and one-way")
	}
}
