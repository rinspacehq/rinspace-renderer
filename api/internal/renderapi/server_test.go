package renderapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/mathrender"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectdiagrams"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectmath"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderstorage"
)

func testConfig() Config {
	cfg := ConfigFromEnv()
	cfg.ServiceToken = "test-token"
	cfg.RendererVersion = "test-version"
	cfg.LateXMLVersion = "0d02309"
	cfg.LateXMLWorkerEndpoint = ""
	cfg.LateXMLWorkerToken = ""
	cfg.DiagramWorkerEndpoint = ""
	cfg.DiagramWorkerToken = ""
	cfg.MathPrimaryEngine = "mathjax-chtml"
	cfg.MathJaxNodeBin = "node"
	cfg.MathJaxScript = testRenderAPIScriptPath("../../../engines/mathjax/render-mathjax.mjs")
	cfg.MathJaxWarmWorkersEnabled = false
	return cfg
}

func TestCapabilities(t *testing.T) {
	server := NewServer(testConfig())
	req := httptest.NewRequest(http.MethodGet, "/api/render/capabilities", nil)
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, expected := range []string{"latexml", "rin-texsvg", "mathjax-chtml", "katex-legacy", "texsvg-fallback", "cloudbase", "tikzcd"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected capabilities to include %q, got %s", expected, body)
		}
	}
}

func TestLocalAssetProviderIsServedAndReported(t *testing.T) {
	cfg := testConfig()
	cfg.StorageProvider = "local"
	cfg.LocalAssetRoot = t.TempDir()
	cfg.LocalPublicBaseURL = "http://127.0.0.1:8090"
	handler := NewServer(cfg)
	svg, err := renderstorage.NormalizeSVGObject([]byte(`<svg viewBox="0 0 10 10"><path d="M0 0L10 10"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	store := handler.(*Server).diagramStorage()
	if _, err := store.PutPublicObjectIfMissing(context.Background(), svg.ObjectID, renderstorage.SVGContentType, svg.Bytes); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/local-assets/"+svg.ObjectID, nil))
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), svg.Bytes) {
		t.Fatalf("local public asset = %d %q", response.Code, response.Body.String())
	}
	capabilities := httptest.NewRecorder()
	handler.ServeHTTP(capabilities, httptest.NewRequest(http.MethodGet, "/api/render/capabilities", nil))
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"provider":"local"`) {
		t.Fatalf("local capabilities = %d %s", capabilities.Code, capabilities.Body.String())
	}
}

func TestWarmMathJaxWorkersReportReadinessMetricsAndCapabilities(t *testing.T) {
	cfg := testConfig()
	cfg.MathJaxWarmWorkersEnabled = true
	cfg.MathJaxWarmWorkerCount = 1
	cfg.MathJaxWarmMaxRequestBytes = 1 << 20
	cfg.MathJaxWarmMaxResponseBytes = 16 << 20
	cfg.MathJaxWarmMaxTasks = 100
	cfg.MathJaxWarmMaxRSSBytes = 2 << 30
	cfg.MathJaxWarmStartTimeout = 10 * time.Second
	cfg.MathJaxWarmStopGrace = 100 * time.Millisecond
	operations := operational.New(io.Discard)
	handler := NewServerWithOperations(cfg, nil, nil, operations)
	server, ok := handler.(*Server)
	if !ok {
		t.Fatalf("expected concrete server, got %T", handler)
	}
	defer server.Close()

	ready := httptest.NewRecorder()
	server.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if ready.Code != http.StatusOK || !strings.Contains(ready.Body.String(), `"math-node":{"ready":true`) {
		t.Fatalf("warm readiness = %d %s", ready.Code, ready.Body.String())
	}
	capabilities := httptest.NewRecorder()
	server.ServeHTTP(capabilities, httptest.NewRequest(http.MethodGet, "/api/render/capabilities", nil))
	for _, expected := range []string{`"enabled":true`, `"transport":"ndjson-stdio"`, `"contractVersion":"rin-node-worker/v1"`, `"mathJaxMode":"warm"`} {
		if !strings.Contains(capabilities.Body.String(), expected) {
			t.Fatalf("warm capabilities omit %s: %s", expected, capabilities.Body.String())
		}
	}
	metrics := httptest.NewRecorder()
	server.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, expected := range []string{
		"rin_renderer_node_worker_alive_math 1",
		"rin_renderer_node_worker_capacity_math 1",
		"rin_renderer_diagram_pool_capacity_texsvg 2",
		"rin_renderer_diagram_pool_active_texsvg 0",
		"rin_renderer_math_batches_in_use_math_node 0",
	} {
		if !strings.Contains(metrics.Body.String(), expected) {
			t.Fatalf("warm metrics omit %q: %s", expected, metrics.Body.String())
		}
	}
}

func TestUnhealthyWarmMathJaxWorkerDegradesReadinessNotLiveness(t *testing.T) {
	cfg := testConfig()
	cfg.MathJaxWarmWorkersEnabled = true
	cfg.MathJaxNodeBin = "/missing/rin-node"
	operations := operational.New(io.Discard)
	server := NewServerWithOperations(cfg, nil, nil, operations).(*Server)
	defer server.Close()

	health := httptest.NewRecorder()
	server.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("worker failure changed liveness: %d %s", health.Code, health.Body.String())
	}
	ready := httptest.NewRecorder()
	server.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if ready.Code != http.StatusServiceUnavailable ||
		!strings.Contains(ready.Body.String(), `"degraded":["math-node"]`) ||
		!strings.Contains(ready.Body.String(), `"reason":"worker_unavailable"`) {
		t.Fatalf("unhealthy warm worker readiness = %d %s", ready.Code, ready.Body.String())
	}
}

func TestMathJaxProcessPerBatchRemainsDefaultRollback(t *testing.T) {
	server := newServer(testConfig(), nil, nil, operational.New(io.Discard))
	defer server.Close()
	if server.warmMath != nil {
		t.Fatal("warm workers should be disabled by default")
	}
	if _, ok := server.primaryMathRenderer().(mathrender.MathJaxRenderer); !ok {
		t.Fatalf("expected process-per-batch renderer, got %T", server.primaryMathRenderer())
	}
}

func TestLivenessReadinessMetricsAndDegradedCapabilitiesAreIndependent(t *testing.T) {
	operations := operational.New(io.Discard)
	if err := operations.AddProbe("database", func(context.Context) error {
		return errors.New("database_unavailable: password=secret")
	}); err != nil {
		t.Fatal(err)
	}
	operations.Count("worker_restarts_total", "job")
	jobs := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusAccepted) })
	server := NewServerWithOperations(testConfig(), jobs, http.NotFoundHandler(), operations)

	health := httptest.NewRecorder()
	server.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("liveness = %d %s", health.Code, health.Body.String())
	}
	ready := httptest.NewRecorder()
	server.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if ready.Code != http.StatusServiceUnavailable || !strings.Contains(ready.Body.String(), `"degraded":["database"]`) || strings.Contains(ready.Body.String(), "secret") {
		t.Fatalf("readiness = %d %s", ready.Code, ready.Body.String())
	}
	capabilities := httptest.NewRecorder()
	server.ServeHTTP(capabilities, httptest.NewRequest(http.MethodGet, "/api/render/capabilities", nil))
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"readiness":"degraded"`) || !strings.Contains(capabilities.Body.String(), `"degraded":["database"]`) {
		t.Fatalf("degraded capabilities = %d %s", capabilities.Code, capabilities.Body.String())
	}
	metrics := httptest.NewRecorder()
	server.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), "rin_renderer_worker_restarts_total_job 1") {
		t.Fatalf("metrics = %d %s", metrics.Code, metrics.Body.String())
	}
	status := httptest.NewRecorder()
	server.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/render/jobs/job-id", nil))
	if status.Code != http.StatusAccepted {
		t.Fatalf("degraded worker blocked status route: %d %s", status.Code, status.Body.String())
	}
}

func TestCanonicalJobHandlerMountAndCapabilities(t *testing.T) {
	jobs := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusAccepted)
	})
	server := NewServerWithJobs(testConfig(), jobs)
	queueRequest := httptest.NewRequest(http.MethodGet, "/api/render/queue", nil)
	queueResponse := httptest.NewRecorder()
	server.ServeHTTP(queueResponse, queueRequest)
	if queueResponse.Code != http.StatusAccepted {
		t.Fatalf("mounted job handler status = %d", queueResponse.Code)
	}
	capabilitiesRequest := httptest.NewRequest(http.MethodGet, "/api/render/capabilities", nil)
	capabilitiesResponse := httptest.NewRecorder()
	server.ServeHTTP(capabilitiesResponse, capabilitiesRequest)
	if capabilitiesResponse.Code != http.StatusOK || !strings.Contains(capabilitiesResponse.Body.String(), `"async":{"enabled":true`) {
		t.Fatalf("async capabilities = %d %s", capabilitiesResponse.Code, capabilitiesResponse.Body.String())
	}
	if !strings.Contains(capabilitiesResponse.Body.String(), `"unified"`) {
		t.Fatalf("async capabilities omit unified: %s", capabilitiesResponse.Body.String())
	}
}

func TestAsyncProjectHandlerReplacesDirectRenderRoute(t *testing.T) {
	projects := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Test-Route", "durable")
		response.WriteHeader(http.StatusAccepted)
	})
	server := NewServerWithAsync(testConfig(), http.NotFoundHandler(), projects)
	request := httptest.NewRequest(http.MethodPost, "/api/render/projects", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("X-Test-Route") != "durable" {
		t.Fatalf("project route = %d, headers %#v", response.Code, response.Header())
	}
}

func TestProjectRenderRequiresToken(t *testing.T) {
	server := NewServer(testConfig())
	req := httptest.NewRequest(http.MethodPost, "/api/render/projects", strings.NewReader(""))
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestProjectRenderRejectsInvalidToken(t *testing.T) {
	server := NewServer(testConfig())
	req := httptest.NewRequest(http.MethodPost, "/api/render/projects", strings.NewReader(""))
	req.Header.Set("X-Rin-Renderer-Token", "wrong-token")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestProjectRenderWithLateXMLInvokesAdapter(t *testing.T) {
	cfg := testConfig()
	cfg.LateXMLBin = fakeRenderAPILateXMLC(t)
	server := NewServer(cfg)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("source", "main.tex")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(`\documentclass{article}
\begin{document}
Rin Renderer smoke test.
\end{document}`)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.WriteField("engine", "latexml"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/render/projects", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Rin-Renderer-Token", "test-token")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var payload ProjectRenderResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Engine != "latexml" || payload.HTML != `<article><h1>Converted</h1><p>Rin Renderer smoke test.</p></article>` {
		t.Fatalf("unexpected latexml payload: %#v", payload)
	}
	if payload.Reader == nil || payload.AssetManifest == nil {
		t.Fatalf("expected reader and asset manifest in response: %#v", payload)
	}
	assetManifest, ok := payload.AssetManifest.(map[string]any)
	if !ok || assetManifest["version"] != "0.1" || assetManifest["files"] == nil {
		t.Fatalf("expected legacy-compatible asset manifest shape, got %#v", payload.AssetManifest)
	}
	if len(payload.Diagnostics) == 0 || payload.Diagnostics[0].Engine != "latexml" {
		t.Fatalf("expected latexml diagnostics, got %#v", payload.Diagnostics)
	}
}

func TestProjectRenderWithLateXMLReplacesSourceMathPlaceholders(t *testing.T) {
	cfg := testConfig()
	cfg.LateXMLBin = fakeRenderAPILateXMLCWithMathPlaceholder(t)
	cfg.KaTeXScript = testRenderAPIKaTeXScriptPath(t)
	server := NewServer(cfg)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("source", "main.tex")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(`\documentclass{article}
\usepackage{amsmath}
\begin{document}
Before.
\begin{align}
\mathbf{x}_{1}\pm\mathbf{x}_{2} &= \ket{x_{1}}\pm\ket{x_{2}}
\end{align}
After.
\end{document}`)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.WriteField("engine", "latexml"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/render/projects", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Rin-Renderer-Token", "test-token")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var payload ProjectRenderResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Math.Count != 1 || payload.Math.MathJaxCount != 1 || payload.Math.FailedCount != 0 {
		t.Fatalf("unexpected math summary: %#v diagnostics=%#v", payload.Math, payload.Diagnostics)
	}
	for _, expected := range []string{`rin-math-display rin-math-mathjax`, `data-rin-math-engine="mathjax-chtml"`, `mjx-container`, `⟩`, `rin-mathjax-chtml-style`} {
		if !strings.Contains(payload.HTML, expected) {
			t.Fatalf("expected rendered HTML to contain %q, got %s", expected, payload.HTML)
		}
	}
	for _, unexpected := range []string{`RINRENDERERMATHPLACEHOLDER`, `<math`, `ltx_eqn_table`, `rin-latexml-katex`} {
		if strings.Contains(payload.HTML, unexpected) {
			t.Fatalf("expected rendered HTML to omit %q, got %s", unexpected, payload.HTML)
		}
	}
}

func TestProjectRenderWithLateXMLUsesWorkerEndpointWhenConfigured(t *testing.T) {
	var workerAuth string
	var workerPayload struct {
		Title        string             `json:"title"`
		MainFile     string             `json:"mainFile"`
		Profile      string             `json:"profile"`
		MaxHTMLBytes int64              `json:"maxHtmlBytes"`
		Files        []projectcore.File `json:"files"`
		Manifest     struct {
			MainFile string `json:"mainFile"`
		} `json:"manifest"`
	}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/render" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&workerPayload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"html":"<html><body><article><h1>Worker Converted</h1><p>Rin Renderer worker test.</p></article></body></html>","diagnostics":[{"severity":"info","code":"latexml.worker.info","message":"worker complete"}]}`))
	}))
	defer worker.Close()

	cfg := testConfig()
	cfg.LateXMLBin = filepath.Join(t.TempDir(), "missing-latexmlc")
	cfg.LateXMLWorkerEndpoint = worker.URL + "/render"
	cfg.LateXMLWorkerToken = "latexml-worker-token"
	cfg.LateXMLProfile = "html5"
	cfg.LateXMLMaxHTMLBytes = 1 << 20
	server := NewServer(cfg)
	req := newProjectRenderRequest(t, "latexml")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if workerAuth != "Bearer latexml-worker-token" || workerPayload.MainFile != "main.tex" || workerPayload.Manifest.MainFile != "main.tex" || workerPayload.Profile != "html5" || workerPayload.MaxHTMLBytes != 1<<20 || len(workerPayload.Files) == 0 {
		t.Fatalf("unexpected worker request auth=%q payload=%#v", workerAuth, workerPayload)
	}
	var payload ProjectRenderResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Engine != "latexml" || !strings.Contains(payload.HTML, "Worker Converted") || len(payload.Diagnostics) == 0 || payload.Diagnostics[0].Code != "latexml.worker.info" || payload.Diagnostics[0].Engine != "latexml" {
		t.Fatalf("unexpected worker-backed payload: %#v", payload)
	}
}

