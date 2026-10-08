package jobcompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/admission"
	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

const compatibilityJobID = "11111111-1111-4111-8111-111111111111"

func TestCompatibilityRouteUsesDurableAdmissionAndReturnsLegacyResponse(t *testing.T) {
	repository, store := successfulOutput(t)
	admit := &fakeAdmission{job: repository.job}
	handler := newTestHandler(t, admit, repository, store, 50*time.Millisecond)
	request := projectRequest(t, "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var compatibility renderapi.ProjectRenderResponse
	if err := json.Unmarshal(response.Body.Bytes(), &compatibility); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || compatibility.HTML != "<article>ok</article>" || compatibility.Source != "private-source" {
		t.Fatalf("compatibility response = %d %s", response.Code, response.Body.String())
	}
	if admit.request.PrincipalID != "principal-a" || admit.request.OwnerScope != "legacy-sync" || admit.request.ContentKind != "latex" || admit.request.ResourceClass != "document-latexml" {
		t.Fatalf("durable admission request = %#v", admit.request)
	}
	if repository.statusCalls == 0 {
		t.Fatal("compatibility route did not wait on durable job status")
	}
}

func TestCompatibilityRouteAdmitsMarkdownToMarkdownExecutor(t *testing.T) {
	repository, store := successfulOutput(t)
	repository.job.ContentKind = "markdown"
	repository.job.DocumentEngine = "unified"
	admit := &fakeAdmission{job: repository.job}
	handler := newTestHandler(t, admit, repository, store, 50*time.Millisecond)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, _ := writer.CreateFormFile("source", "article.zip")
	_, _ = file.Write([]byte("archive"))
	_ = writer.WriteField("contentKind", "markdown")
	_ = writer.WriteField("engine", "auto")
	_ = writer.WriteField("mainFile", "content.md")
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/render/projects", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Rin-Renderer-Token", "token-a")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || admit.request.ContentKind != "markdown" || admit.request.DocumentEngine != "unified" || admit.request.ResourceClass != "document-light" {
		t.Fatalf("Markdown compatibility admission = %d %#v", response.Code, admit.request)
	}
}

func TestRespondAsyncRequiresIdempotencyBeforeReadingBody(t *testing.T) {
	handler := newTestHandler(t, &fakeAdmission{}, &fakeRepository{}, &fakeStore{}, time.Second)
	body := &countingReader{reader: strings.NewReader("not multipart")}
	request := httptest.NewRequest(http.MethodPost, "/api/render/projects", body)
	request.Header.Set("X-Rin-Renderer-Token", "token-a")
	request.Header.Set("Prefer", "respond-async")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || body.reads != 0 {
		t.Fatalf("missing idempotency = %d %s, reads %d", response.Code, response.Body.String(), body.reads)
	}
}

func TestRespondAsyncReturnsAcceptedDurableJob(t *testing.T) {
	repository := &fakeRepository{job: queuedJob()}
	handler := newTestHandler(t, &fakeAdmission{job: repository.job}, repository, &fakeStore{}, time.Second)
	request := projectRequest(t, "publish-1")
	request.Header.Set("Prefer", "respond-async")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Location") != "/api/render/jobs/"+compatibilityJobID || repository.statusCalls != 0 {
		t.Fatalf("respond-async = %d %s, status calls %d", response.Code, response.Body.String(), repository.statusCalls)
	}
}

func TestIdempotentRequestIdentityIsStable(t *testing.T) {
	first := newRequestID("principal-a", "book-a", "publish-1")
	second := newRequestID("principal-a", "book-a", "publish-1")
	if first != second || first == newRequestID("principal-a", "book-b", "publish-1") || len(first) != 32 {
		t.Fatalf("idempotent request IDs = %q / %q", first, second)
	}
}

