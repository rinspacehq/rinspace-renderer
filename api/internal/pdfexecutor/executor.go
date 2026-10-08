package pdfexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

const fixedPDFWallTimeout = 90 * time.Second

type Repository interface {
	Artifact(context.Context, string) (jobpostgres.Artifact, error)
	CreateArtifact(context.Context, jobpostgres.Artifact) (jobpostgres.Artifact, error)
}

type Store interface {
	GetPrivate(context.Context, contracts.ArtifactReference) ([]byte, error)
	PutPrivate(context.Context, orchestration.ArtifactWrite) (contracts.ArtifactReference, error)
}

type Config struct {
	SpoolRoot       string
	RendererVersion string
	PollInterval    time.Duration
	WallTimeout     time.Duration
	Now             func() time.Time
}

type Failure struct {
	Code        string
	Status      int
	Message     string
	ExitCode    *int
	Diagnostics []renderapi.Diagnostic
	Artifacts   []contracts.ArtifactReference
	Retryable   bool
}

type Outcome struct {
	ResultReference contracts.ArtifactReference
	ResultArtifact  jobpostgres.Artifact
	Evidence        jobpostgres.CompletionResultEvidence
	Failure         *Failure
}

type Executor struct {
	repository Repository
	store      Store
	broker     Broker
	config     Config
}

type requestMetadata struct {
	RequestID              string `json:"requestId"`
	Entrypoint             string `json:"entrypoint"`
	OutputKind             string `json:"outputKind"`
	SnapshotHash           string `json:"snapshotHash"`
	SessionID              string `json:"sessionId"`
	DraftRevision          int64  `json:"draftRevision"`
	EnginePolicyID         string `json:"enginePolicyId"`
	ImageDigest            string `json:"imageDigest"`
	PreviewContractVersion string `json:"previewContractVersion"`
}

func New(repository Repository, store Store, broker Broker, config Config) (*Executor, error) {
	if repository == nil || store == nil || broker == nil || strings.TrimSpace(config.SpoolRoot) == "" ||
		strings.TrimSpace(config.RendererVersion) == "" || config.PollInterval <= 0 ||
		config.WallTimeout <= 0 || config.WallTimeout > fixedPDFWallTimeout {
		return nil, errors.New("PDF executor configuration is invalid")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Executor{repository: repository, store: store, broker: broker, config: config}, nil
}

func (executor *Executor) Execute(ctx context.Context, job jobpostgres.Job) Outcome {
	metadata, failure := executor.validateJob(job)
	if failure != nil {
		return Outcome{Failure: failure}
	}
	source, failure := executor.source(ctx, job)
	if failure != nil {
		return Outcome{Failure: failure}
	}
	if failure := executor.recoverStale(ctx, job.ID); failure != nil {
		return Outcome{Failure: failure}
	}
	spool, err := prepareSpool(executor.config.SpoolRoot, job.ID, metadata.Entrypoint, source, metadata.SnapshotHash)
	if err != nil {
		return Outcome{Failure: invalidFailure("invalid_pdf_snapshot", "PDF source snapshot is invalid")}
	}
	started := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if started {
			_, _ = executor.broker.Stop(cleanupCtx, job.ID)
		}
		_, _ = executor.broker.Remove(cleanupCtx, job.ID)
		_ = cleanupSpool(executor.config.SpoolRoot, job.ID)
	}()

	inspection, err := executor.broker.Start(ctx, job.ID)
	if err != nil {
		return Outcome{Failure: retryableFailure("pdf_broker_unavailable", "PDF compiler could not be started")}
	}
	started = true
	if !matchesCompilerImage(inspection.Image, metadata.ImageDigest) {
		return Outcome{Failure: invalidFailure("pdf_image_mismatch", "PDF compiler image does not match the admitted digest")}
	}

	runCtx, cancel := context.WithTimeout(ctx, executor.config.WallTimeout)
	defer cancel()
	inspection, failure = executor.waitForCompletion(runCtx, job.ID, metadata.ImageDigest, inspection)
	if failure != nil {
		collectPartial := false
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			_, stopErr := executor.broker.Stop(context.WithoutCancel(ctx), job.ID)
			failure = &Failure{Code: "pdf_compile_timeout", Status: 504, Message: "PDF compilation exceeded 90 seconds", Retryable: true, Diagnostics: []renderapi.Diagnostic{}}
			collectPartial = stopErr == nil
		} else if ctx.Err() != nil {
			_, stopErr := executor.broker.Stop(context.WithoutCancel(ctx), job.ID)
			failure = retryableFailure("pdf_worker_interrupted", "PDF compilation was interrupted")
			collectPartial = stopErr == nil
		}
		if collectPartial {
			artifactCtx, artifactCancel := context.WithTimeout(context.Background(), 10*time.Second)
			artifacts, diagnostics, _ := executor.collectAndStore(artifactCtx, job, metadata, spool.output, false)
			artifactCancel()
			failure.Artifacts = artifactReferences(artifacts)
			failure.Diagnostics = diagnostics
		}
		return Outcome{Failure: failure}
	}

	artifacts, diagnostics, artifactFailure := executor.collectAndStore(ctx, job, metadata, spool.output, inspection.ExitCode == 0 && !inspection.OOMKilled)
	if artifactFailure != nil && artifactFailure.Code == "unsafe_pdf_quarantined" {
		artifactFailure.Diagnostics = diagnostics
		artifactFailure.Artifacts = artifactReferences(artifacts)
		return Outcome{Failure: artifactFailure}
	}
	if inspection.OOMKilled {
		exitCode := inspection.ExitCode
		return Outcome{Failure: &Failure{Code: "pdf_compile_oom", Status: 503, Message: "PDF compilation exceeded its memory limit", ExitCode: &exitCode, Retryable: true, Diagnostics: diagnostics, Artifacts: artifactReferences(artifacts)}}
	}
	if inspection.ExitCode != 0 {
		exitCode := inspection.ExitCode
		return Outcome{Failure: &Failure{Code: "pdf_compile_failed", Status: 422, Message: "LaTeX compilation failed", ExitCode: &exitCode, Diagnostics: diagnostics, Artifacts: artifactReferences(artifacts)}}
	}
	if artifactFailure != nil {
		artifactFailure.Diagnostics = diagnostics
		artifactFailure.Artifacts = artifactReferences(artifacts)
		return Outcome{Failure: artifactFailure}
	}
	return executor.storeResult(ctx, job, metadata, artifacts, diagnostics)
}

