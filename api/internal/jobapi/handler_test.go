package jobapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/admission"
	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfexecutor"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstpdfexecutor"
)

const testJobID = "11111111-1111-4111-8111-111111111111"

func TestJobRoutesEnforcePrincipalAndOwnerScope(t *testing.T) {
	repository := newFakeRepository()
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{body: []byte(`{"ok":true}`)})

	for _, test := range []struct {
		name  string
		token string
		owner string
		want  int
	}{
		{name: "authorized", token: "token-a", owner: "book-a", want: http.StatusOK},
		{name: "wrong principal", token: "token-b", owner: "book-a", want: http.StatusNotFound},
		{name: "wrong owner", token: "token-a", owner: "book-b", want: http.StatusNotFound},
		{name: "missing auth", owner: "book-a", want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/render/jobs/"+testJobID, nil)
			request.Header.Set("X-Rin-Renderer-Token", test.token)
			request.Header.Set("X-Rin-Renderer-Owner-Scope", test.owner)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "principal-a") || strings.Contains(response.Body.String(), "book-a") {
				t.Fatalf("response leaked authorization identity: %s", response.Body.String())
			}
		})
	}
}

func TestCompletionHealthIsSafeAndReadOnly(t *testing.T) {
	repository := newFakeRepository()
	repository.completionHealth = jobpostgres.CompletionHealth{
		State: "degraded", Pending: 2, Failed: 1, Delivering: 1, Dead: 1,
		OldestAgeSeconds: 75, CheckedAt: time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC),
	}
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/internal/v1/render/completion-health", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"degraded"`) ||
		!strings.Contains(response.Body.String(), `"oldestAgeSeconds":75`) {
		t.Fatalf("completion health = %d %s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"jobId", "owner", "commit", "reference", "secret", "source"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("completion health leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestRunningStatusReportsCoarseProjectProgressAndElapsedTime(t *testing.T) {
	now := time.Now().Add(-3 * time.Second)
	value := statusValue(jobpostgres.Job{ID: testJobID, ContentKind: "markdown", DocumentEngine: "unified", State: "running",
		StartedAt: &now, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)})
	progress, ok := value["progress"].(map[string]any)
	if !ok || progress["completedStages"] != 1 || progress["totalStages"] != 3 || value["stage"] != "document_compile" {
		t.Fatalf("running project progress = %#v", value)
	}
	if elapsed, ok := value["elapsedSeconds"].(int64); !ok || elapsed < 2 {
		t.Fatalf("running elapsed time = %#v", value["elapsedSeconds"])
	}
	if _, exists := value["queue"]; exists {
		t.Fatalf("running status exposed a queued-job estimate: %#v", value)
	}
}

func TestResultCancelQueueAndEventReplay(t *testing.T) {
	repository := newFakeRepository()
	expires := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	repository.job.State = "succeeded"
	repository.job.ResultArtifactID = "result-id"
	repository.artifact = jobpostgres.Artifact{
		ID: "result-id", StorageKey: "render-jobs/v1/result/" + testJobID + "/" + strings.Repeat("a", 64) + ".json",
		SHA256: strings.Repeat("a", 64), Kind: "result", Visibility: "private", ByteSize: 11,
		MediaType: "application/json", ExpiresAt: &expires,
	}
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{body: []byte(`{"ok":true}`)})

	resultRequest := authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/result", nil)
	resultResponse := httptest.NewRecorder()
	handler.ServeHTTP(resultResponse, resultRequest)
	if resultResponse.Code != http.StatusOK || resultResponse.Body.String() != `{"ok":true}` {
		t.Fatalf("result = %d %s", resultResponse.Code, resultResponse.Body.String())
	}
	canonical, err := renderapi.CanonicalResult(testJobID, strings.Repeat("b", 64), "latex", renderapi.ProjectRenderResponse{
		RequestID: "request-a", Title: "Title", HTML: "<article>ok</article>", Engine: "latexml", MainFile: "main.tex",
		Versions: renderapi.Versions{RinRenderer: "test", LateXML: "test"}, Diagnostics: []renderapi.Diagnostic{}, AssetFiles: []renderapi.AssetFile{},
	})
	if err != nil {
		t.Fatal(err)
	}
	storedBody, _ := json.Marshal(jobresult.StoredOutput{SchemaVersion: jobresult.StoredOutputSchemaVersion, Result: canonical, Compatibility: json.RawMessage(`{"source":"private"}`)})
	repository.artifact.SchemaVersion = jobresult.StoredOutputSchemaVersion
	handler.store = &fakeStore{body: storedBody}
	canonicalResponse := httptest.NewRecorder()
	handler.ServeHTTP(canonicalResponse, authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/result", nil))
	if canonicalResponse.Code != http.StatusOK || !strings.Contains(canonicalResponse.Body.String(), `"schemaVersion":"rin-render-result/v1"`) || strings.Contains(canonicalResponse.Body.String(), "private") {
		t.Fatalf("canonical result projection = %d %s", canonicalResponse.Code, canonicalResponse.Body.String())
	}

	repository.job.State = "queued"
	repository.job.ResultArtifactID = ""
	cancelRequest := authorizedRequest(http.MethodDelete, "/api/render/jobs/"+testJobID, nil)
	cancelResponse := httptest.NewRecorder()
	handler.ServeHTTP(cancelResponse, cancelRequest)
	if cancelResponse.Code != http.StatusOK || repository.cancelCalls != 1 {
		t.Fatalf("cancel = %d %s, calls %d", cancelResponse.Code, cancelResponse.Body.String(), repository.cancelCalls)
	}

	queueRequest := authorizedRequest(http.MethodGet, "/api/render/queue", nil)
	queueResponse := httptest.NewRecorder()
	handler.ServeHTTP(queueResponse, queueRequest)
	if queueResponse.Code != http.StatusOK || !strings.Contains(queueResponse.Body.String(), `"scope":"instance"`) {
		t.Fatalf("queue = %d %s", queueResponse.Code, queueResponse.Body.String())
	}

	repository.job.State = "canceled"
	eventRequest := authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/events", nil)
	eventRequest.Header.Set("Last-Event-ID", "7")
	eventResponse := httptest.NewRecorder()
	handler.ServeHTTP(eventResponse, eventRequest)
	if eventResponse.Code != http.StatusOK || repository.afterID != 7 ||
		!strings.Contains(eventResponse.Body.String(), "id: 8\nevent: canceled\n") {
		t.Fatalf("events = %d %q, after %d", eventResponse.Code, eventResponse.Body.String(), repository.afterID)
	}
}

func TestFailedPreviewStatusAndArtifactDownloadStayOwnerScoped(t *testing.T) {
	repository := newFakeRepository()
	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)
	logBody := []byte("/workspace/main.tex:7: Undefined control sequence\n")
	logDigest := sha256.Sum256(logBody)
	logHash := hex.EncodeToString(logDigest[:])
	logID := "render-jobs/v1/debug/" + testJobID + "/log-" + logHash + ".log"
	exitCode := 12
	failure := jobresult.StoredFailure{
		SchemaVersion: jobresult.StoredFailureSchemaVersion, Status: 422, Code: "pdf_compile_failed",
		Error: "LaTeX compilation failed", ExitCode: &exitCode, Diagnostics: []renderapi.Diagnostic{},
		Artifacts: []contracts.ArtifactReference{{
			ArtifactID: logID, SHA256: logHash, Bytes: int64(len(logBody)), MediaType: "text/plain; charset=utf-8",
			Visibility: "private", ExpiresAt: expires.Format(time.RFC3339),
		}},
	}
	failureBody, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	failureDigest := sha256.Sum256(failureBody)
	failureHash := hex.EncodeToString(failureDigest[:])
	resultID := "render-jobs/v1/result/" + testJobID + "/" + failureHash + ".json"
	repository.job.State = "failed"
	repository.job.DocumentEngine = "latexmk"
	repository.job.ResourceClass = "latex-pdf"
	repository.job.PriorityClass = "preview"
	repository.job.ResultArtifactID = resultID
	repository.job.RequestMetadata = json.RawMessage(`{"outputKind":"latex-pdf-preview","snapshotHash":"` + strings.Repeat("a", 64) + `","sessionId":"session-018fcafe","draftRevision":8,"enginePolicyId":"rinspace-latex-pdf-safe-v1","imageDigest":"sha256:` + strings.Repeat("b", 64) + `","entrypoint":"main.tex"}`)
	repository.artifacts[resultID] = jobpostgres.Artifact{
		ID: resultID, StorageKey: resultID, SHA256: failureHash, Kind: "result", Visibility: "private",
		ByteSize: int64(len(failureBody)), MediaType: "application/json", SchemaVersion: jobresult.StoredFailureSchemaVersion, ExpiresAt: &expires,
	}
	repository.artifacts[logID] = jobpostgres.Artifact{
		ID: logID, StorageKey: logID, SHA256: logHash, Kind: "debug", Visibility: "private",
		ByteSize: int64(len(logBody)), MediaType: "text/plain; charset=utf-8", ExpiresAt: &expires,
	}
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{bodies: map[string][]byte{
		resultID: failureBody, logID: logBody,
	}})

	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID, nil))
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"code":"compile_failed"`) ||
		!strings.Contains(statusResponse.Body.String(), `"exitCode":12`) || strings.Contains(statusResponse.Body.String(), "pdf_compile_failed") {
		t.Fatalf("failed preview status = %d %s", statusResponse.Code, statusResponse.Body.String())
	}

	artifactResponse := httptest.NewRecorder()
	handler.ServeHTTP(artifactResponse, authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/artifacts/log", nil))
	if artifactResponse.Code != http.StatusOK || artifactResponse.Body.String() != string(logBody) ||
		artifactResponse.Header().Get("X-Rinspace-Artifact-SHA256") != logHash ||
		artifactResponse.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("failed preview log = %d %q, headers %#v", artifactResponse.Code, artifactResponse.Body.String(), artifactResponse.Header())
	}

	wrongOwner := authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/artifacts/log", nil)
	wrongOwner.Header.Set("X-Rin-Renderer-Owner-Scope", "book-b")
	wrongOwnerResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongOwnerResponse, wrongOwner)
	if wrongOwnerResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-owner artifact = %d %s", wrongOwnerResponse.Code, wrongOwnerResponse.Body.String())
	}
}

// TestFailedDocumentStatusProjectsEngineDiagnostic keeps a failed document job
// from hiding the compiler diagnostic behind a generic transport summary. The
// Control Plane persists what this projection returns, so a Typst or LaTeX
// build error must reach the author instead of collapsing into render_failed.
// TestUnavailableDocumentFailureKeepsStatusServed documents that a committed
// document job with an unreadable stored failure still serves its coarse
// status, instead of degrading into the hard 503 reserved for draft previews.
func TestUnavailableDocumentFailureKeepsStatusServed(t *testing.T) {
	repository := newFakeRepository()
	repository.job.State = "failed"
	repository.job.ContentKind = "typst"
	repository.job.DocumentEngine = "typst"
	repository.job.RequestMetadata = json.RawMessage(`{"outputKind":"document"}`)
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID, nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("document status = %d %s", response.Code, response.Body.String())
	}
}

func TestFailedDocumentStatusProjectsEngineDiagnostic(t *testing.T) {
	repository := newFakeRepository()
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)
	compilerLog := "error: unknown variable: nt\n  ┌─ sections/intro.typ:8:3\n8 │ $\\int_a^b f(x) \\mathrm d x$"
	failure := jobresult.StoredFailure{
		SchemaVersion: jobresult.StoredFailureSchemaVersion, Status: 422, Code: "invalid_project_source",
		Error: "Typst HTML compilation failed",
		Diagnostics: []renderapi.Diagnostic{{
			Severity: "error", Code: "typst.compile.failed", Message: compilerLog, Engine: "typst",
		}},
	}
	failureBody, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	failureDigest := sha256.Sum256(failureBody)
	failureHash := hex.EncodeToString(failureDigest[:])
	resultID := "render-jobs/v1/result/" + testJobID + "/" + failureHash + ".json"
	repository.job.State = "failed"
	repository.job.ContentKind = "typst"
	repository.job.DocumentEngine = "typst"
	repository.job.ResourceClass = "typst-html"
	repository.job.PriorityClass = "publish"
	repository.job.ResultArtifactID = resultID
	repository.job.RequestMetadata = json.RawMessage(`{"outputKind":"document","entrypoint":"main.typ"}`)
	repository.artifacts[resultID] = jobpostgres.Artifact{
		ID: resultID, StorageKey: resultID, SHA256: failureHash, Kind: "result", Visibility: "private",
		ByteSize: int64(len(failureBody)), MediaType: "application/json", SchemaVersion: jobresult.StoredFailureSchemaVersion, ExpiresAt: &expires,
	}
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{bodies: map[string][]byte{resultID: failureBody}})

	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID, nil))
	body := statusResponse.Body.String()
	if statusResponse.Code != http.StatusOK || !strings.Contains(body, `"code":"compile_failed"`) ||
		!strings.Contains(body, `"category":"compile"`) || !strings.Contains(body, "unknown variable: nt") ||
		strings.Contains(body, "invalid_project_source") {
		t.Fatalf("failed document status = %d %s", statusResponse.Code, body)
	}
}

func TestSucceededPreviewArtifactDownloadUsesValidatedResultMetadata(t *testing.T) {
	repository := newFakeRepository()
	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)
	storeBodies := map[string][]byte{}
	media := map[string]string{
		"pdf": "application/pdf", "synctex": "application/gzip", "log": "text/plain; charset=utf-8",
		"aux": "text/plain; charset=utf-8", "fls": "text/plain; charset=utf-8",
	}
	extensions := map[string]string{"pdf": ".pdf", "synctex": ".synctex.gz", "log": ".log", "aux": ".aux", "fls": ".fls"}
	artifacts := make([]pdfexecutor.PreviewArtifact, 0, 5)
	var total int64
	for _, kind := range []string{"pdf", "synctex", "log", "aux", "fls"} {
		body := []byte("verified-" + kind)
		digest := sha256.Sum256(body)
		hash := hex.EncodeToString(digest[:])
		artifactID := "render-jobs/v1/debug/" + testJobID + "/" + kind + "-" + hash + extensions[kind]
		artifacts = append(artifacts, pdfexecutor.PreviewArtifact{
			ArtifactID: artifactID, Kind: kind, Path: ".rinspace/main" + extensions[kind], SHA256: hash,
			Bytes: int64(len(body)), MediaType: media[kind], Visibility: "private", ExpiresAt: expires.Format(time.RFC3339),
		})
		total += int64(len(body))
		storeBodies[artifactID] = body
		repository.artifacts[artifactID] = jobpostgres.Artifact{
			ID: artifactID, StorageKey: artifactID, SHA256: hash, Kind: "debug", Visibility: "private",
			ByteSize: int64(len(body)), MediaType: media[kind], ExpiresAt: &expires,
		}
	}
	result := pdfexecutor.PreviewResult{
		SchemaVersion: pdfexecutor.PreviewSchemaVersion, JobID: testJobID, RequestID: "request-preview",
		ContentKind: "latex", OutputKind: "latex-pdf-preview", DocumentEngine: "latexmk",
		ResourceClass: "latex-pdf", PriorityClass: "preview", SnapshotHash: strings.Repeat("a", 64),
		SessionID: "session-018fcafe", DraftRevision: 8, EnginePolicyID: pdfexecutor.PolicyID,
		ImageDigest: "sha256:" + strings.Repeat("b", 64), IdempotencyKey: strings.Repeat("c", 64),
		Entrypoint: "main.tex", WorkspaceRoot: "/workspace", Artifacts: artifacts, TotalArtifactBytes: total,
		Diagnostics: []renderapi.Diagnostic{}, Cache: pdfexecutor.PreviewCache{}, Versions: pdfexecutor.PreviewVersions{
			RinRenderer: "test", Latexmk: pdfexecutor.LatexmkVersion, TeXLive: pdfexecutor.TeXLiveVersion,
			CompilerImage: "sha256:" + strings.Repeat("b", 64),
		},
	}
	resultBody, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	resultDigest := sha256.Sum256(resultBody)
	resultHash := hex.EncodeToString(resultDigest[:])
	resultID := "render-jobs/v1/result/" + testJobID + "/" + resultHash + ".json"
	storeBodies[resultID] = resultBody
	repository.artifacts[resultID] = jobpostgres.Artifact{
		ID: resultID, StorageKey: resultID, SHA256: resultHash, Kind: "result", Visibility: "private",
		ByteSize: int64(len(resultBody)), MediaType: "application/json", SchemaVersion: pdfexecutor.PreviewSchemaVersion, ExpiresAt: &expires,
	}
	repository.job.State = "succeeded"
	repository.job.DocumentEngine = "latexmk"
	repository.job.ResourceClass = "latex-pdf"
	repository.job.PriorityClass = "preview"
	repository.job.ResultArtifactID = resultID
	repository.job.RequestMetadata = json.RawMessage(`{"outputKind":"latex-pdf-preview"}`)
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{bodies: storeBodies})

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/artifacts/pdf", nil))
	if response.Code != http.StatusOK || response.Body.String() != "verified-pdf" ||
		response.Header().Get("Content-Type") != "application/pdf" ||
		response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("succeeded preview PDF = %d %q, headers %#v", response.Code, response.Body.String(), response.Header())
	}
}

func TestSupportInspectionAndActionsStayOwnerScopedAndPrivate(t *testing.T) {
	repository := newFakeRepository()
	repository.job.State = "failed"
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{})

	supportResponse := httptest.NewRecorder()
	handler.ServeHTTP(supportResponse, authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/support", nil))
	if supportResponse.Code != http.StatusOK || !strings.Contains(supportResponse.Body.String(), `"estimatorVersion":"rin-wait-estimator/v1"`) ||
		strings.Contains(supportResponse.Body.String(), "principal-a") || strings.Contains(supportResponse.Body.String(), "book-a") ||
		strings.Contains(supportResponse.Body.String(), "storageKey") || strings.Contains(supportResponse.Body.String(), "private/path") {
		t.Fatalf("support projection = %d %s", supportResponse.Code, supportResponse.Body.String())
	}
	wrongOwner := authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/support", nil)
	wrongOwner.Header.Set("X-Rin-Renderer-Owner-Scope", "book-b")
	wrongOwnerResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongOwnerResponse, wrongOwner)
	if wrongOwnerResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-owner support = %d %s", wrongOwnerResponse.Code, wrongOwnerResponse.Body.String())
	}
	retryResponse := httptest.NewRecorder()
	handler.ServeHTTP(retryResponse, authorizedRequest(http.MethodPost, "/api/render/jobs/"+testJobID+"/retry", nil))
	if retryResponse.Code != http.StatusOK || repository.retryCalls != 1 || !strings.Contains(retryResponse.Body.String(), `"state":"queued"`) {
		t.Fatalf("support retry = %d %s", retryResponse.Code, retryResponse.Body.String())
	}
	repository.job.State = "failed"
	expireResponse := httptest.NewRecorder()
	handler.ServeHTTP(expireResponse, authorizedRequest(http.MethodPost, "/api/render/jobs/"+testJobID+"/expire", nil))
	if expireResponse.Code != http.StatusOK || repository.expireCalls != 1 || !strings.Contains(expireResponse.Body.String(), `"state":"expired"`) {
		t.Fatalf("support expire = %d %s", expireResponse.Code, expireResponse.Body.String())
	}
}

func TestSubmissionUsesAuthenticatedIdentityAndReturns202(t *testing.T) {
	admit := &fakeAdmission{}
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("contentKind", "markdown")
	_ = writer.WriteField("documentEngine", "unified")
	_ = writer.WriteField("priorityIntent", "publish")
	file, err := writer.CreateFormFile("source", "book.zip")
	if err != nil {
		t.Fatal(err)
	}
	archive := markdownTestArchive(t, "article.md", "# Article")
	_, _ = file.Write(archive)
	_ = writer.WriteField("entrypoint", "article.md")
	_ = writer.Close()
	request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", "publish-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Location") != "/api/render/jobs/"+testJobID {
		t.Fatalf("submit = %d %s, location %q", response.Code, response.Body.String(), response.Header().Get("Location"))
	}
	if !strings.Contains(response.Body.String(), `"queue":{"jobsAheadEstimate":3`) ||
		!strings.Contains(response.Body.String(), `"estimate":null`) {
		t.Fatalf("submission queue summary missing: %s", response.Body.String())
	}
	if admit.request.PrincipalID != "principal-a" || admit.request.OwnerScope != "book-a" ||
		admit.request.ContentKind != "markdown" || admit.request.ResourceClass != "document-light" {
		t.Fatalf("admission request = %#v", admit.request)
	}
}

func TestSubmissionValidatesAndMapsLatexPDFPreview(t *testing.T) {
	admit := &fakeAdmission{}
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	snapshotHash := strings.Repeat("a", 64)
	sessionID := "session-018fcafe"
	entrypoint := "chapters/main.tex"
	policyID := "rinspace-latex-pdf-safe-v1"
	imageDigest := "sha256:" + strings.Repeat("b", 64)
	identity := sha256.Sum256([]byte(sessionID + snapshotHash + entrypoint + policyID + imageDigest))
	idempotencyKey := hex.EncodeToString(identity[:])

	build := func(key string) *http.Request {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for name, value := range map[string]string{
			"contentKind": "latex", "outputKind": "latex-pdf-preview", "documentEngine": "latexmk",
			"priorityIntent": "preview", "priorityClass": "preview", "resourceClass": "latex-pdf",
			"snapshotHash": snapshotHash, "sessionId": sessionID, "draftRevision": "42",
			"entrypoint": entrypoint, "enginePolicyId": policyID, "imageDigest": imageDigest,
			"previewContractVersion": "rin-latex-pdf-preview/v1",
		} {
			_ = writer.WriteField(name, value)
		}
		file, _ := writer.CreateFormFile("source", "snapshot.tar")
		_, _ = file.Write([]byte("bounded deterministic tar fixture"))
		_ = writer.Close()
		request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Idempotency-Key", key)
		return request
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, build(idempotencyKey))
	if response.Code != http.StatusAccepted {
		t.Fatalf("preview submit = %d %s", response.Code, response.Body.String())
	}
	if admit.request.ResourceClass != "latex-pdf" || admit.request.PriorityIntent != "preview" ||
		admit.request.ProjectHash != snapshotHash || admit.request.SourceMediaType != "application/x-tar" ||
		admit.request.SourceSchemaVersion != "rinspace-snapshot/v1" {
		t.Fatalf("preview admission mapping = %#v", admit.request)
	}
	if !strings.Contains(string(admit.request.RequestMetadata), `"draftRevision":42`) ||
		!strings.Contains(string(admit.request.RequestMetadata), `"outputKind":"latex-pdf-preview"`) {
		t.Fatalf("preview metadata = %s", admit.request.RequestMetadata)
	}

	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, build(strings.Repeat("f", 64)))
	if rejected.Code != http.StatusUnprocessableEntity || !strings.Contains(rejected.Body.String(), "invalid_preview_identity") {
		t.Fatalf("invalid preview identity = %d %s", rejected.Code, rejected.Body.String())
	}
}

func TestPreviewStatusProjectsBoundedProtocolMetadata(t *testing.T) {
	job := jobpostgres.Job{
		ID: testJobID, ContentKind: "latex", DocumentEngine: "latexmk", ResourceClass: "latex-pdf",
		PriorityClass: "preview", State: "queued", RequestMetadata: json.RawMessage(`{
			"outputKind":"latex-pdf-preview","snapshotHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sessionId":"session-018fcafe","draftRevision":42,"enginePolicyId":"safe-v1",
			"imageDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","entrypoint":"main.tex"
		}`),
	}
	value := statusValue(job)
	if value["outputKind"] != "latex-pdf-preview" || value["draftRevision"] != float64(42) ||
		value["resourceClass"] != "latex-pdf" || value["priorityClass"] != "preview" {
		t.Fatalf("preview status = %#v", value)
	}
}

func TestCanonicalPreviewPathMatchesFixedCompiler(t *testing.T) {
	for _, accepted := range []string{"main.tex", "章节/正文.tex"} {
		if !canonicalPreviewPath(accepted) {
			t.Fatalf("canonicalPreviewPath(%q) rejected safe path", accepted)
		}
	}
	for _, rejected := range []string{"-shell.tex", "main..tex", "a//b.tex", "../main.tex", "main.pdf"} {
		if canonicalPreviewPath(rejected) {
			t.Fatalf("canonicalPreviewPath(%q) accepted unsafe path", rejected)
		}
	}
	for _, accepted := range []string{"main.typ", "章节/正文.typ"} {
		if !canonicalTypstEntrypoint(accepted) {
			t.Fatalf("canonicalTypstEntrypoint(%q) rejected safe path", accepted)
		}
	}
	for _, rejected := range []string{"-shell.typ", "main..typ", "a//b.typ", "../main.typ", "main.tex"} {
		if canonicalTypstEntrypoint(rejected) {
			t.Fatalf("canonicalTypstEntrypoint(%q) accepted unsafe path", rejected)
		}
	}
}

func TestSubmissionValidatesAndMapsTypstPDFPreview(t *testing.T) {
	admit := &fakeAdmission{}
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	snapshotHash := strings.Repeat("c", 64)
	sessionID := "session-018fcafe-typst"
	entrypoint := "chapters/正文.typ"
	policyID := typstpdfexecutor.PolicyID
	imageDigest := "sha256:" + strings.Repeat("d", 64)
	identity := sha256.Sum256([]byte(sessionID + snapshotHash + entrypoint + policyID + imageDigest))
	idempotencyKey := hex.EncodeToString(identity[:])

	build := func(key string, overrides map[string]string) *http.Request {
		fields := map[string]string{
			"contentKind": "typst", "outputKind": "typst-pdf-preview", "documentEngine": "typst",
			"priorityIntent": "preview", "priorityClass": "preview", "resourceClass": "typst-pdf",
			"snapshotHash": snapshotHash, "sessionId": sessionID, "draftRevision": "17",
			"entrypoint": entrypoint, "enginePolicyId": policyID, "imageDigest": imageDigest,
			"previewContractVersion": "rin-typst-pdf-preview/v1",
		}
		for name, value := range overrides {
			fields[name] = value
		}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for name, value := range fields {
			_ = writer.WriteField(name, value)
		}
		file, _ := writer.CreateFormFile("source", "snapshot.tar")
		_, _ = file.Write([]byte("bounded deterministic typst snapshot fixture"))
		_ = writer.Close()
		request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Idempotency-Key", key)
		return request
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, build(idempotencyKey, nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("Typst preview submit = %d %s", response.Code, response.Body.String())
	}
	if admit.request.ContentKind != "typst" || admit.request.ResourceClass != "typst-pdf" ||
		admit.request.PriorityIntent != "preview" || admit.request.ProjectHash != snapshotHash ||
		admit.request.SourceMediaType != "application/x-tar" || admit.request.SourceSchemaVersion != "rinspace-snapshot/v1" {
		t.Fatalf("Typst preview admission mapping = %#v", admit.request)
	}
	if !strings.Contains(string(admit.request.RequestMetadata), `"draftRevision":17`) ||
		!strings.Contains(string(admit.request.RequestMetadata), `"outputKind":"typst-pdf-preview"`) {
		t.Fatalf("Typst preview metadata = %s", admit.request.RequestMetadata)
	}

	for name, overrides := range map[string]map[string]string{
		"latex contract":       {"previewContractVersion": "rin-latex-pdf-preview/v1"},
		"latex resource class": {"resourceClass": "latex-pdf"},
		"latexmk engine":       {"documentEngine": "latexmk"},
		"tex entrypoint":       {"entrypoint": "main.tex"},
		"preview intent":       {"priorityIntent": "publish", "priorityClass": "publish"},
	} {
		t.Run(name, func(t *testing.T) {
			rejected := httptest.NewRecorder()
			handler.ServeHTTP(rejected, build(strings.Repeat("f", 64), overrides))
			if rejected.Code != http.StatusUnprocessableEntity && rejected.Code != http.StatusBadRequest {
				t.Fatalf("invalid Typst preview identity = %d %s", rejected.Code, rejected.Body.String())
			}
			if !strings.Contains(rejected.Body.String(), "invalid_") {
				t.Fatalf("invalid Typst preview error = %s", rejected.Body.String())
			}
		})
	}
}

func TestSubmissionRequiresPublishedCommitForTypstPDFExport(t *testing.T) {
	now := time.Now()
	build := func(requestID string) (*http.Request, *fakeAdmission) {
		t.Helper()
		admit := &fakeAdmission{}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for name, value := range map[string]string{
			"contentKind": "typst", "outputKind": "typst-pdf-export", "documentEngine": "typst",
			"priorityIntent": "rebuild", "priorityClass": "rebuild", "resourceClass": "typst-pdf",
			"entrypoint": "main.typ", "requestId": requestID,
			"controlProjectId": "book:77", "sourceCommit": strings.Repeat("a", 40),
			"controlProjectHash": strings.Repeat("b", 64),
			"enginePolicyId":     typstpdfexecutor.PolicyID,
			"imageDigest":        "sha256:" + strings.Repeat("c", 64),
		} {
			_ = writer.WriteField(name, value)
		}
		file, _ := writer.CreateFormFile("source", "published.tar")
		_, _ = file.Write([]byte("bounded deterministic published typst archive"))
		_ = writer.Close()
		request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
		request.Header.Set("X-Rin-Renderer-Owner-Scope", "book:77")
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Idempotency-Key", "typst-export")
		return request, admit
	}

	request, admit := build("")
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("Typst export submit = %d %s", response.Code, response.Body.String())
	}
	if admit.request.ResourceClass != "typst-pdf" || admit.request.PriorityIntent != "rebuild" ||
		admit.request.ProjectHash != admit.request.ExpectedSourceSHA256 {
		t.Fatalf("Typst export admission mapping = %#v", admit.request)
	}
	var metadata struct {
		OutputKind         string `json:"outputKind"`
		ControlProjectID   string `json:"controlProjectId"`
		SourceCommit       string `json:"sourceCommit"`
		ControlProjectHash string `json:"controlProjectHash"`
		SnapshotHash       string `json:"snapshotHash"`
	}
	if err := json.Unmarshal(admit.request.RequestMetadata, &metadata); err != nil ||
		metadata.OutputKind != "typst-pdf-export" || metadata.ControlProjectID != "book:77" ||
		metadata.SourceCommit != strings.Repeat("a", 40) || metadata.ControlProjectHash != strings.Repeat("b", 64) ||
		metadata.SnapshotHash != "" {
		t.Fatalf("Typst export metadata = %#v, %v", metadata, err)
	}

	withoutCommit, _ := build("")
	withoutCommitHandler := newTestHandler(t, &fakeAdmission{}, newFakeRepository(), &fakeStore{})
	withoutCommit = rewriteMultipartField(withoutCommit, "sourceCommit", "")
	missingResponse := httptest.NewRecorder()
	withoutCommitHandler.ServeHTTP(missingResponse, withoutCommit)
	if missingResponse.Code != http.StatusUnprocessableEntity || !strings.Contains(missingResponse.Body.String(), "invalid_export_identity") {
		t.Fatalf("Typst export without commit = %d %s", missingResponse.Code, missingResponse.Body.String())
	}

	for name, field := range map[string]string{"image digest": "imageDigest", "engine policy": "enginePolicyId"} {
		t.Run(name, func(t *testing.T) {
			unpinned, _ := build("")
			unpinned = rewriteMultipartField(unpinned, field, "")
			unpinnedHandler := newTestHandler(t, &fakeAdmission{}, newFakeRepository(), &fakeStore{})
			unpinnedResponse := httptest.NewRecorder()
			unpinnedHandler.ServeHTTP(unpinnedResponse, unpinned)
			if unpinnedResponse.Code != http.StatusUnprocessableEntity || !strings.Contains(unpinnedResponse.Body.String(), "invalid_export_identity") {
				t.Fatalf("Typst export without pinned %s = %d %s", field, unpinnedResponse.Code, unpinnedResponse.Body.String())
			}
		})
	}
	_ = now
}

func TestSubmissionRejectsTypstExportBoundaryViolations(t *testing.T) {
	build := func() *http.Request {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for name, value := range map[string]string{
			"contentKind": "typst", "outputKind": "typst-pdf-export", "documentEngine": "typst",
			"priorityIntent": "rebuild", "priorityClass": "rebuild", "resourceClass": "typst-pdf",
			"entrypoint":       "main.typ",
			"controlProjectId": "book:77", "sourceCommit": strings.Repeat("a", 40),
			"controlProjectHash": strings.Repeat("b", 64),
			"enginePolicyId":     typstpdfexecutor.PolicyID,
			"imageDigest":        "sha256:" + strings.Repeat("c", 64),
		} {
			_ = writer.WriteField(name, value)
		}
		file, _ := writer.CreateFormFile("source", "published.tar")
		_, _ = file.Write([]byte("bounded deterministic published typst archive"))
		_ = writer.Close()
		request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
		request.Header.Set("X-Rin-Renderer-Owner-Scope", "book:77")
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Idempotency-Key", "typst-export")
		return request
	}

	for name, mutate := range map[string]func(*http.Request) *http.Request{
		"session identity leak": func(request *http.Request) *http.Request {
			return rewriteMultipartField(request, "sessionId", "preview-session-1")
		},
		"snapshot hash leak": func(request *http.Request) *http.Request {
			return rewriteMultipartField(request, "snapshotHash", strings.Repeat("d", 64))
		},
		"preview contract leak": func(request *http.Request) *http.Request {
			return rewriteMultipartField(request, "previewContractVersion", "typst-preview/v1")
		},
		"preview resource class": func(request *http.Request) *http.Request {
			return rewriteMultipartField(request, "resourceClass", "document-typst")
		},
		"mismatched priority class": func(request *http.Request) *http.Request {
			return rewriteMultipartField(request, "priorityClass", "preview")
		},
	} {
		t.Run(name, func(t *testing.T) {
			handler := newTestHandler(t, &fakeAdmission{}, newFakeRepository(), &fakeStore{})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, mutate(build()))
			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_export_identity") {
				t.Fatalf("Typst export with %s = %d %s", name, response.Code, response.Body.String())
			}
		})
	}

	t.Run("owner scope mismatch", func(t *testing.T) {
		handler := newTestHandler(t, &fakeAdmission{}, newFakeRepository(), &fakeStore{})
		request := rewriteMultipartField(build(), "controlProjectId", "book:78")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_publication_identity") {
			t.Fatalf("Typst export cross-scope = %d %s", response.Code, response.Body.String())
		}
	})
}

func TestSubmissionRejectsTypstMasqueradingAsLatexPreview(t *testing.T) {
	handler := newTestHandler(t, &fakeAdmission{}, newFakeRepository(), &fakeStore{})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range map[string]string{
		"contentKind": "latex", "outputKind": "typst-pdf-preview", "documentEngine": "typst",
		"priorityIntent": "preview", "priorityClass": "preview", "resourceClass": "typst-pdf",
		"snapshotHash": strings.Repeat("a", 64), "sessionId": "session-018fcafe-x", "draftRevision": "1",
		"entrypoint": "main.typ", "enginePolicyId": typstpdfexecutor.PolicyID,
		"imageDigest":            "sha256:" + strings.Repeat("b", 64),
		"previewContractVersion": "rin-typst-pdf-preview/v1",
	} {
		_ = writer.WriteField(name, value)
	}
	file, _ := writer.CreateFormFile("source", "snapshot.tar")
	_, _ = file.Write([]byte("bounded deterministic snapshot fixture"))
	_ = writer.Close()
	request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", strings.Repeat("e", 64))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_document_engine") {
		t.Fatalf("Typst preview under latex content kind = %d %s", response.Code, response.Body.String())
	}
}

func TestTypstDocumentSubmitRequiresConfiguredRenderProfile(t *testing.T) {
	submit := func(t *testing.T, profile string) (*httptest.ResponseRecorder, *fakeAdmission) {
		t.Helper()
		admit := &fakeAdmission{}
		handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for name, value := range map[string]string{
			"contentKind": "typst", "documentEngine": "typst", "priorityIntent": "publish",
			"entrypoint": "main.typ", "documentMode": "article",
		} {
			_ = writer.WriteField(name, value)
		}
		if profile != "" {
			_ = writer.WriteField("renderProfileId", profile)
		}
		file, _ := writer.CreateFormFile("source", "project.zip")
		_, _ = file.Write([]byte("bounded deterministic typst fixture"))
		_ = writer.Close()
		request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Idempotency-Key", strings.Repeat("f", 64))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response, admit
	}

	if response, _ := submit(t, ""); response.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(response.Body.String(), "invalid_render_profile") {
		t.Fatalf("missing Typst render profile = %d %s", response.Code, response.Body.String())
	}
	if response, _ := submit(t, "typst-html-stale000"); response.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(response.Body.String(), "invalid_render_profile") {
		t.Fatalf("stale Typst render profile = %d %s", response.Code, response.Body.String())
	}
	response, admit := submit(t, "typst-html-testprofile")
	if response.Code != http.StatusAccepted {
		t.Fatalf("configured Typst render profile = %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(string(admit.request.RequestMetadata), `"renderProfileId":"typst-html-testprofile"`) {
		t.Fatalf("Typst render profile missing from job metadata: %s", string(admit.request.RequestMetadata))
	}
}

func TestTypstDocumentStatusProjectsRenderProfile(t *testing.T) {
	job := jobpostgres.Job{
		ID: testJobID, ContentKind: "typst", DocumentEngine: "typst", ResourceClass: "document-typst",
		PriorityClass: "publish", State: "running",
		RequestMetadata: json.RawMessage(`{"documentMode":"article","renderProfileId":"typst-html-testprofile"}`),
	}
	if value := statusValue(job); value["renderProfileId"] != "typst-html-testprofile" {
		t.Fatalf("Typst document status = %#v", value)
	}
	preview := jobpostgres.Job{
		ID: testJobID, ContentKind: "typst", DocumentEngine: "typst", ResourceClass: "typst-pdf",
		PriorityClass: "preview", State: "queued",
		RequestMetadata: json.RawMessage(`{"outputKind":"typst-pdf-preview","renderProfileId":""}`),
	}
	if _, ok := statusValue(preview)["renderProfileId"]; ok {
		t.Fatal("a job without a pinned render profile must not project one")
	}
}

func TestRenderProfileIdRejectedOutsideTypstDocumentJobs(t *testing.T) {
	handler := newTestHandler(t, &fakeAdmission{}, newFakeRepository(), &fakeStore{})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range map[string]string{
		"contentKind": "markdown", "documentEngine": "unified", "priorityIntent": "publish",
		"entrypoint": "main.md", "renderProfileId": "typst-html-testprofile",
	} {
		_ = writer.WriteField(name, value)
	}
	file, _ := writer.CreateFormFile("source", "project.zip")
	_, _ = file.Write([]byte("archive"))
	_ = writer.Close()
	request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", strings.Repeat("a", 64))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_job_request") {
		t.Fatalf("render profile outside Typst documents = %d %s", response.Code, response.Body.String())
	}
}

func TestTypstPreviewStatusProjectsBoundedProtocolMetadata(t *testing.T) {
	job := jobpostgres.Job{
		ID: testJobID, ContentKind: "typst", DocumentEngine: "typst", ResourceClass: "typst-pdf",
		PriorityClass: "preview", State: "queued", RequestMetadata: json.RawMessage(`{
			"outputKind":"typst-pdf-preview","snapshotHash":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			"sessionId":"session-018fcafe-typst","draftRevision":17,"enginePolicyId":"rinspace-typst-pdf-safe-v1",
			"imageDigest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","entrypoint":"main.typ"
		}`),
	}
	value := statusValue(job)
	if value["outputKind"] != "typst-pdf-preview" || value["draftRevision"] != float64(17) ||
		value["resourceClass"] != "typst-pdf" || value["sessionId"] != "session-018fcafe-typst" {
		t.Fatalf("Typst preview status = %#v", value)
	}
	var exportMetadata map[string]any
	if err := json.Unmarshal([]byte(`{"outputKind":"typst-pdf-export"}`), &exportMetadata); err != nil {
		t.Fatal(err)
	}
	if previewOutputKind(exportMetadata) {
		t.Fatal("Typst PDF export must not project preview metadata")
	}
}

func TestTypstPDFPreviewArtifactReferenceUsesTypstResult(t *testing.T) {
	repository := newFakeRepository()
	expires := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	pdfBody := []byte("verified-typst-pdf")
	pdfDigest := sha256Hex(pdfBody)
	pdfArtifactID := "render-jobs/v1/typst/" + testJobID + "/" + pdfDigest + ".pdf"
	body, _ := json.Marshal(typstpdfexecutor.Result{
		SchemaVersion: typstpdfexecutor.ResultSchemaVersion, JobID: testJobID, RequestID: testJobID,
		ContentKind: "typst", OutputKind: typstpdfexecutor.PreviewOutputKind, DocumentEngine: "typst",
		ResourceClass: "typst-pdf", PriorityClass: "preview",
		IdempotencyKey:  typstpdfexecutor.PreviewIdempotencyKey("session-018fcafe-typst", strings.Repeat("c", 64), "main.typ", typstpdfexecutor.PolicyID, "sha256:"+strings.Repeat("d", 64)),
		RenderProfileID: "typst-web.v1", Entrypoint: "main.typ", WorkspaceRoot: "/workspace",
		SnapshotHash: strings.Repeat("c", 64), SessionID: "session-018fcafe-typst", DraftRevision: int64Pointer(17),
		EnginePolicyID: typstpdfexecutor.PolicyID, ImageDigest: "sha256:" + strings.Repeat("d", 64),
		Artifacts: []typstpdfexecutor.Artifact{{
			ArtifactID: pdfArtifactID,
			Kind:       "pdf", Path: ".rinspace/typst/" + pdfDigest + ".pdf",
			SHA256: pdfDigest, Bytes: int64(len(pdfBody)), MediaType: "application/pdf",
			Visibility: "private", ExpiresAt: expires.Format(time.RFC3339),
		}},
		TotalArtifactBytes: int64(len(pdfBody)), Diagnostics: []renderapi.Diagnostic{},
		Versions: typstpdfexecutor.Versions{RinRenderer: "test", Typst: "0.15.1", CompilerImage: "sha256:" + strings.Repeat("d", 64)},
	})
	repository.job.State = "succeeded"
	repository.job.ContentKind = "typst"
	repository.job.DocumentEngine = "typst"
	repository.job.ResourceClass = "typst-pdf"
	repository.job.ResultArtifactID = "typst-result"
	repository.job.RequestMetadata = json.RawMessage(`{"outputKind":"typst-pdf-preview"}`)
	repository.artifacts["typst-result"] = jobpostgres.Artifact{
		ID: "typst-result", StorageKey: "typst-result", SHA256: sha256Hex(body),
		Kind: "result", Visibility: "private", ByteSize: int64(len(body)), MediaType: "application/json",
		SchemaVersion: typstpdfexecutor.ResultSchemaVersion, ExpiresAt: &expires,
	}
	repository.artifacts[pdfArtifactID] = jobpostgres.Artifact{
		ID: pdfArtifactID, StorageKey: pdfArtifactID, SHA256: pdfDigest, Kind: "debug", Visibility: "private",
		ByteSize: int64(len(pdfBody)), MediaType: "application/pdf", ExpiresAt: &expires,
	}
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{bodies: map[string][]byte{
		"typst-result": body, pdfArtifactID: pdfBody,
	}})
	for _, test := range []struct {
		kind     string
		expected string
	}{
		{kind: "pdf", expected: "verified-typst-pdf"},
		{kind: "synctex", expected: ""},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/artifacts/"+test.kind, nil))
		if test.expected == "" {
			if response.Code != http.StatusNotFound {
				t.Fatalf("Typst preview %s artifact = %d %s", test.kind, response.Code, response.Body.String())
			}
			continue
		}
		if response.Code != http.StatusOK || response.Body.String() != test.expected ||
			response.Header().Get("X-Rinspace-Artifact-SHA256") != pdfDigest {
			t.Fatalf("Typst preview %s artifact = %d %q %s", test.kind, response.Code, response.Body.String(), response.Header())
		}
	}
}

func int64Pointer(value int64) *int64 { return &value }

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func rewriteMultipartField(request *http.Request, name string, value string) *http.Request {
	_ = request.ParseMultipartForm(1 << 20)
	fields := request.MultipartForm.Value
	fields[name] = []string{value}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for field, values := range fields {
		for _, candidate := range values {
			_ = writer.WriteField(field, candidate)
		}
	}
	file, _ := writer.CreateFormFile("source", "published.tar")
	_, _ = file.Write([]byte("bounded deterministic published typst archive"))
	_ = writer.Close()
	rebuilt := authorizedRequest(request.Method, request.URL.Path, &body)
	rebuilt.Header.Set("X-Rin-Renderer-Owner-Scope", request.Header.Get("X-Rin-Renderer-Owner-Scope"))
	rebuilt.Header.Set("Content-Type", writer.FormDataContentType())
	rebuilt.Header.Set("Idempotency-Key", request.Header.Get("Idempotency-Key"))
	return rebuilt
}

func TestSubmissionPersistsCompleteControlPlaneIdentity(t *testing.T) {
	buildRequest := func(requestID string) (*http.Request, *fakeAdmission) {
		t.Helper()
		admit := &fakeAdmission{}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("contentKind", "markdown")
		_ = writer.WriteField("documentEngine", "unified")
		_ = writer.WriteField("priorityIntent", "publish")
		_ = writer.WriteField("requestId", requestID)
		_ = writer.WriteField("controlProjectId", "article:42")
		_ = writer.WriteField("sourceCommit", strings.Repeat("a", 40))
		_ = writer.WriteField("controlProjectHash", strings.Repeat("b", 64))
		file, err := writer.CreateFormFile("source", "article.zip")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write(markdownTestArchive(t, "article.md", "# Article"))
		_ = writer.WriteField("entrypoint", "article.md")
		_ = writer.Close()
		request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
		request.Header.Set("X-Rin-Renderer-Owner-Scope", "article:42")
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Idempotency-Key", "control-plane-submit")
		return request, admit
	}

	request, admit := buildRequest("render-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("Control Plane submit = %d %s", response.Code, response.Body.String())
	}
	var metadata struct {
		RequestID          string `json:"requestId"`
		ControlProjectID   string `json:"controlProjectId"`
		SourceCommit       string `json:"sourceCommit"`
		ControlProjectHash string `json:"controlProjectHash"`
	}
	if err := json.Unmarshal(admit.request.RequestMetadata, &metadata); err != nil ||
		metadata.RequestID != "render-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || metadata.ControlProjectID != "article:42" ||
		metadata.SourceCommit != strings.Repeat("a", 40) || metadata.ControlProjectHash != strings.Repeat("b", 64) {
		t.Fatalf("stored Control Plane identity = %#v, %v", metadata, err)
	}

	missingRequestID, missingAdmit := buildRequest("")
	missingHandler := newTestHandler(t, missingAdmit, newFakeRepository(), &fakeStore{})
	missingResponse := httptest.NewRecorder()
	missingHandler.ServeHTTP(missingResponse, missingRequestID)
	if missingResponse.Code != http.StatusBadRequest || !strings.Contains(missingResponse.Body.String(), "invalid_publication_identity") {
		t.Fatalf("partial Control Plane identity = %d %s", missingResponse.Code, missingResponse.Body.String())
	}

	localRequest, localAdmit := buildRequest("render-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	localHandler := newTestHandler(t, localAdmit, newFakeRepository(), &fakeStore{})
	localHandler.config.RejectControlPlaneIdentity = true
	localResponse := httptest.NewRecorder()
	localHandler.ServeHTTP(localResponse, localRequest)
	if localResponse.Code != http.StatusBadRequest || !strings.Contains(localResponse.Body.String(), "control_plane_unavailable") {
		t.Fatalf("local Control Plane submission = %d %s", localResponse.Code, localResponse.Body.String())
	}
	if localAdmit.request.OwnerScope != "" {
		t.Fatal("local mode admitted a Control Plane publication job")
	}
}

func TestSubmissionAcceptsCanonicalMarkdownBookManifest(t *testing.T) {
	admit := &fakeAdmission{}
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("contentKind", "markdown")
	_ = writer.WriteField("documentEngine", "unified")
	_ = writer.WriteField("priorityIntent", "publish")
	_ = writer.WriteField("documentMode", "book")
	_ = writer.WriteField("title", "Book")
	_ = writer.WriteField("bookPages", `[{"path":"intro.md","id":"intro","title":"Intro"},{"path":"chapter.md","id":"chapter","title":"Chapter"}]`)
	file, err := writer.CreateFormFile("source", "book.zip")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write(markdownBookTestArchive(t))
	_ = writer.Close()
	request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", "publish-book-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit Markdown Book = %d %s", response.Code, response.Body.String())
	}
	var metadata struct {
		DocumentMode string                             `json:"documentMode"`
		BookPages    []struct{ Path, ID, Title string } `json:"bookPages"`
	}
	if err := json.Unmarshal(admit.request.RequestMetadata, &metadata); err != nil || metadata.DocumentMode != "book" ||
		len(metadata.BookPages) != 2 || metadata.BookPages[0].Path != "intro.md" || metadata.BookPages[1].ID != "chapter" {
		t.Fatalf("stored Markdown Book request metadata = %#v, %v", metadata, err)
	}
	if admit.request.ProjectHash == strings.Repeat("0", 64) || admit.request.OptionsHash == "" {
		t.Fatalf("Markdown Book identities were not computed: %#v", admit.request)
	}
}

func TestSubmissionAcceptsLaTeXBookModeWithoutMarkdownPageManifest(t *testing.T) {
	admit := &fakeAdmission{}
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("contentKind", "latex")
	_ = writer.WriteField("documentEngine", "auto")
	_ = writer.WriteField("priorityIntent", "publish")
	_ = writer.WriteField("documentMode", "book")
	_ = writer.WriteField("entrypoint", "main.tex")
	file, err := writer.CreateFormFile("source", "book.zip")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write(markdownTestArchive(t, "main.tex", `\documentclass{book}\begin{document}\chapter{One}\section{A}\end{document}`))
	_ = writer.Close()
	request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", "publish-latex-book-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit LaTeX Book = %d %s", response.Code, response.Body.String())
	}
	var metadata struct {
		DocumentMode string `json:"documentMode"`
	}
	if err := json.Unmarshal(admit.request.RequestMetadata, &metadata); err != nil || metadata.DocumentMode != "book" {
		t.Fatalf("stored LaTeX Book request metadata = %#v, %v", metadata, err)
	}
}

func TestSubmissionRejectsInvalidMarkdownBookManifest(t *testing.T) {
	admit := &fakeAdmission{}
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("contentKind", "markdown")
	_ = writer.WriteField("documentEngine", "unified")
	_ = writer.WriteField("priorityIntent", "publish")
	_ = writer.WriteField("documentMode", "book")
	_ = writer.WriteField("bookPages", `[{"path":"intro.md","id":"intro"}] {}`)
	file, _ := writer.CreateFormFile("source", "book.zip")
	_, _ = file.Write(markdownTestArchive(t, "intro.md", "# Intro"))
	_ = writer.Close()
	request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", "publish-book-invalid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_book_manifest") || admit.request.Source != nil {
		t.Fatalf("invalid Markdown Book manifest = %d %s", response.Code, response.Body.String())
	}
}

func TestSubmissionAcceptsSinglePageMarkdownBookManifest(t *testing.T) {
	admit := &fakeAdmission{}
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("contentKind", "markdown")
	_ = writer.WriteField("documentEngine", "unified")
	_ = writer.WriteField("priorityIntent", "publish")
	_ = writer.WriteField("documentMode", "book")
	_ = writer.WriteField("bookPages", `[{"path":"intro.md","id":"intro","title":"Intro"}]`)
	file, _ := writer.CreateFormFile("source", "book.zip")
	_, _ = file.Write(markdownBookTestArchive(t))
	_ = writer.Close()
	request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", "publish-book-single-page")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("single-page Markdown Book manifest = %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(string(admit.request.RequestMetadata), "intro") {
		t.Fatalf("expected single-page manifest metadata, got %s", admit.request.RequestMetadata)
	}
}

func markdownTestArchive(t *testing.T, path string, source string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	file, err := writer.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, source)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func markdownBookTestArchive(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	for _, item := range []struct{ path, source string }{{"intro.md", "# Intro"}, {"chapter.md", "# Chapter"}} {
		file, err := writer.Create(item.path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(file, item.source)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestSubmissionRejectsMissingIdempotencyBeforeReadingBody(t *testing.T) {
	admit := &fakeAdmission{}
	handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
	body := &countingReader{reader: strings.NewReader("not multipart")}
	request := authorizedRequest(http.MethodPost, "/api/render/jobs", body)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || body.reads != 0 || admit.request.Source != nil {
		t.Fatalf("missing idempotency = %d %s, body reads %d", response.Code, response.Body.String(), body.reads)
	}
}

func TestSubmissionRejectsEngineContentMismatch(t *testing.T) {
	handler := newTestHandler(t, &fakeAdmission{}, newFakeRepository(), &fakeStore{})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("contentKind", "markdown")
	_ = writer.WriteField("documentEngine", "latexml")
	_ = writer.WriteField("priorityIntent", "publish")
	file, _ := writer.CreateFormFile("source", "book.zip")
	_, _ = file.Write([]byte("archive"))
	_ = writer.Close()
	request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", "publish-2")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_document_engine") {
		t.Fatalf("engine mismatch = %d %s", response.Code, response.Body.String())
	}
}

func newTestHandler(t *testing.T, admit Admission, repository Repository, store PrivateStore) *Handler {
	t.Helper()
	handler, err := New(StaticTokenAuthenticator{Principals: map[string]string{
		"token-a": "principal-a", "token-b": "principal-b",
	}}, admit, repository, store, Config{
		RendererVersion: "test", MaxSourceBytes: 1024,
		TypstProfileID: "typst-html-testprofile",
		EventPoll:      time.Millisecond,
		Estimation: jobpostgres.EstimationPolicy{MinimumSamples: 20, Scheduling: jobpostgres.SchedulingPolicy{
			Resources:     jobpostgres.ResourceCapacities{DocumentLight: 2, DocumentLaTeXML: 1, MathNode: 2, TeXSVG: 2, BatchMigration: 1, LatexPDF: 1, DocumentTypst: 1, TypstPDF: 1, Heavy: 1},
			Weights:       jobpostgres.PriorityWeights{Publish: 8, Preview: 5, Rebuild: 3, Migration: 1},
			AgingInterval: 5 * time.Minute, MaxAgingSteps: 12, PrincipalRunning: 2,
		}},
		Now: func() time.Time { return time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func authorizedRequest(method string, path string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("X-Rin-Renderer-Token", "token-a")
	request.Header.Set("X-Rin-Renderer-Owner-Scope", "book-a")
	return request
}

type fakeAdmission struct{ request admission.Request }

func (service *fakeAdmission) Admit(_ context.Context, request admission.Request) (admission.Result, error) {
	service.request = request
	now := time.Now()
	return admission.Result{Job: jobpostgres.Job{ID: testJobID, State: "queued", ContentKind: request.ContentKind, DocumentEngine: request.DocumentEngine, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}}, nil
}

type fakeRepository struct {
	job              jobpostgres.Job
	artifact         jobpostgres.Artifact
	artifacts        map[string]jobpostgres.Artifact
	cancelCalls      int
	retryCalls       int
	expireCalls      int
	afterID          int64
	completionHealth jobpostgres.CompletionHealth
}

func (repository *fakeRepository) CompletionHealth(context.Context, time.Time) (jobpostgres.CompletionHealth, error) {
	if repository.completionHealth.State == "" {
		return jobpostgres.CompletionHealth{State: "healthy", CheckedAt: time.Now()}, nil
	}
	return repository.completionHealth, nil
}

func newFakeRepository() *fakeRepository {
	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	return &fakeRepository{artifacts: map[string]jobpostgres.Artifact{}, job: jobpostgres.Job{
		ID: testJobID, PrincipalID: "principal-a", OwnerScope: "book-a", ContentKind: "latex",
		DocumentEngine: "latexml", State: "queued", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
}

func (repository *fakeRepository) AuthorizedJob(_ context.Context, id, principal, owner string) (jobpostgres.Job, error) {
	if id != repository.job.ID || principal != repository.job.PrincipalID || owner != repository.job.OwnerScope {
		return jobpostgres.Job{}, jobpostgres.ErrNotFound
	}
	return repository.job, nil
}
func (repository *fakeRepository) AuthorizedEvents(_ context.Context, id, principal, owner string, after int64, _ int) ([]jobpostgres.JobEvent, error) {
	if _, err := repository.AuthorizedJob(context.Background(), id, principal, owner); err != nil {
		return nil, err
	}
	repository.afterID = after
	return []jobpostgres.JobEvent{{ID: 8, JobID: id, Type: "canceled", Stage: "attempt", Payload: json.RawMessage(`{}`), CreatedAt: time.Now()}}, nil
}
func (*fakeRepository) Queue(_ context.Context, now time.Time) (jobpostgres.QueueSummary, error) {
	return jobpostgres.QueueSummary{QueuedProjects: 1, ActiveProjects: 2, Scope: "instance", CalculatedAt: now}, nil
}
func (*fakeRepository) AuthorizedJobQueue(_ context.Context, _, _, _ string, now time.Time, _ jobpostgres.EstimationPolicy) (jobpostgres.JobQueueSummary, error) {
	return jobpostgres.JobQueueSummary{JobsAheadEstimate: 3, QueuedProjects: 7, ActiveProjects: 2, Scope: "instance", CalculatedAt: now}, nil
}
func (repository *fakeRepository) Artifact(_ context.Context, id string) (jobpostgres.Artifact, error) {
	if artifact, ok := repository.artifacts[id]; ok {
		return artifact, nil
	}
	return repository.artifact, nil
}
func (repository *fakeRepository) RequestCancellation(_ context.Context, _ string, _ time.Time) (jobpostgres.Cancellation, error) {
	repository.cancelCalls++
	repository.job.State = "canceled"
	return jobpostgres.Cancellation{Job: repository.job}, nil
}
func (repository *fakeRepository) AuthorizedSupportSnapshot(_ context.Context, id, principal, owner string, now time.Time) (jobpostgres.SupportSnapshot, error) {
	if _, err := repository.AuthorizedJob(context.Background(), id, principal, owner); err != nil {
		return jobpostgres.SupportSnapshot{}, err
	}
	return jobpostgres.SupportSnapshot{
		Job:       jobpostgres.SupportJob{JobID: id, ContentKind: repository.job.ContentKind, State: repository.job.State, ExpiresAt: repository.job.ExpiresAt},
		Attempts:  []jobpostgres.SupportAttempt{{AttemptID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", AttemptNo: 1, State: "failed", ErrorCode: "document_invalid"}},
		Estimator: &jobpostgres.SupportEstimate{EstimatorVersion: jobpostgres.WaitEstimatorVersion, EstimatedStartAt: now, EarliestStartAt: now, LatestStartAt: now, Confidence: "low", SampleCount: 20, Scope: "instance", CalculatedAt: now},
		Artifacts: []jobpostgres.SupportArtifact{{ArtifactID: "source-id", Kind: "source", Visibility: "private", SHA256: strings.Repeat("a", 64), ByteSize: 10, MediaType: "application/zip"}},
		Scope:     "instance", InspectedAt: now,
	}, nil
}
func (repository *fakeRepository) AuthorizedManualRetry(_ context.Context, id, principal, owner string, _ time.Time) (jobpostgres.Job, error) {
	if _, err := repository.AuthorizedJob(context.Background(), id, principal, owner); err != nil {
		return jobpostgres.Job{}, err
	}
	repository.retryCalls++
	repository.job.State = "queued"
	return repository.job, nil
}
func (repository *fakeRepository) AuthorizedManualExpire(_ context.Context, id, principal, owner string, _ time.Time) (jobpostgres.Job, error) {
	if _, err := repository.AuthorizedJob(context.Background(), id, principal, owner); err != nil {
		return jobpostgres.Job{}, err
	}
	repository.expireCalls++
	repository.job.State = "expired"
	return repository.job, nil
}

type fakeStore struct {
	body   []byte
	bodies map[string][]byte
	err    error
}

type countingReader struct {
	reader io.Reader
	reads  int
}

func (reader *countingReader) Read(body []byte) (int, error) {
	reader.reads++
	return reader.reader.Read(body)
}

func (store *fakeStore) GetPrivate(_ context.Context, reference contracts.ArtifactReference) ([]byte, error) {
	if body, ok := store.bodies[reference.ArtifactID]; ok {
		return body, store.err
	}
	return store.body, store.err
}
