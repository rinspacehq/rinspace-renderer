package jobapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/admission"
	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfexecutor"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstpdfexecutor"
)

type Identity struct {
	PrincipalID string
	OwnerScope  string
}

var (
	controlProjectIDPattern = regexp.MustCompile(`^(article|book|tag-wiki|pdf):[1-9][0-9]*$`)
	controlRequestIDPattern = regexp.MustCompile(`^render-[0-9a-f]{32}$`)
	controlCommitPattern    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	controlHashPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	previewSessionPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	previewPolicyPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{2,127}$`)
	previewImagePattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	renderProfilePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{2,127}$`)
)

type Authenticator interface {
	Authenticate(*http.Request) (Identity, error)
}

type StaticTokenAuthenticator struct {
	Principals map[string]string
}

func (auth StaticTokenAuthenticator) Authenticate(request *http.Request) (Identity, error) {
	token := strings.TrimSpace(request.Header.Get("X-Rin-Renderer-Token"))
	if token == "" {
		authorization := strings.TrimSpace(request.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
			token = strings.TrimSpace(authorization[len("Bearer "):])
		}
	}
	principal := strings.TrimSpace(auth.Principals[token])
	owner := strings.TrimSpace(request.Header.Get("X-Rin-Renderer-Owner-Scope"))
	if principal == "" || owner == "" {
		return Identity{}, errors.New("renderer job authentication failed")
	}
	return Identity{PrincipalID: principal, OwnerScope: owner}, nil
}

type Admission interface {
	Admit(context.Context, admission.Request) (admission.Result, error)
}

type Repository interface {
	AuthorizedJob(context.Context, string, string, string) (jobpostgres.Job, error)
	AuthorizedEvents(context.Context, string, string, string, int64, int) ([]jobpostgres.JobEvent, error)
	Queue(context.Context, time.Time) (jobpostgres.QueueSummary, error)
	AuthorizedJobQueue(context.Context, string, string, string, time.Time, jobpostgres.EstimationPolicy) (jobpostgres.JobQueueSummary, error)
	AuthorizedSupportSnapshot(context.Context, string, string, string, time.Time) (jobpostgres.SupportSnapshot, error)
	AuthorizedManualRetry(context.Context, string, string, string, time.Time) (jobpostgres.Job, error)
	AuthorizedManualExpire(context.Context, string, string, string, time.Time) (jobpostgres.Job, error)
	Artifact(context.Context, string) (jobpostgres.Artifact, error)
	RequestCancellation(context.Context, string, time.Time) (jobpostgres.Cancellation, error)
	CompletionHealth(context.Context, time.Time) (jobpostgres.CompletionHealth, error)
}

type PrivateStore interface {
	GetPrivate(context.Context, contracts.ArtifactReference) ([]byte, error)
}

type Config struct {
	RendererVersion string
	// RejectControlPlaneIdentity prevents local-only deployments from accepting
	// publication jobs that would require a signed completion callback.
	RejectControlPlaneIdentity bool
	// TypstProfileID is the configured Typst HTML render profile the Control
	// Plane must request for typst document jobs. It is empty when no Typst
	// compiler is pinned, in which case typst submissions fail closed.
	TypstProfileID      string
	MaxSourceBytes      int64
	ProjectFileMaxCount int
	ProjectFileMaxBytes int64
	EventPoll           time.Duration
	Estimation          jobpostgres.EstimationPolicy
	Now                 func() time.Time
}

type Handler struct {
	auth       Authenticator
	admission  Admission
	repository Repository
	store      PrivateStore
	config     Config
	mux        *http.ServeMux
}

func New(auth Authenticator, admit Admission, repository Repository, store PrivateStore, config Config) (*Handler, error) {
	if auth == nil || admit == nil || repository == nil || store == nil || config.MaxSourceBytes <= 0 ||
		config.EventPoll <= 0 || strings.TrimSpace(config.RendererVersion) == "" {
		return nil, errors.New("renderer job API configuration is invalid")
	}
	if err := config.Estimation.Validate(); err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.ProjectFileMaxCount <= 0 {
		config.ProjectFileMaxCount = 700
	}
	if config.ProjectFileMaxBytes <= 0 {
		config.ProjectFileMaxBytes = config.MaxSourceBytes
	}
	handler := &Handler{auth: auth, admission: admit, repository: repository, store: store, config: config, mux: http.NewServeMux()}
	handler.mux.HandleFunc("POST /api/render/jobs", handler.submit)
	handler.mux.HandleFunc("GET /api/render/jobs/{jobId}", handler.status)
	handler.mux.HandleFunc("GET /api/render/jobs/{jobId}/result", handler.result)
	handler.mux.HandleFunc("GET /api/render/jobs/{jobId}/artifacts/{artifactKind}", handler.artifact)
	handler.mux.HandleFunc("GET /api/render/jobs/{jobId}/events", handler.events)
	handler.mux.HandleFunc("DELETE /api/render/jobs/{jobId}", handler.cancel)
	handler.mux.HandleFunc("GET /api/render/jobs/{jobId}/support", handler.support)
	handler.mux.HandleFunc("POST /api/render/jobs/{jobId}/retry", handler.retry)
	handler.mux.HandleFunc("POST /api/render/jobs/{jobId}/expire", handler.expire)
	handler.mux.HandleFunc("GET /api/render/queue", handler.queue)
	handler.mux.HandleFunc("GET /internal/v1/render/completion-health", handler.completionHealth)
	return handler, nil
}

func (handler *Handler) completionHealth(response http.ResponseWriter, request *http.Request) {
	health, err := handler.repository.CompletionHealth(request.Context(), handler.config.Now().UTC())
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "completion_health_unavailable", "completion delivery health is unavailable")
		return
	}
	writeJSON(response, http.StatusOK, health)
}