func (executor *Executor) validateJob(job jobpostgres.Job) (requestMetadata, *Failure) {
	var metadata requestMetadata
	if !jobIDPattern.MatchString(job.ID) || job.ContentKind != "latex" || job.DocumentEngine != "latexmk" ||
		job.ResourceClass != "latex-pdf" || job.PriorityClass != "preview" || job.RendererVersion != executor.config.RendererVersion ||
		len(job.RequestMetadata) == 0 || json.Unmarshal(job.RequestMetadata, &metadata) != nil || job.ProjectHash != metadata.SnapshotHash {
		return requestMetadata{}, invalidFailure("invalid_pdf_job", "PDF job contract is invalid")
	}
	if metadata.OutputKind != "latex-pdf-preview" || metadata.PreviewContractVersion != PreviewSchemaVersion ||
		!sha256Pattern.MatchString(metadata.SnapshotHash) || !sessionPattern.MatchString(metadata.SessionID) ||
		metadata.DraftRevision < 0 || metadata.EnginePolicyID != PolicyID ||
		!imageDigestPattern.MatchString(metadata.ImageDigest) || !canonicalEntrypoint(metadata.Entrypoint) ||
		!sha256Pattern.MatchString(job.OptionsHash) {
		return requestMetadata{}, invalidFailure("invalid_pdf_job", "PDF job identity is invalid")
	}
	return metadata, nil
}