func TestProjectRenderWithAutoUsesLateXMLPrimary(t *testing.T) {
	cfg := testConfig()
	cfg.LateXMLBin = fakeRenderAPILateXMLC(t)
	server := NewServer(cfg)
	req := newProjectRenderRequest(t, "auto")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var payload ProjectRenderResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Engine != "latexml" || payload.Fallback || payload.PrimaryEngine != "latexml" || payload.FallbackEngine != "" {
		t.Fatalf("unexpected auto success routing payload: %#v", payload)
	}
}

func TestProjectRenderWithAutoReportsLaTeXMLFailureWithoutFallback(t *testing.T) {
	cfg := testConfig()
	cfg.LateXMLBin = filepath.Join(t.TempDir(), "missing-latexmlc")
	server := NewServer(cfg)
	req := newProjectRenderRequest(t, "auto")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 for a hard latexml failure, got %d: %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "latexml.unavailable") {
		t.Fatalf("expected the latexml failure to be reported, got %s", body)
	}
	for _, unexpected := range []string{"legacy-rinjs", "renderer.engine.auto_fallback"} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("auto must report the failure instead of degrading (%q found): %s", unexpected, body)
		}
	}
}

func TestProjectRenderWithLateXMLUsesRinDiagramPipeline(t *testing.T) {
	const cloudbaseToken = "cloudbase-token"
	rawSVG := `<?xml version="1.0"?><svg height="10" width="10" onload="alert(1)"><script>alert(1)</script><path d="M0 0L10 10"/></svg>`
	svgObject, err := renderstorage.NormalizeSVGObject([]byte(rawSVG))
	if err != nil {
		t.Fatal(err)
	}
	normalizedSVG := string(svgObject.Bytes)
	expectedObjectID := svgObject.ObjectID

	var workerPayload DiagramRequest
	var workerPath string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerPath = r.URL.Path
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&workerPayload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"diagram-id","url":"/rin/api/diagrams/local-diagram","svg":%q,"cached":false,"type":"tikzcd","diagnostics":[{"severity":"warning","code":"diagram.worker.warning","message":"worker warning"}]}`, rawSVG)
	}))
	defer worker.Close()

	var postPath string
	var uploadedBody string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			http.NotFound(w, r)
		case http.MethodPost:
			postPath = r.URL.Path
			if r.Header.Get("Authorization") != "Bearer "+cloudbaseToken {
				http.Error(w, "missing CloudBase token", http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			uploadedBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"Id":"object-id","Key":%q}`, "rin-renderer/"+expectedObjectID)
		case http.MethodGet:
			_, _ = io.WriteString(w, normalizedSVG)
		default:
			http.NotFound(w, r)
		}
	}))
	defer storage.Close()

	cfg := testConfig()
	cfg.LateXMLBin = fakeRenderAPILateXMLCWithPlaceholder(t)
	cfg.DiagramWorkerEndpoint = worker.URL
	cfg.StorageBaseURL = storage.URL
	cfg.StoragePublicBaseURL = "https://cdn.example"
	cfg.StorageAccessToken = cloudbaseToken
	server := NewServer(cfg)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("source", "main.tex")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(`\documentclass{article}
\definecolor{diagramblue}{HTML}{245B78}
\tikzcdset{every arrow/.append style={line width=.55pt}}
\begin{document}
Before.
\begin{center}
\begin{tikzcd}[column sep=huge]
A \arrow[r,color=diagramblue] & B
\end{tikzcd}
\end{center}
After.
\end{document}`)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.WriteField("engine", "latexml"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/render/projects", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Rin-Renderer-Token", "test-token")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if workerPath != "/tikzcd" || workerPayload.Type != "tikzcd" || workerPayload.Options != "column sep=huge" {
		t.Fatalf("unexpected diagram worker request path=%q payload=%#v", workerPath, workerPayload)
	}
	if strings.TrimSpace(workerPayload.Body) != `A \arrow[r,color=diagramblue] & B` || !strings.Contains(workerPayload.Source, `\begin{tikzcd}[column sep=huge]`) {
		t.Fatalf("unexpected extracted diagram payload: %#v", workerPayload)
	}
	for _, expected := range []string{`\definecolor{diagramblue}{HTML}{245B78}`, `\tikzcdset{every arrow/.append style={line width=.55pt}}`} {
		if !strings.Contains(workerPayload.Source, expected) {
			t.Fatalf("expected standalone diagram source to inherit %q, got %#v", expected, workerPayload)
		}
	}
	if strings.Contains(workerPayload.Source, `\begin{center}`) || strings.Contains(workerPayload.Source, `\end{center}`) {
		t.Fatalf("expected layout wrapper to stay out of diagram worker payload: %#v", workerPayload)
	}
	expectedUploadPath := "/v1/storages/object/rin-renderer/" + expectedObjectID
	if postPath != expectedUploadPath || uploadedBody != normalizedSVG {
		t.Fatalf("unexpected CloudBase upload: post=%q body=%q", postPath, uploadedBody)
	}
	var payload ProjectRenderResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Diagrams) != 1 || payload.Diagrams[0].ObjectID != expectedObjectID || payload.Diagrams[0].Type != "tikzcd" {
		t.Fatalf("unexpected diagram refs: %#v", payload.Diagrams)
	}
	foundDiagramDiagnostic := false
	for _, diagnostic := range payload.Diagnostics {
		if diagnostic.Code == "diagram.worker.warning" {
			foundDiagramDiagnostic = diagnostic.Source["path"] == "main.tex" && diagnostic.Source["line"] == "7" && diagnostic.Source["column"] == "1"
		}
	}
	if !foundDiagramDiagnostic {
		t.Fatalf("diagram source location was not preserved in diagnostics: %#v", payload.Diagnostics)
	}
	if strings.Contains(payload.HTML, "RINRENDERERDIAGRAMPLACEHOLDER") || strings.Contains(payload.HTML, `\begin{tikzcd}`) {
		t.Fatalf("expected placeholders and source to be removed from HTML: %s", payload.HTML)
	}
	if strings.Contains(payload.HTML, "/rin/api/diagrams") {
		t.Fatalf("expected project HTML to avoid local diagram URLs, got %s", payload.HTML)
	}
	expectedURL := "https://cdn.example/v1/storages/object/public/rin-renderer/" + expectedObjectID
	for _, expected := range []string{`<div class="rin-align-block rin-align-center" data-rin-align="center"><figure class="rin-tikz rin-tikz-tikzcd"`, `<div class="rin-tikz-svg">`, `src="` + expectedURL + `"`, `data-rin-diagram-object-id="` + expectedObjectID + `"`} {
		if !strings.Contains(payload.HTML, expected) {
			t.Fatalf("expected HTML to contain %q, got %s", expected, payload.HTML)
		}
	}
	if len(payload.GeneratedArtifacts) != 1 || payload.GeneratedArtifacts[0].ArtifactID != expectedObjectID || payload.GeneratedArtifacts[0].SHA256 != svgObject.Hash {
		t.Fatalf("expected verified generated artifact metadata, got %#v", payload.GeneratedArtifacts)
	}
}

