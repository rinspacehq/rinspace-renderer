package admission

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

const (
	CodeNotReady            = "renderer_not_ready"
	CodePrincipalQuota      = "principal_queue_quota"
	CodeQueueCapacity       = "renderer_queue_capacity"
	CodeStorageCapacity     = "renderer_source_capacity"
	CodeSourceTooLarge      = "source_too_large"
	CodeInvalidSource       = "invalid_source"
	CodePriorityForbidden   = "priority_forbidden"
	CodeIdempotencyConflict = "idempotency_conflict"
	CodeAdmissionPending    = "admission_pending"
	CodeAdmissionFailed     = "admission_failed"
)

type Error struct {
	StatusCode int
	Code       string
	RetryAfter time.Duration
	Err        error
}

func (err *Error) Error() string {
	if err.Err == nil {
		return err.Code
	}
	return err.Code + ": " + err.Err.Error()
}

func (err *Error) Unwrap() error { return err.Err }

type Readiness interface {
	Ready(context.Context) error
}

type ReadinessFunc func(context.Context) error

func (function ReadinessFunc) Ready(ctx context.Context) error { return function(ctx) }

type Repository interface {
	ReserveAdmission(context.Context, jobpostgres.AdmissionReservationInput, jobpostgres.AdmissionLimits) (jobpostgres.AdmissionReservation, error)
	CommitAdmission(context.Context, jobpostgres.CommitAdmissionInput) (jobpostgres.Job, error)
	AbortAdmission(context.Context, string, string) error
}

type Config struct {
	Limits         jobpostgres.AdmissionLimits
	MaxSourceBytes int64
	ReservationTTL time.Duration
	SourceTTL      time.Duration
	JobTTL         time.Duration
	IdempotencyTTL time.Duration
	RetryAfter     time.Duration
	Now            func() time.Time
	Random         io.Reader
}

type Service struct {
	repository Repository
	artifacts  orchestration.ArtifactStore
	readiness  Readiness
	priorities PriorityAssigner
	config     Config
}

type Request struct {
	PrincipalID          string
	OwnerScope           string
	ContentKind          string
	DocumentEngine       string
	ResourceClass        string
	PriorityIntent       string
	ProjectHash          string
	OptionsHash          string
	RendererVersion      string
	RequestMetadata      json.RawMessage
	MaxAttempts          int16
	IdempotencyKey       string
	ExpectedSourceSHA256 string
	DeclaredSourceBytes  *int64
	Source               io.Reader
	SourceMediaType      string
	SourceSchemaVersion  string
}

type Result struct {
	Job    jobpostgres.Job
	Reused bool
}

