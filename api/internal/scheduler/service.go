package scheduler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
)

type Repository interface {
	AcquireLeadership(context.Context) (*jobpostgres.Leadership, error)
	AcquireResourceLeadership(context.Context, string) (*jobpostgres.Leadership, error)
	ClaimNext(context.Context, *jobpostgres.Leadership, jobpostgres.ClaimInput) (jobpostgres.Lease, error)
	Heartbeat(context.Context, string, string, time.Time, time.Time) error
	RefineWorkload(context.Context, jobpostgres.RefineWorkloadInput) (jobpostgres.WorkloadRecord, error)
	FinishAttempt(context.Context, jobpostgres.FinishInput) (jobpostgres.Job, error)
	RecoverExpiredLeases(context.Context, *jobpostgres.Leadership, time.Time, time.Time, int) (int, error)
	AcquireSubwork(context.Context, jobpostgres.SubworkAllocationInput) (jobpostgres.SubworkAllocation, error)
	HeartbeatSubwork(context.Context, string, string, time.Time, time.Time) error
	ReleaseSubwork(context.Context, string, string) error
	RequestCancellation(context.Context, string, time.Time) (jobpostgres.Cancellation, error)
	CancellationRequested(context.Context, string, string, time.Time) (bool, error)
	ConfirmCancellation(context.Context, string, string, time.Time) (jobpostgres.Job, error)
	InterruptActiveLeases(context.Context, *jobpostgres.Leadership, time.Time, int) (int, error)
}

type Config struct {
	LeaseDuration      time.Duration
	RetryBackoff       time.Duration
	RecoveryLimit      int
	Policy             jobpostgres.SchedulingPolicy
	ClaimResourceClass string
	Now                func() time.Time
	Random             io.Reader
}

type Service struct {
	repository Repository
	config     Config
}

type Lease struct {
	Job            jobpostgres.Job
	AttemptID      string
	AttemptNo      int16
	WorkerID       string
	Token          string
	LeaseExpiresAt time.Time
}

type SubworkLease struct {
	Allocation jobpostgres.SubworkAllocation
	Token      string
}