func TestReplaceDiagramPlaceholdersUsesStandaloneFigureOutsideLateXMLFigure(t *testing.T) {
	placeholder := "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000001"
	replacements := map[string]diagramHTMLReplacement{
		placeholder: diagramReplacementHTML(projectdiagrams.Diagram{
			ID:         "diagram-000001",
			Type:       "tikzcd",
			SourceFile: "main.tex",
		}, DiagramRenderResponse{
			ID:           "diagram-id",
			Type:         "tikzcd",
			CloudBaseURL: "https://cdn.example/diagram.svg",
			ObjectID:     "diagrams/v1/example.svg",
		}),
	}

	html := `<article><p>` + placeholder + `</p></article>`
	got := replaceDiagramPlaceholders(html, replacements)

	if !strings.Contains(got, `<figure class="rin-tikz rin-tikz-tikzcd"`) {
		t.Fatalf("expected standalone placeholder to become a Rin figure, got %s", got)
	}
	if strings.Contains(got, placeholder) {
		t.Fatalf("expected placeholder to be removed, got %s", got)
	}
}

func TestReplaceDiagramPlaceholdersSplitsMixedParagraphOutsideFigure(t *testing.T) {
	placeholder := "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000001"
	replacements := map[string]diagramHTMLReplacement{
		placeholder: diagramReplacementHTML(projectdiagrams.Diagram{
			ID:         "diagram-000001",
			Type:       "tikzcd",
			SourceFile: "main.tex",
		}, DiagramRenderResponse{
			ID:           "diagram-id",
			Type:         "tikzcd",
			CloudBaseURL: "https://cdn.example/diagram.svg",
			ObjectID:     "diagrams/v1/example.svg",
		}),
	}

	html := `<article><p>Text before ` + placeholder + ` text after.</p></article>`
	got := replaceDiagramPlaceholders(html, replacements)

	if strings.Contains(got, `<p>Text before <figure`) {
		t.Fatalf("expected mixed paragraph to be split before inserting standalone figure, got %s", got)
	}
	for _, expected := range []string{`<p>Text before </p>`, `<figure class="rin-tikz rin-tikz-tikzcd"`, `<p> text after.</p>`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected HTML to contain %q, got %s", expected, got)
		}
	}
}

func TestReplaceDiagramPlaceholdersPreservesLateXMLFigureCaption(t *testing.T) {
	placeholder := "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000001"
	replacements := map[string]diagramHTMLReplacement{
		placeholder: diagramReplacementHTML(projectdiagrams.Diagram{
			ID:         "diagram-000001",
			Type:       "tikzcd",
			SourceFile: "main.tex",
			Layout: projectdiagrams.Layout{
				Alignment: "center",
			},
		}, DiagramRenderResponse{
			ID:           "diagram-id",
			Type:         "tikzcd",
			CloudBaseURL: "https://cdn.example/diagram.svg",
			ObjectID:     "diagrams/v1/example.svg",
		}),
	}

	html := `<article><figure id="fig:a" class="ltx_figure"><p>` + placeholder + `</p><figcaption class="ltx_caption"><span class="ltx_tag">Figure 1</span>A caption.</figcaption></figure></article>`
	got := replaceDiagramPlaceholders(html, replacements)

	for _, expected := range []string{
		`<figure id="fig:a" class="ltx_figure">`,
		`<figcaption class="ltx_caption">`,
		`A caption.`,
		`<div class="rin-align-block rin-align-center" data-rin-align="center"><div class="rin-tikz rin-tikz-tikzcd"`,
		`src="https://cdn.example/diagram.svg"`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected HTML to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, placeholder) || strings.Contains(got, `<figure class="rin-tikz`) || strings.Count(got, "<figure") != 1 {
		t.Fatalf("expected LaTeXML figure shell to be preserved without nested Rin figure, got %s", got)
	}
}

func TestReplaceDiagramPlaceholdersAvoidsNestedFigureInMixedLateXMLFigure(t *testing.T) {
	placeholder := "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000001"
	replacements := map[string]diagramHTMLReplacement{
		placeholder: diagramReplacementHTML(projectdiagrams.Diagram{
			ID:         "diagram-000001",
			Type:       "tikzcd",
			SourceFile: "main.tex",
		}, DiagramRenderResponse{
			ID:           "diagram-id",
			Type:         "tikzcd",
			CloudBaseURL: "https://cdn.example/diagram.svg",
			ObjectID:     "diagrams/v1/example.svg",
		}),
	}

	html := `<article><figure class="ltx_figure"><p>Text before ` + placeholder + ` text after.</p><figcaption>Caption.</figcaption></figure></article>`
	got := replaceDiagramPlaceholders(html, replacements)

	if strings.Contains(got, `<figure class="rin-tikz`) || strings.Count(got, "<figure") != 1 {
		t.Fatalf("expected mixed LaTeXML figure to avoid nested Rin figure, got %s", got)
	}
	if strings.Contains(got, `<p>Text before <div`) {
		t.Fatalf("expected mixed LaTeXML figure paragraph to be split before inserting diagram div, got %s", got)
	}
	for _, expected := range []string{`<p>Text before </p>`, `<p> text after.</p>`, `<div class="rin-tikz rin-tikz-tikzcd"`, `<figcaption>Caption.</figcaption>`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected HTML to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, placeholder) {
		t.Fatalf("expected placeholder to be removed, got %s", got)
	}
}

func TestReplaceDiagramPlaceholdersPreservesLateXMLSubfigureContainer(t *testing.T) {
	placeholder := "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000001"
	replacements := map[string]diagramHTMLReplacement{
		placeholder: diagramReplacementHTML(projectdiagrams.Diagram{
			ID:         "diagram-000001",
			Type:       "tikzcd",
			SourceFile: "main.tex",
		}, DiagramRenderResponse{
			ID:           "diagram-id",
			Type:         "tikzcd",
			CloudBaseURL: "https://cdn.example/diagram.svg",
			ObjectID:     "diagrams/v1/example.svg",
		}),
	}

	html := `<article><div id="fig:left" class="ltx_subfigure"><p>` + placeholder + `</p><div class="ltx_caption">Left caption.</div></div></article>`
	got := replaceDiagramPlaceholders(html, replacements)

	for _, expected := range []string{
		`<div id="fig:left" class="ltx_subfigure">`,
		`<div class="rin-tikz rin-tikz-tikzcd"`,
		`<div class="ltx_caption">Left caption.</div>`,
		`src="https://cdn.example/diagram.svg"`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected HTML to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, placeholder) || strings.Contains(got, `<figure class="rin-tikz`) {
		t.Fatalf("expected LaTeXML subfigure container to avoid standalone Rin figure, got %s", got)
	}
}

func TestReplaceDiagramPlaceholdersPreservesLateXMLMinipageContainer(t *testing.T) {
	placeholder := "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000001"
	replacements := map[string]diagramHTMLReplacement{
		placeholder: diagramReplacementHTML(projectdiagrams.Diagram{
			ID:         "diagram-000001",
			Type:       "xymatrix",
			SourceFile: "main.tex",
		}, DiagramRenderResponse{
			ID:           "diagram-id",
			Type:         "xymatrix",
			CloudBaseURL: "https://cdn.example/matrix.svg",
			ObjectID:     "diagrams/v1/matrix.svg",
		}),
	}

	html := `<article><div class="ltx_minipage"><p>Before.</p><p>` + placeholder + `</p><p>After.</p></div></article>`
	got := replaceDiagramPlaceholders(html, replacements)

	for _, expected := range []string{
		`<div class="ltx_minipage">`,
		`<p>Before.</p>`,
		`<div class="rin-tikz rin-tikz-xymatrix"`,
		`<p>After.</p>`,
		`src="https://cdn.example/matrix.svg"`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected HTML to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, placeholder) || strings.Contains(got, `<figure class="rin-tikz`) {
		t.Fatalf("expected LaTeXML minipage container to avoid standalone Rin figure, got %s", got)
	}
}

func TestReplaceDiagramPlaceholdersScansLargeLateXMLContainersEfficiently(t *testing.T) {
	placeholder := "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000001"
	replacements := map[string]diagramHTMLReplacement{
		placeholder: diagramReplacementHTML(projectdiagrams.Diagram{
			ID:         "diagram-000001",
			Type:       "tikzcd",
			SourceFile: "main.tex",
		}, DiagramRenderResponse{
			ID:           "diagram-id",
			Type:         "tikzcd",
			CloudBaseURL: "https://cdn.example/diagram.svg",
			ObjectID:     "diagrams/v1/example.svg",
		}),
	}

	var html strings.Builder
	html.WriteString(`<article>`)
	for index := 0; index < 2000; index++ {
		html.WriteString(`<div class="ltx_para"><p>Filler paragraph.</p></div>`)
	}
	html.WriteString(`<DIV class="ltx_minipage"><p>Before.</p><p>`)
	html.WriteString(placeholder)
	html.WriteString(`</p><DIV class="ltx_caption">Caption.</DIV></DIV></article>`)

	got := replaceDiagramPlaceholders(html.String(), replacements)
	for _, expected := range []string{
		`<DIV class="ltx_minipage">`,
		`<div class="rin-tikz rin-tikz-tikzcd"`,
		`src="https://cdn.example/diagram.svg"`,
		`<DIV class="ltx_caption">Caption.</DIV>`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected large HTML replacement to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, placeholder) || strings.Contains(got, `<figure class="rin-tikz`) {
		t.Fatalf("expected large LaTeXML container replacement to avoid standalone figure, got %s", got)
	}
}

func TestLateXMLPostprocessPreservesFigureLabelsAndReferences(t *testing.T) {
	placeholder := "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000001"
	replacements := map[string]diagramHTMLReplacement{
		placeholder: diagramReplacementHTML(projectdiagrams.Diagram{
			ID:         "diagram-000001",
			Type:       "tikzpicture",
			SourceFile: "main.tex",
		}, DiagramRenderResponse{
			ID:           "diagram-id",
			Type:         "tikzpicture",
			CloudBaseURL: "https://cdn.example/diagram.svg",
			ObjectID:     "diagrams/v1/example.svg",
		}),
	}

	html := strings.Join([]string{
		`<article>`,
		`<figure id="fig:outer" class="ltx_figure">`,
		`<p><a id="fig:diagram" class="ltx_label"></a>` + placeholder + `</p>`,
		`<figcaption class="ltx_caption"><span class="ltx_tag">Figure 1</span>Diagram caption.</figcaption>`,
		`</figure>`,
		`<p>See <a class="ltx_ref" href="#fig:outer">Figure 1</a> and <a class="ltx_ref" href="#fig:diagram">the diagram label</a>.</p>`,
		`</article>`,
	}, "")

	got := replaceDiagramPlaceholders(normalizeLateXMLHTML(html, projectcore.Manifest{}), replacements)

	for _, expected := range []string{
		`<figure id="fig:outer" class="ltx_figure rin-float rin-float-figure">`,
		`<a id="fig:diagram" class="ltx_label rin-label"></a>`,
		`<figcaption class="ltx_caption rin-caption">`,
		`Diagram caption.`,
		`<a class="ltx_ref rin-ref" href="#fig:outer">Figure 1</a>`,
		`<a class="ltx_ref rin-ref" href="#fig:diagram">the diagram label</a>`,
		`<div class="rin-tikz rin-tikz-tikzpicture"`,
		`src="https://cdn.example/diagram.svg"`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected HTML to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, placeholder) || strings.Contains(got, `<figure class="rin-tikz`) || strings.Count(got, "<figure") != 1 {
		t.Fatalf("expected LaTeXML figure shell and refs to be preserved without nested Rin figure, got %s", got)
	}
}