func TestCompatibilityTimeoutCancellationPolicy(t *testing.T) {
	for _, test := range []struct {
		name, idempotency string
		wantCancel        int
	}{
		{name: "preview-like request cancels", wantCancel: 1},
		{name: "idempotent publish detaches", idempotency: "publish-1", wantCancel: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeRepository{job: queuedJob()}
			handler := newTestHandler(t, &fakeAdmission{job: repository.job}, repository, &fakeStore{}, 3*time.Millisecond)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, projectRequest(t, test.idempotency))
			if response.Code != http.StatusGatewayTimeout || repository.cancelCalls != test.wantCancel {
				t.Fatalf("timeout = %d %s, cancels %d", response.Code, response.Body.String(), repository.cancelCalls)
			}
		})
	}
}

func TestCompatibilityDisconnectCancellationPolicy(t *testing.T) {
	for _, test := range []struct {
		name, idempotency string
		wantCancel        int
	}{
		{name: "non-idempotent request cancels", wantCancel: 1},
		{name: "idempotent request detaches", idempotency: "publish-1", wantCancel: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeRepository{job: queuedJob()}
			handler := newTestHandler(t, &fakeAdmission{job: repository.job}, repository, &fakeStore{}, time.Second)
			request := projectRequest(t, test.idempotency)
			ctx, cancel := context.WithCancel(request.Context())
			cancel()
			request = request.WithContext(ctx)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if repository.cancelCalls != test.wantCancel {
				t.Fatalf("disconnect cancellations = %d, want %d", repository.cancelCalls, test.wantCancel)
			}
		})
	}
}