func (executor *Executor) source(ctx context.Context, job jobpostgres.Job) ([]byte, *Failure) {
	artifact, err := executor.repository.Artifact(ctx, job.SourceArtifactID)
	if err != nil || artifact.Kind != "source" || artifact.Visibility != "private" || artifact.ExpiresAt == nil ||
		artifact.MediaType != "application/x-tar" || artifact.SchemaVersion != SnapshotSchemaVersion ||
		!artifact.ExpiresAt.After(executor.config.Now()) || artifact.ByteSize <= 0 || artifact.ByteSize > snapshotTotalBytes+snapshotFileLimit*512+1024 {
		return nil, retryableFailure("pdf_source_unavailable", "PDF source snapshot is unavailable")
	}
	reference := contracts.ArtifactReference{
		ArtifactID: artifact.StorageKey, SHA256: artifact.SHA256, Bytes: artifact.ByteSize,
		MediaType: artifact.MediaType, Visibility: artifact.Visibility, ExpiresAt: artifact.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if err := reference.Validate(); err != nil {
		return nil, retryableFailure("pdf_source_unavailable", "PDF source snapshot is unavailable")
	}
	body, err := executor.store.GetPrivate(ctx, reference)
	if err != nil || int64(len(body)) != artifact.ByteSize {
		return nil, retryableFailure("pdf_source_unavailable", "PDF source snapshot is unavailable")
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return nil, invalidFailure("invalid_pdf_snapshot", "PDF source snapshot hash is invalid")
	}
	return body, nil
}

func (executor *Executor) recoverStale(ctx context.Context, jobID string) *Failure {
	target := filepath.Join(executor.config.SpoolRoot, jobID)
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return retryableFailure("pdf_spool_unavailable", "PDF spool could not be inspected")
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := executor.broker.Remove(cleanupCtx, jobID); err != nil {
		return retryableFailure("pdf_recovery_failed", "A stale PDF compiler could not be removed")
	}
	if err := cleanupSpool(executor.config.SpoolRoot, jobID); err != nil {
		return retryableFailure("pdf_recovery_failed", "A stale PDF spool could not be removed")
	}
	return nil
}

func (executor *Executor) waitForCompletion(ctx context.Context, jobID, digest string, inspection BrokerInspection) (BrokerInspection, *Failure) {
	for {
		if !matchesCompilerImage(inspection.Image, digest) {
			return BrokerInspection{}, invalidFailure("pdf_image_mismatch", "PDF compiler image changed during execution")
		}
		if inspection.State == "missing" {
			return BrokerInspection{}, retryableFailure("pdf_container_lost", "PDF compiler container disappeared")
		}
		if !inspection.Running && (inspection.State == "exited" || inspection.State == "dead") {
			return inspection, nil
		}
		if !waitContext(ctx, executor.config.PollInterval) {
			return BrokerInspection{}, retryableFailure("pdf_worker_interrupted", "PDF compilation was interrupted")
		}
		var err error
		inspection, err = executor.broker.Inspect(ctx, jobID)
		if err != nil {
			return BrokerInspection{}, retryableFailure("pdf_broker_unavailable", "PDF compiler state is unavailable")
		}
	}
}

func matchesCompilerImage(image, digest string) bool {
	return strings.HasSuffix(strings.TrimSpace(image), "@"+digest) && strings.Count(image, "@") == 1
}

func invalidFailure(code, message string) *Failure {
	return &Failure{Code: code, Status: 422, Message: message, Diagnostics: []renderapi.Diagnostic{}}
}

func retryableFailure(code, message string) *Failure {
	return &Failure{Code: code, Status: 503, Message: message, Retryable: true, Diagnostics: []renderapi.Diagnostic{}}
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func artifactReferences(artifacts []PreviewArtifact) []contracts.ArtifactReference {
	references := make([]contracts.ArtifactReference, 0, len(artifacts))
	for _, artifact := range artifacts {
		references = append(references, contracts.ArtifactReference{
			ArtifactID: artifact.ArtifactID, SHA256: artifact.SHA256, Bytes: artifact.Bytes,
			MediaType: artifact.MediaType, Visibility: artifact.Visibility, ExpiresAt: artifact.ExpiresAt,
		})
	}
	return references
}

func (executor *Executor) storeResult(ctx context.Context, job jobpostgres.Job, metadata requestMetadata, artifacts []PreviewArtifact, diagnostics []renderapi.Diagnostic) Outcome {
	identity := sha256.Sum256([]byte(metadata.SessionID + metadata.SnapshotHash + metadata.Entrypoint + metadata.EnginePolicyID + metadata.ImageDigest))
	result := PreviewResult{
		SchemaVersion: PreviewSchemaVersion, JobID: job.ID, RequestID: firstNonEmpty(metadata.RequestID, job.ID),
		ContentKind: "latex", OutputKind: "latex-pdf-preview", DocumentEngine: "latexmk",
		ResourceClass: "latex-pdf", PriorityClass: "preview", SnapshotHash: metadata.SnapshotHash,
		SessionID: metadata.SessionID, DraftRevision: metadata.DraftRevision, EnginePolicyID: metadata.EnginePolicyID,
		ImageDigest: metadata.ImageDigest, IdempotencyKey: hex.EncodeToString(identity[:]), Entrypoint: metadata.Entrypoint,
		WorkspaceRoot: "/workspace", Artifacts: artifacts, Diagnostics: diagnostics, Cache: PreviewCache{Hit: false},
		Versions: PreviewVersions{RinRenderer: executor.config.RendererVersion, Latexmk: LatexmkVersion, TeXLive: TeXLiveVersion, CompilerImage: metadata.ImageDigest},
	}
	for _, artifact := range artifacts {
		result.TotalArtifactBytes += artifact.Bytes
	}
	if err := result.Validate(); err != nil {
		return Outcome{Failure: invalidFailure("invalid_pdf_result", "PDF result contract is invalid")}
	}
	body, err := json.Marshal(result)
	if err != nil {
		return Outcome{Failure: retryableFailure("pdf_result_storage_failed", "PDF result could not be encoded")}
	}
	digest := sha256.Sum256(body)
	resultHash := hex.EncodeToString(digest[:])
	expiresAt := executor.config.Now().UTC().Truncate(time.Second).Add(artifactTTL)
	reference, err := executor.store.PutPrivate(ctx, orchestration.ArtifactWrite{
		ArtifactID: "render-jobs/v1/result/" + job.ID + "/" + resultHash + ".json", Body: body,
		MediaType: "application/json", SchemaVersion: PreviewSchemaVersion, ExpiresAt: &expiresAt,
	})
	if err != nil {
		return Outcome{Failure: retryableFailure("pdf_result_storage_failed", "PDF result could not be stored")}
	}
	artifact := jobpostgres.Artifact{
		ID: reference.ArtifactID, SHA256: reference.SHA256, Kind: "result", Visibility: reference.Visibility,
		StorageKey: reference.ArtifactID, ByteSize: reference.Bytes, MediaType: reference.MediaType,
		SchemaVersion: PreviewSchemaVersion, ExpiresAt: &expiresAt,
	}
	return Outcome{ResultReference: reference, ResultArtifact: artifact, Evidence: jobpostgres.CompletionResultEvidence{RendererProjectHash: metadata.SnapshotHash, ResultHash: resultHash}}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