func TestNormalizeLateXMLHTMLMarksReferencedAssets(t *testing.T) {
	manifest := projectcore.Manifest{
		AssetInventory: projectcore.AssetInventory{
			References: []projectcore.Reference{{
				Kind:     "asset",
				Command:  "includegraphics",
				RawRef:   "chart",
				Path:     "figures/chart.png",
				Resolved: true,
			}},
			Assets: []projectcore.Asset{
				{Path: "figures/chart.png", MIME: "image/png", Referenced: true},
				{Path: "figures/unused.png", MIME: "image/png", Referenced: false},
			},
		},
	}
	html := strings.Join([]string{
		`<article>`,
		`<img src="figures/chart.png?cache=1">`,
		`<object data="./figures/chart.png"></object>`,
		`<a href="chart.png">download</a>`,
		`<img src="https://cdn.example/chart.png">`,
		`<img src="figures/unused.png">`,
		`</article>`,
	}, "")

	got := normalizeLateXMLHTML(html, manifest)

	if strings.Count(got, `data-rin-asset-path="figures/chart.png"`) != 3 {
		t.Fatalf("expected all referenced local asset tags to be marked, got %s", got)
	}
	for _, unexpected := range []string{
		`<img src="https://cdn.example/chart.png" data-rin-asset-path=`,
		`<img src="figures/unused.png" data-rin-asset-path=`,
	} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("expected HTML not to contain %q, got %s", unexpected, got)
		}
	}
}

func TestNormalizeLateXMLHTMLStripsUnsafeBodyOutput(t *testing.T) {
	html := `<article><p onclick="alert(1)">OK</p><script>alert(1)</script><iframe src="/x"></iframe><a href="javascript:alert(1)">bad</a><img src='javascript:alert(2)'><img src="data:image/png;base64,iVBORw0KGgo="><img src="fallback.png" srcset="data:image/png;base64,iVBORw0KGgo= 1x"><img src="https://cdn.example/safe.png"></article>`

	got := normalizeLateXMLHTML(html, projectcore.Manifest{})
	lower := strings.ToLower(got)

	for _, unexpected := range []string{"<script", "<iframe", "onclick=", "javascript:", "data:image"} {
		if strings.Contains(lower, unexpected) {
			t.Fatalf("expected unsafe HTML fragment %q to be stripped, got %s", unexpected, got)
		}
	}
	if !strings.Contains(got, "OK") || !strings.Contains(got, "bad") {
		t.Fatalf("expected safe text content to remain, got %s", got)
	}
	if !strings.Contains(got, `src="https://cdn.example/safe.png"`) {
		t.Fatalf("expected safe external image to remain, got %s", got)
	}
}

func TestNormalizeLateXMLHTMLAddsStableHeadingIDs(t *testing.T) {
	html := `<article><h2 class="ltx_title">第一章 引言</h2><h3>Setup</h3><h2>第一章 引言</h2></article>`

	got := normalizeLateXMLHTML(html, projectcore.Manifest{})

	for _, expected := range []string{
		`<h2 id="第一章-引言" class="ltx_title">第一章 引言</h2>`,
		`<h3 id="setup">Setup</h3>`,
		`<h2 id="第一章-引言-2">第一章 引言</h2>`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected normalized HTML to contain %q, got %s", expected, got)
		}
	}
}

func TestNormalizeLateXMLHTMLAddsMathMLClasses(t *testing.T) {
	html := `<article><p>Inline <math class="ltx_Math" alttext="x"><mi>x</mi></math>.</p><div class="ltx_equation"><math display="block"><mi>y</mi></math></div><math class="rin-math rin-math-inline"><mi>z</mi></math></article>`

	got := normalizeLateXMLHTML(html, projectcore.Manifest{})

	for _, expected := range []string{
		`<math class="ltx_Math rin-math rin-math-inline" alttext="x">`,
		`<math display="block" class="rin-math rin-math-display">`,
		`<math class="rin-math rin-math-inline">`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected normalized MathML to contain %q, got %s", expected, got)
		}
	}
	for _, unexpected := range []string{`rin-math-inline rin-math-inline`, `rin-math-display rin-math-display`} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("expected normalized MathML not to duplicate %q, got %s", unexpected, got)
		}
	}
}

func TestRenderLateXMLMathHTMLUsesServerMathJax(t *testing.T) {
	cfg := testConfig()
	server := &Server{cfg: cfg}
	html := strings.Join([]string{
		`<article><p>Inline <math class="ltx_Math rin-math rin-math-inline" alttext="\not\exists x"><mi>x</mi></math>.</p>`,
		`<div class="ltx_equation"><math display="block" class="rin-math rin-math-display" alttext="\not\exists x"><mi>x</mi></math></div></article>`,
	}, "")

	got, summary, _, diagnostics := server.renderLateXMLMathHTML(t.Context(), "request-id", html)

	if summary.Count != 2 || summary.MathJaxCount != 2 || summary.FailedCount != 0 {
		t.Fatalf("unexpected math summary: %#v, diagnostics: %#v", summary, diagnostics)
	}
	for _, expected := range []string{
		`rin-math-mathjax`, `data-rin-math-engine="mathjax-chtml"`,
		`data-rin-math-source="\not\exists x"`, `data-latex="\not\exists x"`,
		`mjx-container`, `mjx-c.mjx-c2204`, `∄`, `rin-mathjax-chtml-style`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected rendered math HTML to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, `<math`) {
		t.Fatalf("expected MathML elements to be replaced, got %s", got)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %#v", diagnostics)
	}
}

func TestRenderLateXMLMathHTMLPreservesBook260StretchyDelimiterContract(t *testing.T) {
	cfg := testConfig()
	server := &Server{cfg: cfg}
	source := `\left(\begin{bmatrix}a_1\\a_2\\a_3\end{bmatrix}\right)`
	html := `<article><math display="block" class="rin-math rin-math-display" alttext="` + source + `"><mi>x</mi></math></article>`

	got, summary, _, diagnostics := server.renderLateXMLMathHTML(t.Context(), "request-id", html)

	if summary.Count != 1 || summary.MathJaxCount != 1 || summary.SVGFallbackCount != 0 || summary.FailedCount != 0 {
		t.Fatalf("unexpected stretchy math summary: %#v diagnostics=%#v", summary, diagnostics)
	}
	for _, expected := range []string{
		`rin-math-display rin-math-mathjax`, `data-rin-math-source="` + source + `"`,
		`data-latex="` + source + `"`, `mjx-stretchy-v`, `rin-mathjax-chtml-style`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected stretchy MathJax contract to retain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, `rin-math-svg`) || strings.Contains(got, `RINRENDERERSHAREDMATH`) {
		t.Fatalf("expected native CHTML with no internal placeholders, got %s", got)
	}
}

func TestNormalizeLateXMLMathSourceCleansPGFColorAndTurnbox(t *testing.T) {
	source := normalizeLateXMLMathSource("\\displaystyle D(x)=1={\\color[rgb]{1,0,0}%\n\\definecolor[named]{pgfstrokecolor}{rgb}{1,0,0}1}+\\turnbox{90.0}{$\\ddots$}")

	for _, expected := range []string{`D(x)=1=`, `{\color{red}1}`, `+\ddots`} {
		if !strings.Contains(source, expected) {
			t.Fatalf("expected normalized source to contain %q, got %s", expected, source)
		}
	}
	for _, unexpected := range []string{`\displaystyle`, `\color[rgb]`, `\definecolor`, `\turnbox`} {
		if strings.Contains(source, unexpected) {
			t.Fatalf("expected normalized source to omit %q, got %s", unexpected, source)
		}
	}
}

func TestNormalizeLateXMLMathSourceDoesNotGroupMatrixForMathJax(t *testing.T) {
	source := normalizeLateXMLMathSource("\\begin{aligned}&\\begin{vmatrix}1&2\\\\3&4\\end{%\nvmatrix}\\end{aligned}")

	if strings.Contains(source, `{\begin{vmatrix}`) || strings.Contains(source, `\end{vmatrix}}`) {
		t.Fatalf("expected MathJax source to keep matrix environment unwrapped, got %s", source)
	}
	for _, expected := range []string{`\begin{aligned}&\begin{vmatrix}`, `\end{vmatrix}\end{aligned}`} {
		if !strings.Contains(source, expected) {
			t.Fatalf("expected normalized source to contain %q, got %s", expected, source)
		}
	}
	if strings.Contains(source, "%") || strings.Contains(source, "\n") {
		t.Fatalf("expected line continuation artifacts to be removed, got %q", source)
	}
}

func TestNormalizeMathSourceForTeXSVGGroupsMatrixEnvironments(t *testing.T) {
	source := normalizeMathSourceForTeXSVG("\\begin{aligned}&\\begin{vmatrix}1&2\\\\3&4\\end{%\nvmatrix}\\end{aligned}")

	for _, expected := range []string{`\begin{aligned}&{\begin{vmatrix}`, `\end{vmatrix}}\end{aligned}`} {
		if !strings.Contains(source, expected) {
			t.Fatalf("expected normalized source to contain %q, got %s", expected, source)
		}
	}
	if strings.Contains(source, "%") || strings.Contains(source, "\n") {
		t.Fatalf("expected line continuation artifacts to be removed, got %q", source)
	}
}

func TestNormalizeLateXMLMathSourcePreservesNegatedExistsSource(t *testing.T) {
	for _, input := range []string{`\not\exists k\in\mathbb{R}`, `\not \exists k\in\mathbb{R}`} {
		source := normalizeLateXMLMathSource(input)
		if source != input {
			t.Fatalf("expected negated exists source to remain %q, got %q", input, source)
		}
	}
}

func TestNormalizeLateXMLMathSourceWrapsOperatorNameUnicodeText(t *testing.T) {
	source := normalizeLateXMLMathSource(`\operatorname{Sin，g}_{\bullet}(X)`)

	if source != `\operatorname{Sin\text{，}g}_{\bullet}(X)` {
		t.Fatalf("expected operatorname unicode punctuation to be wrapped for KaTeX, got %q", source)
	}
}

func TestReplaceSourceMathPlaceholdersHTMLUsesServerMathJax(t *testing.T) {
	cfg := testConfig()
	server := &Server{cfg: cfg}
	unit := projectmath.Unit{
		ID:           "math-000001",
		Environment:  "align",
		RenderSource: `\begin{aligned}\mathbf{x}_{1}\pm\mathbf{x}_{2} &= \ket{x_{1}}\pm\ket{x_{2}}\end{aligned}`,
		Placeholder:  "RINRENDERERMATHPLACEHOLDERMATH000001",
		DisplayMode:  true,
	}
	html := `<article><p>Before RINRENDERERMATHPLACEHOLDERMATH000001 After.</p></article>`

	got, summary, _, diagnostics := server.replaceSourceMathPlaceholdersHTML(t.Context(), "request-id", html, []projectmath.Unit{unit})

	if summary.Count != 1 || summary.MathJaxCount != 1 || summary.FailedCount != 0 {
		t.Fatalf("unexpected math summary: %#v diagnostics=%#v", summary, diagnostics)
	}
	for _, expected := range []string{`id="math-000001"`, `rin-math-display rin-math-mathjax`, `data-rin-math-engine="mathjax-chtml"`, `mjx-container`, `⟩`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected source math placeholder replacement to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, unit.Placeholder) || strings.Contains(got, `<math`) {
		t.Fatalf("expected source math placeholder to be fully replaced, got %s", got)
	}
}

