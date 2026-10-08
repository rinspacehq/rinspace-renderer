package jobcompat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/admission"
	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

type Admission interface {
	Admit(context.Context, admission.Request) (admission.Result, error)
}

type Repository interface {
	AuthorizedJob(context.Context, string, string, string) (jobpostgres.Job, error)
	Artifact(context.Context, string) (jobpostgres.Artifact, error)
	RequestCancellation(context.Context, string, time.Time) (jobpostgres.Cancellation, error)
}

type Store interface {
	GetPrivate(context.Context, contracts.ArtifactReference) ([]byte, error)
}

type Config struct {
	RendererVersion   string
	DefaultEngine     string
	DefaultOwnerScope string
	MaxSourceBytes    int64
	WaitLimit         time.Duration
	PollInterval      time.Duration
	Now               func() time.Time
}

type Handler struct {
	auth       jobapi.Authenticator
	admission  Admission
	repository Repository
	store      Store
	config     Config
}

type requestMetadata struct {
	RequestID        string `json:"requestId"`
	SourceName       string `json:"sourceName"`
	Entrypoint       string `json:"entrypoint"`
	Title            string `json:"title"`
	MetadataMainFile string `json:"metadataMainFile"`
	ActiveFile       string `json:"activeFile"`
	ProjectStatus    string `json:"projectStatus"`
	MathPolicy       string `json:"mathPolicy"`
	Renderer         string `json:"renderer"`
	Options          string `json:"options"`
}

func New(auth jobapi.Authenticator, admit Admission, repository Repository, store Store, config Config) (*Handler, error) {
	if auth == nil || admit == nil || repository == nil || store == nil || strings.TrimSpace(config.RendererVersion) == "" ||
		strings.TrimSpace(config.DefaultEngine) == "" || strings.TrimSpace(config.DefaultOwnerScope) == "" ||
		config.MaxSourceBytes <= 0 || config.WaitLimit <= 0 || config.PollInterval <= 0 {
		return nil, errors.New("renderer compatibility handler configuration is invalid")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Handler{auth: auth, admission: admit, repository: repository, store: store, config: config}, nil
}

func (handler *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	requestID := newRequestID("", "", "")
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		writeError(response, http.StatusMethodNotAllowed, requestID, "method not allowed")
		return
	}
	if strings.TrimSpace(request.Header.Get("X-Rin-Renderer-Owner-Scope")) == "" {
		request.Header.Set("X-Rin-Renderer-Owner-Scope", handler.config.DefaultOwnerScope)
	}
	identity, err := handler.auth.Authenticate(request)
	if err != nil {
		writeError(response, http.StatusUnauthorized, requestID, "renderer authentication failed")
		return
	}
	respondAsync := strings.Contains(strings.ToLower(request.Header.Get("Prefer")), "respond-async")
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if respondAsync && idempotencyKey == "" {
		writeError(response, http.StatusBadRequest, requestID, "respond-async requires Idempotency-Key")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, handler.config.MaxSourceBytes+(1<<20))
	if err := request.ParseMultipartForm(handler.config.MaxSourceBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(response, http.StatusRequestEntityTooLarge, requestID, "project archive too large")
			return
		}
		writeError(response, http.StatusBadRequest, requestID, "invalid multipart render request")
		return
	}
	defer request.MultipartForm.RemoveAll()
	file, header, err := request.FormFile("source")
	if err != nil {
		writeError(response, http.StatusBadRequest, requestID, "missing source archive")
		return
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, handler.config.MaxSourceBytes+1))
	if err != nil {
		writeError(response, http.StatusBadRequest, requestID, "failed to read source archive")
		return
	}
	if int64(len(body)) > handler.config.MaxSourceBytes {
		writeError(response, http.StatusRequestEntityTooLarge, requestID, "project archive too large")
		return
	}
	contentKind := firstNonEmpty(request.FormValue("contentKind"), request.FormValue("content_kind"), "latex")
	if contentKind != "latex" && contentKind != "markdown" {
		writeError(response, http.StatusBadRequest, requestID, "unsupported content kind")
		return
	}
	engine := firstNonEmpty(request.FormValue("engine"), handler.config.DefaultEngine)
	if contentKind == "markdown" && (engine == "auto" || engine == handler.config.DefaultEngine) {
		engine = "unified"
	}
	if contentKind == "latex" && engine != "auto" && engine != "latexml" ||
		contentKind == "markdown" && engine != "unified" && engine != "rin-markdown" {
		writeError(response, http.StatusBadRequest, requestID, "unsupported document engine")
		return
	}
	priorityIntent := firstNonEmpty(request.FormValue("priorityIntent"), request.Header.Get("X-Rin-Renderer-Priority-Intent"), "rebuild")
	if idempotencyKey != "" {
		requestID = newRequestID(identity.PrincipalID, identity.OwnerScope, idempotencyKey)
	}
	metadata := requestMetadata{
		RequestID: requestID, SourceName: header.Filename, Entrypoint: request.FormValue("mainFile"),
		Title: request.FormValue("title"), MetadataMainFile: metadataPath(request, "mainFile", "main_file"),
		ActiveFile:    firstNonEmpty(request.FormValue("activeFile"), request.FormValue("activePath"), metadataPath(request, "activePath", "activeFile", "active_path", "active_file")),
		ProjectStatus: request.FormValue("status"), MathPolicy: request.FormValue("mathPolicy"),
		Renderer: request.FormValue("renderer"), Options: request.FormValue("options"),
	}
	metadataBody, _ := json.Marshal(metadata)
	projectDigest := sha256.Sum256(body)
	optionsDigest := sha256.Sum256(metadataBody)
	declared := int64(len(body))
	admitted, err := handler.admission.Admit(request.Context(), admission.Request{
		PrincipalID: identity.PrincipalID, OwnerScope: identity.OwnerScope,
		ContentKind: contentKind, DocumentEngine: engine, ResourceClass: resourceClass(engine),
		PriorityIntent: priorityIntent, ProjectHash: hex.EncodeToString(projectDigest[:]),
		OptionsHash: hex.EncodeToString(optionsDigest[:]), RendererVersion: handler.config.RendererVersion,
		RequestMetadata: metadataBody, MaxAttempts: 2, IdempotencyKey: idempotencyKey,
		ExpectedSourceSHA256: hex.EncodeToString(projectDigest[:]), DeclaredSourceBytes: &declared,
		Source: bytes.NewReader(body), SourceMediaType: "application/zip", SourceSchemaVersion: "rin-project-archive/v1",
	})
	if err != nil {
		writeAdmissionError(response, requestID, err)
		return
	}
	location := "/api/render/jobs/" + admitted.Job.ID
	response.Header().Set("Location", location)
	if respondAsync {
		response.Header().Set("Preference-Applied", "respond-async")
		writeJSON(response, http.StatusAccepted, map[string]any{"requestId": requestID, "jobId": admitted.Job.ID, "state": admitted.Job.State, "location": location})
		return
	}
	handler.wait(response, request, identity, admitted.Job.ID, requestID, location, idempotencyKey != "")
}