func TestCompatibilityFailurePreservesStatusAndDiagnostics(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour)
	failure := jobresult.StoredFailure{
		SchemaVersion: jobresult.StoredFailureSchemaVersion, Status: http.StatusUnprocessableEntity,
		Code: "invalid_project_source", Error: "invalid project archive",
		Diagnostics: []renderapi.Diagnostic{{Severity: "error", Code: "latex.invalid", Message: "bad source", Engine: "latexml"}},
	}
	body, _ := json.Marshal(failure)
	repository := &fakeRepository{job: queuedJob(), artifact: jobpostgres.Artifact{
		ID: "failure-id", StorageKey: "failure-id", SHA256: strings.Repeat("a", 64), Kind: "result",
		Visibility: "private", ByteSize: int64(len(body)), MediaType: "application/json",
		SchemaVersion: jobresult.StoredFailureSchemaVersion, ExpiresAt: &expiresAt,
	}}
	repository.job.State, repository.job.ResultArtifactID = "failed", "failure-id"
	handler := newTestHandler(t, &fakeAdmission{job: repository.job}, repository, &fakeStore{body: body}, time.Second)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, projectRequest(t, ""))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"code":"latex.invalid"`) || !strings.Contains(response.Body.String(), "invalid project archive") {
		t.Fatalf("failure compatibility = %d %s", response.Code, response.Body.String())
	}
}

func TestCompatibilityAdmissionOverloadCannotBypassQueue(t *testing.T) {
	admit := &fakeAdmission{err: &admission.Error{StatusCode: 429, Code: admission.CodePrincipalQuota, RetryAfter: 15 * time.Second, Err: errors.New("full")}}
	repository := &fakeRepository{}
	handler := newTestHandler(t, admit, repository, &fakeStore{}, time.Second)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, projectRequest(t, ""))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "15" || repository.statusCalls != 0 {
		t.Fatalf("overload = %d %s, status calls %d", response.Code, response.Body.String(), repository.statusCalls)
	}
}

func newTestHandler(t *testing.T, admit Admission, repository Repository, store Store, wait time.Duration) *Handler {
	t.Helper()
	handler, err := New(jobapi.StaticTokenAuthenticator{Principals: map[string]string{"token-a": "principal-a"}}, admit, repository, store, Config{
		RendererVersion: "test", DefaultEngine: "latexml", DefaultOwnerScope: "legacy-sync",
		MaxSourceBytes: 1024, WaitLimit: wait, PollInterval: time.Millisecond,
		Now: func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func projectRequest(t *testing.T, idempotency string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, _ := writer.CreateFormFile("source", "book.zip")
	_, _ = file.Write([]byte("archive"))
	_ = writer.WriteField("engine", "latexml")
	_ = writer.WriteField("title", "Title")
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/render/projects", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Rin-Renderer-Token", "token-a")
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	return request
}

func successfulOutput(t *testing.T) (*fakeRepository, *fakeStore) {
	t.Helper()
	job := queuedJob()
	job.State, job.ResultArtifactID = "succeeded", "result-id"
	legacy := renderapi.ProjectRenderResponse{
		RequestID: "request-a", Title: "Title", HTML: "<article>ok</article>", Engine: "latexml", MainFile: "main.tex",
		Versions: renderapi.Versions{RinRenderer: "test", LateXML: "test"}, Diagrams: []renderapi.DiagramRef{}, Assets: []renderapi.AssetRef{},
		AssetFiles: []renderapi.AssetFile{}, Diagnostics: []renderapi.Diagnostic{}, Source: "private-source",
	}
	canonical, err := renderapi.CanonicalResult(job.ID, job.ProjectHash, "latex", legacy)
	if err != nil {
		t.Fatal(err)
	}
	compatibility, _ := json.Marshal(legacy)
	body, _ := json.Marshal(jobresult.StoredOutput{SchemaVersion: jobresult.StoredOutputSchemaVersion, Result: canonical, Compatibility: compatibility})
	expiresAt := time.Now().Add(time.Hour)
	repository := &fakeRepository{job: job, artifact: jobpostgres.Artifact{
		ID: "result-id", StorageKey: "result-id", SHA256: strings.Repeat("a", 64), Kind: "result",
		Visibility: "private", ByteSize: int64(len(body)), MediaType: "application/json",
		SchemaVersion: jobresult.StoredOutputSchemaVersion, ExpiresAt: &expiresAt,
	}}
	return repository, &fakeStore{body: body}
}

func queuedJob() jobpostgres.Job {
	return jobpostgres.Job{ID: compatibilityJobID, PrincipalID: "principal-a", OwnerScope: "legacy-sync", ContentKind: "latex", DocumentEngine: "latexml", State: "queued", ProjectHash: strings.Repeat("b", 64), ExpiresAt: time.Now().Add(time.Hour)}
}

type fakeAdmission struct {
	job     jobpostgres.Job
	request admission.Request
	err     error
}

func (fake *fakeAdmission) Admit(_ context.Context, request admission.Request) (admission.Result, error) {
	fake.request = request
	return admission.Result{Job: fake.job}, fake.err
}

type fakeRepository struct {
	job         jobpostgres.Job
	artifact    jobpostgres.Artifact
	statusCalls int
	cancelCalls int
}

func (fake *fakeRepository) AuthorizedJob(ctx context.Context, id, principal, owner string) (jobpostgres.Job, error) {
	fake.statusCalls++
	if err := ctx.Err(); err != nil {
		return jobpostgres.Job{}, err
	}
	if id != fake.job.ID || principal != fake.job.PrincipalID || owner != fake.job.OwnerScope {
		return jobpostgres.Job{}, jobpostgres.ErrNotFound
	}
	return fake.job, nil
}
func (fake *fakeRepository) Artifact(context.Context, string) (jobpostgres.Artifact, error) {
	return fake.artifact, nil
}
func (fake *fakeRepository) RequestCancellation(_ context.Context, _ string, _ time.Time) (jobpostgres.Cancellation, error) {
	fake.cancelCalls++
	return jobpostgres.Cancellation{Job: fake.job}, nil
}

type fakeStore struct{ body []byte }

func (fake *fakeStore) GetPrivate(context.Context, contracts.ArtifactReference) ([]byte, error) {
	return fake.body, nil
}

type countingReader struct {
	reader io.Reader
	reads  int
}

func (reader *countingReader) Read(body []byte) (int, error) {
	reader.reads++
	return reader.reader.Read(body)
}