func TestReplaceSourceMathPlaceholdersHTMLUsesProjectPreambleMacros(t *testing.T) {
	cfg := testConfig()
	server := &Server{cfg: cfg}
	unit := projectmath.Unit{
		ID:           "math-000001",
		Environment:  "display",
		RenderSource: `\Phi_\calE\colon \bbZ \to \calE`,
		Placeholder:  "RINRENDERERMATHPLACEHOLDERMATH000001",
		DisplayMode:  true,
	}
	html := `<article>RINRENDERERMATHPLACEHOLDERMATH000001</article>`
	macros := map[string]string{`\calE`: `\mathcal{E}`, `\bbZ`: `\mathbb{Z}`}

	got, summary, _, diagnostics := server.replaceSourceMathPlaceholdersHTML(t.Context(), "request-id", html, []projectmath.Unit{unit}, macros)

	if summary.Count != 1 || summary.MathJaxCount != 1 || summary.FailedCount != 0 {
		t.Fatalf("unexpected macro-aware math summary: %#v diagnostics=%#v", summary, diagnostics)
	}
	for _, expected := range []string{`rin-math-mathjax`, `data-rin-math-engine="mathjax-chtml"`, `data-rin-math-source="\Phi_\calE\colon \bbZ \to \calE"`, `NCM-C`, `NCM-DS`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected macro-aware MathJax output to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, `rin-math-fallback`) || strings.Contains(got, unit.Placeholder) {
		t.Fatalf("expected project macros to avoid raw TeX fallback, got %s", got)
	}
}

func TestReplaceSourceMathPlaceholdersHTMLKeepsMathJaxMatrixSourceUnwrapped(t *testing.T) {
	cfg := testConfig()
	server := &Server{cfg: cfg}
	unit := projectmath.Unit{
		ID:           "math-000001",
		Environment:  "inline",
		RenderSource: `\mathbf{x}={\begin{bmatrix}a_{1}\\a_{2}\\\cdots\\a_{n}\end{bmatrix}}=\ket{x}`,
		Placeholder:  "RINRENDERERMATHPLACEHOLDERMATH000001",
		DisplayMode:  false,
	}
	html := `<article><p>列向量：RINRENDERERMATHPLACEHOLDERMATH000001。</p></article>`

	got, summary, _, diagnostics := server.replaceSourceMathPlaceholdersHTML(t.Context(), "request-id", html, []projectmath.Unit{unit})

	if summary.Count != 1 || summary.MathJaxCount != 1 || summary.SVGFallbackCount != 0 || summary.FailedCount != 0 {
		t.Fatalf("unexpected math summary: %#v diagnostics=%#v", summary, diagnostics)
	}
	for _, expected := range []string{`data-rin-math-source="\mathbf{x}={\begin{bmatrix}`, `data-latex="\mathbf{x}={\begin{bmatrix}`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected MathJax matrix source to contain %q, got %s", expected, got)
		}
	}
	for _, unexpected := range []string{`{{\begin{bmatrix}`, `{{{bmatrix}}}`} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("expected MathJax matrix source to avoid %q, got %s", unexpected, got)
		}
	}
}

func TestRenderLateXMLMathHTMLCollapsesEquationTable(t *testing.T) {
	cfg := testConfig()
	server := &Server{cfg: cfg}
	html := strings.Join([]string{
		`<article><table id="eq1" class="ltx_equationgroup ltx_eqn_table rin-equation rin-equation-table"><tbody><tr>`,
		`<td><math class="rin-math rin-math-display" display="block" alttext="\displaystyle x"><mi>x</mi></math></td>`,
		`<td><math class="rin-math rin-math-display" display="block" alttext="\displaystyle = y"><mi>y</mi></math></td>`,
		`</tr></tbody></table></article>`,
	}, "")

	got, summary, _, diagnostics := server.renderLateXMLMathHTML(t.Context(), "request-id", html)

	if summary.Count != 1 || summary.MathJaxCount != 1 || summary.FailedCount != 0 {
		t.Fatalf("unexpected math summary: %#v, diagnostics: %#v", summary, diagnostics)
	}
	for _, expected := range []string{`id="eq1"`, `rin-math-display rin-math-mathjax`, `\begin{aligned}x &amp; = y\end{aligned}`, `mjx-container`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected collapsed equation table to contain %q, got %s", expected, got)
		}
	}
	for _, unexpected := range []string{`<table`, `<td`, `<math`} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("expected equation table markup to be replaced, got %s", got)
		}
	}
}

func TestRenderLateXMLMathHTMLCollapsesBook260VectorEquationTable(t *testing.T) {
	cfg := testConfig()
	server := &Server{cfg: cfg}
	html := strings.Join([]string{
		`<article><table id="Ch12.S5.EGx2" class="ltx_equationgroup ltx_eqn_align ltx_eqn_table rin-equation rin-equation-table"><tbody><tr>`,
		`<td><math class="rin-math rin-math-display" display="block" alttext="\displaystyle\mathbf{x}_{1}\pm\mathbf{x}_{2}"><mi>x</mi></math></td>`,
		`<td><math class="rin-math rin-math-display" display="block" alttext="\displaystyle=\ket{x_{1}}\pm\ket{x_{2}}=\begin{bmatrix}a_{1}\pm b_{1}\\a_{2}\pm b_{2}\\\cdots\\a_{n}\pm b_{n}\end{bmatrix}"><mi>y</mi></math></td>`,
		`</tr></tbody></table></article>`,
	}, "")

	got, summary, _, diagnostics := server.renderLateXMLMathHTML(t.Context(), "request-id", html)

	if summary.Count != 1 || summary.MathJaxCount != 1 || summary.SVGFallbackCount != 0 || summary.FailedCount != 0 {
		t.Fatalf("unexpected math summary: %#v, diagnostics: %#v", summary, diagnostics)
	}
	for _, expected := range []string{`id="Ch12.S5.EGx2"`, `rin-math-display rin-math-mathjax`, `\begin{aligned}\mathbf{x}_{1}\pm\mathbf{x}_{2} &amp; =\ket{x_{1}}`, `mjx-container`, `⟩`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected book260 equation table to contain %q, got %s", expected, got)
		}
	}
	for _, unexpected := range []string{`<table`, `<td`, `<math`, `rin-math-svg`} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("expected book260 equation table to be MathJax-rendered as one block, got %s", got)
		}
	}
}

func TestRenderLateXMLMathHTMLCanPreferTexSVGForComplexMath(t *testing.T) {
	const workerToken = "worker-token"
	const cloudbaseToken = "cloudbase-token"
	rawSVG := `<svg height="10" width="20"><path d="M0 0L20 10"/></svg>`
	svgObject, err := renderstorage.NormalizeSVGObject([]byte(rawSVG))
	if err != nil {
		t.Fatal(err)
	}
	expectedObjectID := svgObject.ObjectID

	var workerPayload DiagramRequest
	var workerCalls int
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerCalls++
		if r.Header.Get("Authorization") != "Bearer "+workerToken {
			http.Error(w, "missing worker token", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&workerPayload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"math-id","url":"/rendered/math-id","svg":%q,"cached":false,"type":"math"}`, rawSVG)
	}))
	defer worker.Close()

	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			http.NotFound(w, r)
		case http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer "+cloudbaseToken {
				http.Error(w, "missing CloudBase token", http.StatusUnauthorized)
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"Id":"object-id","Key":%q}`, "rin-renderer/"+expectedObjectID)
		case http.MethodGet:
			_, _ = w.Write(svgObject.Bytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer storage.Close()

	cfg := testConfig()
	cfg.MathOutputStrategy = "complex-svg"
	cfg.DiagramWorkerEndpoint = worker.URL
	cfg.DiagramWorkerToken = workerToken
	cfg.StorageBaseURL = storage.URL
	cfg.StorageAccessToken = cloudbaseToken
	server := &Server{cfg: cfg}
	html := `<article><p><math class="rin-math rin-math-display" display="block" alttext="\displaystyle k\begin{bmatrix}a_1\\a_2\end{bmatrix}"><mi>x</mi></math></p></article>`

	got, summary, artifacts, diagnostics := server.renderLateXMLMathHTML(t.Context(), "request-id", html)

	if summary.Count != 1 || summary.MathJaxCount != 0 || summary.SVGFallbackCount != 1 || summary.FailedCount != 0 {
		t.Fatalf("unexpected math summary: %#v diagnostics=%#v", summary, diagnostics)
	}
	if workerCalls != 1 || workerPayload.Type != "math" || workerPayload.Options != "display" {
		t.Fatalf("expected one display math SVG worker call, calls=%d payload=%#v", workerCalls, workerPayload)
	}
	if len(artifacts) != 1 || artifacts[0].ArtifactID != expectedObjectID || artifacts[0].SHA256 != svgObject.Hash {
		t.Fatalf("expected verified math artifact metadata, got %#v", artifacts)
	}
	for _, expected := range []string{`rin-math-svg`, `data-rin-math-engine="rin-texsvg"`, expectedObjectID, `loading="lazy"`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected configured SVG math HTML to contain %q, got %s", expected, got)
		}
	}
	for _, unexpected := range []string{`mjx-container`, `rin-mathjax-chtml-style`, `<math`} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("expected configured SVG math to avoid %q, got %s", unexpected, got)
		}
	}
}