func (handler *Handler) wait(response http.ResponseWriter, request *http.Request, identity jobapi.Identity, jobID, requestID, location string, detachAllowed bool) {
	timer := time.NewTimer(handler.config.WaitLimit)
	defer timer.Stop()
	ticker := time.NewTicker(handler.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-request.Context().Done():
			handler.cancelDisconnected(request, jobID, detachAllowed)
			return
		default:
		}
		job, err := handler.repository.AuthorizedJob(request.Context(), jobID, identity.PrincipalID, identity.OwnerScope)
		if err != nil {
			if request.Context().Err() != nil {
				handler.cancelDisconnected(request, jobID, detachAllowed)
				return
			}
			writeError(response, http.StatusServiceUnavailable, requestID, "renderer job status unavailable")
			return
		}
		switch job.State {
		case "succeeded":
			handler.writeCompatibilityResult(response, request, job, requestID)
			return
		case "failed":
			handler.writeCompatibilityFailure(response, request, job, requestID)
			return
		case "canceled":
			writeError(response, http.StatusConflict, requestID, "renderer job canceled")
			return
		case "expired":
			writeError(response, http.StatusGone, requestID, "renderer job expired")
			return
		}
		select {
		case <-request.Context().Done():
			handler.cancelDisconnected(request, jobID, detachAllowed)
			return
		case <-timer.C:
			if !detachAllowed {
				cancelContext, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), 5*time.Second)
				_, _ = handler.repository.RequestCancellation(cancelContext, jobID, handler.config.Now().UTC().Truncate(time.Second))
				cancel()
			}
			response.Header().Set("Location", location)
			writeError(response, http.StatusGatewayTimeout, requestID, "renderer compatibility wait timed out")
			return
		case <-ticker.C:
		}
	}
}