func (handler *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(response, request)
}

func (handler *Handler) identity(response http.ResponseWriter, request *http.Request) (Identity, bool) {
	identity, err := handler.auth.Authenticate(request)
	if err != nil {
		writeError(response, http.StatusUnauthorized, "unauthorized", "job authentication is required")
		return Identity{}, false
	}
	return identity, true
}

func (handler *Handler) submit(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(response, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, handler.config.MaxSourceBytes+(1<<20))
	if err := request.ParseMultipartForm(handler.config.MaxSourceBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(response, http.StatusRequestEntityTooLarge, "source_too_large", "source archive exceeds the configured limit")
			return
		}
		writeError(response, http.StatusBadRequest, "invalid_source", "invalid multipart job request")
		return
	}
	defer request.MultipartForm.RemoveAll()
	file, header, err := request.FormFile("source")
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid_source", "source archive is required")
		return
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, handler.config.MaxSourceBytes+1))
	if err != nil || int64(len(body)) > handler.config.MaxSourceBytes {
		writeError(response, http.StatusRequestEntityTooLarge, "source_too_large", "source archive exceeds the configured limit")
		return
	}
	contentKind := strings.TrimSpace(request.FormValue("contentKind"))
	outputKind := strings.TrimSpace(request.FormValue("outputKind"))
	if outputKind == "" {
		outputKind = "document"
	}
	documentEngine := firstNonEmpty(request.FormValue("documentEngine"), request.FormValue("engine"))
	priorityIntent := strings.TrimSpace(request.FormValue("priorityIntent"))
	if contentKind == "" || documentEngine == "" || priorityIntent == "" {
		writeError(response, http.StatusBadRequest, "invalid_job_request", "contentKind, documentEngine, and priorityIntent are required")
		return
	}
	isPreview := outputKind == "latex-pdf-preview" || outputKind == "typst-pdf-preview"
	isTypstExport := outputKind == "typst-pdf-export"
	if outputKind != "document" && !isPreview && !isTypstExport {
		writeError(response, http.StatusBadRequest, "invalid_job_request", "outputKind is invalid")
		return
	}
	resourceClass := "document-light"
	switch contentKind {
	case "latex":
		resourceClass = "document-latexml"
	case "markdown":
	case "typst":
		resourceClass = "document-typst"
	default:
		writeError(response, http.StatusBadRequest, "invalid_source", "unsupported contentKind")
		return
	}
	if isPreview || isTypstExport {
		resourceClass = previewResourceClassFor(contentKind)
	}
	if !validDocumentEngine(contentKind, documentEngine) && !(isPreview && contentKind == "latex" && documentEngine == "latexmk") {
		writeError(response, http.StatusBadRequest, "invalid_document_engine", "documentEngine is not supported for contentKind")
		return
	}
	previewSnapshotHash := strings.TrimSpace(request.FormValue("snapshotHash"))
	previewSessionID := strings.TrimSpace(request.FormValue("sessionId"))
	previewEnginePolicyID := strings.TrimSpace(request.FormValue("enginePolicyId"))
	previewImageDigest := strings.TrimSpace(request.FormValue("imageDigest"))
	previewContractVersion := strings.TrimSpace(request.FormValue("previewContractVersion"))
	previewDraftRevision := int64(0)
	entrypoint := strings.TrimSpace(request.FormValue("entrypoint"))
	controlProjectID := strings.TrimSpace(request.FormValue("controlProjectId"))
	controlRequestID := strings.TrimSpace(request.FormValue("requestId"))
	controlSourceCommit := strings.TrimSpace(request.FormValue("sourceCommit"))
	controlProjectHash := strings.TrimSpace(request.FormValue("controlProjectHash"))
	if handler.config.RejectControlPlaneIdentity && (controlProjectID != "" || controlRequestID != "" || controlSourceCommit != "" || controlProjectHash != "") {
		writeError(response, http.StatusBadRequest, "control_plane_unavailable", "Control Plane publication jobs require the control-plane completion mode")
		return
	}
	if isPreview {
		var parseErr error
		previewDraftRevision, parseErr = strconv.ParseInt(strings.TrimSpace(request.FormValue("draftRevision")), 10, 64)
		identityDigest := sha256.Sum256([]byte(previewSessionID + previewSnapshotHash + entrypoint + previewEnginePolicyID + previewImageDigest))
		if documentEngine != previewEngineFor(contentKind) || priorityIntent != "preview" ||
			request.FormValue("resourceClass") != resourceClass || request.FormValue("priorityClass") != "preview" ||
			!controlHashPattern.MatchString(previewSnapshotHash) || !previewSessionPattern.MatchString(previewSessionID) ||
			parseErr != nil || previewDraftRevision < 0 || !previewPolicyPattern.MatchString(previewEnginePolicyID) ||
			!previewImagePattern.MatchString(previewImageDigest) || previewContractVersion != previewContractFor(contentKind) ||
			!canonicalPreviewEntrypoint(contentKind, entrypoint) || idempotencyKey != hex.EncodeToString(identityDigest[:]) {
			writeError(response, http.StatusUnprocessableEntity, "invalid_preview_identity", "PDF preview identity or policy is invalid")
			return
		}
	} else if isTypstExport {
		// A committed export runs the same pinned compiler image under the same
		// fixed isolation policy as a preview; only the identity differs, so the
		// image digest and policy id are required rather than optional.
		if documentEngine != "typst" || (priorityIntent != "publish" && priorityIntent != "rebuild") ||
			request.FormValue("priorityClass") != priorityIntent || request.FormValue("resourceClass") != resourceClass ||
			previewSnapshotHash != "" || previewSessionID != "" || previewContractVersion != "" ||
			!previewPolicyPattern.MatchString(previewEnginePolicyID) || !previewImagePattern.MatchString(previewImageDigest) ||
			!canonicalTypstEntrypoint(entrypoint) {
			writeError(response, http.StatusUnprocessableEntity, "invalid_export_identity", "Typst PDF export identity is invalid")
			return
		}
		if controlProjectID == "" || controlSourceCommit == "" || controlProjectHash == "" {
			writeError(response, http.StatusUnprocessableEntity, "invalid_export_identity", "Typst PDF export requires the published source commit identity")
			return
		}
	} else if jobpostgres.IsPreviewResourceClass(request.FormValue("resourceClass")) || priorityIntent == "preview" {
		writeError(response, http.StatusBadRequest, "invalid_job_request", "preview scheduling requires the PDF preview contract")
		return
	}
	if controlProjectID != "" || controlRequestID != "" || controlSourceCommit != "" || controlProjectHash != "" {
		if controlProjectID != identity.OwnerScope || !controlProjectIDPattern.MatchString(controlProjectID) ||
			(controlRequestID == "" && !isTypstExport) || (controlRequestID != "" && !controlRequestIDPattern.MatchString(controlRequestID)) ||
			!controlCommitPattern.MatchString(controlSourceCommit) ||
			strings.Trim(controlSourceCommit, "0") == "" || !controlHashPattern.MatchString(controlProjectHash) {
			writeError(response, http.StatusBadRequest, "invalid_publication_identity", "Control Plane publication identity is invalid")
			return
		}
	}
	documentMode := strings.TrimSpace(request.FormValue("documentMode"))
	if documentMode == "" {
		documentMode = "article"
	}
	if documentMode != "article" && documentMode != "book" {
		writeError(response, http.StatusBadRequest, "invalid_job_request", "documentMode is invalid")
		return
	}
	renderProfileID := strings.TrimSpace(request.FormValue("renderProfileId"))
	switch {
	case contentKind == "typst" && outputKind == "document":
		// A Typst HTML document job must name the exact profile this Renderer
		// currently serves. Rejecting an unknown, missing or stale profile is
		// what stops a same-commit rebuild from silently reusing an old result.
		if handler.config.TypstProfileID == "" || renderProfileID != handler.config.TypstProfileID {
			writeError(response, http.StatusUnprocessableEntity, "invalid_render_profile", "Typst render profile is not the configured profile")
			return
		}
	case renderProfileID != "":
		writeError(response, http.StatusBadRequest, "invalid_job_request", "renderProfileId is only valid for Typst document jobs")
		return
	}
	var bookPages []projectidentity.MarkdownBookPage
	if contentKind == "markdown" && documentMode == "book" {
		bookPages, err = decodeMarkdownBookPages(request.FormValue("bookPages"), handler.config.ProjectFileMaxCount)
		if err != nil {
			writeError(response, http.StatusBadRequest, "invalid_book_manifest", "Markdown Book page manifest is invalid")
			return
		}
	} else if strings.TrimSpace(request.FormValue("bookPages")) != "" {
		writeError(response, http.StatusBadRequest, "invalid_job_request", "Only Markdown Book jobs can include a Book page manifest")
		return
	}
	digest := sha256.Sum256(body)
	projectHash := hex.EncodeToString(digest[:])
	if isPreview {
		projectHash = previewSnapshotHash
	}
	if contentKind == "markdown" {
		limits := projectcore.Limits{
			ArchiveMaxBytes: handler.config.MaxSourceBytes, FileMaxCount: handler.config.ProjectFileMaxCount,
			FileMaxBytes: handler.config.ProjectFileMaxBytes,
		}
		var project projectidentity.Project
		var projectErr error
		if documentMode == "book" {
			project, projectErr = projectidentity.ImportMarkdownBook(header.Filename, body, request.FormValue("title"), bookPages, limits)
		} else {
			project, projectErr = projectidentity.Import(header.Filename, body, contracts.ContentKindMarkdown,
				request.FormValue("entrypoint"), limits)
		}
		if projectErr != nil {
			writeError(response, http.StatusBadRequest, "invalid_source", "Markdown project archive is invalid")
			return
		}
		projectHash = project.Graph.ProjectHash
	}
	canonicalBookPages, _ := json.Marshal(bookPages)
	optionsDigest := sha256.Sum256([]byte(strings.Join([]string{
		request.FormValue("entrypoint"), documentEngine, request.FormValue("options"),
		request.FormValue("title"), request.FormValue("projectStatus"), documentMode, string(canonicalBookPages),
		outputKind, previewEnginePolicyID, previewImageDigest, previewContractVersion, renderProfileID,
	}, "\x00")))
	requestMetadata, err := json.Marshal(map[string]any{
		"sourceName": header.Filename, "entrypoint": request.FormValue("entrypoint"), "outputKind": outputKind,
		"requestId": controlRequestID,
		"options":   request.FormValue("options"), "title": request.FormValue("title"),
		"projectStatus": request.FormValue("projectStatus"), "documentMode": documentMode,
		"bookPages":       bookPages,
		"renderProfileId": renderProfileID,
		"sourceCommit":    controlSourceCommit, "controlProjectHash": controlProjectHash,
		"controlProjectId": controlProjectID,
		"snapshotHash":     previewSnapshotHash, "sessionId": previewSessionID,
		"draftRevision": previewDraftRevision, "enginePolicyId": previewEnginePolicyID,
		"imageDigest": previewImageDigest, "previewContractVersion": previewContractVersion,
	})
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid_job_request", "job metadata is invalid")
		return
	}
	declared := int64(len(body))
	result, err := handler.admission.Admit(request.Context(), admission.Request{
		PrincipalID: identity.PrincipalID, OwnerScope: identity.OwnerScope,
		ContentKind: contentKind, DocumentEngine: documentEngine, ResourceClass: resourceClass,
		PriorityIntent: priorityIntent, ProjectHash: projectHash,
		OptionsHash: hex.EncodeToString(optionsDigest[:]), RendererVersion: handler.config.RendererVersion,
		RequestMetadata: requestMetadata,
		MaxAttempts:     2, IdempotencyKey: idempotencyKey,
		ExpectedSourceSHA256: hex.EncodeToString(digest[:]), DeclaredSourceBytes: &declared,
		Source: bytes.NewReader(body), SourceMediaType: previewSourceMediaType(isPreview),
		SourceSchemaVersion: previewSourceSchemaVersion(isPreview),
	})
	if err != nil {
		writeAdmissionError(response, err)
		return
	}
	location := "/api/render/jobs/" + result.Job.ID
	queue, err := handler.repository.AuthorizedJobQueue(
		request.Context(), result.Job.ID, identity.PrincipalID, identity.OwnerScope,
		handler.config.Now().UTC().Truncate(time.Second),
		handler.config.Estimation,
	)
	if err != nil {
		writeRepositoryError(response, err)
		return
	}
	job := statusValue(result.Job)
	job["queue"] = queue
	response.Header().Set("Location", location)
	writeJSON(response, http.StatusAccepted, map[string]any{
		"job": job, "queue": queue, "location": location, "reused": result.Reused,
	})
}