func TestRenderLateXMLMathHTMLFallsBackToTexSVG(t *testing.T) {
	const workerToken = "worker-token"
	const cloudbaseToken = "cloudbase-token"
	rawSVG := `<svg height="10" width="20"><script>alert(1)</script><path d="M0 0L20 10"/></svg>`
	svgObject, err := renderstorage.NormalizeSVGObject([]byte(rawSVG))
	if err != nil {
		t.Fatal(err)
	}
	expectedObjectID := svgObject.ObjectID

	var workerPath string
	var workerPayload DiagramRequest
	var workerCalls int
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerCalls++
		workerPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer "+workerToken {
			http.Error(w, "missing worker token", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&workerPayload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"math-id","url":"/rendered/math-id","svg":%q,"cached":false,"type":"math"}`, rawSVG)
	}))
	defer worker.Close()

	var uploadedBody string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			http.NotFound(w, r)
		case http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer "+cloudbaseToken {
				http.Error(w, "missing CloudBase token", http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			uploadedBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"Id":"object-id","Key":%q}`, "rin-renderer/"+expectedObjectID)
		case http.MethodGet:
			_, _ = w.Write(svgObject.Bytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer storage.Close()

	cfg := testConfig()
	cfg.KaTeXScript = testRenderAPIKaTeXScriptPath(t)
	cfg.DiagramWorkerEndpoint = worker.URL
	cfg.DiagramWorkerToken = workerToken
	cfg.StorageBaseURL = storage.URL
	cfg.StorageAccessToken = cloudbaseToken
	server := &Server{cfg: cfg}
	html := `<article><p><math class="rin-math rin-math-inline" alttext="\definitelyunsupportedcommand{x}"><mi>x</mi></math><math class="rin-math rin-math-inline" alttext="\definitelyunsupportedcommand{x}"><mi>x</mi></math></p></article>`

	got, summary, _, diagnostics := server.renderLateXMLMathHTML(t.Context(), "request-id", html)

	if summary.Count != 2 || summary.MathJaxCount != 0 || summary.KaTeXCount != 0 || summary.SVGFallbackCount != 2 || summary.FailedCount != 0 {
		t.Fatalf("unexpected math summary: %#v diagnostics=%#v", summary, diagnostics)
	}
	if workerCalls != 1 {
		t.Fatalf("expected repeated fallback math to reuse one worker render, got %d calls", workerCalls)
	}
	if workerPath != "/math" || workerPayload.Type != "math" || workerPayload.Options != "inline" || workerPayload.Body != `\definitelyunsupportedcommand{x}` {
		t.Fatalf("unexpected math worker request path=%q payload=%#v", workerPath, workerPayload)
	}
	if strings.Contains(uploadedBody, "<script") {
		t.Fatalf("expected uploaded SVG to be sanitized, got %s", uploadedBody)
	}
	for _, expected := range []string{`rin-math-svg`, `data-rin-math-engine="rin-texsvg"`, expectedObjectID, `loading="lazy"`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected SVG fallback HTML to contain %q, got %s", expected, got)
		}
	}
	if !strings.Contains(fmt.Sprintf("%#v", diagnostics), "math.primary_fallback") {
		t.Fatalf("expected primary math fallback diagnostic, got %#v", diagnostics)
	}
}

func TestMathRenderCacheZeroValueFailsClosedAtFallbackLimit(t *testing.T) {
	cache := &mathRenderCache{}
	server := &Server{cfg: Config{MathTexSVGFallbackMaxCount: 0}}

	_, err := cache.renderSVG(t.Context(), server, "request-id", `\unsupported{x}`, false)

	if err == nil || !strings.Contains(err.Error(), "fallback limit reached") {
		t.Fatalf("expected bounded fallback error, got %v", err)
	}
	if len(cache.svg) != 1 || cache.svgRenderJobs != 0 {
		t.Fatalf("zero-value cache was not initialized and bounded: %#v", cache)
	}
}

func TestRenderLateXMLMathHTMLUsesEscapedSourceWhenFallbackUnavailable(t *testing.T) {
	cfg := testConfig()
	cfg.DiagramWorkerEndpoint = ""
	cfg.StorageBaseURL = ""
	server := &Server{cfg: cfg}
	source := `\definitelyunsupportedcommand{<script>alert(1)</script>}`
	html := `<article><math class="rin-math rin-math-inline" alttext="` + stdhtml.EscapeString(source) + `"><mi>x</mi></math></article>`

	got, summary, _, diagnostics := server.renderLateXMLMathHTML(t.Context(), "request-id", html)

	if summary.Count != 1 || summary.MathJaxCount != 0 || summary.SVGFallbackCount != 0 || summary.FailedCount != 1 {
		t.Fatalf("unexpected unavailable fallback summary: %#v diagnostics=%#v", summary, diagnostics)
	}
	for _, expected := range []string{`rin-math-fallback`, `rin-math-source-fallback`, `&lt;script&gt;alert(1)&lt;/script&gt;`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected escaped source fallback to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, `<script>`) || strings.Contains(got, `<math`) || strings.Contains(got, `RINRENDERERSHAREDMATH`) {
		t.Fatalf("expected no executable/raw/internal fallback markup, got %s", got)
	}
	formatted := fmt.Sprintf("%#v", diagnostics)
	for _, code := range []string{"math.primary_fallback", "math.fallback.unavailable"} {
		if !strings.Contains(formatted, code) {
			t.Fatalf("expected diagnostic %q, got %#v", code, diagnostics)
		}
	}
}

func TestPrependMathJaxStyleHTMLMergesLaterGlyphRules(t *testing.T) {
	html := `<style class="rin-mathjax-chtml-style" data-rin-math-engine="mathjax-chtml">mjx-container{display:inline-block}</style><p>Math</p>`
	glyphCSS := `mjx-container{display:inline-block}
mjx-c.mjx-c2204{padding:0.789em 0.556em 0.105em 0}`

	got := prependMathJaxStyleHTML(html, glyphCSS)
	if strings.Count(got, `rin-mathjax-chtml-style`) != 1 {
		t.Fatalf("expected one merged MathJax style, got %s", got)
	}
	if strings.Count(got, `mjx-container{display:inline-block}`) != 1 {
		t.Fatalf("expected shared MathJax rule to be deduplicated, got %s", got)
	}
	if !strings.Contains(got, `mjx-c.mjx-c2204{padding:0.789em 0.556em 0.105em 0}`) {
		t.Fatalf("expected later MathJax glyph rule to be retained, got %s", got)
	}
}

func TestNamespaceMathJaxIdentifiersKeepsRepeatedEquationTagsUnique(t *testing.T) {
	fragment := `<mjx-container id="unrelated"><mjx-mtable id="mjx-eqn:$\ast$" aria-labelledby="mjx-eqn:$\ast$"><a href="#mjx-eqn:$\ast$"><span style="clip-path:url(#mjx-eqn:$\ast$)">tag</span></a></mjx-mtable></mjx-container>`
	first := namespaceMathJaxIdentifiers(fragment, "rin-math-000001")
	second := namespaceMathJaxIdentifiers(fragment, "rin-math-000002")
	combined := first + second

	for _, expected := range []string{
		`id="rin-math-000001-mjx-eqn:$\ast$"`,
		`href="#rin-math-000001-mjx-eqn:$\ast$"`,
		`aria-labelledby="rin-math-000001-mjx-eqn:$\ast$"`,
		`url(#rin-math-000001-mjx-eqn:$\ast$)`,
		`id="rin-math-000002-mjx-eqn:$\ast$"`,
	} {
		if !strings.Contains(combined, expected) {
			t.Fatalf("namespaced MathJax HTML is missing %q: %s", expected, combined)
		}
	}
	if strings.Count(combined, `id="mjx-eqn:$\ast$"`) != 0 || strings.Count(combined, `id="unrelated"`) != 2 {
		t.Fatalf("MathJax IDs should be namespaced without changing unrelated IDs: %s", combined)
	}
}

func TestNormalizeLateXMLHTMLAddsStableSemanticClasses(t *testing.T) {
	html := strings.Join([]string{
		`<article>`,
		`<figure class="ltx_figure"><figcaption class="ltx_caption">Figure caption.</figcaption></figure>`,
		`<figure class="ltx_table"><table class="ltx_tabular"><tr><td>A</td></tr></table></figure>`,
		`<div class="ltx_subfigure"><a id="fig:a" class="ltx_label"></a><div class="ltx_caption">Subcaption.</div></div>`,
		`<div class="ltx_minipage"><p>Minipage.</p></div>`,
		`<div class="ltx_theorem"><h6 class="ltx_title ltx_title_theorem">Lemma 1</h6><p>Statement.</p></div>`,
		`<div class="ltx_theorem ltx_theorem_definition"><h6 class="ltx_title ltx_title_theorem">Definition 1</h6><p>Statement.</p></div>`,
		`<div class="ltx_proof">Proof.</div>`,
		`<a class="ltx_ref" href="#fig:a">Figure 1</a>`,
		`<a class="ltx_cite" href="#bib.knuth">[1]</a>`,
		`<div class="ltx_bibliography"><div id="bib.knuth" class="ltx_bibitem">Knuth</div></div>`,
		`<table class="ltx_longtable rin-table"><tr><td>B</td></tr></table>`,
		`<ol class="ltx_enumerate"><li class="ltx_item" style="list-style-type:none;"><span class="ltx_tag ltx_tag_item">1.</span> Item</li></ol>`,
		`<ul class="ltx_itemize"><li class="ltx_item"><span class="ltx_tag ltx_tag_item">•</span> Bullet</li></ul>`,
		`<table class="ltx_equation ltx_eqn_table"><tr><td><math display="block"><mi>x</mi></math></td></tr></table>`,
		`</article>`,
	}, "")

	got := normalizeLateXMLHTML(html, projectcore.Manifest{})

	for _, expected := range []string{
		`<figure class="ltx_figure rin-float rin-float-figure">`,
		`<figcaption class="ltx_caption rin-caption">`,
		`<figure class="ltx_table rin-float rin-float-table">`,
		`<table class="ltx_tabular rin-table rin-table-tabular">`,
		`<div class="ltx_subfigure rin-subfloat">`,
		`<a id="fig:a" class="ltx_label rin-label"></a>`,
		`<div class="ltx_caption rin-caption">Subcaption.</div>`,
		`<div class="ltx_minipage rin-minipage">`,
		`<div class="ltx_theorem rin-env rin-env-theorem">`,
		`<h6 class="ltx_title ltx_title_theorem rin-env-title">`,
		`<div class="ltx_theorem ltx_theorem_definition rin-env rin-env-definition">`,
		`<div class="ltx_proof rin-env rin-env-proof">`,
		`<a class="ltx_ref rin-ref" href="#fig:a">`,
		`<a class="ltx_cite rin-citation" href="#bib.knuth">`,
		`<div class="ltx_bibliography rin-bibliography">`,
		`<div id="bib.knuth" class="ltx_bibitem rin-bib-item">`,
		`<table class="ltx_longtable rin-table rin-table-longtable">`,
		`<ol class="ltx_enumerate rin-list rin-list-enumerate">`,
		`<li class="ltx_item rin-list-item" style="list-style-type:none;">`,
		`<span class="ltx_tag ltx_tag_item rin-list-marker">1.</span>`,
		`<ul class="ltx_itemize rin-list rin-list-itemize">`,
		`<table class="ltx_equation ltx_eqn_table rin-equation rin-equation-table">`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected normalized semantic HTML to contain %q, got %s", expected, got)
		}
	}
	for _, unexpected := range []string{`rin-table rin-table rin-table-longtable`, `rin-caption rin-caption`, `rin-ref rin-ref`, `rin-citation rin-citation`, `rin-env-theorem rin-env-theorem`, `rin-env-proof rin-env-proof`, `rin-list rin-list rin-list-enumerate`, `rin-list-item rin-list-item`} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("expected normalized semantic HTML not to duplicate %q, got %s", unexpected, got)
		}
	}
	if !strings.Contains(got, `<h6 class="ltx_title ltx_title_theorem rin-env-title">`) {
		t.Fatalf("expected theorem title to receive the title class, got %s", got)
	}
}

func testRenderAPIKaTeXScriptPath(t *testing.T) string {
	t.Helper()
	return testRenderAPIScriptPath("../../../engines/katex/render-katex.mjs")
}

func testRenderAPIMathJaxScriptPath(t *testing.T) string {
	t.Helper()
	return testRenderAPIScriptPath("../../../engines/mathjax/render-mathjax.mjs")
}