func (handler *Handler) cancelDisconnected(request *http.Request, jobID string, detachAllowed bool) {
	if detachAllowed {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), 5*time.Second)
	defer cancel()
	_, _ = handler.repository.RequestCancellation(ctx, jobID, handler.config.Now().UTC().Truncate(time.Second))
}

func (handler *Handler) output(request *http.Request, job jobpostgres.Job) ([]byte, jobpostgres.Artifact, error) {
	if job.ResultArtifactID == "" {
		return nil, jobpostgres.Artifact{}, errors.New("job output artifact is missing")
	}
	artifact, err := handler.repository.Artifact(request.Context(), job.ResultArtifactID)
	if err != nil || artifact.Visibility != "private" || artifact.ExpiresAt == nil {
		return nil, jobpostgres.Artifact{}, errors.New("job output artifact is unavailable")
	}
	reference := contracts.ArtifactReference{ArtifactID: artifact.StorageKey, SHA256: artifact.SHA256, Bytes: artifact.ByteSize, MediaType: artifact.MediaType, Visibility: artifact.Visibility, ExpiresAt: artifact.ExpiresAt.UTC().Format(time.RFC3339)}
	body, err := handler.store.GetPrivate(request.Context(), reference)
	return body, artifact, err
}

func (handler *Handler) writeCompatibilityResult(response http.ResponseWriter, request *http.Request, job jobpostgres.Job, requestID string) {
	body, artifact, err := handler.output(request, job)
	if err != nil || artifact.SchemaVersion != jobresult.StoredOutputSchemaVersion {
		writeError(response, http.StatusBadGateway, requestID, "renderer result unavailable")
		return
	}
	output, err := jobresult.Decode(body)
	if err != nil || output.Result.JobID != job.ID {
		writeError(response, http.StatusBadGateway, requestID, "renderer result unavailable")
		return
	}
	var compatibility renderapi.ProjectRenderResponse
	if err := json.Unmarshal(output.Compatibility, &compatibility); err != nil {
		writeError(response, http.StatusBadGateway, requestID, "renderer result unavailable")
		return
	}
	writeJSON(response, http.StatusOK, compatibility)
}

func (handler *Handler) writeCompatibilityFailure(response http.ResponseWriter, request *http.Request, job jobpostgres.Job, requestID string) {
	body, artifact, err := handler.output(request, job)
	if err != nil || artifact.SchemaVersion != jobresult.StoredFailureSchemaVersion {
		writeError(response, http.StatusBadGateway, requestID, "renderer job failed")
		return
	}
	var failure jobresult.StoredFailure
	if err := json.Unmarshal(body, &failure); err != nil || failure.Validate() != nil {
		writeError(response, http.StatusBadGateway, requestID, "renderer job failed")
		return
	}
	writeJSON(response, failure.Status, renderapi.ErrorResponse{RequestID: requestID, Error: failure.Error, Diagnostics: failure.Diagnostics})
}

func resourceClass(engine string) string {
	if engine == "latexml" || engine == "auto" {
		return "document-latexml"
	}
	return "document-light"
}

func metadataPath(request *http.Request, keys ...string) string {
	for _, field := range []string{"project", "metadata"} {
		raw := strings.TrimSpace(request.FormValue(field))
		if raw == "" {
			continue
		}
		var values map[string]any
		if json.Unmarshal([]byte(raw), &values) != nil {
			continue
		}
		if value := metadataString(values, keys...); value != "" {
			return value
		}
		if nested, ok := values["project"].(map[string]any); ok {
			if value := metadataString(nested, keys...); value != "" {
				return value
			}
		}
	}
	return ""
}

func metadataString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func newRequestID(principalID, ownerScope, idempotencyKey string) string {
	if idempotencyKey != "" {
		digest := sha256.Sum256([]byte(strings.Join([]string{principalID, ownerScope, idempotencyKey}, "\x00")))
		return hex.EncodeToString(digest[:16])
	}
	body := make([]byte, 16)
	if _, err := rand.Read(body); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(body)
}

func writeAdmissionError(response http.ResponseWriter, requestID string, err error) {
	var rejected *admission.Error
	if errors.As(err, &rejected) {
		if rejected.RetryAfter > 0 {
			response.Header().Set("Retry-After", strconv.Itoa(int(rejected.RetryAfter.Seconds())))
		}
		writeError(response, rejected.StatusCode, requestID, rejected.Error())
		return
	}
	writeError(response, http.StatusInternalServerError, requestID, "renderer admission failed")
}

func writeError(response http.ResponseWriter, status int, requestID string, message string) {
	writeJSON(response, status, renderapi.ErrorResponse{RequestID: requestID, Error: message})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