func New(repository Repository, artifacts orchestration.ArtifactStore, readiness Readiness, priorities PriorityAssigner, config Config) (*Service, error) {
	if repository == nil || artifacts == nil || readiness == nil || priorities == nil {
		return nil, errors.New("admission dependencies are required")
	}
	if config.Limits.GlobalNonTerminal <= 0 || config.Limits.PrincipalQueued <= 0 || config.Limits.PreviewQueued <= 0 ||
		config.Limits.PrivateSourceBytes <= 0 || config.MaxSourceBytes <= 0 ||
		config.ReservationTTL <= 0 || config.SourceTTL <= 0 || config.JobTTL <= 0 ||
		config.IdempotencyTTL <= 0 || config.RetryAfter <= 0 {
		return nil, errors.New("admission configuration must be positive")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &Service{repository: repository, artifacts: artifacts, readiness: readiness, priorities: priorities, config: config}, nil
}

func (service *Service) Admit(ctx context.Context, request Request) (result Result, returnedErr error) {
	started := time.Now()
	defer func() {
		metrics := operational.Default()
		if returnedErr != nil {
			code := CodeAdmissionFailed
			var admissionError *Error
			if errors.As(returnedErr, &admissionError) {
				code = admissionError.Code
			}
			metrics.Count("admission_rejections_total", code)
			metrics.Event("admission_rejected", slog.String("content_kind", request.ContentKind),
				slog.String("engine", request.DocumentEngine), slog.String("resource_class", request.ResourceClass),
				slog.String("failure_code", code), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
			return
		}
		metrics.Count("admissions_total", request.ContentKind, request.ResourceClass)
		metrics.Event("job_admitted", slog.String("request_id", result.Job.ID), slog.String("job_id", result.Job.ID),
			slog.String("content_kind", request.ContentKind), slog.String("engine", request.DocumentEngine),
			slog.String("resource_class", request.ResourceClass), slog.String("priority_class", result.Job.PriorityClass),
			slog.String("renderer_version", request.RendererVersion), slog.Int64("input_bytes", result.Job.DeclaredSourceBytes),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()))
	}()
	if err := validateRequest(request); err != nil {
		return Result{}, rejection(400, CodeInvalidSource, 0, err)
	}
	declaredBytes := service.config.MaxSourceBytes
	if request.DeclaredSourceBytes != nil {
		if *request.DeclaredSourceBytes < 0 {
			return Result{}, rejection(400, CodeInvalidSource, 0, errors.New("declared source bytes cannot be negative"))
		}
		if *request.DeclaredSourceBytes > service.config.MaxSourceBytes {
			return Result{}, rejection(413, CodeSourceTooLarge, 0, jobpostgres.ErrStorageCapacity)
		}
		declaredBytes = *request.DeclaredSourceBytes
	}
	if err := service.readiness.Ready(ctx); err != nil {
		return Result{}, rejection(503, CodeNotReady, service.config.RetryAfter, err)
	}
	priorityClass, err := service.priorities.AssignPriority(ctx, request.PrincipalID, request.PriorityIntent)
	if err != nil {
		return Result{}, rejection(403, CodePriorityForbidden, 0, err)
	}

	now := service.config.Now().UTC().Truncate(time.Second)
	jobID, err := randomUUID(service.config.Random)
	if err != nil {
		return Result{}, rejection(503, CodeAdmissionFailed, service.config.RetryAfter, err)
	}
	token, err := randomBytes(service.config.Random, 32)
	if err != nil {
		return Result{}, rejection(503, CodeAdmissionFailed, service.config.RetryAfter, err)
	}
	tokenHash := sha256Hex(token)
	workload, err := jobpostgres.AdmissionWorkload(jobpostgres.WorkloadAdmission{
		ContentKind: request.ContentKind, DocumentEngine: request.DocumentEngine,
		ResourceClass: request.ResourceClass, PriorityClass: priorityClass, ProjectBytes: declaredBytes,
	})
	if err != nil {
		return Result{}, rejection(400, CodeInvalidSource, 0, err)
	}
	reservation, err := service.repository.ReserveAdmission(ctx, jobpostgres.AdmissionReservationInput{
		Job: jobpostgres.Job{
			ID:              jobID,
			PrincipalID:     request.PrincipalID,
			OwnerScope:      request.OwnerScope,
			ContentKind:     request.ContentKind,
			DocumentEngine:  request.DocumentEngine,
			ResourceClass:   request.ResourceClass,
			PriorityClass:   priorityClass,
			ProjectHash:     request.ProjectHash,
			OptionsHash:     request.OptionsHash,
			RendererVersion: request.RendererVersion,
			RequestMetadata: append(json.RawMessage(nil), request.RequestMetadata...),
			MaxAttempts:     request.MaxAttempts,
			ExpiresAt:       now.Add(service.config.JobTTL),
		},
		Workload:             workload,
		DeclaredSourceBytes:  declaredBytes,
		AdmissionTokenHash:   tokenHash,
		IdempotencyKey:       request.IdempotencyKey,
		RequestHash:          requestHash(request, priorityClass),
		IdempotencyExpiresAt: now.Add(service.config.IdempotencyTTL),
		ReservationExpiresAt: now.Add(service.config.ReservationTTL),
		Now:                  now,
	}, service.config.Limits)
	if err != nil {
		return Result{}, mapRepositoryError(err, service.config.RetryAfter)
	}
	if reservation.Reused {
		if reservation.Uploading {
			return Result{}, rejection(503, CodeAdmissionPending, service.config.RetryAfter, errors.New("matching admission is still uploading"))
		}
		return Result{Job: reservation.Job, Reused: true}, nil
	}

	abort := func() {
		_ = service.repository.AbortAdmission(context.WithoutCancel(ctx), reservation.Job.ID, tokenHash)
	}
	body, readErr := io.ReadAll(io.LimitReader(request.Source, service.config.MaxSourceBytes+1))
	if readErr != nil {
		abort()
		return Result{}, rejection(400, CodeInvalidSource, 0, fmt.Errorf("read source: %w", readErr))
	}
	if int64(len(body)) > service.config.MaxSourceBytes {
		abort()
		return Result{}, rejection(413, CodeSourceTooLarge, 0, jobpostgres.ErrStorageCapacity)
	}
	if request.DeclaredSourceBytes != nil && int64(len(body)) != *request.DeclaredSourceBytes {
		abort()
		return Result{}, rejection(400, CodeInvalidSource, 0, errors.New("streamed source bytes do not match declaration"))
	}
	digest := sha256Hex(body)
	if digest != request.ExpectedSourceSHA256 {
		abort()
		return Result{}, rejection(400, CodeInvalidSource, 0, errors.New("streamed source hash does not match declaration"))
	}
	observation := observeAdmissionSource(body, request.ContentKind, request.RequestMetadata)
	workload, err = jobpostgres.AdmissionWorkload(jobpostgres.WorkloadAdmission{
		ContentKind: request.ContentKind, DocumentEngine: request.DocumentEngine,
		ResourceClass: request.ResourceClass, PriorityClass: priorityClass,
		ProjectBytes: int64(len(body)), FileCount: observation.FileCount,
		DocumentClass: observation.DocumentClass,
	})
	if err != nil {
		abort()
		return Result{}, rejection(400, CodeInvalidSource, 0, err)
	}
	expiresAt := now.Add(service.config.SourceTTL)
	artifactID := "render-jobs/v1/source/" + reservation.Job.ID + "/" + digest
	reference, err := service.artifacts.PutPrivate(ctx, orchestration.ArtifactWrite{
		ArtifactID:    artifactID,
		Body:          body,
		MediaType:     request.SourceMediaType,
		SchemaVersion: request.SourceSchemaVersion,
		ExpiresAt:     &expiresAt,
	})
	if err != nil {
		abort()
		return Result{}, rejection(503, CodeAdmissionFailed, service.config.RetryAfter, fmt.Errorf("store source artifact: %w", err))
	}
	job, err := service.repository.CommitAdmission(ctx, jobpostgres.CommitAdmissionInput{
		JobID:              reservation.Job.ID,
		AdmissionTokenHash: tokenHash,
		Artifact: jobpostgres.Artifact{
			ID:            reference.ArtifactID,
			SHA256:        reference.SHA256,
			Kind:          "source",
			Visibility:    reference.Visibility,
			StorageKey:    reference.ArtifactID,
			ByteSize:      reference.Bytes,
			MediaType:     reference.MediaType,
			SchemaVersion: request.SourceSchemaVersion,
			ExpiresAt:     &expiresAt,
		},
		Workload:          workload,
		QueuedAt:          now,
		PreviewQueueLimit: service.config.Limits.PreviewQueued,
	})
	if err != nil {
		_ = service.artifacts.DeletePrivate(context.WithoutCancel(ctx), reference)
		abort()
		return Result{}, mapRepositoryError(fmt.Errorf("commit queued job: %w", err), service.config.RetryAfter)
	}
	return Result{Job: job}, nil
}

func validateRequest(request Request) error {
	if strings.TrimSpace(request.PrincipalID) == "" || strings.TrimSpace(request.ContentKind) == "" ||
		strings.TrimSpace(request.DocumentEngine) == "" || strings.TrimSpace(request.ResourceClass) == "" ||
		strings.TrimSpace(request.RendererVersion) == "" ||
		strings.TrimSpace(request.SourceMediaType) == "" || request.Source == nil || request.MaxAttempts <= 0 {
		return errors.New("admission request is incomplete")
	}
	if len(request.RequestMetadata) > 64<<10 || len(request.RequestMetadata) > 0 && (!json.Valid(request.RequestMetadata) || request.RequestMetadata[0] != '{') {
		return errors.New("admission request metadata must be a bounded JSON object")
	}
	for _, digest := range []string{request.ProjectHash, request.OptionsHash, request.ExpectedSourceSHA256} {
		if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
			return errors.New("admission request contains an invalid SHA-256")
		}
	}
	return nil
}