func testRenderAPIScriptPath(relative string) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot resolve test file path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), relative))
}

func TestNormalizeLateXMLHTMLStabilizesTheoremLikeParagraphs(t *testing.T) {
	html := strings.Join([]string{
		`<article>`,
		`<div id="def:v" class="ltx_para ltx_noindent">`,
		`<p class="ltx_p"><span class="ltx_text ltx_font_bold">Definition 1.1 (向量空间)</span></p>`,
		`<ol class="ltx_enumerate"><li class="ltx_item" style="list-style-type:none;"><span class="ltx_tag ltx_tag_item">1.</span> Axiom.</li></ol>`,
		`</div>`,
		`<div id="thm:v" class="ltx_para"><p class="ltx_p"><span class="ltx_text ltx_font_bold">Theorem</span> Statement.</p></div>`,
		`</article>`,
	}, "")

	got := normalizeLateXMLHTML(html, projectcore.Manifest{})

	for _, expected := range []string{
		`<div id="def:v" class="ltx_para ltx_noindent rin-env rin-env-definition">`,
		`<span class="ltx_text ltx_font_bold rin-env-title">Definition 1.1 (向量空间)</span>`,
		`<div id="thm:v" class="ltx_para rin-env rin-env-theorem">`,
		`<span class="ltx_text ltx_font_bold rin-env-title">Theorem</span>`,
		`<ol class="ltx_enumerate rin-list rin-list-enumerate">`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected normalized theorem-like HTML to contain %q, got %s", expected, got)
		}
	}
}

func TestNormalizeLateXMLHTMLGroupsParagraphEnvironmentContinuations(t *testing.T) {
	html := strings.Join([]string{
		`<article>`,
		`<div id="def-angle" class="ltx_para ltx_noindent">`,
		`<p class="ltx_p"><span class="ltx_text ltx_font_bold">Definition 1.18 (<span class="rin-math rin-math-inline">n</span>维向量的夹角)</span> 给定两个向量，定义：</p>`,
		`</div>`,
		`<div id="def-angle-math" class="ltx_para"><div class="rin-math rin-math-display">formula</div></div>`,
		`<div id="after-def" class="ltx_para ltx_noindent"><p class="ltx_p">After.</p></div>`,
		`<div id="note" class="ltx_para ltx_noindent">`,
		`<p class="ltx_p"><span class="ltx_text ltx_font_bold">Note</span> 如果 A 可逆，所以</p>`,
		`</div>`,
		`<div id="note-math" class="ltx_para"><div class="rin-math rin-math-display">det</div></div>`,
		`<section id="next"></section>`,
		`</article>`,
	}, "")

	got := normalizeLateXMLHTML(html, projectcore.Manifest{})

	for _, expected := range []string{
		`<div class="rin-env rin-env-definition rin-env-group">`,
		`<span class="ltx_text ltx_font_bold rin-env-title">Definition 1.18 (<span class="rin-math rin-math-inline">n</span>维向量的夹角)</span>`,
		`<div id="def-angle-math" class="ltx_para"><div class="rin-math rin-math-display">formula</div></div>`,
		`<div id="after-def" class="ltx_para ltx_noindent"><p class="ltx_p">After.</p></div>`,
		`<div class="rin-env rin-env-note rin-env-group">`,
		`<div id="note-math" class="ltx_para"><div class="rin-math rin-math-display">det</div></div>`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected grouped paragraph environment HTML to contain %q, got %s", expected, got)
		}
	}
	if strings.Contains(got, `After.</p></div></div>`) {
		t.Fatalf("expected noindent paragraph after definition to remain outside environment group, got %s", got)
	}
}

func TestNormalizeLateXMLHTMLRemovesTrailingBreaks(t *testing.T) {
	html := `<article><ul class="ltx_itemize"><li class="ltx_item"><p class="ltx_p">Item.<br class="ltx_break"></p></li></ul><p>A<br class="ltx_break">B</p></article>`

	got := normalizeLateXMLHTML(html, projectcore.Manifest{})

	if strings.Contains(got, `Item.<br class="ltx_break"></p>`) {
		t.Fatalf("expected trailing LaTeXML break to be removed, got %s", got)
	}
	if !strings.Contains(got, `A<br class="ltx_break">B`) {
		t.Fatalf("expected interior LaTeXML break to remain, got %s", got)
	}
}

func TestNormalizeLateXMLHTMLRemovesNondeterministicGeneratorFooter(t *testing.T) {
	html := `<div class="ltx_page_content"><article><p>Stable body.</p></article></div>` +
		`<footer class="ltx_page_footer"><div class="ltx_page_logo">Generated on Mon Aug 10 13:03:03 2026 by LaTeXML</div></footer>`

	got := normalizeLateXMLHTML(html, projectcore.Manifest{})

	if strings.Contains(got, "ltx_page_footer") || strings.Contains(got, "13:03:03") {
		t.Fatalf("expected nondeterministic LaTeXML footer to be removed, got %s", got)
	}
	if !strings.Contains(got, "Stable body.") {
		t.Fatalf("expected document body to remain, got %s", got)
	}
}

func TestReaderPayloadBuildsTOCAndPagesFromHeadings(t *testing.T) {
	html := `<article><p>Preface.</p><h2 id="intro">Intro</h2><p>A</p><h3 id="setup">Setup</h3><p>B</p><h2 id="next">Next</h2><p>C</p></article>`

	reader := readerPayload("Book", html)
	raw, err := json.Marshal(reader)
	if err != nil {
		t.Fatalf("marshal reader: %v", err)
	}
	var decoded struct {
		Version string `json:"version"`
		Title   string `json:"title"`
		TOC     []struct {
			ID    string `json:"id"`
			Text  string `json:"text"`
			Level int    `json:"level"`
		} `json:"toc"`
		Pages []struct {
			ID    string `json:"id"`
			Text  string `json:"text"`
			Level int    `json:"level"`
			HTML  string `json:"html"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode reader: %v", err)
	}

	if decoded.Version != "latexml-0.1" || decoded.Title != "Book" || len(decoded.TOC) != 3 || len(decoded.Pages) != 2 {
		t.Fatalf("unexpected reader payload: %#v", decoded)
	}
	if decoded.TOC[0].ID != "intro" || decoded.TOC[1].ID != "setup" || decoded.TOC[2].ID != "next" {
		t.Fatalf("unexpected toc: %#v", decoded.TOC)
	}
	if decoded.Pages[0].ID != "intro" || !strings.Contains(decoded.Pages[0].HTML, "Preface.") || !strings.Contains(decoded.Pages[0].HTML, `<h3 id="setup">Setup</h3>`) {
		t.Fatalf("unexpected first page: %#v", decoded.Pages[0])
	}
	if decoded.Pages[1].ID != "next" || strings.Contains(decoded.Pages[1].HTML, "Setup") || !strings.Contains(decoded.Pages[1].HTML, "<p>C</p>") {
		t.Fatalf("unexpected second page: %#v", decoded.Pages[1])
	}
}

func TestReaderPayloadKeepsUnsectionedChaptersAndChapterIntroductionsOnTheirOwnPages(t *testing.T) {
	html := `<article><h2 id="chapter-2" class="ltx_title_chapter">Chapter 2</h2>` +
		`<h3 id="section-2-3" class="ltx_title_section">2.3 Smoothness</h3><p>Second chapter.</p>` +
		`<h2 id="chapter-3" class="ltx_title_chapter">Chapter 3</h2><p>Third chapter only.</p>` +
		`<h2 id="chapter-4" class="ltx_title_chapter">Chapter 4</h2><p>Fourth chapter intro.</p>` +
		`<h3 id="section-4-1" class="ltx_title_section">4.1 Subgroups</h3><p>Fourth chapter body.</p>` +
		`<h2 id="bibliography" class="ltx_title_bibliography">Bibliography</h2><p>References.</p></article>`
	reader := readerPayload("Book", html)
	raw, err := json.Marshal(reader)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Pages []struct {
			ID   string `json:"id"`
			HTML string `json:"html"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Pages) != 4 {
		t.Fatalf("expected section pages plus standalone chapter and bibliography, got %#v", decoded.Pages)
	}
	for index, id := range []string{"section-2-3", "chapter-3", "section-4-1", "bibliography"} {
		if decoded.Pages[index].ID != id {
			t.Fatalf("page %d: expected %q, got %q", index, id, decoded.Pages[index].ID)
		}
	}
	if strings.Contains(decoded.Pages[0].HTML, "Chapter 3") || strings.Contains(decoded.Pages[0].HTML, "Chapter 4") {
		t.Fatalf("chapter content leaked into the preceding section: %s", decoded.Pages[0].HTML)
	}
	if !strings.Contains(decoded.Pages[1].HTML, "Third chapter only.") || strings.Contains(decoded.Pages[1].HTML, "Chapter 4") {
		t.Fatalf("unsectioned chapter was not isolated: %s", decoded.Pages[1].HTML)
	}
	if !strings.Contains(decoded.Pages[2].HTML, "Chapter 4") || !strings.Contains(decoded.Pages[2].HTML, "Fourth chapter intro.") || !strings.Contains(decoded.Pages[2].HTML, "Fourth chapter body.") || strings.Contains(decoded.Pages[2].HTML, "Bibliography") {
		t.Fatalf("first section did not own its chapter heading and introduction: %s", decoded.Pages[2].HTML)
	}
}

func TestReaderPayloadPreservesLateXMLGeneratedDocumentChrome(t *testing.T) {
	html := strings.Join([]string{
		`<article>`,
		`<h1 class="ltx_title ltx_title_document">线性代数</h1>`,
		`<div class="ltx_authors">Elysium</div>`,
		`<div class="ltx_dates">(August 8, 2026)</div>`,
		`<div class="ltx_TOC ltx_list_toc"><h6>Contents</h6><ul><li>Intro</li></ul></div>`,
		`<h2 id="preface">前言</h2><p>Reader body.</p>`,
		`</article>`,
	}, "")

	reader := readerPayload("Book", html)
	raw, err := json.Marshal(reader)
	if err != nil {
		t.Fatalf("marshal reader: %v", err)
	}
	var decoded struct {
		Pages []struct {
			HTML string `json:"html"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode reader: %v", err)
	}
	if len(decoded.Pages) != 1 {
		t.Fatalf("expected one reader page, got %#v", decoded.Pages)
	}
	pageHTML := decoded.Pages[0].HTML
	for _, expected := range []string{"ltx_title_document", "ltx_TOC", "Contents", "ltx_authors", "ltx_dates", "August 8"} {
		if !strings.Contains(pageHTML, expected) {
			t.Fatalf("expected reader page to preserve generated document chrome %q, got %s", expected, pageHTML)
		}
	}
	if !strings.Contains(pageHTML, `<h2 id="preface">前言</h2>`) || !strings.Contains(pageHTML, "Reader body.") {
		t.Fatalf("expected reader body to remain, got %s", pageHTML)
	}

	fallbackHTML := strings.Join([]string{
		`<article>`,
		`<h1 class="ltx_title ltx_title_document">线性代数</h1>`,
		`<div class="ltx_TOC ltx_list_toc"><h6>Contents</h6></div>`,
		`<p>Fallback body.</p>`,
		`</article>`,
	}, "")
	fallback := readerPayload("Book", fallbackHTML)
	raw, err = json.Marshal(fallback)
	if err != nil {
		t.Fatalf("marshal fallback reader: %v", err)
	}
	for _, expected := range []string{"ltx_title_document", "ltx_TOC", "Contents"} {
		if !strings.Contains(string(raw), expected) {
			t.Fatalf("expected fallback reader page to preserve generated document chrome %q, got %s", expected, raw)
		}
	}
	if !strings.Contains(string(raw), "Fallback body.") {
		t.Fatalf("expected fallback reader body to remain, got %s", raw)
	}
}

func TestProjectRenderWithLateXMLReportsUnavailableBinary(t *testing.T) {
	cfg := testConfig()
	cfg.LateXMLBin = filepath.Join(t.TempDir(), "missing-latexmlc")
	server := NewServer(cfg)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("source", "main.tex")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(`\documentclass{article}
\begin{document}
Rin Renderer smoke test.
\end{document}`)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.WriteField("engine", "latexml"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/render/projects", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Rin-Renderer-Token", "test-token")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "latexml.unavailable") {
		t.Fatalf("expected unavailable diagnostic, got %s", resp.Body.String())
	}
}

func TestProjectRenderRejectsUnsafeArchivePath(t *testing.T) {
	server := NewServer(testConfig())
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("../main.tex")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := entry.Write([]byte(`\documentclass{article}`)); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("source", "bad.zip")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(archive.Bytes()); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/render/projects", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Rin-Renderer-Token", "test-token")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "unsafe archive path") {
		t.Fatalf("expected unsafe path error, got %s", resp.Body.String())
	}
}

func fakeRenderAPILateXMLC(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "latexmlc")
	script := `#!/bin/sh
set -eu
dest=""
log=""
main=""
for arg in "$@"; do
  case "$arg" in
    --destination=*) dest="${arg#--destination=}" ;;
    --log=*) log="${arg#--log=}" ;;
    *.tex) main="$arg" ;;
  esac
done
test -n "$dest"
test -n "$log"
test -n "$main"
test -f "$main"
mkdir -p "$(dirname "$dest")"
printf '%s\n' 'Info: fake latexml conversion complete' > "$log"
cat > "$dest" <<'HTML'
<!doctype html><html><body><article><h1>Converted</h1><p>Rin Renderer smoke test.</p></article></body></html>
HTML
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake latexmlc: %v", err)
	}
	return bin
}

