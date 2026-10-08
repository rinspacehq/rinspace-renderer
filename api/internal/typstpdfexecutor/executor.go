package typstpdfexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

const (
	// fixedTypstPDFWallTimeout mirrors the measured broker/profile wall limit for
	// the typst-pdf workload. The executor never accepts a longer budget from a
	// job or from source.
	fixedTypstPDFWallTimeout = 30 * time.Second
	// TypstVersion is the pinned compiler version the typst-pdf image ships.
	// It is recorded in every result so a renderProfileId upgrade can tell a
	// stale artifact from a current one.
	TypstVersion = "0.15.1"
	// RenderProfileID is the frozen preview profile for this compiler, image
	// policy and adapter. The full computed profile lands with T16; until then
	// the id is derived deterministically from the pinned inputs.
	renderProfilePrefix = "typst-pdf-preview-v1"
	// typstPDFProfileSchema tags the profile derivation so an adapter or
	// final-output contract change produces a new id even when the compiler
	// image digest is unchanged.
	typstPDFProfileSchema = "rin-typst-pdf-profile/v1"
)

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
	// The committed export branch carries the published project identity
	// instead of a session draft identity. Both branches are mutually
	// exclusive and a job that mixes them is rejected by validateJob.
	ControlProjectID   string `json:"controlProjectId"`
	SourceCommit       string `json:"sourceCommit"`
	ControlProjectHash string `json:"controlProjectHash"`
}

// isExport reports whether the job is a committed export rather than a draft
// preview. Output kind is validated before this is consulted.
func (metadata requestMetadata) isExport() bool {
	return metadata.OutputKind == ExportOutputKind
}

