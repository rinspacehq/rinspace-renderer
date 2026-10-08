package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

func TestAsyncMarkdownDurableJob(t *testing.T) {
	runAsyncMarkdownDurableJob(t, false)
}

func TestAsyncMarkdownDurableJobLocal(t *testing.T) {
	runAsyncMarkdownDurableJob(t, true)
}

func runAsyncMarkdownDurableJob(t *testing.T, local bool) {
	t.Helper()
	previousOperations := operational.Default()
	operational.SetDefault(operational.New(io.Discard))
	t.Cleanup(func() { operational.SetDefault(previousOperations) })
	databaseURL := strings.TrimSpace(os.Getenv("RIN_RENDERER_TEST_DATABASE_URL"))
	if local {
		databaseURL = strings.TrimSpace(os.Getenv("RIN_RENDERER_TEST_LOCAL_DATABASE_URL"))
	}
	if databaseURL == "" {
		t.Skip("isolated Renderer test database is not configured")
	}
	t.Setenv("RIN_RENDERER_DATABASE_URL", databaseURL)
	t.Setenv("RIN_RENDERER_ARTIFACT_ROOT", t.TempDir())
	t.Setenv("RIN_RENDERER_SERVICE_TOKEN", "async-markdown-test-token")
	if local {
		t.Setenv("RIN_RENDERER_COMPLETION_MODE", "local")
	} else {
		t.Setenv("RIN_RENDERER_COMPLETION_MODE", "control-plane")
		configureCompletionDispatcherTest(t)
	}
	cfg := renderapi.ConfigFromEnv()
	if local {
		cfg.StorageProvider = "local"
		cfg.LocalAssetRoot = t.TempDir()
		cfg.LocalPublicBaseURL = "http://127.0.0.1:8090"
	}
	cfg.RendererVersion = "async-markdown-test"
	cfg.MarkdownScript = filepath.Clean(filepath.Join("..", "engines", "markdown", "worker.mjs"))
	cfg.MarkdownWorkerCount = 1

	t.Setenv("RIN_RENDERER_RUNTIME_ROLE", "api")
	apiRuntime, err := buildAsyncRuntime(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if apiRuntime == nil || apiRuntime.role != "api" || apiRuntime.workerCancel != nil {
		t.Fatalf("API-only runtime was not created correctly: %#v", apiRuntime)
	}
	defer apiRuntime.Close()
	t.Setenv("RIN_RENDERER_RUNTIME_ROLE", "worker")
	t.Setenv("RIN_RENDERER_WORKER_ID", "async-markdown-worker")
	workerRuntime, err := buildAsyncRuntime(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if workerRuntime == nil || workerRuntime.role != "worker" || workerRuntime.workerCancel == nil {
		t.Fatalf("worker-only runtime was not created correctly: %#v", workerRuntime)
	}
	if local && workerRuntime.dispatcherCancel != nil {
		t.Fatal("local worker started a Control Plane completion dispatcher")
	}
	defer workerRuntime.Close()

	source := "# Durable Markdown\n\nRendered by the queued **unified** adapter."
	archive := asyncMarkdownArchive(t, source)
	var submission struct {
		Job struct {
			JobID string `json:"jobId"`
			State string `json:"state"`
		} `json:"job"`
	}
	admissionDeadline := time.Now().Add(10 * time.Second)
	for {
		response := submitAsyncMarkdown(t, apiRuntime.handler, archive, local)
		if response.Code == http.StatusAccepted {
			if err := json.Unmarshal(response.Body.Bytes(), &submission); err != nil {
				t.Fatal(err)
			}
			break
		}
		if response.Code != http.StatusServiceUnavailable || time.Now().After(admissionDeadline) {
			t.Fatalf("submit async Markdown = %d %s", response.Code, response.Body.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if submission.Job.JobID == "" || submission.Job.State != "queued" {
		t.Fatalf("unexpected async submission: %#v", submission)
	}

	state := ""
	completionDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(completionDeadline) {
		request := authorizedAsyncMarkdownRequest(http.MethodGet, "/api/render/jobs/"+submission.Job.JobID, nil)
		response := httptest.NewRecorder()
		apiRuntime.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("read async Markdown status = %d %s", response.Code, response.Body.String())
		}
		var status struct {
			State string `json:"state"`
			Stage string `json:"stage"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &status)
		state = status.State
		if state == "succeeded" {
			if status.Stage != "store" {
				t.Fatalf("terminal Markdown stage = %q", status.Stage)
			}
			break
		}
		if state == "failed" || state == "canceled" || state == "expired" {
			supportRequest := authorizedAsyncMarkdownRequest(http.MethodGet, "/api/render/jobs/"+submission.Job.JobID+"/support", nil)
			supportResponse := httptest.NewRecorder()
			apiRuntime.handler.ServeHTTP(supportResponse, supportRequest)
			t.Fatalf("async Markdown reached terminal state %q: %s; support=%d %s", state, response.Body.String(), supportResponse.Code, supportResponse.Body.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if state != "succeeded" {
		t.Fatalf("async Markdown did not complete before deadline; last state %q", state)
	}

	request := authorizedAsyncMarkdownRequest(http.MethodGet, "/api/render/jobs/"+submission.Job.JobID+"/result", nil)
	response := httptest.NewRecorder()
	apiRuntime.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("read async Markdown result = %d %s", response.Code, response.Body.String())
	}
	var result contracts.RenderResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(); err != nil || (!local && result.RequestID != "render-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") || result.ContentKind != contracts.ContentKindMarkdown ||
		result.Inline == nil || !strings.Contains(result.Inline.Pages[0].Fragment, "<strong>unified</strong>") {
		t.Fatalf("invalid durable Markdown result: %#v, %v", result, err)
	}
}

func TestAsyncMarkdownBookDurableJob(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("RIN_RENDERER_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("RIN_RENDERER_TEST_DATABASE_URL is not configured")
	}
	t.Setenv("RIN_RENDERER_DATABASE_URL", databaseURL)
	t.Setenv("RIN_RENDERER_ARTIFACT_ROOT", t.TempDir())
	t.Setenv("RIN_RENDERER_SERVICE_TOKEN", "async-markdown-book-test-token")
	configureCompletionDispatcherTest(t)
	cfg := renderapi.ConfigFromEnv()
	cfg.RendererVersion = "async-markdown-book-test"
	cfg.MarkdownScript = filepath.Clean(filepath.Join("..", "engines", "markdown", "worker.mjs"))
	cfg.MarkdownWorkerCount = 1
	runtime, err := buildAsyncRuntime(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runtime == nil {
		t.Fatal("async runtime was not created")
	}
	defer runtime.Close()

	archive := asyncMarkdownBookArchive(t)
	var submission struct {
		Job struct {
			JobID string `json:"jobId"`
			State string `json:"state"`
		} `json:"job"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		response := submitAsyncMarkdownBook(t, runtime.handler, archive)
		if response.Code == http.StatusAccepted {
			if err := json.Unmarshal(response.Body.Bytes(), &submission); err != nil {
				t.Fatal(err)
			}
			break
		}
		if response.Code != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("submit async Markdown Book = %d %s", response.Code, response.Body.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if submission.Job.JobID == "" || submission.Job.State != "queued" {
		t.Fatalf("unexpected async Book submission: %#v", submission)
	}

	state := ""
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		request := authorizedAsyncMarkdownBookRequest(http.MethodGet, "/api/render/jobs/"+submission.Job.JobID, nil)
		response := httptest.NewRecorder()
		runtime.handler.ServeHTTP(response, request)
		var status struct {
			State    string                                     `json:"state"`
			Stage    string                                     `json:"stage"`
			Progress struct{ CompletedStages, TotalStages int } `json:"progress"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &status) != nil {
			t.Fatalf("read async Markdown Book status = %d %s", response.Code, response.Body.String())
		}
		state = status.State
		if status.Progress.TotalStages != 3 || status.Progress.CompletedStages < 0 || status.Progress.CompletedStages > status.Progress.TotalStages {
			t.Fatalf("invalid Book project progress: %#v", status)
		}
		if state == "succeeded" {
			break
		}
		if state == "failed" || state == "canceled" || state == "expired" {
			t.Fatalf("async Markdown Book reached terminal state %q: %s", state, response.Body.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if state != "succeeded" {
		t.Fatalf("async Markdown Book did not complete before deadline; last state %q", state)
	}

	request := authorizedAsyncMarkdownBookRequest(http.MethodGet, "/api/render/jobs/"+submission.Job.JobID+"/result", nil)
	response := httptest.NewRecorder()
	runtime.handler.ServeHTTP(response, request)
	var result contracts.RenderResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Validate() != nil ||
		result.RequestID != "render-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
		result.Inline == nil || len(result.Inline.Pages) != 2 || result.Inline.Pages[0].ID != "intro" ||
		result.Inline.Pages[1].ID != "chapter" || result.Inline.Pages[1].SourcePath != "chapter.md" || len(result.Inline.WorkUnits) != 0 {
		t.Fatalf("invalid durable Markdown Book result: status=%d result=%#v body=%s", response.Code, result, response.Body.String())
	}
}

func configureCompletionDispatcherTest(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	t.Setenv("RIN_RENDERER_CONTROL_PLANE_EVENT_URL", server.URL+"/internal/v1/events/renderer")
	t.Setenv("RIN_RENDERER_CONTROL_PLANE_EVENT_HMAC_KEY", strings.Repeat("k", 32))
	t.Setenv("RIN_RENDERER_CONTROL_PLANE_EVENT_HMAC_KEY_ID", "test-current")
}

func submitAsyncMarkdown(t *testing.T, handler http.Handler, archive []byte, local bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("source", "article.zip")
	_, _ = part.Write(archive)
	_ = writer.WriteField("contentKind", "markdown")
	_ = writer.WriteField("documentEngine", "unified")
	_ = writer.WriteField("entrypoint", "article.md")
	_ = writer.WriteField("priorityIntent", "publish")
	if !local {
		_ = writer.WriteField("requestId", "render-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		_ = writer.WriteField("controlProjectId", "article:42")
		_ = writer.WriteField("sourceCommit", strings.Repeat("a", 40))
		_ = writer.WriteField("controlProjectHash", strings.Repeat("a", 64))
	}
	_ = writer.Close()
	request := authorizedAsyncMarkdownRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", fmt.Sprintf("async-markdown-durable-%t-%d", local, time.Now().UnixNano()))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func submitAsyncMarkdownBook(t *testing.T, handler http.Handler, archive []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("source", "book.zip")
	_, _ = part.Write(archive)
	_ = writer.WriteField("contentKind", "markdown")
	_ = writer.WriteField("documentEngine", "unified")
	_ = writer.WriteField("documentMode", "book")
	_ = writer.WriteField("bookPages", `[{"path":"intro.md","id":"intro","title":"Intro"},{"path":"chapter.md","id":"chapter","title":"Chapter"}]`)
	_ = writer.WriteField("title", "Durable Book")
	_ = writer.WriteField("priorityIntent", "publish")
	_ = writer.WriteField("requestId", "render-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	_ = writer.WriteField("controlProjectId", "book:43")
	_ = writer.WriteField("sourceCommit", strings.Repeat("b", 40))
	_ = writer.WriteField("controlProjectHash", strings.Repeat("b", 64))
	_ = writer.Close()
	request := authorizedAsyncMarkdownBookRequest(http.MethodPost, "/api/render/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", "async-markdown-book-durable-v1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func authorizedAsyncMarkdownRequest(method string, path string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("X-Rin-Renderer-Token", "async-markdown-test-token")
	request.Header.Set("X-Rin-Renderer-Owner-Scope", "article:42")
	return request
}

func authorizedAsyncMarkdownBookRequest(method string, path string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("X-Rin-Renderer-Token", "async-markdown-book-test-token")
	request.Header.Set("X-Rin-Renderer-Owner-Scope", "book:43")
	return request
}

func asyncMarkdownArchive(t *testing.T, source string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	file, _ := writer.Create("article.md")
	_, _ = io.WriteString(file, source)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func asyncMarkdownBookArchive(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	for _, item := range []struct{ path, source string }{
		{"intro.md", "# Intro\n\n[Chapter](chapter.md) with $x^2$."},
		{"chapter.md", "# Chapter\n\n```js\nconst answer = 42\n```"},
	} {
		file, _ := writer.Create(item.path)
		_, _ = io.WriteString(file, item.source)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}