func mapRepositoryError(err error, retryAfter time.Duration) error {
	switch {
	case errors.Is(err, jobpostgres.ErrPrincipalQuota):
		return rejection(429, CodePrincipalQuota, retryAfter, err)
	case errors.Is(err, jobpostgres.ErrGlobalCapacity), errors.Is(err, jobpostgres.ErrPreviewQueueCapacity):
		return rejection(503, CodeQueueCapacity, retryAfter, err)
	case errors.Is(err, jobpostgres.ErrStorageCapacity):
		return rejection(503, CodeStorageCapacity, retryAfter, err)
	case errors.Is(err, jobpostgres.ErrIdempotencyConflict):
		return rejection(409, CodeIdempotencyConflict, 0, err)
	default:
		return rejection(503, CodeAdmissionFailed, retryAfter, err)
	}
}

func rejection(status int, code string, retryAfter time.Duration, err error) *Error {
	return &Error{StatusCode: status, Code: code, RetryAfter: retryAfter, Err: err}
}

func requestHash(request Request, priorityClass string) string {
	metadata := string(request.RequestMetadata)
	if jobpostgres.IsPreviewResourceClass(request.ResourceClass) {
		// The protocol idempotency identity intentionally excludes draftRevision:
		// unchanged content at a later journal revision must reuse the same result.
		// Entrypoint/policy/image are already bound by OptionsHash and owner context.
		metadata = ""
	}
	fields := []string{
		request.PrincipalID, request.OwnerScope, request.ContentKind, request.DocumentEngine,
		request.ResourceClass, priorityClass, request.ProjectHash, request.OptionsHash,
		request.RendererVersion, fmt.Sprintf("%d", request.MaxAttempts), request.ExpectedSourceSHA256,
		request.SourceMediaType, request.SourceSchemaVersion, metadata,
	}
	digest := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(digest[:])
}

func randomUUID(reader io.Reader) (string, error) {
	body, err := randomBytes(reader, 16)
	if err != nil {
		return "", err
	}
	body[6] = (body[6] & 0x0f) | 0x40
	body[8] = (body[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(body)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func randomBytes(reader io.Reader, size int) ([]byte, error) {
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("generate admission secret: %w", err)
	}
	return body, nil
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