func New(repository Repository, store Store, broker Broker, config Config) (*Executor, error) {
	if repository == nil || store == nil || broker == nil || strings.TrimSpace(config.SpoolRoot) == "" ||
		strings.TrimSpace(config.RendererVersion) == "" || config.PollInterval <= 0 ||
		config.WallTimeout <= 0 || config.WallTimeout > fixedTypstPDFWallTimeout {
		return nil, errors.New("Typst PDF executor configuration is invalid")
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
	source, failure := executor.source(ctx, job, metadata)
	if failure != nil {
		return Outcome{Failure: failure}
	}
	if failure := executor.recoverStale(ctx, job.ID); failure != nil {
		return Outcome{Failure: failure}
	}
	var spool preparedSpool
	var err error
	if metadata.isExport() {
		spool, err = prepareExportSpool(executor.config.SpoolRoot, job.ID, metadata.Entrypoint, source, job.ProjectHash)
		if err != nil {
			return Outcome{Failure: invalidFailure("invalid_typst_project_archive", "Typst PDF project archive is invalid")}
		}
	} else {
		spool, err = prepareSpool(executor.config.SpoolRoot, job.ID, metadata.Entrypoint, source, metadata.SnapshotHash)
		if err != nil {
			return Outcome{Failure: invalidFailure("invalid_typst_snapshot", "Typst PDF source snapshot is invalid")}
		}
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
		return Outcome{Failure: retryableFailure("typst_broker_unavailable", "Typst compiler could not be started")}
	}
	started = true
	if !matchesCompilerImage(inspection.Image, metadata.ImageDigest) {
		return Outcome{Failure: invalidFailure("typst_image_mismatch", "Typst compiler image does not match the admitted digest")}
	}

	runCtx, cancel := context.WithTimeout(ctx, executor.config.WallTimeout)
	defer cancel()
	inspection, failure = executor.waitForCompletion(runCtx, job.ID, metadata.ImageDigest, inspection)
	if failure != nil {
		collectPartial := false
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			_, stopErr := executor.broker.Stop(context.WithoutCancel(ctx), job.ID)
			failure = &Failure{Code: "typst_compile_timeout", Status: 504,
				Message:   fmt.Sprintf("Typst compilation exceeded %d seconds", int(executor.config.WallTimeout/time.Second)),
				Retryable: true, Diagnostics: []renderapi.Diagnostic{}}
			collectPartial = stopErr == nil
		} else if ctx.Err() != nil {
			_, stopErr := executor.broker.Stop(context.WithoutCancel(ctx), job.ID)
			failure = retryableFailure("typst_worker_interrupted", "Typst compilation was interrupted")
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
	if artifactFailure != nil && artifactFailure.Code == "unsafe_typst_pdf_quarantined" {
		artifactFailure.Diagnostics = diagnostics
		artifactFailure.Artifacts = artifactReferences(artifacts)
		return Outcome{Failure: artifactFailure}
	}
	if inspection.OOMKilled {
		exitCode := inspection.ExitCode
		return Outcome{Failure: &Failure{Code: "typst_compile_oom", Status: 503, Message: "Typst compilation exceeded its memory limit", ExitCode: &exitCode, Retryable: true, Diagnostics: diagnostics, Artifacts: artifactReferences(artifacts)}}
	}
	if inspection.ExitCode != 0 {
		exitCode := inspection.ExitCode
		return Outcome{Failure: &Failure{Code: "typst_compile_failed", Status: 422, Message: "Typst compilation failed", ExitCode: &exitCode, Diagnostics: diagnostics, Artifacts: artifactReferences(artifacts)}}
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
	if !jobIDPattern.MatchString(job.ID) || job.ContentKind != "typst" || job.DocumentEngine != "typst" ||
		job.ResourceClass != ResourceClass || job.RendererVersion != executor.config.RendererVersion ||
		len(job.RequestMetadata) == 0 || json.Unmarshal(job.RequestMetadata, &metadata) != nil ||
		!sha256Pattern.MatchString(job.OptionsHash) || !sha256Pattern.MatchString(job.ProjectHash) ||
		!canonicalTypstEntrypoint(metadata.Entrypoint) || !imageDigestPattern.MatchString(metadata.ImageDigest) ||
		metadata.EnginePolicyID != PolicyID {
		return requestMetadata{}, invalidFailure("invalid_typst_job", "Typst PDF job contract is invalid")
	}
	switch metadata.OutputKind {
	case PreviewOutputKind:
		if job.PriorityClass != "preview" || job.ProjectHash != metadata.SnapshotHash ||
			metadata.PreviewContractVersion != PreviewContractVersion ||
			!sha256Pattern.MatchString(metadata.SnapshotHash) || !sessionPattern.MatchString(metadata.SessionID) ||
			metadata.DraftRevision < 0 || metadata.ControlProjectID != "" || metadata.SourceCommit != "" ||
			metadata.ControlProjectHash != "" {
			return requestMetadata{}, invalidFailure("invalid_typst_job", "Typst PDF preview identity is invalid")
		}
	case ExportOutputKind:
		if (job.PriorityClass != "publish" && job.PriorityClass != "rebuild") ||
			metadata.PreviewContractVersion != "" || metadata.SnapshotHash != "" || metadata.SessionID != "" ||
			metadata.DraftRevision != 0 || !canonicalControlProjectID(metadata.ControlProjectID) ||
			!commitPattern.MatchString(metadata.SourceCommit) || strings.Trim(metadata.SourceCommit, "0") == "" ||
			!sha256Pattern.MatchString(metadata.ControlProjectHash) {
			return requestMetadata{}, invalidFailure("invalid_typst_job", "Typst PDF export identity is invalid")
		}
	default:
		return requestMetadata{}, invalidFailure("invalid_typst_job", "Typst PDF job output kind is invalid")
	}
	return metadata, nil
}

func (executor *Executor) source(ctx context.Context, job jobpostgres.Job, metadata requestMetadata) ([]byte, *Failure) {
	artifact, err := executor.repository.Artifact(ctx, job.SourceArtifactID)
	if err != nil || artifact.Kind != "source" || artifact.Visibility != "private" || artifact.ExpiresAt == nil ||
		!artifact.ExpiresAt.After(executor.config.Now()) || artifact.ByteSize <= 0 {
		return nil, retryableFailure("typst_source_unavailable", "Typst PDF source snapshot is unavailable")
	}
	if metadata.isExport() {
		// Only the exact published archive the job hash was computed from may
		// be compiled; a preview snapshot or a different commit is rejected.
		if artifact.MediaType != ExportArchiveMediaType || artifact.SchemaVersion != ExportArchiveSchemaVersion ||
			artifact.SHA256 != job.ProjectHash || artifact.ByteSize > snapshotTotalBytes {
			return nil, invalidFailure("invalid_typst_project_archive", "Typst PDF project archive does not match the published commit")
		}
	} else if artifact.MediaType != "application/x-tar" || artifact.SchemaVersion != SnapshotSchemaVersion ||
		artifact.ByteSize > snapshotTotalBytes+snapshotFileLimit*512+1024 {
		return nil, retryableFailure("typst_source_unavailable", "Typst PDF source snapshot is unavailable")
	}
	reference := contracts.ArtifactReference{
		ArtifactID: artifact.StorageKey, SHA256: artifact.SHA256, Bytes: artifact.ByteSize,
		MediaType: artifact.MediaType, Visibility: artifact.Visibility, ExpiresAt: artifact.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if err := reference.Validate(); err != nil {
		return nil, retryableFailure("typst_source_unavailable", "Typst PDF source snapshot is unavailable")
	}
	body, err := executor.store.GetPrivate(ctx, reference)
	if err != nil || int64(len(body)) != artifact.ByteSize {
		return nil, retryableFailure("typst_source_unavailable", "Typst PDF source snapshot is unavailable")
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return nil, invalidFailure("invalid_typst_snapshot", "Typst PDF source snapshot hash is invalid")
	}
	return body, nil
}

func (executor *Executor) recoverStale(ctx context.Context, jobID string) *Failure {
	target := filepath.Join(executor.config.SpoolRoot, jobID)
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return retryableFailure("typst_spool_unavailable", "Typst spool could not be inspected")
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := executor.broker.Remove(cleanupCtx, jobID); err != nil {
		return retryableFailure("typst_recovery_failed", "A stale Typst compiler could not be removed")
	}
	if err := cleanupSpool(executor.config.SpoolRoot, jobID); err != nil {
		return retryableFailure("typst_recovery_failed", "A stale Typst spool could not be removed")
	}
	return nil
}

func (executor *Executor) waitForCompletion(ctx context.Context, jobID, digest string, inspection BrokerInspection) (BrokerInspection, *Failure) {
	for {
		if !matchesCompilerImage(inspection.Image, digest) {
			return BrokerInspection{}, invalidFailure("typst_image_mismatch", "Typst compiler image changed during execution")
		}
		if inspection.State == "missing" {
			return BrokerInspection{}, retryableFailure("typst_container_lost", "Typst compiler container disappeared")
		}
		if !inspection.Running && (inspection.State == "exited" || inspection.State == "dead") {
			return inspection, nil
		}
		if !waitContext(ctx, executor.config.PollInterval) {
			return BrokerInspection{}, retryableFailure("typst_worker_interrupted", "Typst compilation was interrupted")
		}
		var err error
		inspection, err = executor.broker.Inspect(ctx, jobID)
		if err != nil {
			return BrokerInspection{}, retryableFailure("typst_broker_unavailable", "Typst compiler state is unavailable")
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

func artifactReferences(artifacts []Artifact) []contracts.ArtifactReference {
	references := make([]contracts.ArtifactReference, 0, len(artifacts))
	for _, artifact := range artifacts {
		references = append(references, contracts.ArtifactReference{
			ArtifactID: artifact.ArtifactID, SHA256: artifact.SHA256, Bytes: artifact.Bytes,
			MediaType: artifact.MediaType, Visibility: artifact.Visibility, ExpiresAt: artifact.ExpiresAt,
		})
	}
	return references
}

// RenderProfileID derives the frozen Typst PDF render profile from the pinned
// compiler version, compiler image digest (which carries the bundled fonts and
// packages), isolation policy, Renderer version and final-output contract. It
// is stable for equal inputs so cache keys and result reuse stay deterministic,
// and it changes whenever a font, package, isolation policy, adapter/final
// output contract or Renderer version changes, so a same-commit rebuild under a
// new profile can never reuse a stale artifact.
func RenderProfileID(imageDigest, rendererVersion string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		typstPDFProfileSchema, TypstVersion, imageDigest, PolicyID, rendererVersion, finaloutput.ContractVersion,
	}, "\x00")))
	return renderProfilePrefix + "." + hex.EncodeToString(digest[:8])
}

func (executor *Executor) storeResult(ctx context.Context, job jobpostgres.Job, metadata requestMetadata, artifacts []Artifact, diagnostics []renderapi.Diagnostic) Outcome {
	result := Result{
		SchemaVersion: ResultSchemaVersion, JobID: job.ID, RequestID: firstNonEmpty(metadata.RequestID, job.ID),
		ContentKind: "typst", OutputKind: metadata.OutputKind, DocumentEngine: "typst",
		ResourceClass: ResourceClass, PriorityClass: job.PriorityClass, EnginePolicyID: metadata.EnginePolicyID,
		ImageDigest: metadata.ImageDigest, Entrypoint: metadata.Entrypoint, WorkspaceRoot: WorkspaceRoot,
		RenderProfileID: RenderProfileID(metadata.ImageDigest, executor.config.RendererVersion),
		Artifacts:       artifacts, Diagnostics: diagnostics, Cache: Cache{Hit: false},
		Versions: Versions{RinRenderer: executor.config.RendererVersion, Typst: TypstVersion, CompilerImage: metadata.ImageDigest},
	}
	rendererProjectHash := metadata.SnapshotHash
	if metadata.isExport() {
		result.ControlProjectID = metadata.ControlProjectID
		result.SourceCommit = metadata.SourceCommit
		result.ControlProjectHash = metadata.ControlProjectHash
		result.IdempotencyKey = ExportIdempotencyKey(metadata.ControlProjectID, metadata.SourceCommit, metadata.Entrypoint, metadata.EnginePolicyID, metadata.ImageDigest)
		rendererProjectHash = job.ProjectHash
	} else {
		result.SnapshotHash = metadata.SnapshotHash
		result.SessionID = metadata.SessionID
		result.DraftRevision = &metadata.DraftRevision
		result.IdempotencyKey = PreviewIdempotencyKey(metadata.SessionID, metadata.SnapshotHash, metadata.Entrypoint, metadata.EnginePolicyID, metadata.ImageDigest)
	}
	for _, artifact := range artifacts {
		result.TotalArtifactBytes += artifact.Bytes
	}
	if err := result.Validate(); err != nil {
		return Outcome{Failure: invalidFailure("invalid_typst_result", "Typst PDF result contract is invalid")}
	}
	body, err := json.Marshal(result)
	if err != nil {
		return Outcome{Failure: retryableFailure("typst_result_storage_failed", "Typst PDF result could not be encoded")}
	}
	digest := sha256.Sum256(body)
	resultHash := hex.EncodeToString(digest[:])
	expiresAt := executor.config.Now().UTC().Truncate(time.Second).Add(artifactTTL)
	reference, err := executor.store.PutPrivate(ctx, orchestration.ArtifactWrite{
		ArtifactID: "render-jobs/v1/result/" + job.ID + "/" + resultHash + ".json", Body: body,
		MediaType: "application/json", SchemaVersion: ResultSchemaVersion, ExpiresAt: &expiresAt,
	})
	if err != nil {
		return Outcome{Failure: retryableFailure("typst_result_storage_failed", "Typst PDF result could not be stored")}
	}
	artifact := jobpostgres.Artifact{
		ID: reference.ArtifactID, SHA256: reference.SHA256, Kind: "result", Visibility: reference.Visibility,
		StorageKey: reference.ArtifactID, ByteSize: reference.Bytes, MediaType: reference.MediaType,
		SchemaVersion: ResultSchemaVersion, ExpiresAt: &expiresAt,
	}
	return Outcome{ResultReference: reference, ResultArtifact: artifact, Evidence: jobpostgres.CompletionResultEvidence{RendererProjectHash: rendererProjectHash, ResultHash: resultHash}}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