func New(repository Repository, config Config) (*Service, error) {
	if repository == nil {
		return nil, errors.New("scheduler repository is required")
	}
	if config.LeaseDuration <= 0 || config.RetryBackoff <= 0 || config.RecoveryLimit <= 0 {
		return nil, errors.New("scheduler configuration must be positive")
	}
	if err := config.Policy.Validate(); err != nil {
		return nil, err
	}
	if config.ClaimResourceClass != "" && !jobpostgres.IsDedicatedWorkerResourceClass(config.ClaimResourceClass) {
		return nil, errors.New("scheduler claim resource class is invalid")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &Service{repository: repository, config: config}, nil
}

func (service *Service) AcquireLeadership(ctx context.Context) (*jobpostgres.Leadership, error) {
	if service.config.ClaimResourceClass != "" {
		return service.repository.AcquireResourceLeadership(ctx, service.config.ClaimResourceClass)
	}
	return service.repository.AcquireLeadership(ctx)
}

func (service *Service) Claim(ctx context.Context, leadership *jobpostgres.Leadership, workerID string) (Lease, error) {
	if workerID == "" {
		return Lease{}, errors.New("scheduler worker ID is required")
	}
	attemptID, err := randomUUID(service.config.Random)
	if err != nil {
		return Lease{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := io.ReadFull(service.config.Random, tokenBytes); err != nil {
		return Lease{}, fmt.Errorf("generate renderer lease token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	now := service.config.Now().UTC().Truncate(time.Second)
	leaseExpiresAt := now.Add(service.config.LeaseDuration)
	databaseLease, err := service.repository.ClaimNext(ctx, leadership, jobpostgres.ClaimInput{
		AttemptID:      attemptID,
		WorkerID:       workerID,
		LeaseTokenHash: hashToken(token),
		Now:            now,
		LeaseExpiresAt: leaseExpiresAt,
		Policy:         service.config.Policy,
		ResourceClass:  service.config.ClaimResourceClass,
	})
	if err != nil {
		return Lease{}, err
	}
	return Lease{
		Job: databaseLease.Job, AttemptID: databaseLease.AttemptID,
		AttemptNo: databaseLease.AttemptNo, WorkerID: databaseLease.WorkerID,
		Token: token, LeaseExpiresAt: databaseLease.LeaseExpiresAt,
	}, nil
}

func (service *Service) Heartbeat(ctx context.Context, lease Lease) (Lease, error) {
	now := service.config.Now().UTC().Truncate(time.Second)
	leaseExpiresAt := now.Add(service.config.LeaseDuration)
	if err := service.repository.Heartbeat(ctx, lease.AttemptID, hashToken(lease.Token), now, leaseExpiresAt); err != nil {
		return Lease{}, err
	}
	lease.LeaseExpiresAt = leaseExpiresAt
	return lease, nil
}

func (service *Service) RefineWorkload(ctx context.Context, lease Lease, refinement jobpostgres.WorkloadRefinement) (jobpostgres.WorkloadRecord, error) {
	now := service.config.Now().UTC().Truncate(time.Second)
	return service.repository.RefineWorkload(ctx, jobpostgres.RefineWorkloadInput{
		AttemptID: lease.AttemptID, LeaseTokenHash: hashToken(lease.Token), Now: now,
		Refinement: refinement,
	})
}

func (service *Service) Succeed(ctx context.Context, lease Lease) (jobpostgres.Job, error) {
	now := service.config.Now().UTC().Truncate(time.Second)
	return service.repository.FinishAttempt(ctx, jobpostgres.FinishInput{
		AttemptID: lease.AttemptID, LeaseTokenHash: hashToken(lease.Token),
		Now: now, Succeeded: true,
	})
}

func (service *Service) SucceedWithResult(ctx context.Context, lease Lease, artifact jobpostgres.Artifact, evidence jobpostgres.CompletionResultEvidence) (jobpostgres.Job, error) {
	now := service.config.Now().UTC().Truncate(time.Second)
	return service.repository.FinishAttempt(ctx, jobpostgres.FinishInput{
		AttemptID: lease.AttemptID, LeaseTokenHash: hashToken(lease.Token),
		Now: now, Succeeded: true, ResultArtifact: &artifact, CompletionResult: &evidence,
	})
}

func (service *Service) Fail(ctx context.Context, lease Lease, retryable bool, errorCode string) (jobpostgres.Job, error) {
	return service.fail(ctx, lease, retryable, errorCode, nil)
}

func (service *Service) FailWithResult(ctx context.Context, lease Lease, retryable bool, errorCode string, artifact jobpostgres.Artifact) (jobpostgres.Job, error) {
	return service.fail(ctx, lease, retryable, errorCode, &artifact)
}

func (service *Service) fail(ctx context.Context, lease Lease, retryable bool, errorCode string, artifact *jobpostgres.Artifact) (jobpostgres.Job, error) {
	if errorCode == "" {
		return jobpostgres.Job{}, errors.New("scheduler failure code is required")
	}
	now := service.config.Now().UTC().Truncate(time.Second)
	return service.repository.FinishAttempt(ctx, jobpostgres.FinishInput{
		AttemptID: lease.AttemptID, LeaseTokenHash: hashToken(lease.Token), Now: now,
		Retryable: retryable, ErrorCode: errorCode,
		RetryAvailableAt: now.Add(service.config.RetryBackoff),
		ResultArtifact:   artifact,
	})
}

func (service *Service) Recover(ctx context.Context, leadership *jobpostgres.Leadership) (int, error) {
	now := service.config.Now().UTC().Truncate(time.Second)
	return service.repository.RecoverExpiredLeases(
		ctx, leadership, now, now.Add(service.config.RetryBackoff), service.config.RecoveryLimit,
	)
}

func (service *Service) InterruptAndRecover(ctx context.Context, leadership *jobpostgres.Leadership) (int, error) {
	now := service.config.Now().UTC().Truncate(time.Second)
	interrupted, err := service.repository.InterruptActiveLeases(ctx, leadership, now, service.config.RecoveryLimit)
	if err != nil || interrupted == 0 {
		return interrupted, err
	}
	_, err = service.repository.RecoverExpiredLeases(
		ctx, leadership, now, now.Add(service.config.RetryBackoff), service.config.RecoveryLimit,
	)
	return interrupted, err
}

func (service *Service) RequestCancellation(ctx context.Context, jobID string) (jobpostgres.Cancellation, error) {
	return service.repository.RequestCancellation(ctx, jobID, service.config.Now().UTC().Truncate(time.Second))
}

func (service *Service) CancellationRequested(ctx context.Context, lease Lease) (bool, error) {
	return service.repository.CancellationRequested(
		ctx, lease.AttemptID, hashToken(lease.Token), service.config.Now().UTC().Truncate(time.Second),
	)
}

func (service *Service) ConfirmCancellation(ctx context.Context, lease Lease) (jobpostgres.Job, error) {
	return service.repository.ConfirmCancellation(
		ctx, lease.AttemptID, hashToken(lease.Token), service.config.Now().UTC().Truncate(time.Second),
	)
}

func (service *Service) AcquireSubwork(ctx context.Context, resourceClass string, jobID string, workKey string, ownerID string) (SubworkLease, error) {
	allocationID, err := randomUUID(service.config.Random)
	if err != nil {
		return SubworkLease{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := io.ReadFull(service.config.Random, tokenBytes); err != nil {
		return SubworkLease{}, fmt.Errorf("generate renderer subwork token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	now := service.config.Now().UTC().Truncate(time.Second)
	allocation, err := service.repository.AcquireSubwork(ctx, jobpostgres.SubworkAllocationInput{
		AllocationID: allocationID, ResourceClass: resourceClass, JobID: jobID,
		WorkKey: workKey, OwnerID: ownerID, LeaseTokenHash: hashToken(token), Now: now,
		LeaseExpiresAt: now.Add(service.config.LeaseDuration), Capacities: service.config.Policy.Resources,
	})
	if err != nil {
		return SubworkLease{}, err
	}
	return SubworkLease{Allocation: allocation, Token: token}, nil
}

func (service *Service) HeartbeatSubwork(ctx context.Context, lease SubworkLease) (SubworkLease, error) {
	now := service.config.Now().UTC().Truncate(time.Second)
	expiresAt := now.Add(service.config.LeaseDuration)
	if err := service.repository.HeartbeatSubwork(ctx, lease.Allocation.ID, hashToken(lease.Token), now, expiresAt); err != nil {
		return SubworkLease{}, err
	}
	lease.Allocation.LeaseExpiresAt = expiresAt
	return lease, nil
}

func (service *Service) ReleaseSubwork(ctx context.Context, lease SubworkLease) error {
	return service.repository.ReleaseSubwork(ctx, lease.Allocation.ID, hashToken(lease.Token))
}

func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func randomUUID(reader io.Reader) (string, error) {
	body := make([]byte, 16)
	if _, err := io.ReadFull(reader, body); err != nil {
		return "", fmt.Errorf("generate renderer attempt ID: %w", err)
	}
	body[6] = (body[6] & 0x0f) | 0x40
	body[8] = (body[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(body)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