func decodeMarkdownBookPages(value string, maxPages int) ([]projectidentity.MarkdownBookPage, error) {
	if strings.TrimSpace(value) == "" || maxPages < 1 || len(value) > 1<<20 {
		return nil, errors.New("missing or oversized Markdown Book page manifest")
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var pages []projectidentity.MarkdownBookPage
	if err := decoder.Decode(&pages); err != nil {
		return nil, err
	}
	if len(pages) < 1 || len(pages) > maxPages {
		return nil, fmt.Errorf("Markdown Book page count %d is outside limits", len(pages))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("Markdown Book page manifest has trailing data")
	}
	return pages, nil
}

func (handler *Handler) status(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	job, err := handler.repository.AuthorizedJob(request.Context(), request.PathValue("jobId"), identity.PrincipalID, identity.OwnerScope)
	if err != nil {
		writeRepositoryError(response, err)
		return
	}
	value := statusValue(job)
	if job.State == "failed" {
		// Every failed job projects its stored failure so the Control Plane can
		// carry the engine diagnostic instead of a generic message. A draft
		// preview cannot be diagnosed without its detail, so an unreadable
		// preview failure stays a hard error while a document job degrades to
		// the coarse classification the Control Plane already had.
		failure, err := handler.storedFailure(request.Context(), job)
		if err == nil {
			value["error"] = publicJobFailure(job, failure)
		} else if previewJob(job) {
			writeError(response, http.StatusServiceUnavailable, "failure_unavailable", "job failure details are unavailable")
			return
		}
	}
	if job.State == "queued" {
		queue, err := handler.repository.AuthorizedJobQueue(
			request.Context(), job.ID, identity.PrincipalID, identity.OwnerScope,
			handler.config.Now().UTC().Truncate(time.Second),
			handler.config.Estimation,
		)
		if err != nil {
			writeRepositoryError(response, err)
			return
		}
		value["queue"] = queue
	}
	writeJSON(response, http.StatusOK, value)
}

func (handler *Handler) artifact(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	job, err := handler.repository.AuthorizedJob(request.Context(), request.PathValue("jobId"), identity.PrincipalID, identity.OwnerScope)
	if err != nil {
		writeRepositoryError(response, err)
		return
	}
	kind := request.PathValue("artifactKind")
	if !pdfArtifactJob(job) || !pdfArtifactKind(job, kind) || job.ResultArtifactID == "" ||
		(job.State != "succeeded" && job.State != "failed") {
		writeError(response, http.StatusNotFound, "artifact_not_found", "job artifact is not available")
		return
	}
	reference, err := handler.previewArtifactReference(request.Context(), job, kind)
	if err != nil {
		writeError(response, http.StatusNotFound, "artifact_not_found", "job artifact is not available")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, reference.ExpiresAt)
	if err != nil || !expiresAt.After(handler.config.Now().UTC()) {
		writeError(response, http.StatusGone, "artifact_expired", "job artifact has expired")
		return
	}
	artifact, err := handler.repository.Artifact(request.Context(), reference.ArtifactID)
	if err != nil || artifact.StorageKey != reference.ArtifactID || artifact.Visibility != "private" ||
		artifact.SHA256 != reference.SHA256 || artifact.ByteSize != reference.Bytes || artifact.MediaType != reference.MediaType {
		writeError(response, http.StatusNotFound, "artifact_not_found", "job artifact is not available")
		return
	}
	body, err := handler.store.GetPrivate(request.Context(), reference)
	digest := sha256.Sum256(body)
	if err != nil || int64(len(body)) != reference.Bytes || hex.EncodeToString(digest[:]) != reference.SHA256 {
		writeError(response, http.StatusNotFound, "artifact_not_found", "job artifact is not available")
		return
	}
	response.Header().Set("Content-Type", reference.MediaType)
	response.Header().Set("Content-Length", strconv.FormatInt(reference.Bytes, 10))
	response.Header().Set("Cache-Control", "private, no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.Header().Set("X-Rinspace-Artifact-SHA256", reference.SHA256)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(body)
}

func (handler *Handler) result(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	job, err := handler.repository.AuthorizedJob(request.Context(), request.PathValue("jobId"), identity.PrincipalID, identity.OwnerScope)
	if err != nil {
		writeRepositoryError(response, err)
		return
	}
	if job.State == "expired" {
		writeError(response, http.StatusGone, "job_expired", "job result has expired")
		return
	}
	if job.State != "succeeded" || job.ResultArtifactID == "" {
		writeError(response, http.StatusConflict, "result_not_ready", "job result is not available")
		return
	}
	artifact, err := handler.repository.Artifact(request.Context(), job.ResultArtifactID)
	if err != nil || artifact.Visibility != "private" {
		writeError(response, http.StatusNotFound, "result_not_found", "job result is not available")
		return
	}
	reference := contracts.ArtifactReference{
		ArtifactID: artifact.StorageKey, SHA256: artifact.SHA256, Bytes: artifact.ByteSize,
		MediaType: artifact.MediaType, Visibility: artifact.Visibility,
	}
	if artifact.ExpiresAt != nil {
		reference.ExpiresAt = artifact.ExpiresAt.UTC().Format(time.RFC3339)
	}
	body, err := handler.store.GetPrivate(request.Context(), reference)
	if err != nil {
		writeError(response, http.StatusNotFound, "result_not_found", "job result is not available")
		return
	}
	if artifact.SchemaVersion == jobresult.StoredOutputSchemaVersion {
		output, err := jobresult.Decode(body)
		if err != nil || output.Result.JobID != job.ID {
			writeError(response, http.StatusNotFound, "result_not_found", "job result is not available")
			return
		}
		writeJSON(response, http.StatusOK, output.Result)
		return
	}
	response.Header().Set("Content-Type", artifact.MediaType)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(body)
}

func (handler *Handler) storedFailure(ctx context.Context, job jobpostgres.Job) (jobresult.StoredFailure, error) {
	body, artifact, err := handler.resultArtifactBody(ctx, job)
	if err != nil || artifact.SchemaVersion != jobresult.StoredFailureSchemaVersion {
		return jobresult.StoredFailure{}, errors.New("stored job failure is unavailable")
	}
	var failure jobresult.StoredFailure
	if err := json.Unmarshal(body, &failure); err != nil || failure.Validate() != nil {
		return jobresult.StoredFailure{}, errors.New("stored job failure is invalid")
	}
	return failure, nil
}

func (handler *Handler) previewArtifactReference(ctx context.Context, job jobpostgres.Job, kind string) (contracts.ArtifactReference, error) {
	body, artifact, err := handler.resultArtifactBody(ctx, job)
	if err != nil {
		return contracts.ArtifactReference{}, err
	}
	if job.State == "succeeded" && artifact.SchemaVersion == pdfexecutor.PreviewSchemaVersion {
		var result pdfexecutor.PreviewResult
		if json.Unmarshal(body, &result) != nil || result.Validate() != nil || result.JobID != job.ID {
			return contracts.ArtifactReference{}, errors.New("stored PDF result is invalid")
		}
		for _, candidate := range result.Artifacts {
			if candidate.Kind == kind {
				return contracts.ArtifactReference{
					ArtifactID: candidate.ArtifactID, SHA256: candidate.SHA256, Bytes: candidate.Bytes,
					MediaType: candidate.MediaType, Visibility: candidate.Visibility, ExpiresAt: candidate.ExpiresAt,
				}, nil
			}
		}
	}
	if job.State == "succeeded" && artifact.SchemaVersion == typstpdfexecutor.ResultSchemaVersion {
		var result typstpdfexecutor.Result
		if json.Unmarshal(body, &result) != nil || result.Validate() != nil || result.JobID != job.ID {
			return contracts.ArtifactReference{}, errors.New("stored Typst PDF result is invalid")
		}
		for _, candidate := range result.Artifacts {
			if candidate.Kind == kind {
				return contracts.ArtifactReference{
					ArtifactID: candidate.ArtifactID, SHA256: candidate.SHA256, Bytes: candidate.Bytes,
					MediaType: candidate.MediaType, Visibility: candidate.Visibility, ExpiresAt: candidate.ExpiresAt,
				}, nil
			}
		}
	}
	if job.State == "failed" && artifact.SchemaVersion == jobresult.StoredFailureSchemaVersion {
		var failure jobresult.StoredFailure
		if json.Unmarshal(body, &failure) != nil || failure.Validate() != nil {
			return contracts.ArtifactReference{}, errors.New("stored PDF failure is invalid")
		}
		for _, candidate := range failure.Artifacts {
			if strings.HasPrefix(path.Base(candidate.ArtifactID), kind+"-") {
				return candidate, nil
			}
		}
	}
	return contracts.ArtifactReference{}, errors.New("PDF artifact kind is unavailable")
}

func (handler *Handler) resultArtifactBody(ctx context.Context, job jobpostgres.Job) ([]byte, jobpostgres.Artifact, error) {
	if job.ResultArtifactID == "" {
		return nil, jobpostgres.Artifact{}, errors.New("job has no stored result")
	}
	artifact, err := handler.repository.Artifact(ctx, job.ResultArtifactID)
	if err != nil || artifact.Visibility != "private" || artifact.StorageKey == "" || artifact.ByteSize <= 0 {
		return nil, jobpostgres.Artifact{}, errors.New("stored result artifact is unavailable")
	}
	reference := contracts.ArtifactReference{
		ArtifactID: artifact.StorageKey, SHA256: artifact.SHA256, Bytes: artifact.ByteSize,
		MediaType: artifact.MediaType, Visibility: artifact.Visibility,
	}
	if artifact.ExpiresAt != nil {
		reference.ExpiresAt = artifact.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if err := reference.Validate(); err != nil {
		return nil, jobpostgres.Artifact{}, err
	}
	body, err := handler.store.GetPrivate(ctx, reference)
	digest := sha256.Sum256(body)
	if err != nil || int64(len(body)) != reference.Bytes || hex.EncodeToString(digest[:]) != reference.SHA256 {
		return nil, jobpostgres.Artifact{}, errors.New("stored result artifact integrity failed")
	}
	return body, artifact, nil
}

func publicPreviewFailure(failure jobresult.StoredFailure) map[string]any {
	code, category := "unavailable", "internal"
	switch failure.Code {
	case "invalid_pdf_snapshot", "invalid_pdf_job", "invalid_typst_snapshot", "invalid_typst_job":
		code, category = "snapshot_invalid", "input"
	case "pdf_image_mismatch", "typst_image_mismatch":
		code, category = "entrypoint_invalid", "input"
	case "pdf_compile_failed", "typst_compile_failed":
		code, category = "compile_failed", "compile"
	case "pdf_compile_timeout", "typst_compile_timeout":
		code, category = "timeout", "limit"
	case "pdf_compile_oom", "pdf_artifact_quota_exceeded", "typst_compile_oom", "typst_artifact_quota_exceeded":
		code, category = "resource_exhausted", "limit"
	case "invalid_pdf_artifact", "unsafe_pdf_quarantined", "invalid_typst_artifact", "unsafe_typst_pdf_quarantined":
		code, category = "artifact_invalid", "compile"
	case "pdf_worker_interrupted", "typst_worker_interrupted":
		code, category = "canceled", "cancel"
	}
	value := map[string]any{
		"code": code, "category": category, "retryable": failure.Retryable,
		"message": boundedFailureMessage(failure.Error, "PDF compilation failed"),
	}
	if failure.ExitCode != nil {
		value["exitCode"] = *failure.ExitCode
	}
	return value
}

// publicJobFailure projects a stored job failure onto the bounded public shape
// shared by every job kind. Draft PDF previews keep their dedicated code
// vocabulary, while published documents classify the failure from the engine
// diagnostic so authors read the compiler error instead of a coarse transport
// classification.
func publicJobFailure(job jobpostgres.Job, failure jobresult.StoredFailure) map[string]any {
	if previewJob(job) {
		return publicPreviewFailure(failure)
	}
	code, category := documentFailureClass(failure)
	message := boundedFailureMessage(failure.Error, "Document compilation failed")
	if detail := documentFailureDetail(failure); detail != "" && !strings.Contains(message, detail) {
		message = boundedFailureMessage(message+": "+detail, "Document compilation failed")
	}
	value := map[string]any{"code": code, "category": category, "retryable": failure.Retryable, "message": message}
	if failure.ExitCode != nil {
		value["exitCode"] = *failure.ExitCode
	}
	return value
}

// documentFailureDetail returns the bounded engine diagnostic, which carries
// the pinned compiler log for Markdown, LaTeX and Typst document builds.
func documentFailureDetail(failure jobresult.StoredFailure) string {
	for _, diagnostic := range failure.Diagnostics {
		if message := strings.TrimSpace(diagnostic.Message); message != "" {
			return message
		}
	}
	return ""
}

// documentFailureClass prefers the engine diagnostic over the stored failure
// code, because a rejected document is classified as invalid_project_source
// before its compiler output is read.
func documentFailureClass(failure jobresult.StoredFailure) (string, string) {
	candidates := make([]string, 0, len(failure.Diagnostics)+1)
	for _, diagnostic := range failure.Diagnostics {
		if code := strings.TrimSpace(diagnostic.Code); code != "" {
			candidates = append(candidates, code)
		}
	}
	candidates = append(candidates, failure.Code)
	for _, code := range candidates {
		switch {
		case strings.Contains(code, "timeout"):
			return "timeout", "limit"
		case strings.Contains(code, "compile"), strings.Contains(code, "render_failed"):
			return "compile_failed", "compile"
		case strings.Contains(code, "invalid_project_source"), strings.Contains(code, "invalid_request_metadata"),
			strings.Contains(code, "invalid_manifest"), strings.Contains(code, "invalid_book"):
			return "source_invalid", "input"
		case strings.Contains(code, "cancel"):
			return "canceled", "cancel"
		}
	}
	if len(failure.Diagnostics) > 0 {
		return "compile_failed", "compile"
	}
	return "unavailable", "internal"
}

func boundedFailureMessage(value string, fallback string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return fallback
	}
	if len([]rune(text)) > 512 {
		return string([]rune(text)[:512])
	}
	return text
}

func previewJob(job jobpostgres.Job) bool {
	var metadata map[string]any
	return json.Unmarshal(job.RequestMetadata, &metadata) == nil && previewOutputKind(metadata)
}

// previewOutputKind reports whether request metadata describes a draft PDF
// preview. Both LaTeX and Typst previews expose the same bounded status
// projection, but they never share a resource class or result contract.
func previewOutputKind(metadata map[string]any) bool {
	outputKind, _ := metadata["outputKind"].(string)
	return outputKind == "latex-pdf-preview" || outputKind == typstpdfexecutor.PreviewOutputKind
}

func previewArtifactKind(value string) bool {
	switch value {
	case "pdf", "synctex", "log", "aux", "fls", "bbl", "blg", "toc", "out":
		return true
	default:
		return false
	}
}

// pdfArtifactJob reports whether the job exposes a private PDF artifact
// download. Draft previews and committed Typst exports share the endpoint but
// never share a result contract or a resource class.
func pdfArtifactJob(job jobpostgres.Job) bool {
	if previewJob(job) {
		return true
	}
	var metadata map[string]any
	if json.Unmarshal(job.RequestMetadata, &metadata) != nil {
		return false
	}
	outputKind, _ := metadata["outputKind"].(string)
	return outputKind == typstpdfexecutor.ExportOutputKind
}

// pdfArtifactKind bounds the downloadable artifact kinds per output contract.
// A committed Typst export never carries TeX aux, fls or synctex files.
func pdfArtifactKind(job jobpostgres.Job, kind string) bool {
	if previewJob(job) {
		return previewArtifactKind(kind)
	}
	return kind == "pdf" || kind == "log"
}

func (handler *Handler) cancel(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	jobID := request.PathValue("jobId")
	if _, err := handler.repository.AuthorizedJob(request.Context(), jobID, identity.PrincipalID, identity.OwnerScope); err != nil {
		writeRepositoryError(response, err)
		return
	}
	cancellation, err := handler.repository.RequestCancellation(request.Context(), jobID, handler.config.Now().UTC().Truncate(time.Second))
	if err != nil {
		writeRepositoryError(response, err)
		return
	}
	operational.Default().Count("cancellations_total", cancellation.Job.ResourceClass)
	operational.Default().Event("job_canceled", slog.String("request_id", cancellation.Job.ID), slog.String("job_id", cancellation.Job.ID),
		slog.String("content_kind", cancellation.Job.ContentKind), slog.String("engine", cancellation.Job.DocumentEngine),
		slog.String("resource_class", cancellation.Job.ResourceClass), slog.String("state", cancellation.Job.State))
	writeJSON(response, http.StatusOK, statusValue(cancellation.Job))
}

func (handler *Handler) support(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	snapshot, err := handler.repository.AuthorizedSupportSnapshot(request.Context(), request.PathValue("jobId"),
		identity.PrincipalID, identity.OwnerScope, handler.config.Now().UTC().Truncate(time.Second))
	if err != nil {
		writeRepositoryError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, snapshot)
}

func (handler *Handler) retry(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	job, err := handler.repository.AuthorizedManualRetry(request.Context(), request.PathValue("jobId"),
		identity.PrincipalID, identity.OwnerScope, handler.config.Now().UTC().Truncate(time.Second))
	if err != nil {
		writeRepositoryError(response, err)
		return
	}
	operational.Default().Count("support_actions_total", "retry")
	operational.Default().Event("support_action", slog.String("request_id", job.ID), slog.String("job_id", job.ID),
		slog.String("content_kind", job.ContentKind), slog.String("engine", job.DocumentEngine),
		slog.String("resource_class", job.ResourceClass), slog.String("state", job.State))
	writeJSON(response, http.StatusOK, statusValue(job))
}

func (handler *Handler) expire(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	job, err := handler.repository.AuthorizedManualExpire(request.Context(), request.PathValue("jobId"),
		identity.PrincipalID, identity.OwnerScope, handler.config.Now().UTC().Truncate(time.Second))
	if err != nil {
		writeRepositoryError(response, err)
		return
	}
	operational.Default().Count("support_actions_total", "expire")
	operational.Default().Event("support_action", slog.String("request_id", job.ID), slog.String("job_id", job.ID),
		slog.String("content_kind", job.ContentKind), slog.String("engine", job.DocumentEngine),
		slog.String("resource_class", job.ResourceClass), slog.String("state", job.State))
	writeJSON(response, http.StatusOK, statusValue(job))
}

func (handler *Handler) queue(response http.ResponseWriter, request *http.Request) {
	if _, ok := handler.identity(response, request); !ok {
		return
	}
	summary, err := handler.repository.Queue(request.Context(), handler.config.Now().UTC().Truncate(time.Second))
	if err != nil {
		writeError(response, http.StatusInternalServerError, "queue_unavailable", "queue status is unavailable")
		return
	}
	metrics := operational.Default()
	metrics.Gauge("queue_projects", float64(summary.QueuedProjects), "queued")
	metrics.Gauge("queue_projects", float64(summary.ActiveProjects), "active")
	metrics.Gauge("queue_oldest_wait_seconds", summary.OldestWait.Seconds())
	writeJSON(response, http.StatusOK, summary)
}

func (handler *Handler) events(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	afterID := parseEventID(firstNonEmpty(request.Header.Get("Last-Event-ID"), request.URL.Query().Get("after")))
	jobID := request.PathValue("jobId")
	if _, err := handler.repository.AuthorizedJob(request.Context(), jobID, identity.PrincipalID, identity.OwnerScope); err != nil {
		writeRepositoryError(response, err)
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := response.(http.Flusher)
	for {
		events, err := handler.repository.AuthorizedEvents(request.Context(), jobID, identity.PrincipalID, identity.OwnerScope, afterID, 100)
		if err != nil {
			return
		}
		for _, event := range events {
			payload, _ := json.Marshal(map[string]any{
				"id": event.ID, "type": event.Type, "stage": event.Stage,
				"payload": json.RawMessage(event.Payload), "createdAt": event.CreatedAt,
			})
			_, _ = response.Write([]byte("id: " + strconv.FormatInt(event.ID, 10) + "\n"))
			_, _ = response.Write([]byte("event: " + event.Type + "\n"))
			_, _ = response.Write([]byte("data: " + string(payload) + "\n\n"))
			afterID = event.ID
		}
		if flusher != nil {
			flusher.Flush()
		}
		job, err := handler.repository.AuthorizedJob(request.Context(), jobID, identity.PrincipalID, identity.OwnerScope)
		if err != nil || terminal(job.State) {
			return
		}
		timer := time.NewTimer(handler.config.EventPoll)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func statusValue(job jobpostgres.Job) map[string]any {
	completedStages := 0
	switch job.State {
	case "running":
		completedStages = 1
	case "succeeded":
		completedStages = 3
	case "failed", "canceled", "expired":
		if job.StartedAt != nil {
			completedStages = 1
		}
	}
	value := map[string]any{
		"jobId": job.ID, "contentKind": job.ContentKind, "documentEngine": job.DocumentEngine,
		"resourceClass": job.ResourceClass, "priorityClass": job.PriorityClass,
		"state": job.State, "createdAt": job.CreatedAt, "updatedAt": job.UpdatedAt,
		"expiresAt": job.ExpiresAt, "cancelRequested": job.CancelRequested,
		"stage":    statusStage(job),
		"progress": map[string]any{"completedStages": completedStages, "totalStages": 3},
	}
	var metadata map[string]any
	if json.Unmarshal(job.RequestMetadata, &metadata) == nil {
		// The render profile is echoed for every job that pinned one so the
		// Control Plane can persist which configuration produced a result and
		// refuse to let a stale profile overwrite a newer activation.
		if profile, ok := metadata["renderProfileId"].(string); ok && strings.TrimSpace(profile) != "" {
			value["renderProfileId"] = strings.TrimSpace(profile)
		}
		if previewOutputKind(metadata) {
			for _, key := range []string{"outputKind", "snapshotHash", "sessionId", "draftRevision", "enginePolicyId", "imageDigest", "entrypoint"} {
				if field, ok := metadata[key]; ok {
					value[key] = field
				}
			}
		}
	}
	if job.QueuedAt != nil {
		value["queuedAt"] = job.QueuedAt
	}
	if job.StartedAt != nil {
		value["startedAt"] = job.StartedAt
		if job.State == "running" {
			elapsed := time.Since(*job.StartedAt)
			if elapsed < 0 {
				elapsed = 0
			}
			value["elapsedSeconds"] = int64(elapsed / time.Second)
		}
	}
	if job.FinishedAt != nil {
		value["finishedAt"] = job.FinishedAt
	}
	return value
}

func statusStage(job jobpostgres.Job) string {
	if job.CancelRequested {
		return "cancel"
	}
	switch job.State {
	case "queued":
		return "queue"
	case "running":
		return "document_compile"
	case "succeeded":
		return "store"
	case "failed":
		return "failed"
	case "canceled":
		return "canceled"
	case "expired":
		return "expired"
	default:
		return ""
	}
}

func terminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "canceled" || state == "expired"
}

func parseEventID(value string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if id < 0 {
		return 0
	}
	return id
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func validDocumentEngine(contentKind string, engine string) bool {
	switch contentKind {
	case "latex":
		return engine == "auto" || engine == "latexml"
	case "markdown":
		return engine == "auto" || engine == "unified"
	case "typst":
		return engine == "typst"
	default:
		return false
	}
}

// previewResourceClassFor maps a content kind to its dedicated PDF build pool.
// Typst and LaTeX previews are isolated from each other and from publishing.
func previewResourceClassFor(contentKind string) string {
	if contentKind == "typst" {
		return typstpdfexecutor.ResourceClass
	}
	return "latex-pdf"
}

// previewEngineFor is the only document engine a draft PDF preview accepts for
// the given content kind; Typst is never allowed to masquerade as latexmk.
func previewEngineFor(contentKind string) string {
	if contentKind == "typst" {
		return "typst"
	}
	return "latexmk"
}

func previewContractFor(contentKind string) string {
	if contentKind == "typst" {
		return typstpdfexecutor.PreviewContractVersion
	}
	return pdfexecutor.PreviewSchemaVersion
}

func canonicalPreviewEntrypoint(contentKind, value string) bool {
	if contentKind == "typst" {
		return canonicalTypstEntrypoint(value)
	}
	return canonicalPreviewPath(value)
}

func canonicalTypstEntrypoint(value string) bool {
	return canonicalPreviewPathWithExtension(value, ".typ")
}

func canonicalPreviewPath(value string) bool {
	return canonicalPreviewPathWithExtension(value, ".tex")
}

func canonicalPreviewPathWithExtension(value string, extension string) bool {
	if value == "" || len([]byte(value)) > 255 || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "-") ||
		strings.Contains(value, "\\") || strings.Contains(value, "..") || strings.ContainsAny(value, "\x00\r\n") ||
		strings.Contains(value, "//") || path.Clean(value) != value || !strings.HasSuffix(strings.ToLower(value), extension) {
		return false
	}
	first := strings.SplitN(value, "/", 2)[0]
	return !strings.Contains(first, ":") && first != "." && first != ".."
}

func previewSourceMediaType(preview bool) string {
	if preview {
		return "application/x-tar"
	}
	return "application/zip"
}

func previewSourceSchemaVersion(preview bool) string {
	if preview {
		return "rinspace-snapshot/v1"
	}
	return "rin-project-archive/v1"
}

func writeAdmissionError(response http.ResponseWriter, err error) {
	var rejected *admission.Error
	if errors.As(err, &rejected) {
		if rejected.RetryAfter > 0 {
			response.Header().Set("Retry-After", strconv.Itoa(int(rejected.RetryAfter.Seconds())))
		}
		writeError(response, rejected.StatusCode, rejected.Code, rejected.Error())
		return
	}
	writeError(response, http.StatusInternalServerError, "admission_failed", "job admission failed")
}

func writeRepositoryError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jobpostgres.ErrNotFound):
		writeError(response, http.StatusNotFound, "job_not_found", "job was not found")
	case errors.Is(err, jobpostgres.ErrCancellationNotAllowed):
		writeError(response, http.StatusConflict, "cancellation_not_allowed", "job cannot be canceled")
	case errors.Is(err, jobpostgres.ErrSupportActionNotAllowed):
		writeError(response, http.StatusConflict, "support_action_not_allowed", "job support action is not allowed")
	default:
		writeError(response, http.StatusInternalServerError, "job_unavailable", "job operation failed")
	}
}

func writeError(response http.ResponseWriter, status int, code string, message string) {
	writeJSON(response, status, map[string]string{"code": code, "message": message})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