func fakeRenderAPILateXMLCWithPlaceholder(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "latexmlc")
	script := `#!/bin/sh
set -eu
dest=""
log=""
main=""
for arg in "$@"; do
  case "$arg" in
    --destination=*) dest="${arg#--destination=}" ;;
    --log=*) log="${arg#--log=}" ;;
    *.tex) main="$arg" ;;
  esac
done
test -n "$dest"
test -n "$log"
test -n "$main"
test -f "$main"
if grep -Fq '\begin{tikzcd}' "$main"; then
  printf '%s\n' 'expected diagram source to be replaced before LaTeXML' >&2
  exit 4
fi
if grep -Fq '\begin{center}' "$main"; then
  printf '%s\n' 'expected diagram layout wrapper to be replaced before LaTeXML' >&2
  exit 4
fi
placeholder="$(grep -o 'RINRENDERERDIAGRAMPLACEHOLDER[A-Z0-9]*' "$main" | head -n 1)"
test -n "$placeholder"
mkdir -p "$(dirname "$dest")"
printf '%s\n' 'Info: fake latexml conversion complete' > "$log"
cat > "$dest" <<HTML
<!doctype html><html><body><article><p>Before.</p><p>$placeholder</p><p>After.</p></article></body></html>
HTML
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake latexmlc: %v", err)
	}
	return bin
}

func fakeRenderAPILateXMLCWithMathPlaceholder(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "latexmlc")
	script := `#!/bin/sh
set -eu
dest=""
log=""
main=""
for arg in "$@"; do
  case "$arg" in
    --destination=*) dest="${arg#--destination=}" ;;
    --log=*) log="${arg#--log=}" ;;
    *.tex) main="$arg" ;;
  esac
done
test -n "$dest"
test -n "$log"
test -n "$main"
test -f "$main"
if grep -Fq '\begin{align}' "$main"; then
  printf '%s\n' 'expected source display math to be replaced before LaTeXML' >&2
  exit 4
fi
placeholder="$(grep -o 'RINRENDERERMATHPLACEHOLDER[A-Z0-9]*' "$main" | head -n 1)"
test -n "$placeholder"
mkdir -p "$(dirname "$dest")"
printf '%s\n' 'Info: fake latexml conversion complete' > "$log"
cat > "$dest" <<HTML
<!doctype html><html><body><article><p>Before.</p><p>$placeholder</p><p>After.</p></article></body></html>
HTML
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake latexmlc: %v", err)
	}
	return bin
}

func newProjectRenderRequest(t *testing.T, engine string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("source", "main.tex")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(`\documentclass{article}
\begin{document}
Rin Renderer smoke test.
\end{document}`)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if engine != "" {
		if err := writer.WriteField("engine", engine); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/render/projects", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Rin-Renderer-Token", "test-token")
	return req
}

func TestDiagramRenderRequiresWorkerEndpoint(t *testing.T) {
	server := NewServer(testConfig())
	req := httptest.NewRequest(http.MethodPost, "/api/render/diagrams/tikzcd", strings.NewReader(`{"body":"A \\arrow[r] & B"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "RIN_RENDERER_TEXSVG_ENDPOINT") {
		t.Fatalf("expected missing worker endpoint error, got %s", resp.Body.String())
	}
}

func TestDiagramRenderUploadsWorkerSVGToCloudBase(t *testing.T) {
	const workerToken = "worker-token"
	const cloudbaseToken = "cloudbase-token"
	rawSVG := " \r\n<svg height=\"10\" width=\"10\"><script>alert(1)</script><path stroke=\"#000\"/></svg>\r\n"
	svgObject, err := renderstorage.NormalizeSVGObject([]byte(rawSVG))
	if err != nil {
		t.Fatal(err)
	}
	normalizedSVG := string(svgObject.Bytes)
	hash := svgObject.Hash
	expectedObjectID := svgObject.ObjectID

	var workerPath string
	var workerAuth string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerPath = r.URL.Path
		workerAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var payload DiagramRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if payload.Type != "tikzcd" || strings.TrimSpace(payload.Body) != `A \arrow[r] & B` {
			http.Error(w, "unexpected worker payload", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"diagram-id","url":"/rendered/diagram-id","svg":%q,"cached":true,"type":"tikzcd"}`, rawSVG)
	}))
	defer worker.Close()

	var headPath string
	var postPath string
	var uploadedBody string
	var upsert string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			headPath = r.URL.Path
			http.NotFound(w, r)
		case http.MethodPost:
			postPath = r.URL.Path
			if r.Header.Get("Authorization") != "Bearer "+cloudbaseToken {
				http.Error(w, "missing CloudBase token", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Content-Type") != "image/svg+xml; charset=utf-8" {
				http.Error(w, "unexpected content type", http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(r.Body)
			uploadedBody = string(body)
			upsert = r.Header.Get("X-Upsert")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"Id":"object-id","Key":%q}`, "rin-renderer/"+expectedObjectID)
		case http.MethodGet:
			_, _ = io.WriteString(w, normalizedSVG)
		default:
			http.NotFound(w, r)
		}
	}))
	defer storage.Close()

	cfg := testConfig()
	cfg.DiagramWorkerEndpoint = worker.URL
	cfg.DiagramWorkerToken = workerToken
	cfg.StorageBaseURL = storage.URL
	cfg.StorageAccessToken = cloudbaseToken
	server := NewServer(cfg)
	req := httptest.NewRequest(http.MethodPost, "/api/render/diagrams/tikzcd", strings.NewReader(`{"body":"A \\arrow[r] & B"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Rin-Renderer-Token", "test-token")
	resp := httptest.NewRecorder()

	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if workerPath != "/tikzcd" || workerAuth != "Bearer "+workerToken {
		t.Fatalf("unexpected worker request path/auth: %q %q", workerPath, workerAuth)
	}
	expectedPublicPath := "/v1/storages/object/public/rin-renderer/" + expectedObjectID
	expectedUploadPath := "/v1/storages/object/rin-renderer/" + expectedObjectID
	if headPath != expectedPublicPath || postPath != expectedUploadPath || uploadedBody != normalizedSVG || upsert != "true" {
		t.Fatalf("unexpected CloudBase interaction: head=%q post=%q body=%q upsert=%q", headPath, postPath, uploadedBody, upsert)
	}
	var payload DiagramRenderResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode diagram response: %v", err)
	}
	expectedURL := storage.URL + expectedPublicPath
	if payload.ID != "diagram-id" || payload.Type != "tikzcd" || payload.SVGHash != hash || payload.ObjectID != expectedObjectID || payload.URL != expectedURL || payload.CloudBaseURL != expectedURL || !payload.Uploaded {
		t.Fatalf("unexpected diagram response: %#v", payload)
	}
}

func TestDiagramRenderReusesVerifiedSharedServiceArtifact(t *testing.T) {
	rawSVG := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><path d="M0 0L10 10"/></svg>`
	svgObject, err := renderstorage.NormalizeSVGObject([]byte(rawSVG))
	if err != nil {
		t.Fatal(err)
	}
	workerCalls := 0
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"shared-diagram","svg":%q,"type":"tikzpicture"}`, rawSVG)
	}))
	defer worker.Close()
	stored := false
	postCalls := 0
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			if stored {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.NotFound(w, r)
		case http.MethodPost:
			postCalls++
			stored = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"Id":"object-id","Key":%q}`, "rin-renderer/"+svgObject.ObjectID)
		case http.MethodGet:
			_, _ = w.Write(svgObject.Bytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer storage.Close()

	cfg := testConfig()
	cfg.DiagramWorkerEndpoint = worker.URL
	cfg.StorageBaseURL = storage.URL
	cfg.StorageAccessToken = "cloudbase-token"
	server := NewServer(cfg)
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/render/diagrams/tikz", strings.NewReader(`{"body":"\\draw (0,0)--(1,1);"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Rin-Renderer-Token", "test-token")
		return req
	}
	first := httptest.NewRecorder()
	server.ServeHTTP(first, request())
	second := httptest.NewRecorder()
	server.ServeHTTP(second, request())
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("responses = %d %s / %d %s", first.Code, first.Body, second.Code, second.Body)
	}
	var firstPayload, secondPayload DiagramRenderResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPayload); err != nil {
		t.Fatal(err)
	}
	if workerCalls != 1 || postCalls != 1 || firstPayload.Cached || !firstPayload.Uploaded ||
		!secondPayload.Cached || secondPayload.Uploaded || firstPayload.ObjectID != secondPayload.ObjectID {
		t.Fatalf("shared cache calls worker=%d post=%d first=%#v second=%#v", workerCalls, postCalls, firstPayload, secondPayload)
	}
}
