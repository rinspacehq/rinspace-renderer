package renderapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/diagramengine"
	"github.com/rinspacehq/rinspace-renderer/api/internal/diagramservice"
	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
	"github.com/rinspacehq/rinspace-renderer/api/internal/latexmladapter"
	"github.com/rinspacehq/rinspace-renderer/api/internal/mathrender"
	"github.com/rinspacehq/rinspace-renderer/api/internal/mathservice"
	"github.com/rinspacehq/rinspace-renderer/api/internal/nodeworker"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectdiagrams"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectmath"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderstorage"
)

type Server struct {
	cfg            Config
	mux            *http.ServeMux
	projectEngines *projectEngineRegistry
	jobs           http.Handler
	projects       http.Handler
	operations     *operational.Registry
	primaryMath    mathrender.Renderer
	warmMath       *mathrender.WarmMathJaxRenderer
	diagrams       *diagramservice.Service
	diagramPool    *diagramservice.ResourcePool
	localAssets    *renderstorage.LocalFileStore
}

var processDiagramPools sync.Map

func processDiagramPool(parallelism int) (*diagramservice.ResourcePool, error) {
	if parallelism <= 0 {
		parallelism = diagramservice.DefaultParallelism
	}
	if existing, ok := processDiagramPools.Load(parallelism); ok {
		return existing.(*diagramservice.ResourcePool), nil
	}
	created, err := diagramservice.NewResourcePool(parallelism)
	if err != nil {
		return nil, err
	}
	actual, _ := processDiagramPools.LoadOrStore(parallelism, created)
	return actual.(*diagramservice.ResourcePool), nil
}

func NewServer(cfg Config) http.Handler {
	return NewServerWithJobs(cfg, nil)
}

func NewServerWithJobs(cfg Config, jobs http.Handler) http.Handler {
	return newServer(cfg, jobs, nil, operational.Default())
}

func NewServerWithAsync(cfg Config, jobs http.Handler, projects http.Handler) http.Handler {
	return newServer(cfg, jobs, projects, operational.Default())
}

func NewServerWithOperations(cfg Config, jobs http.Handler, projects http.Handler, operations *operational.Registry) http.Handler {
	return newServer(cfg, jobs, projects, operations)
}

func newServer(cfg Config, jobs http.Handler, projects http.Handler, operations *operational.Registry) *Server {
	if operations == nil {
		operations = operational.Default()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), jobs: jobs, projects: projects, operations: operations}
	if cfg.StorageProvider == "local" {
		var err error
		s.localAssets, err = renderstorage.NewLocalFileStore(cfg.LocalAssetRoot, cfg.LocalPublicBaseURL)
		if err != nil {
			panic(fmt.Sprintf("configure local public assets: %v", err))
		}
	} else if cfg.StorageProvider != "" && cfg.StorageProvider != "cloudbase" {
		panic(fmt.Sprintf("unsupported public asset storage provider %q", cfg.StorageProvider))
	}
	s.operations.Gauge("math_batches_in_use", 0, "math_node")
	if err := s.configureMathRenderer(); err != nil {
		panic(fmt.Sprintf("configure math renderer: %v", err))
	}
	if err := s.configureDiagramService(); err != nil {
		panic(fmt.Sprintf("configure diagram service: %v", err))
	}
	if err := s.registerProjectEngines(); err != nil {
		panic(fmt.Sprintf("register project engines: %v", err))
	}
	s.routes()
	return s
}

func (s *Server) configureDiagramService() error {
	pool, err := processDiagramPool(s.cfg.DiagramParallelism)
	if err != nil {
		return err
	}
	fontVersion := strings.Join([]string{
		firstNonEmpty(s.cfg.TeXLiveVersion, "texlive-unreported"),
		firstNonEmpty(s.cfg.DvisvgmVersion, "dvisvgm-unreported"),
	}, "+")
	service, err := diagramservice.New(diagramservice.Config{
		Renderer: s.diagramWorker(), Store: s.diagramStorage(),
		Cache:         diagramservice.NewMemoryCache(diagramservice.DefaultMemoryCacheEntries),
		EngineVersion: firstNonEmpty(s.cfg.DiagramEngineVersion, "rin-texsvg-unreported"),
		FontVersion:   fontVersion, MaxSourceBytes: s.cfg.DiagramMaxBytes,
		Parallelism: s.cfg.DiagramParallelism, Timeout: s.cfg.DiagramTimeout, Pool: pool,
	})
	if err != nil {
		return err
	}
	s.diagrams = service
	s.diagramPool = pool
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleMetrics(response http.ResponseWriter, request *http.Request) {
	if s.diagramPool != nil {
		s.operations.Gauge("diagram_pool_capacity", float64(s.diagramPool.Capacity()), "texsvg")
		s.operations.Gauge("diagram_pool_active", float64(s.diagramPool.Active()), "texsvg")
	}
	s.operations.Metrics(response, request)
}

func (s *Server) Close() error {
	if s.warmMath != nil {
		return s.warmMath.Close()
	}
	return nil
}

func (s *Server) configureMathRenderer() error {
	if s.cfg.MathPrimaryEngine == "katex" || !s.cfg.MathJaxWarmWorkersEnabled {
		return nil
	}
	environment := map[string]string{}
	for _, key := range []string{
		"RIN_RENDERER_MATHJAX_EM", "RIN_RENDERER_MATHJAX_EX",
		"RIN_RENDERER_MATHJAX_CONTAINER_WIDTH", "RIN_RENDERER_MATHJAX_FONT_URL",
	} {
		if value, ok := os.LookupEnv(key); ok {
			environment[key] = value
		}
	}
	environment["RIN_NODE_WORKER_MAX_REQUEST_BYTES"] = strconv.FormatInt(s.cfg.MathJaxWarmMaxRequestBytes, 10)
	renderer, err := mathrender.NewWarmMathJaxRenderer(mathrender.WarmMathJaxConfig{
		NodeBin: s.cfg.MathJaxNodeBin, ScriptPath: s.cfg.MathJaxScript,
		Timeout: s.cfg.MathTimeout, MaxSourceBytes: s.cfg.MathSourceMaxBytes,
		Workers: s.cfg.MathJaxWarmWorkerCount, MaxRequestBytes: s.cfg.MathJaxWarmMaxRequestBytes,
		MaxResponseBytes: s.cfg.MathJaxWarmMaxResponseBytes, MaxTasks: s.cfg.MathJaxWarmMaxTasks,
		MaxRSSBytes: s.cfg.MathJaxWarmMaxRSSBytes, StartTimeout: s.cfg.MathJaxWarmStartTimeout,
		StopGrace: s.cfg.MathJaxWarmStopGrace, Environment: environment,
	})
	if err != nil {
		return err
	}
	s.primaryMath = renderer
	s.warmMath = renderer
	if err := s.operations.AddProbe("math-node", renderer.Ready); err != nil {
		return err
	}
	return s.operations.AddCollector("math-node", func(context.Context) error {
		snapshot := renderer.Snapshot()
		s.operations.Gauge("node_worker_capacity", float64(snapshot.Capacity), "math")
		s.operations.Gauge("node_worker_alive", float64(snapshot.Alive), "math")
		s.operations.Gauge("node_worker_active", float64(snapshot.Active), "math")
		s.operations.Gauge("node_worker_starts", float64(snapshot.Starts), "math")
		s.operations.Gauge("node_worker_restarts", float64(snapshot.Restarts), "math")
		s.operations.Gauge("node_worker_crashes", float64(snapshot.Crashes), "math")
		s.operations.Gauge("node_worker_cancellations", float64(snapshot.Cancellations), "math")
		s.operations.Gauge("node_worker_protocol_errors", float64(snapshot.ProtocolErrors), "math")
		s.operations.Gauge("node_worker_recycles", float64(snapshot.Recycles), "math")
		s.operations.Gauge("node_worker_tasks", float64(snapshot.Tasks), "math")
		s.operations.Gauge("node_worker_rss_bytes", float64(snapshot.RSSBytes), "math")
		return nil
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /ready", s.handleReady)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /api/render/capabilities", s.handleCapabilities)
	if s.localAssets != nil {
		s.mux.Handle("GET "+renderstorage.LocalAssetPath, s.localAssets)
		s.mux.Handle("HEAD "+renderstorage.LocalAssetPath, s.localAssets)
	}
	if s.projects != nil {
		s.mux.Handle("POST /api/render/projects", s.projects)
	} else {
		s.mux.HandleFunc("POST /api/render/projects", s.handleProjectRender)
	}
	s.mux.HandleFunc("POST /api/render/diagrams/{type}", s.handleDiagramRender)
	s.mux.HandleFunc("POST /api/render/pdf/inspect", s.handlePDFInspection)
	if s.jobs != nil {
		s.mux.Handle("/api/render/jobs", s.jobs)
		s.mux.Handle("/api/render/jobs/", s.jobs)
		s.mux.Handle("/api/render/queue", s.jobs)
		s.mux.Handle("/internal/v1/render/completion-health", s.jobs)
	}
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	snapshot := s.operations.Snapshot(ctx)
	status := http.StatusOK
	if !snapshot.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, snapshot)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "rin-renderer-api",
		"version": s.cfg.RendererVersion,
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	documentEngines := []string{"latexml", "auto"}
	if s.jobs != nil {
		documentEngines = append(documentEngines, "unified")
	}
	if typstProfileConfigured(s.cfg) {
		documentEngines = append(documentEngines, "typst")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	operations := s.operations.Snapshot(ctx)
	mathJaxMode := "process-per-batch"
	if s.warmMath != nil {
		mathJaxMode = "warm"
	}
	writeJSON(w, http.StatusOK, CapabilitiesResponse{
		Version: s.cfg.RendererVersion,
		Engines: map[string][]string{
			"document": documentEngines,
			"pdf":      {"pdf-inspect"},
			"diagram":  {"rin-texsvg"},
			"math":     {"mathjax-chtml", "texsvg-fallback", "katex-legacy"},
		},
		DiagramTypes: supportedDiagramTypes(),
		Limits: Limits{
			ProjectArchiveMaxBytes:     s.cfg.ProjectArchiveMaxBytes,
			ProjectFileMaxCount:        s.cfg.ProjectFileMaxCount,
			ProjectFileMaxBytes:        s.cfg.ProjectFileMaxBytes,
			DiagramMaxBytes:            s.cfg.DiagramMaxBytes,
			FinalOutputMaxBytes:        finalOutputLimit(s.cfg),
			RenderTimeoutSeconds:       int64(s.cfg.RenderTimeout / time.Second),
			DiagramTimeoutSeconds:      int64(s.cfg.DiagramTimeout / time.Second),
			DiagramParallelism:         s.cfg.DiagramParallelism,
			MathSourceMaxBytes:         s.cfg.MathSourceMaxBytes,
			MathTexSVGFallbackMaxCount: s.cfg.MathTexSVGFallbackMaxCount,
			MathTimeoutSeconds:         int64(s.cfg.MathTimeout / time.Second),
			MathParallelism:            s.cfg.MathParallelism,
		},
		Versions: s.versions(),
		Storage:  s.storageInfo(),
		Async: AsyncCapabilities{
			Enabled: s.jobs != nil, QueueScope: "instance", EventReplay: s.jobs != nil,
			PollingFallback: s.jobs != nil, Cancellation: s.jobs != nil,
			Readiness: operations.Status, Degraded: operations.Degraded,
		},
		NodeWorkers: NodeWorkerCapabilities{
			Enabled: s.warmMath != nil, Transport: "ndjson-stdio", ContractVersion: nodeworker.ContractVersion,
			MathJaxMode: mathJaxMode,
			Count:       s.cfg.MathJaxWarmWorkerCount, MaxRequestBytes: s.cfg.MathJaxWarmMaxRequestBytes,
			MaxResponseBytes: s.cfg.MathJaxWarmMaxResponseBytes, MaxTasks: s.cfg.MathJaxWarmMaxTasks,
			MaxRSSBytes: s.cfg.MathJaxWarmMaxRSSBytes,
		},
	})
}

func (s *Server) handleProjectRender(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	if !s.requireServiceToken(w, r, requestID) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.ProjectArchiveMaxBytes+1)
	if err := r.ParseMultipartForm(s.cfg.ProjectArchiveMaxBytes); err != nil {
		writeError(w, http.StatusBadRequest, requestID, "invalid multipart render request", err)
		return
	}

	file, header, err := r.FormFile("source")
	if err != nil {
		writeError(w, http.StatusBadRequest, requestID, "missing source archive", err)
		return
	}
	defer file.Close()

	body, err := io.ReadAll(io.LimitReader(file, s.cfg.ProjectArchiveMaxBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, requestID, "failed to read source archive", err)
		return
	}
	if int64(len(body)) > s.cfg.ProjectArchiveMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, requestID, "project archive too large", nil)
		return
	}

	outcome, preparationErr := s.orchestrateProject(r.Context(), projectRenderInput{
		RequestID:        requestID,
		ArchiveName:      header.Filename,
		Archive:          body,
		Engine:           r.FormValue("engine"),
		Title:            r.FormValue("title"),
		ExplicitMainFile: r.FormValue("mainFile"),
		MetadataMainFile: metadataPathFromForm(r, "mainFile", "main_file"),
		ActiveFile:       firstNonEmpty(r.FormValue("activeFile"), r.FormValue("activePath"), metadataPathFromForm(r, "activePath", "activeFile", "active_path", "active_file")),
		ProjectStatus:    r.FormValue("status"),
		MathPolicy:       r.FormValue("mathPolicy"),
		Renderer:         r.FormValue("renderer"),
	})
	if preparationErr != nil {
		writeError(w, preparationErr.Status, requestID, preparationErr.Message, preparationErr.Cause)
		return
	}
	if outcome.Err != nil {
		writeJSON(w, outcome.Status, ErrorResponse{
			RequestID:   requestID,
			Error:       outcome.Err.Error(),
			Diagnostics: outcome.Diagnostics,
		})
		return
	}
	writeJSON(w, http.StatusOK, outcome.Response)
}

func (s *Server) handleDiagramRender(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	if !s.requireServiceToken(w, r, requestID) {
		return
	}

	kind, ok := normalizeDiagramType(r.PathValue("type"))
	if !ok {
		writeError(w, http.StatusBadRequest, requestID, "unsupported diagram type", nil)
		return
	}

	var req DiagramRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.DiagramMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, requestID, "invalid diagram request", err)
		return
	}
	req.Type = kind
	req.Body = strings.TrimSpace(req.Body)
	req.Source = strings.TrimSpace(req.Source)
	if req.Body == "" && req.Source == "" {
		writeError(w, http.StatusBadRequest, requestID, "empty diagram body", nil)
		return
	}

	rendered, err := s.renderDiagramToCloudBase(r.Context(), requestID, kind, req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{
			RequestID: requestID, Error: "diagram render failed: " + err.Error(), Diagnostics: rendered.Diagnostics,
		})
		return
	}

	writeJSON(w, http.StatusOK, rendered)
}

func (s *Server) renderDiagramToCloudBase(ctx context.Context, requestID string, kind string, req DiagramRequest) (DiagramRenderResponse, error) {
	item, err := newDiagramBatchItem(kind, req, "", nil)
	if err != nil {
		return DiagramRenderResponse{}, err
	}
	result, err := s.diagrams.ResolveDiagrams(ctx, orchestration.DiagramBatch{
		ContractVersion: orchestration.DiagramBatchContractVersion,
		Items:           []orchestration.DiagramBatchItem{item},
		OutputStrategy:  "svg",
	})
	if err != nil {
		return DiagramRenderResponse{}, err
	}
	response := diagramResponseFromResolved(requestID, result.Units[0], s.versions())
	s.operations.Count("cache_requests_total", "diagram", result.Units[0].Cache.Status)
	if result.Units[0].State != "succeeded" {
		return response, errors.New(firstDiagramFailure(result.Units[0]))
	}
	return response, nil
}

func newDiagramBatchItem(kind string, req DiagramRequest, wrapperID string, location *contracts.SourceLocation) (orchestration.DiagramBatchItem, error) {
	kind, ok := diagramservice.NormalizeType(kind)
	if !ok {
		return orchestration.DiagramBatchItem{}, errors.New("unsupported diagram type")
	}
	body := strings.TrimSpace(req.Body)
	source := strings.TrimSpace(req.Source)
	mode := orchestration.DiagramSourceComplete
	if source == "" {
		source = body
		mode = orchestration.DiagramSourceBody
	}
	if source == "" {
		return orchestration.DiagramBatchItem{}, errors.New("empty diagram body")
	}
	id, err := contracts.NewWorkID()
	if err != nil {
		return orchestration.DiagramBatchItem{}, err
	}
	item := orchestration.DiagramBatchItem{
		Unit: contracts.WorkUnit{
			Kind: contracts.WorkUnitDiagram, ID: id, Source: source, DiagramType: kind,
			Options: strings.TrimSpace(req.Options), SourceLocation: location,
		},
		SourceMode: mode, Body: body, WrapperID: wrapperID,
	}
	if err := item.Unit.Validate(); err != nil {
		return orchestration.DiagramBatchItem{}, err
	}
	return item, nil
}

func diagramResponseFromResolved(requestID string, unit orchestration.DiagramResolvedUnit, versions Versions) DiagramRenderResponse {
	diagnostics := make([]Diagnostic, 0, len(unit.Diagnostics))
	for _, item := range unit.Diagnostics {
		source := map[string]string{}
		if item.SourceLocation != nil {
			source["path"] = item.SourceLocation.Path
			source["line"] = strconv.Itoa(item.SourceLocation.Start.Line)
			source["column"] = strconv.Itoa(item.SourceLocation.Start.Column)
		}
		diagnostics = append(diagnostics, Diagnostic{
			Severity: item.Severity, Code: item.Code, Message: item.Message,
			Engine: "rin-texsvg", Source: source,
		})
	}
	return DiagramRenderResponse{
		RequestID: requestID, ID: unit.DiagramID, Type: unit.Source.DiagramType,
		SVGHash: unit.SVGHash, SVGBytes: unit.SVGBytes, ObjectID: unit.ObjectID, CloudBaseURL: unit.URL, URL: unit.URL,
		Uploaded: unit.Uploaded, Engine: "rin-texsvg", Versions: versions, Diagnostics: diagnostics,
		Cached:    unit.Cache.Status == "hit" || unit.EngineCached || !unit.Uploaded,
		RenderURL: unit.RenderURL,
	}
}

func firstDiagramFailure(unit orchestration.DiagramResolvedUnit) string {
	for _, diagnostic := range unit.Diagnostics {
		if strings.TrimSpace(diagnostic.Message) != "" {
			return diagnostic.Message
		}
	}
	return "diagram render failed"
}

type diagramHTMLReplacement struct {
	Standalone string
	FigureBody string
}

func replaceDiagramPlaceholders(body string, replacements map[string]diagramHTMLReplacement) string {
	if len(replacements) == 0 || strings.TrimSpace(body) == "" {
		return body
	}
	body = replaceDiagramPlaceholdersInFigures(body, replacements)
	body = replaceDiagramPlaceholdersInLateXMLDiagramContainers(body, replacements)
	for placeholder, replacement := range replacements {
		if !strings.Contains(body, placeholder) {
			continue
		}
		body = replacePlaceholderContainers(body, placeholder, replacement.Standalone)
		body = replacePlaceholderParagraphs(body, placeholder, replacement.Standalone)
		body = strings.ReplaceAll(body, placeholder, replacement.Standalone)
	}
	return body
}

func replaceDiagramPlaceholdersInFigures(body string, replacements map[string]diagramHTMLReplacement) string {
	figurePattern := regexp.MustCompile(`(?is)<figure\b[^>]*>.*?</figure>`)
	return figurePattern.ReplaceAllStringFunc(body, func(figure string) string {
		if !containsAnyPlaceholder(figure, replacements) {
			return figure
		}
		return replaceDiagramPlaceholdersInFigure(figure, replacements)
	})
}

func replaceDiagramPlaceholdersInFigure(figure string, replacements map[string]diagramHTMLReplacement) string {
	replacementHTML := figure
	for placeholder, replacement := range replacements {
		if !strings.Contains(replacementHTML, placeholder) {
			continue
		}
		target := replacement.FigureBody
		replacementHTML = replacePlaceholderContainers(replacementHTML, placeholder, target)
		replacementHTML = replacePlaceholderParagraphs(replacementHTML, placeholder, target)
		replacementHTML = strings.ReplaceAll(replacementHTML, placeholder, target)
	}
	return replacementHTML
}

func replaceDiagramPlaceholdersInLateXMLDiagramContainers(body string, replacements map[string]diagramHTMLReplacement) string {
	if !containsAnyPlaceholder(body, replacements) {
		return body
	}
	var out strings.Builder
	cursor := 0
	for cursor < len(body) {
		start := nextHTMLTagStart(body, "div", cursor, false)
		if start < 0 {
			break
		}
		openEnd := strings.IndexByte(body[start:], '>')
		if openEnd < 0 {
			break
		}
		openEnd += start
		openTag := body[start : openEnd+1]
		if !isLateXMLDiagramContainer(openTag) {
			out.WriteString(body[cursor : openEnd+1])
			cursor = openEnd + 1
			continue
		}
		end := matchingHTMLContainerEnd(body, "div", start)
		if end < 0 {
			break
		}
		container := body[start:end]
		if !containsAnyPlaceholder(container, replacements) {
			out.WriteString(body[cursor : openEnd+1])
			cursor = openEnd + 1
			continue
		}
		out.WriteString(body[cursor:start])
		out.WriteString(replaceDiagramPlaceholdersInExistingContainer(container, replacements))
		cursor = end
	}
	out.WriteString(body[cursor:])
	return out.String()
}

func replaceDiagramPlaceholdersInExistingContainer(container string, replacements map[string]diagramHTMLReplacement) string {
	replacementHTML := container
	for placeholder, replacement := range replacements {
		if !strings.Contains(replacementHTML, placeholder) {
			continue
		}
		target := replacement.FigureBody
		replacementHTML = replacePlaceholderContainers(replacementHTML, placeholder, target)
		replacementHTML = replacePlaceholderParagraphs(replacementHTML, placeholder, target)
		replacementHTML = strings.ReplaceAll(replacementHTML, placeholder, target)
	}
	return replacementHTML
}

func isLateXMLDiagramContainer(openTag string) bool {
	classes := strings.Fields(getHTMLAttribute(openTag, "class"))
	for _, className := range classes {
		switch className {
		case "ltx_subfigure", "ltx_subfloat", "ltx_minipage":
			return true
		}
	}
	return false
}

func matchingHTMLContainerEnd(htmlText string, tag string, start int) int {
	depth := 0
	cursor := start
	for cursor < len(htmlText) {
		open := nextHTMLTagStart(htmlText, tag, cursor, false)
		close := nextHTMLTagStart(htmlText, tag, cursor, true)
		if open < 0 && close < 0 {
			return -1
		}
		if open >= 0 && (close < 0 || open < close) {
			end := strings.IndexByte(htmlText[open:], '>')
			if end < 0 {
				return -1
			}
			end += open
			if !isSelfClosingHTMLTag(htmlText[open : end+1]) {
				depth++
			}
			cursor = end + 1
			continue
		}
		end := strings.IndexByte(htmlText[close:], '>')
		if end < 0 {
			return -1
		}
		end += close
		depth--
		cursor = end + 1
		if depth == 0 {
			return cursor
		}
	}
	return -1
}

func nextHTMLTagStart(htmlText string, tag string, start int, closing bool) int {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return -1
	}
	for cursor := start; cursor < len(htmlText); {
		index := strings.IndexByte(htmlText[cursor:], '<')
		if index < 0 {
			return -1
		}
		index += cursor
		tagStart := index + 1
		if closing {
			if tagStart >= len(htmlText) || htmlText[tagStart] != '/' {
				cursor = index + 1
				continue
			}
			tagStart++
		} else if tagStart < len(htmlText) && htmlText[tagStart] == '/' {
			cursor = index + 1
			continue
		}
		if htmlTagNameEqual(htmlText, tagStart, tag) {
			return index
		}
		cursor = index + 1
	}
	return -1
}

func htmlTagNameEqual(htmlText string, start int, tag string) bool {
	if start < 0 || start+len(tag) > len(htmlText) {
		return false
	}
	for index := 0; index < len(tag); index++ {
		if asciiLower(htmlText[start+index]) != tag[index] {
			return false
		}
	}
	next := start + len(tag)
	return next >= len(htmlText) || isHTMLTagBoundary(htmlText[next])
}

func asciiLower(ch byte) byte {
	if ch >= 'A' && ch <= 'Z' {
		return ch + ('a' - 'A')
	}
	return ch
}

func isHTMLTagBoundary(ch byte) bool {
	return isHTMLSpace(ch) || ch == '>' || ch == '/'
}

func isSelfClosingHTMLTag(tag string) bool {
	trimmed := strings.TrimSpace(tag)
	return strings.HasSuffix(trimmed, "/>")
}

func replacePlaceholderContainers(body string, placeholder string, replacement string) string {
	if !strings.Contains(body, placeholder) {
		return body
	}
	for _, tag := range []string{"p", "div", "span"} {
		container := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*>\s*` + regexp.QuoteMeta(placeholder) + `\s*</` + tag + `>`)
		body = container.ReplaceAllStringFunc(body, func(string) string {
			return replacement
		})
	}
	return body
}

func replacePlaceholderParagraphs(body string, placeholder string, replacement string) string {
	if !strings.Contains(body, placeholder) {
		return body
	}
	paragraph := regexp.MustCompile(`(?is)<p\b[^>]*>.*?` + regexp.QuoteMeta(placeholder) + `.*?</p>`)
	return paragraph.ReplaceAllStringFunc(body, func(value string) string {
		openEnd := strings.IndexByte(value, '>')
		closeStart := strings.LastIndex(strings.ToLower(value), "</p>")
		if openEnd < 0 || closeStart < openEnd {
			return strings.ReplaceAll(value, placeholder, replacement)
		}
		openTag := value[:openEnd+1]
		closeTag := value[closeStart:]
		parts := strings.Split(value[openEnd+1:closeStart], placeholder)
		var out strings.Builder
		for index, part := range parts {
			if strings.TrimSpace(part) != "" {
				out.WriteString(openTag)
				out.WriteString(part)
				out.WriteString(closeTag)
			}
			if index < len(parts)-1 {
				out.WriteString(replacement)
			}
		}
		return out.String()
	})
}

func containsAnyPlaceholder(value string, replacements map[string]diagramHTMLReplacement) bool {
	for placeholder := range replacements {
		if strings.Contains(value, placeholder) {
			return true
		}
	}
	return false
}

func diagramReplacementHTML(diagram projectdiagrams.Diagram, rendered DiagramRenderResponse) diagramHTMLReplacement {
	return diagramHTMLReplacement{
		Standalone: diagramContainerHTML("figure", diagram, rendered),
		FigureBody: diagramContainerHTML("div", diagram, rendered),
	}
}

func diagramPlaceholdersFromExtracted(diagrams []projectdiagrams.Diagram) []projectcore.DiagramPlaceholder {
	placeholders := make([]projectcore.DiagramPlaceholder, 0, len(diagrams))
	for _, diagram := range diagrams {
		placeholders = append(placeholders, projectcore.DiagramPlaceholder{
			ID:           diagram.ID,
			Type:         diagram.Type,
			Options:      diagram.Options,
			Body:         diagram.Body,
			Source:       diagram.Source,
			SourceFile:   diagram.SourceFile,
			SourceLine:   diagram.SourceLine,
			SourceColumn: diagram.SourceColumn,
			Placeholder:  diagram.Placeholder,
			Layout: projectcore.DiagramLayout{
				Alignment: diagram.Layout.Alignment,
			},
		})
	}
	return placeholders
}

func diagramContainerHTML(tagName string, diagram projectdiagrams.Diagram, rendered DiagramRenderResponse) string {
	kind := firstNonEmpty(rendered.Type, diagram.Type)
	url := firstNonEmpty(rendered.CloudBaseURL, rendered.URL)
	diagramID := firstNonEmpty(rendered.ID, diagram.ID)
	diagramHTML := fmt.Sprintf(
		`<%s class="rin-tikz rin-tikz-%s" data-diagram-id="%s" data-source-file="%s"><div class="rin-tikz-svg"><img src="%s" alt="%s diagram" loading="lazy" decoding="async" data-rin-diagram-object-id="%s"></div></%s>`,
		tagName,
		stdhtml.EscapeString(diagramClassSuffix(kind)),
		stdhtml.EscapeString(diagramID),
		stdhtml.EscapeString(diagram.SourceFile),
		stdhtml.EscapeString(url),
		stdhtml.EscapeString(kind),
		stdhtml.EscapeString(rendered.ObjectID),
		tagName,
	)
	if strings.TrimSpace(diagram.Layout.Alignment) == "" {
		return diagramHTML
	}
	align := diagramClassSuffix(diagram.Layout.Alignment)
	return fmt.Sprintf(
		`<div class="rin-align-block rin-align-%s" data-rin-align="%s">%s</div>`,
		stdhtml.EscapeString(align),
		stdhtml.EscapeString(align),
		diagramHTML,
	)
}

func diagramClassSuffix(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			out.WriteRune(r)
		case r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-' || r == '_':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func normalizeLateXMLHTML(html string, manifest projectcore.Manifest) string {
	html = stripUnsafeLateXMLHTML(html)
	html = removeHTMLBlocksByClass(html, "footer", "ltx_page_footer")
	html = normalizeLateXMLMathML(html)
	html = normalizeLateXMLSemanticClasses(html)
	html = normalizeLateXMLParagraphEnvironments(html)
	html = normalizeLateXMLTrailingBreaks(html)
	html, _ = normalizeReaderHeadingHTML(html)
	return markLateXMLAssetReferences(html, manifest)
}

func normalizeLateXMLMathML(html string) string {
	if strings.TrimSpace(html) == "" {
		return html
	}
	mathPattern := regexp.MustCompile(`(?is)<math\b[^>]*>`)
	return mathPattern.ReplaceAllStringFunc(html, func(tag string) string {
		displayClass := "rin-math-inline"
		if strings.EqualFold(getHTMLAttribute(tag, "display"), "block") {
			displayClass = "rin-math-display"
		}
		return addHTMLClasses(tag, "rin-math", displayClass)
	})
}

var lateXMLMathElementPattern = regexp.MustCompile(`(?is)<math\b[^>]*>.*?</math>`)
var lateXMLTableElementPattern = regexp.MustCompile(`(?is)<table\b[^>]*>.*?</table>`)
var lateXMLTableRowPattern = regexp.MustCompile(`(?is)<tr\b[^>]*>.*?</tr>`)
var lateXMLPGFStrokeColorDefinitionPattern = regexp.MustCompile(`(?is)\\definecolor(?:\[[^\]]+\])?\{pgfstrokecolor\}\{rgb\}\{[^}]+\}`)
var lateXMLTurnboxMathPattern = regexp.MustCompile(`(?is)\\turnbox\{[^}]+\}\{\$([^$]+)\$\}`)
var lateXMLOperatorNamePattern = regexp.MustCompile(`(?s)\\operatorname\{([^{}]*)\}`)
var lateXMLMathLineContinuationPattern = regexp.MustCompile(`%\s*(?:\r?\n|\r)\s*`)

func (s *Server) primaryMathRenderer() mathrender.Renderer {
	if s.primaryMath != nil {
		return s.primaryMath
	}
	if s.cfg.MathPrimaryEngine == "katex" {
		renderer := mathrender.NewKaTeXRenderer(s.cfg.KaTeXScript, s.cfg.MathTimeout, s.cfg.MathSourceMaxBytes)
		renderer.NodeBin = s.cfg.KaTeXNodeBin
		return renderer
	}
	renderer := mathrender.NewMathJaxRenderer(s.cfg.MathJaxScript, s.cfg.MathTimeout, s.cfg.MathSourceMaxBytes)
	renderer.NodeBin = s.cfg.MathJaxNodeBin
	return renderer
}

const (
	// v3 drops the recovery macro pack: LaTeX semantics belong to the source and
	// to LaTeXML, so a cached unit rendered with invented definitions must not be
	// replayed. v2 also enabled the boldsymbol package.
	sharedMathPluginVersion        = "rin-math-tex-input/v3"
	sharedMathSanitizerVersion     = "rin-math-output/v1"
	sharedMathCompatibilityVersion = "rin-reader-math/v1"
)

func (s *Server) sharedMathService(requestID string, cache *mathRenderCache) (*mathservice.Service, error) {
	primaryVersion := s.cfg.MathJaxVersion
	if s.cfg.MathPrimaryEngine == "katex" {
		primaryVersion = s.cfg.KaTeXVersion
	}
	fallbackVersion := strings.Join([]string{
		firstNonEmpty(s.cfg.DiagramEngineVersion, "rin-texsvg"),
		firstNonEmpty(s.cfg.TeXLiveVersion, "texlive-unreported"),
		firstNonEmpty(s.cfg.DvisvgmVersion, "dvisvgm-unreported"),
	}, "+")
	config := mathservice.Config{
		Primary:              s.primaryMathRenderer(),
		PrimaryEngine:        s.cfg.MathPrimaryEngine,
		PrimaryVersion:       primaryVersion,
		FontVersion:          s.cfg.MathJaxFontVersion,
		PluginVersion:        sharedMathPluginVersion,
		SanitizerVersion:     sharedMathSanitizerVersion,
		CompatibilityVersion: sharedMathCompatibilityVersion,
	}
	if strings.TrimSpace(s.cfg.DiagramWorkerEndpoint) != "" && strings.TrimSpace(s.cfg.StorageBaseURL) != "" {
		config.FallbackVersion = fallbackVersion
		config.Fallback = func(ctx context.Context, item orchestration.MathBatchItem) (mathservice.FallbackResult, error) {
			source := normalizeMathSourceForTeXSVG(item.Unit.Source)
			rendered, err := cache.renderSVG(ctx, s, requestID, source, *item.Unit.Display)
			if err != nil {
				return mathservice.FallbackResult{}, err
			}
			diagnostics := make([]contracts.Diagnostic, 0, len(rendered.Diagnostics))
			for _, current := range rendered.Diagnostics {
				diagnostics = append(diagnostics, contracts.Diagnostic{
					Code: current.Code, Severity: current.Severity, Message: current.Message,
					Stage: "math", SourceLocation: item.Unit.SourceLocation,
				})
			}
			return mathservice.FallbackResult{
				URL: firstNonEmpty(rendered.CloudBaseURL, rendered.URL), SVGHash: rendered.SVGHash,
				EngineVersion: fallbackVersion,
				Artifact: &contracts.ArtifactReference{
					ArtifactID: rendered.ObjectID, SHA256: rendered.SVGHash, Bytes: rendered.SVGBytes,
					MediaType: renderstorage.SVGContentType, Visibility: "public",
				},
				Diagnostics: diagnostics,
			}, nil
		}
	}
	return mathservice.New(config)
}

func newMathBatchItem(source string, display bool, wrapperID string, location *contracts.SourceLocation, macroContexts ...map[string]string) (orchestration.MathBatchItem, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return orchestration.MathBatchItem{}, errors.New("math source is empty")
	}
	id, err := contracts.NewWorkID()
	if err != nil {
		return orchestration.MathBatchItem{}, err
	}
	macros := firstMathMacroContext(macroContexts)
	hash := orchestration.ComputeMathMacroContextHash(macros)
	return orchestration.MathBatchItem{
		Unit: contracts.WorkUnit{
			Kind: contracts.WorkUnitMath, ID: id, Source: source, Display: &display,
			MacroContextHash: hash, SourceLocation: location,
		},
		WrapperID: strings.TrimSpace(wrapperID),
	}, nil
}

func firstMathMacroContext(contexts []map[string]string) map[string]string {
	if len(contexts) == 0 || len(contexts[0]) == 0 {
		return map[string]string{}
	}
	macros := make(map[string]string, len(contexts[0]))
	for name, definition := range contexts[0] {
		macros[name] = definition
	}
	return macros
}

func (s *Server) mathBatch(items []orchestration.MathBatchItem, macroContexts ...map[string]string) orchestration.MathBatch {
	macros := firstMathMacroContext(macroContexts)
	hash := orchestration.ComputeMathMacroContextHash(macros)
	return orchestration.MathBatch{
		ContractVersion: orchestration.MathBatchContractVersion,
		Items:           items, MacroContexts: map[string]map[string]string{hash: macros},
		OutputStrategy: orchestration.MathOutputStrategy(cleanMathOutputStrategy(s.cfg.MathOutputStrategy)),
	}
}

func projectMathSourceLocation(unit projectmath.Unit) *contracts.SourceLocation {
	if strings.TrimSpace(unit.SourceFile) == "" || unit.SourceLine < 1 || unit.SourceColumn < 1 {
		return nil
	}
	return &contracts.SourceLocation{
		Path:  unit.SourceFile,
		Start: contracts.SourcePosition{Line: unit.SourceLine, Column: unit.SourceColumn},
	}
}

func sharedMathToken(id string) string {
	return "RINRENDERERSHAREDMATH" + strings.ToUpper(strings.TrimPrefix(id, "rw_"))
}

func resolvedMathUnits(result orchestration.MathBatchResult) map[string]orchestration.MathResolvedUnit {
	resolved := make(map[string]orchestration.MathResolvedUnit, len(result.Units))
	for _, unit := range result.Units {
		resolved[unit.ID] = unit
	}
	return resolved
}

func incrementMathSummaryResolved(summary *MathSummary, unit orchestration.MathResolvedUnit, occurrences int) {
	if summary == nil || occurrences < 1 {
		return
	}
	operational.Default().Count("cache_requests_total", "math", unit.Cache.Status)
	if unit.State != "succeeded" {
		summary.FailedCount += occurrences
		return
	}
	switch strings.ToLower(strings.TrimSpace(unit.Engine)) {
	case "mathjax", "mathjax-chtml":
		summary.MathJaxCount += occurrences
	case "katex", "katex-legacy":
		summary.KaTeXCount += occurrences
	case "rin-texsvg", "texsvg", "texsvg-fallback":
		summary.SVGFallbackCount += occurrences
	default:
		summary.FailedCount += occurrences
	}
}

func diagnosticsFromSharedMath(unit orchestration.MathResolvedUnit, source string) []Diagnostic {
	result := make([]Diagnostic, 0, len(unit.Diagnostics))
	for _, current := range unit.Diagnostics {
		result = append(result, Diagnostic{
			Severity: current.Severity, Code: current.Code, Message: current.Message,
			Engine: unit.Engine, Source: map[string]string{"source": source},
		})
	}
	return result
}

func sharedMathServiceDiagnostic(err error) Diagnostic {
	return Diagnostic{Severity: "warning", Code: "math.service.failed", Message: err.Error(), Engine: "shared-math"}
}

func escapedMathFallbackHTML(source string, display bool, id string) string {
	tagName := "span"
	displayClass := "rin-math-inline"
	if display {
		tagName = "div"
		displayClass = "rin-math-display"
	}
	attrs := ""
	if strings.TrimSpace(id) != "" {
		attrs += fmt.Sprintf(` id="%s"`, stdhtml.EscapeString(strings.TrimSpace(id)))
	}
	attrs += fmt.Sprintf(` class="rin-math %s rin-math-fallback" data-rin-math-engine="unavailable" data-rin-math-source="%s"`,
		displayClass, stdhtml.EscapeString(source))
	body := `<code class="rin-math-source-fallback">` + stdhtml.EscapeString(source) + `</code>`
	return fmt.Sprintf("<%s%s>%s</%s>", tagName, attrs, body, tagName)
}

func (s *Server) replaceSourceMathPlaceholdersHTML(ctx context.Context, requestID string, html string, units []projectmath.Unit, macroContexts ...map[string]string) (string, MathSummary, []contracts.ArtifactReference, []Diagnostic) {
	if len(units) == 0 || strings.TrimSpace(html) == "" {
		return html, MathSummary{}, nil, nil
	}
	cache := newMathRenderCache()
	service, err := s.sharedMathService(requestID, cache)
	if err != nil {
		summary := MathSummary{}
		for _, unit := range units {
			occurrences := strings.Count(html, unit.Placeholder)
			if occurrences == 0 {
				continue
			}
			summary.Count += occurrences
			summary.FailedCount += occurrences
			html = replaceMathPlaceholder(html, unit.Placeholder, escapedMathFallbackHTML(unit.RenderSource, unit.DisplayMode, unit.ID))
		}
		return html, summary, nil, []Diagnostic{sharedMathServiceDiagnostic(err)}
	}
	type sourceTarget struct {
		item        orchestration.MathBatchItem
		placeholder string
		occurrences int
	}
	targets := make([]sourceTarget, 0, len(units))
	summary := MathSummary{}
	diagnostics := []Diagnostic{}
	for _, unit := range units {
		occurrences := strings.Count(html, unit.Placeholder)
		if occurrences == 0 {
			continue
		}
		item, itemErr := newMathBatchItem(normalizeLateXMLMathSource(unit.RenderSource), unit.DisplayMode, unit.ID, projectMathSourceLocation(unit), firstMathMacroContext(macroContexts))
		if itemErr != nil {
			summary.Count += occurrences
			summary.FailedCount += occurrences
			diagnostics = append(diagnostics, sharedMathServiceDiagnostic(itemErr))
			html = replaceMathPlaceholder(html, unit.Placeholder, escapedMathFallbackHTML(unit.RenderSource, unit.DisplayMode, unit.ID))
			continue
		}
		targets = append(targets, sourceTarget{item: item, placeholder: unit.Placeholder, occurrences: occurrences})
	}
	if len(targets) == 0 {
		return html, summary, nil, diagnostics
	}
	items := make([]orchestration.MathBatchItem, len(targets))
	for index := range targets {
		items[index] = targets[index].item
	}
	started := time.Now()
	operational.Default().AddGauge("math_batches_in_use", 1, "math_node")
	result, err := service.ResolveMath(ctx, s.mathBatch(items, firstMathMacroContext(macroContexts)))
	operational.Default().AddGauge("math_batches_in_use", -1, "math_node")
	operational.Default().Observe("stage_duration_seconds", time.Since(started), "math")
	if err != nil {
		diagnostics = append(diagnostics, sharedMathServiceDiagnostic(err))
		for _, target := range targets {
			summary.Count += target.occurrences
			summary.FailedCount += target.occurrences
			html = replaceMathPlaceholder(html, target.placeholder, escapedMathFallbackHTML(target.item.Unit.Source, *target.item.Unit.Display, target.item.WrapperID))
		}
		return html, summary, nil, diagnostics
	}
	resolved := resolvedMathUnits(result)
	for index, target := range targets {
		unit := resolved[target.item.Unit.ID]
		summary.Count += target.occurrences
		incrementMathSummaryResolved(&summary, unit, target.occurrences)
		diagnostics = append(diagnostics, diagnosticsFromSharedMath(unit, target.item.Unit.Source)...)
		namespacedHTML := namespaceMathJaxIdentifiers(unit.HTML, fmt.Sprintf("rin-source-math-%06d", index+1))
		html = replaceMathPlaceholder(html, target.placeholder, namespacedHTML)
	}
	if summary.MathJaxCount > 0 && len(result.CSS) > 0 {
		html = prependMathJaxStyleHTML(html, strings.Join(result.CSS, "\n"))
	}
	return html, summary, artifactsFromMathResult(result), diagnostics
}

func replaceMathPlaceholder(html string, placeholder string, replacement string) string {
	html = replacePlaceholderContainers(html, placeholder, replacement)
	html = replacePlaceholderParagraphs(html, placeholder, replacement)
	return strings.ReplaceAll(html, placeholder, replacement)
}

func mergeMathSummary(a MathSummary, b MathSummary) MathSummary {
	return MathSummary{
		Count:            a.Count + b.Count,
		MathJaxCount:     a.MathJaxCount + b.MathJaxCount,
		KaTeXCount:       a.KaTeXCount + b.KaTeXCount,
		SVGFallbackCount: a.SVGFallbackCount + b.SVGFallbackCount,
		FailedCount:      a.FailedCount + b.FailedCount,
	}
}

func (s *Server) renderLateXMLMathHTML(ctx context.Context, requestID string, html string, macroContexts ...map[string]string) (string, MathSummary, []contracts.ArtifactReference, []Diagnostic) {
	if strings.TrimSpace(html) == "" {
		return html, MathSummary{}, nil, nil
	}
	cache := newMathRenderCache()
	service, err := s.sharedMathService(requestID, cache)
	if err != nil {
		return html, MathSummary{}, nil, []Diagnostic{sharedMathServiceDiagnostic(err)}
	}
	type htmlTarget struct {
		item     orchestration.MathBatchItem
		token    string
		fallback string
	}
	targets := []htmlTarget{}
	summary := MathSummary{}
	diagnostics := []Diagnostic{}
	html = lateXMLTableElementPattern.ReplaceAllStringFunc(html, func(fragment string) string {
		openEnd := strings.Index(fragment, ">")
		if openEnd < 0 {
			return fragment
		}
		openTag := fragment[:openEnd+1]
		if !isLateXMLEquationTable(openTag) {
			return fragment
		}
		source, ok := lateXMLEquationTableSource(fragment)
		if !ok {
			return fragment
		}
		item, itemErr := newMathBatchItem(source, true, getHTMLAttribute(openTag, "id"), nil, firstMathMacroContext(macroContexts))
		if itemErr != nil {
			diagnostics = append(diagnostics, sharedMathServiceDiagnostic(itemErr))
			return fragment
		}
		token := sharedMathToken(item.Unit.ID)
		targets = append(targets, htmlTarget{item: item, token: token, fallback: fragment})
		return token
	})
	replaced := lateXMLMathElementPattern.ReplaceAllStringFunc(html, func(fragment string) string {
		openEnd := strings.Index(fragment, ">")
		if openEnd < 0 {
			return fragment
		}
		openTag := fragment[:openEnd+1]
		source := normalizeLateXMLMathSource(getHTMLAttribute(openTag, "alttext"))
		if source == "" {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: "warning",
				Code:     "math.source.missing",
				Message:  "MathML element has no TeX source; leaving original MathML in place.",
				Engine:   s.cfg.MathPrimaryEngine,
			})
			return addHTMLClasses(openTag, "rin-math-fallback") + fragment[openEnd+1:]
		}
		displayMode := isLateXMLDisplayMath(openTag)
		item, itemErr := newMathBatchItem(source, displayMode, "", nil, firstMathMacroContext(macroContexts))
		if itemErr != nil {
			diagnostics = append(diagnostics, sharedMathServiceDiagnostic(itemErr))
			return addHTMLClasses(openTag, "rin-math-fallback") + fragment[openEnd+1:]
		}
		token := sharedMathToken(item.Unit.ID)
		fallback := addHTMLClasses(openTag, "rin-math-fallback") + fragment[openEnd+1:]
		targets = append(targets, htmlTarget{item: item, token: token, fallback: fallback})
		return token
	})
	if len(targets) == 0 {
		return replaced, summary, nil, diagnostics
	}
	items := make([]orchestration.MathBatchItem, len(targets))
	for index := range targets {
		items[index] = targets[index].item
	}
	started := time.Now()
	operational.Default().AddGauge("math_batches_in_use", 1, "math_node")
	result, err := service.ResolveMath(ctx, s.mathBatch(items, firstMathMacroContext(macroContexts)))
	operational.Default().AddGauge("math_batches_in_use", -1, "math_node")
	operational.Default().Observe("stage_duration_seconds", time.Since(started), "math")
	if err != nil {
		diagnostics = append(diagnostics, sharedMathServiceDiagnostic(err))
		for _, target := range targets {
			summary.Count++
			summary.FailedCount++
			replaced = strings.ReplaceAll(replaced, target.token, target.fallback)
		}
		return replaced, summary, nil, diagnostics
	}
	resolved := resolvedMathUnits(result)
	for index, target := range targets {
		unit := resolved[target.item.Unit.ID]
		summary.Count++
		incrementMathSummaryResolved(&summary, unit, 1)
		diagnostics = append(diagnostics, diagnosticsFromSharedMath(unit, target.item.Unit.Source)...)
		namespacedHTML := namespaceMathJaxIdentifiers(unit.HTML, fmt.Sprintf("rin-latexml-math-%06d", index+1))
		replaced = strings.ReplaceAll(replaced, target.token, namespacedHTML)
	}
	if summary.MathJaxCount > 0 && len(result.CSS) > 0 {
		replaced = prependMathJaxStyleHTML(replaced, strings.Join(result.CSS, "\n"))
	}
	return replaced, summary, artifactsFromMathResult(result), diagnostics
}

var mathJaxIdentifierPattern = regexp.MustCompile(`\bid="(mjx-[^"]+)"`)

func namespaceMathJaxIdentifiers(fragment string, namespace string) string {
	namespace = strings.TrimSpace(namespace)
	if fragment == "" || namespace == "" {
		return fragment
	}
	matches := mathJaxIdentifierPattern.FindAllStringSubmatch(fragment, -1)
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		identifier := match[1]
		if _, duplicate := seen[identifier]; duplicate {
			continue
		}
		seen[identifier] = struct{}{}
		namespaced := namespace + "-" + identifier
		for _, attribute := range []string{"href", "xlink:href"} {
			fragment = strings.ReplaceAll(fragment, attribute+`="#`+identifier+`"`, attribute+`="#`+namespaced+`"`)
		}
		for _, attribute := range []string{"aria-labelledby", "aria-describedby"} {
			fragment = strings.ReplaceAll(fragment, attribute+`="`+identifier+`"`, attribute+`="`+namespaced+`"`)
		}
		fragment = strings.ReplaceAll(fragment, `url(#`+identifier+`)`, `url(#`+namespaced+`)`)
		fragment = strings.ReplaceAll(fragment, `id="`+identifier+`"`, `id="`+namespaced+`"`)
	}
	return fragment
}

func artifactsFromMathResult(result orchestration.MathBatchResult) []contracts.ArtifactReference {
	items := make([]contracts.ArtifactReference, 0, len(result.Units))
	for _, unit := range result.Units {
		if unit.Artifact != nil {
			items = append(items, *unit.Artifact)
		}
	}
	return items
}

func uniqueArtifactReferences(items []contracts.ArtifactReference) []contracts.ArtifactReference {
	byID := make(map[string]contracts.ArtifactReference, len(items))
	for _, item := range items {
		byID[item.ArtifactID] = item
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]contracts.ArtifactReference, 0, len(ids))
	for _, id := range ids {
		result = append(result, byID[id])
	}
	return result
}

func isLateXMLEquationTable(openTag string) bool {
	classes := htmlClassSet(openTag)
	return hasHTMLClass(classes, "ltx_equation") || hasHTMLClass(classes, "ltx_equationgroup") || hasHTMLClass(classes, "ltx_eqn_table")
}

func lateXMLEquationTableSource(table string) (string, bool) {
	rows := []string{}
	for _, row := range lateXMLTableRowPattern.FindAllString(table, -1) {
		cells := lateXMLMathSources(row)
		if len(cells) == 0 {
			continue
		}
		rows = append(rows, strings.Join(cells, " & "))
	}
	if len(rows) == 0 {
		cells := lateXMLMathSources(table)
		if len(cells) == 0 {
			return "", false
		}
		rows = append(rows, strings.Join(cells, " & "))
	}
	for _, math := range lateXMLMathElementPattern.FindAllString(table, -1) {
		openEnd := strings.Index(math, ">")
		if openEnd < 0 || strings.TrimSpace(getHTMLAttribute(math[:openEnd+1], "alttext")) == "" {
			return "", false
		}
	}
	source := strings.Join(rows, ` \\ `)
	if len(rows) > 1 || strings.Contains(source, " & ") {
		source = `\begin{aligned}` + source + `\end{aligned}`
	}
	return source, true
}

func lateXMLMathSources(fragment string) []string {
	matches := lateXMLMathElementPattern.FindAllString(fragment, -1)
	sources := make([]string, 0, len(matches))
	for _, math := range matches {
		openEnd := strings.Index(math, ">")
		if openEnd < 0 {
			continue
		}
		source := normalizeLateXMLMathSource(getHTMLAttribute(math[:openEnd+1], "alttext"))
		if source != "" {
			sources = append(sources, source)
		}
	}
	return sources
}

func normalizeLateXMLMathSource(source string) string {
	source = strings.TrimSpace(source)
	source = lateXMLMathLineContinuationPattern.ReplaceAllString(source, "")
	for {
		trimmed := strings.TrimSpace(strings.TrimPrefix(source, `\displaystyle`))
		if trimmed == source {
			break
		}
		source = trimmed
	}
	source = lateXMLPGFStrokeColorDefinitionPattern.ReplaceAllString(source, "")
	source = strings.ReplaceAll(source, `\color[rgb]{1,0,0}`, `\color{red}`)
	source = strings.ReplaceAll(source, `\color[rgb]{0,0,1}`, `\color{blue}`)
	source = strings.ReplaceAll(source, `\color[rgb]{0,1,0}`, `\color{green}`)
	source = lateXMLTurnboxMathPattern.ReplaceAllString(source, `$1`)
	source = normalizeOperatorNameUnicodeText(source)
	return source
}

func normalizeMathSourceForTeXSVG(source string) string {
	return wrapLateXMLMatrixEnvironmentsForTeX(normalizeLateXMLMathSource(source))
}

func wrapLateXMLMatrixEnvironmentsForTeX(source string) string {
	// LaTeX's amsmath can misread a matrix environment that follows an aligned
	// column marker, e.g. \begin{aligned}&\begin{vmatrix}...\end{vmatrix}.
	// Grouping matrix environments is visually neutral and keeps the TeX SVG
	// fallback compatible with formulas that MathJax accepts directly.
	for _, env := range []string{"matrix", "pmatrix", "bmatrix", "Bmatrix", "vmatrix", "Vmatrix", "smallmatrix"} {
		source = strings.ReplaceAll(source, `\begin{`+env+`}`, `{\begin{`+env+`}`)
		source = strings.ReplaceAll(source, `\end{`+env+`}`, `\end{`+env+`}}`)
	}
	return source
}

func normalizeOperatorNameUnicodeText(source string) string {
	return lateXMLOperatorNamePattern.ReplaceAllStringFunc(source, func(match string) string {
		parts := lateXMLOperatorNamePattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		normalized := wrapNonASCIIText(parts[1])
		if normalized == parts[1] {
			return match
		}
		return `\operatorname{` + normalized + `}`
	})
}

func wrapNonASCIIText(source string) string {
	var out strings.Builder
	changed := false
	for _, r := range source {
		if r <= unicode.MaxASCII {
			out.WriteRune(r)
			continue
		}
		changed = true
		out.WriteString(`\text{`)
		out.WriteRune(r)
		out.WriteString(`}`)
	}
	if !changed {
		return source
	}
	return out.String()
}

func isLateXMLDisplayMath(openTag string) bool {
	if strings.EqualFold(getHTMLAttribute(openTag, "display"), "block") {
		return true
	}
	return hasHTMLClass(htmlClassSet(openTag), "rin-math-display")
}

type mathRenderCache struct {
	svg           map[string]mathCachedSVG
	svgRenderJobs int
}

type mathCachedSVG struct {
	rendered DiagramRenderResponse
	err      error
}

func newMathRenderCache() *mathRenderCache {
	return &mathRenderCache{
		svg: make(map[string]mathCachedSVG),
	}
}

var mathJaxStyleHTMLPattern = regexp.MustCompile(`(?is)<style\b[^>]*\brin-mathjax-chtml-style\b[^>]*>(.*?)</style>`)

func mergeMathJaxCSS(blocks ...string) string {
	return mathservice.MergeCSSInOrder(blocks...)
}

func prependMathJaxStyleHTML(html string, css string) string {
	css = strings.TrimSpace(css)
	if css == "" {
		return html
	}
	if match := mathJaxStyleHTMLPattern.FindStringSubmatchIndex(html); len(match) >= 4 {
		existingCSS := html[match[2]:match[3]]
		css = mergeMathJaxCSS(existingCSS, css)
		css = strings.ReplaceAll(css, "</", "<\\/")
		style := fmt.Sprintf(`<style class="rin-mathjax-chtml-style" data-rin-math-engine="mathjax-chtml">%s</style>`, css)
		return html[:match[0]] + style + html[match[1]:]
	}
	css = strings.ReplaceAll(css, "</", "<\\/")
	style := fmt.Sprintf(`<style class="rin-mathjax-chtml-style" data-rin-math-engine="mathjax-chtml">%s</style>`, css)
	return style + "\n" + html
}

func (c *mathRenderCache) renderSVG(ctx context.Context, server *Server, requestID string, source string, displayMode bool) (DiagramRenderResponse, error) {
	if c == nil {
		return server.renderMathSVGToCloudBase(ctx, requestID, source, displayMode)
	}
	if c.svg == nil {
		c.svg = make(map[string]mathCachedSVG)
	}
	key := mathCacheKey("rin-texsvg", source, displayMode)
	if cached, ok := c.svg[key]; ok {
		operational.Default().Count("cache_requests_total", "texsvg", "hit")
		return cached.rendered, cached.err
	}
	operational.Default().Count("cache_requests_total", "texsvg", "miss")
	if c.svgRenderJobs >= server.cfg.MathTexSVGFallbackMaxCount {
		err := fmt.Errorf("TeX SVG math fallback limit reached: %d", server.cfg.MathTexSVGFallbackMaxCount)
		c.svg[key] = mathCachedSVG{err: err}
		return DiagramRenderResponse{}, err
	}
	c.svgRenderJobs++
	started := time.Now()
	rendered, err := server.renderMathSVGToCloudBase(ctx, requestID, source, displayMode)
	operational.Default().Observe("stage_duration_seconds", time.Since(started), "diagram")
	c.svg[key] = mathCachedSVG{rendered: rendered, err: err}
	return rendered, err
}

func mathCacheKey(engine string, source string, displayMode bool) string {
	mode := "inline"
	if displayMode {
		mode = "display"
	}
	return strings.TrimSpace(engine) + "\x00" + mode + "\x00" + strings.TrimSpace(source)
}

func (s *Server) renderMathSVGToCloudBase(ctx context.Context, requestID string, source string, displayMode bool) (DiagramRenderResponse, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return DiagramRenderResponse{}, errors.New("empty math source")
	}
	renderCtx := ctx
	cancel := func() {}
	if s.cfg.MathTimeout > 0 {
		renderCtx, cancel = context.WithTimeout(ctx, s.cfg.MathTimeout)
	}
	defer cancel()

	options := "display"
	if !displayMode {
		options = "inline"
	}
	rendered, err := s.diagramWorker().Render(renderCtx, diagramengine.Request{
		Type:    "math",
		Options: options,
		Body:    source,
		Source:  source,
	})
	if err != nil {
		return DiagramRenderResponse{}, fmt.Errorf("math SVG worker render failed: %w", err)
	}
	svgObject, err := renderstorage.NormalizeSVGObject([]byte(rendered.SVG))
	if err != nil {
		return DiagramRenderResponse{}, fmt.Errorf("math SVG worker returned invalid SVG: %w", err)
	}
	stored, err := s.diagramStorage().PutPublicObjectIfMissing(renderCtx, svgObject.ObjectID, renderstorage.SVGContentType, svgObject.Bytes)
	if err != nil {
		return DiagramRenderResponse{}, fmt.Errorf("CloudBase math SVG upload failed: %w", err)
	}
	if stored.ObjectID != svgObject.ObjectID || !s.verifyMathSVGStored(renderCtx, stored.ObjectID, svgObject.Bytes) {
		return DiagramRenderResponse{}, errors.New("CloudBase math SVG failed post-write integrity verification")
	}
	mathID := strings.TrimSpace(rendered.ID)
	if mathID == "" {
		mathID = "math-svg-sha256-" + svgObject.Hash[:12]
	}
	return DiagramRenderResponse{
		RequestID:    requestID,
		ID:           mathID,
		Type:         "math",
		SVGHash:      svgObject.Hash,
		SVGBytes:     int64(len(svgObject.Bytes)),
		ObjectID:     stored.ObjectID,
		CloudBaseURL: stored.CloudBaseURL,
		URL:          stored.URL,
		Uploaded:     stored.Uploaded,
		Engine:       "rin-texsvg",
		Versions:     s.versions(),
		Diagnostics:  diagnosticsFromDiagramWorker(rendered.Diagnostics),
		Cached:       rendered.Cached || !stored.Uploaded,
		RenderURL:    rendered.URL,
	}, nil
}

func (s *Server) verifyMathSVGStored(ctx context.Context, objectID string, expected []byte) bool {
	for attempt, delay := range []time.Duration{0, 50 * time.Millisecond, 150 * time.Millisecond} {
		if attempt > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false
			case <-timer.C:
			}
		}
		body, err := s.diagramStorage().ReadPublicObject(ctx, objectID, int64(len(expected)))
		if err == nil && len(body) == len(expected) && renderstorage.SVGHash(body) == renderstorage.SVGHash(expected) {
			return true
		}
	}
	return false
}

func normalizeLateXMLSemanticClasses(html string) string {
	if strings.TrimSpace(html) == "" {
		return html
	}
	openTagPattern := regexp.MustCompile(`(?is)<[a-z][a-z0-9:-]*\b[^>]*>`)
	return openTagPattern.ReplaceAllStringFunc(html, func(tag string) string {
		tagName := htmlTagName(tag)
		classes := htmlClassSet(tag)
		add := semanticClassesForLateXMLTag(tagName, classes)
		if len(add) == 0 {
			return tag
		}
		return addHTMLClasses(tag, add...)
	})
}

func semanticClassesForLateXMLTag(tagName string, classes map[string]bool) []string {
	tagName = strings.ToLower(strings.TrimSpace(tagName))
	switch {
	case tagName == "figure" && hasHTMLClass(classes, "ltx_figure"):
		return []string{"rin-float", "rin-float-figure"}
	case tagName == "figure" && hasHTMLClass(classes, "ltx_table"):
		return []string{"rin-float", "rin-float-table"}
	case tagName == "table" && hasHTMLClassPrefix(classes, "ltx_longtable"):
		return []string{"rin-table", "rin-table-longtable"}
	case tagName == "table" && hasHTMLClassPrefix(classes, "ltx_tabular"):
		return []string{"rin-table", "rin-table-tabular"}
	case tagName == "table" && (hasHTMLClass(classes, "ltx_equation") || hasHTMLClass(classes, "ltx_equationgroup") || hasHTMLClass(classes, "ltx_eqn_table")):
		return []string{"rin-equation", "rin-equation-table"}
	case tagName == "ol" && hasHTMLClass(classes, "ltx_enumerate"):
		return []string{"rin-list", "rin-list-enumerate"}
	case tagName == "ul" && hasHTMLClass(classes, "ltx_itemize"):
		return []string{"rin-list", "rin-list-itemize"}
	case tagName == "li" && hasHTMLClass(classes, "ltx_item"):
		return []string{"rin-list-item"}
	case tagName == "span" && hasHTMLClass(classes, "ltx_tag_item"):
		return []string{"rin-list-marker"}
	case hasHTMLClass(classes, "ltx_caption"):
		return []string{"rin-caption"}
	case hasHTMLClass(classes, "ltx_subfigure") || hasHTMLClass(classes, "ltx_subfloat"):
		return []string{"rin-subfloat"}
	case hasHTMLClass(classes, "ltx_minipage"):
		return []string{"rin-minipage"}
	case isSemanticBlockTag(tagName) && hasHTMLClassPrefix(classes, "ltx_theorem"):
		kind := latexXMLTheoremKind(classes)
		if kind == "" {
			kind = "theorem"
		}
		return []string{"rin-env", "rin-env-" + kind}
	case isSemanticBlockTag(tagName) && hasHTMLClass(classes, "ltx_proof"):
		return []string{"rin-env", "rin-env-proof"}
	case hasHTMLClass(classes, "ltx_title_theorem") || hasHTMLClass(classes, "ltx_title_proof"):
		return []string{"rin-env-title"}
	case hasHTMLClassPrefix(classes, "ltx_ref"):
		return []string{"rin-ref"}
	case hasHTMLClassPrefix(classes, "ltx_label"):
		return []string{"rin-label"}
	case hasHTMLClassPrefix(classes, "ltx_cite"):
		return []string{"rin-citation"}
	case hasHTMLClass(classes, "ltx_bibliography"):
		return []string{"rin-bibliography"}
	case hasHTMLClass(classes, "ltx_bibitem"):
		return []string{"rin-bib-item"}
	default:
		return nil
	}
}

func isSemanticBlockTag(tagName string) bool {
	switch tagName {
	case "article", "section", "div", "li", "blockquote":
		return true
	default:
		return false
	}
}

func latexXMLTheoremKind(classes map[string]bool) string {
	const prefix = "ltx_theorem_"
	for className := range classes {
		if !strings.HasPrefix(className, prefix) {
			continue
		}
		kind := strings.TrimPrefix(className, prefix)
		var builder strings.Builder
		for _, char := range kind {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' {
				builder.WriteRune(char)
			}
		}
		if builder.Len() > 0 {
			return builder.String()
		}
	}
	return ""
}

var lateXMLTrailingBreakPattern = regexp.MustCompile(`(?is)<br\b[^>]*\bclass\s*=\s*("[^"]*\bltx_break\b[^"]*"|'[^']*\bltx_break\b[^']*')[^>]*>\s*</p>`)

func normalizeLateXMLParagraphEnvironments(htmlText string) string {
	if strings.TrimSpace(htmlText) == "" || !strings.Contains(htmlText, "ltx_font_bold") {
		return htmlText
	}
	var out strings.Builder
	cursor := 0
	for cursor < len(htmlText) {
		start := nextHTMLTagStart(htmlText, "div", cursor, false)
		if start < 0 {
			break
		}
		openEnd := strings.IndexByte(htmlText[start:], '>')
		if openEnd < 0 {
			break
		}
		openEnd += start
		openTag := htmlText[start : openEnd+1]
		if !hasHTMLClass(htmlClassSet(openTag), "ltx_para") {
			out.WriteString(htmlText[cursor : openEnd+1])
			cursor = openEnd + 1
			continue
		}
		end := matchingHTMLContainerEnd(htmlText, "div", start)
		if end < 0 {
			break
		}
		block := htmlText[start:end]
		out.WriteString(htmlText[cursor:start])
		marked, kind, ok := markLateXMLParagraphEnvironmentTitle(block)
		if !ok {
			out.WriteString(block)
			cursor = end
			continue
		}
		groupEnd := lateXMLParagraphEnvironmentGroupEnd(htmlText, end)
		if groupEnd > end {
			out.WriteString(`<div class="rin-env rin-env-` + kind + ` rin-env-group">`)
			out.WriteString(marked)
			out.WriteString(htmlText[end:groupEnd])
			out.WriteString(`</div>`)
			cursor = groupEnd
			continue
		}
		out.WriteString(addLateXMLEnvironmentClasses(marked, kind))
		cursor = end
	}
	out.WriteString(htmlText[cursor:])
	return out.String()
}

func normalizeLateXMLParagraphEnvironmentBlock(block string) string {
	marked, kind, ok := markLateXMLParagraphEnvironmentTitle(block)
	if !ok {
		return block
	}
	return addLateXMLEnvironmentClasses(marked, kind)
}

func addLateXMLEnvironmentClasses(block string, kind string) string {
	openEnd := strings.IndexByte(block, '>')
	if openEnd < 0 {
		return block
	}
	if kind == "" {
		return block
	}
	openTag := addHTMLClasses(block[:openEnd+1], "rin-env", "rin-env-"+kind)
	return openTag + block[openEnd+1:]
}

func markLateXMLParagraphEnvironmentTitle(block string) (string, string, bool) {
	title, ok := lateXMLParagraphEnvironmentTitle(block)
	if !ok {
		return block, "", false
	}
	titleTag := addHTMLClasses(block[title.start:title.end], "rin-env-title")
	return block[:title.start] + titleTag + block[title.end:], title.kind, true
}

type lateXMLParagraphEnvironmentTitleInfo struct {
	start int
	end   int
	kind  string
}

func lateXMLParagraphEnvironmentTitle(block string) (lateXMLParagraphEnvironmentTitleInfo, bool) {
	openEnd := strings.IndexByte(block, '>')
	if openEnd < 0 {
		return lateXMLParagraphEnvironmentTitleInfo{}, false
	}
	cursor := openEnd + 1
	for cursor < len(block) {
		start := nextHTMLTagStart(block, "span", cursor, false)
		if start < 0 {
			return lateXMLParagraphEnvironmentTitleInfo{}, false
		}
		tagEnd := strings.IndexByte(block[start:], '>')
		if tagEnd < 0 {
			return lateXMLParagraphEnvironmentTitleInfo{}, false
		}
		tagEnd += start
		openTag := block[start : tagEnd+1]
		classes := htmlClassSet(openTag)
		end := matchingHTMLContainerEnd(block, "span", start)
		if end < 0 {
			return lateXMLParagraphEnvironmentTitleInfo{}, false
		}
		if hasHTMLClass(classes, "ltx_text") && hasHTMLClass(classes, "ltx_font_bold") {
			closeStart := strings.LastIndex(strings.ToLower(block[:end]), "</span")
			if closeStart < tagEnd+1 {
				return lateXMLParagraphEnvironmentTitleInfo{}, false
			}
			titleText := plainHTMLText(block[tagEnd+1 : closeStart])
			if len([]rune(titleText)) > 160 {
				return lateXMLParagraphEnvironmentTitleInfo{}, false
			}
			kind := lateXMLEnvironmentKindFromTitle(titleText)
			if kind != "" {
				return lateXMLParagraphEnvironmentTitleInfo{start: start, end: end, kind: kind}, true
			}
		}
		cursor = end
	}
	return lateXMLParagraphEnvironmentTitleInfo{}, false
}

func lateXMLParagraphEnvironmentGroupEnd(htmlText string, start int) int {
	groupEnd := start
	cursor := start
	for cursor < len(htmlText) {
		next := skipHTMLWhitespace(htmlText, cursor)
		if next >= len(htmlText) {
			return groupEnd
		}
		divStart := nextHTMLTagStart(htmlText, "div", next, false)
		if divStart != next {
			return groupEnd
		}
		openEnd := strings.IndexByte(htmlText[divStart:], '>')
		if openEnd < 0 {
			return groupEnd
		}
		openEnd += divStart
		openTag := htmlText[divStart : openEnd+1]
		classes := htmlClassSet(openTag)
		if !hasHTMLClass(classes, "ltx_para") || hasHTMLClass(classes, "ltx_noindent") {
			return groupEnd
		}
		end := matchingHTMLContainerEnd(htmlText, "div", divStart)
		if end < 0 {
			return groupEnd
		}
		if _, ok := lateXMLParagraphEnvironmentTitle(htmlText[divStart:end]); ok {
			return groupEnd
		}
		groupEnd = end
		cursor = end
	}
	return groupEnd
}

func skipHTMLWhitespace(value string, start int) int {
	for start < len(value) {
		switch value[start] {
		case ' ', '\n', '\r', '\t', '\f':
			start++
		default:
			return start
		}
	}
	return start
}

func normalizeLateXMLTrailingBreaks(htmlText string) string {
	if strings.TrimSpace(htmlText) == "" || !strings.Contains(htmlText, "ltx_break") {
		return htmlText
	}
	return lateXMLTrailingBreakPattern.ReplaceAllString(htmlText, "</p>")
}

func lateXMLEnvironmentKindFromTitle(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	if index := strings.IndexAny(title, " (（"); index >= 0 {
		title = strings.TrimSpace(title[:index])
	}
	if index := strings.IndexRune(title, '\u00a0'); index >= 0 {
		title = strings.TrimSpace(title[:index])
	}
	switch title {
	case "definition":
		return "definition"
	case "theorem":
		return "theorem"
	case "lemma":
		return "lemma"
	case "proposition":
		return "proposition"
	case "corollary":
		return "corollary"
	case "example":
		return "example"
	case "remark":
		return "remark"
	case "note":
		return "note"
	case "problem":
		return "problem"
	case "exercise":
		return "exercise"
	case "proof":
		return "proof"
	case "solution":
		return "solution"
	case "property":
		return "property"
	case "axiom":
		return "axiom"
	case "postulate":
		return "postulate"
	case "claim":
		return "claim"
	case "conjecture":
		return "conjecture"
	case "assumption":
		return "assumption"
	case "conclusion":
		return "conclusion"
	default:
		return ""
	}
}

var readerHeadingPattern = regexp.MustCompile(`(?is)<h([2-4])\b([^>]*)>(.*?)</h[2-4]>`)
var htmlIDAttrPattern = regexp.MustCompile(`(?is)\bid\s*=\s*["']([^"']+)["']`)
var htmlTagPattern = regexp.MustCompile(`(?is)<[^>]+>`)

type readerHeading struct {
	ID    string
	Text  string
	Kind  string
	Level int
	Start int
}

func normalizeReaderHeadingHTML(htmlText string) (string, []readerHeading) {
	if strings.TrimSpace(htmlText) == "" {
		return htmlText, nil
	}
	matches := readerHeadingPattern.FindAllStringSubmatchIndex(htmlText, -1)
	if len(matches) == 0 {
		return htmlText, nil
	}
	existing := existingHTMLIDs(htmlText)
	used := map[string]int{}
	var builder strings.Builder
	builder.Grow(len(htmlText) + len(matches)*16)
	headings := make([]readerHeading, 0, len(matches))
	last := 0
	for index, match := range matches {
		start, end := match[0], match[1]
		levelStart, levelEnd := match[2], match[3]
		attrsStart, attrsEnd := match[4], match[5]
		innerStart, innerEnd := match[6], match[7]
		builder.WriteString(htmlText[last:start])
		headingStart := builder.Len()
		level, _ := strconv.Atoi(htmlText[levelStart:levelEnd])
		attrs := htmlText[attrsStart:attrsEnd]
		inner := htmlText[innerStart:innerEnd]
		id := htmlIDFromAttrs(attrs)
		if id == "" {
			id = uniqueReaderHeadingID(slugifyReaderHeading(inner, fmt.Sprintf("section-%d", index+1)), existing, used)
			builder.WriteString(fmt.Sprintf("<h%d id=\"%s\"%s>%s</h%d>", level, stdhtml.EscapeString(id), attrs, inner, level))
		} else {
			builder.WriteString(htmlText[start:end])
		}
		text := plainHTMLText(inner)
		if text != "" {
			headings = append(headings, readerHeading{
				ID:    id,
				Text:  text,
				Kind:  readerHeadingKind(attrs),
				Level: level,
				Start: headingStart,
			})
		}
		last = end
	}
	builder.WriteString(htmlText[last:])
	return builder.String(), headings
}

func readerHeadingKind(attrs string) string {
	classes := htmlClassSet("<span" + attrs + ">")
	for _, kind := range []string{"chapter", "section"} {
		if classes["rin-heading-"+kind] || classes["ltx_title_"+kind] {
			return kind
		}
	}
	return ""
}

func existingHTMLIDs(htmlText string) map[string]bool {
	existing := map[string]bool{}
	htmlIDAttrPattern.ReplaceAllStringFunc(htmlText, func(match string) string {
		id := htmlIDFromAttrs(match)
		if id != "" {
			existing[id] = true
		}
		return match
	})
	return existing
}

func htmlIDFromAttrs(attrs string) string {
	groups := htmlIDAttrPattern.FindStringSubmatch(attrs)
	if len(groups) < 2 {
		return ""
	}
	return strings.TrimSpace(stdhtml.UnescapeString(groups[1]))
}

func plainHTMLText(value string) string {
	withoutTags := htmlTagPattern.ReplaceAllString(value, " ")
	decoded := stdhtml.UnescapeString(withoutTags)
	return strings.Join(strings.Fields(decoded), " ")
}

func slugifyReaderHeading(value string, fallback string) string {
	value = strings.ToLower(plainHTMLText(value))
	var builder strings.Builder
	lastDash := false
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			builder.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash && builder.Len() > 0 {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(builder.String(), "-")
	runes := []rune(slug)
	if len(runes) > 72 {
		slug = strings.Trim(string(runes[:72]), "-")
	}
	if slug == "" {
		return fallback
	}
	return slug
}

func uniqueReaderHeadingID(base string, existing map[string]bool, used map[string]int) string {
	used[base]++
	candidate := base
	if used[base] > 1 {
		candidate = fmt.Sprintf("%s-%d", base, used[base])
	}
	for existing[candidate] {
		used[base]++
		candidate = fmt.Sprintf("%s-%d", base, used[base])
	}
	existing[candidate] = true
	return candidate
}

func stripUnsafeLateXMLHTML(html string) string {
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`),
		regexp.MustCompile(`(?is)<iframe\b[^>]*>.*?</iframe\s*>`),
		regexp.MustCompile(`(?is)<iframe\b[^>]*/\s*>`),
	} {
		html = pattern.ReplaceAllString(html, "")
	}
	eventAttr := regexp.MustCompile(`(?is)\s+on[a-z][a-z0-9_-]*\s*=\s*("[^"]*"|'[^']*'|[^\s"'=<>` + "`" + `]+)`)
	html = eventAttr.ReplaceAllString(html, "")
	javascriptAttr := regexp.MustCompile(`(?is)\s+(href|src|data)\s*=\s*("javascript:[^"]*"|'javascript:[^']*'|javascript:[^\s"'=<>` + "`" + `]+)`)
	html = javascriptAttr.ReplaceAllString(html, "")
	imageTag := regexp.MustCompile(`(?is)<img\b[^>]*>`)
	return imageTag.ReplaceAllStringFunc(html, func(tag string) string {
		for _, attribute := range []string{"src", "srcset"} {
			value := strings.ToLower(strings.TrimSpace(stdhtml.UnescapeString(getHTMLAttribute(tag, attribute))))
			if strings.HasPrefix(value, "data:") || strings.Contains(value, ", data:") {
				return ""
			}
		}
		return tag
	})
}

func markLateXMLAssetReferences(html string, manifest projectcore.Manifest) string {
	lookup := lateXMLAssetLookup(manifest)
	if len(lookup) == 0 || strings.TrimSpace(html) == "" {
		return html
	}
	tagPattern := regexp.MustCompile(`(?is)<(img|object|a|embed|source)\b[^>]*>`)
	return tagPattern.ReplaceAllStringFunc(html, func(tag string) string {
		if getHTMLAttribute(tag, "data-rin-asset-path") != "" {
			return tag
		}
		tagName := htmlTagName(tag)
		for _, attr := range assetReferenceAttributes(tagName) {
			raw := getHTMLAttribute(tag, attr)
			key := normalizeHTMLAssetKey(raw)
			if key == "" {
				continue
			}
			if assetPath := lookup[key]; assetPath != "" {
				return addHTMLAttribute(tag, "data-rin-asset-path", assetPath)
			}
		}
		return tag
	})
}

func lateXMLAssetLookup(manifest projectcore.Manifest) map[string]string {
	eligible := make(map[string]bool)
	for _, asset := range manifest.AssetInventory.Assets {
		if asset.Referenced {
			eligible[asset.Path] = true
		}
	}
	for _, ref := range manifest.AssetInventory.References {
		if ref.Resolved && ref.Path != "" {
			eligible[ref.Path] = true
		}
	}
	if len(eligible) == 0 {
		return nil
	}

	lookup := make(map[string]string)
	add := func(raw string, assetPath string) {
		if !eligible[assetPath] {
			return
		}
		key := normalizeHTMLAssetKey(raw)
		if key == "" {
			return
		}
		if existing := lookup[key]; existing == "" || existing == assetPath {
			lookup[key] = assetPath
		} else {
			delete(lookup, key)
		}
	}

	baseCounts := make(map[string]int)
	basePath := make(map[string]string)
	for assetPath := range eligible {
		add(assetPath, assetPath)
		base := path.Base(assetPath)
		baseCounts[base]++
		basePath[base] = assetPath
	}
	for base, count := range baseCounts {
		if count == 1 {
			add(base, basePath[base])
		}
	}
	for _, ref := range manifest.AssetInventory.References {
		if ref.Resolved && ref.Path != "" {
			add(ref.Path, ref.Path)
			add(ref.RawRef, ref.Path)
			if path.Ext(ref.RawRef) == "" && path.Ext(ref.Path) != "" {
				add(ref.RawRef+path.Ext(ref.Path), ref.Path)
			}
		}
	}
	return lookup
}

func assetReferenceAttributes(tagName string) []string {
	switch strings.ToLower(strings.TrimSpace(tagName)) {
	case "object":
		return []string{"data"}
	case "a":
		return []string{"href"}
	default:
		return []string{"src"}
	}
}

func htmlTagName(tag string) string {
	tag = strings.TrimSpace(strings.TrimPrefix(tag, "<"))
	index := 0
	for index < len(tag) {
		ch := tag[index]
		if !(ch == '-' || ch == ':' || ch == '_' || ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z') {
			break
		}
		index++
	}
	return strings.ToLower(tag[:index])
}

func getHTMLAttribute(tag string, attr string) string {
	attr = strings.ToLower(strings.TrimSpace(attr))
	cursor := 0
	for cursor < len(tag) {
		for cursor < len(tag) && !isHTMLAttrNameStart(tag[cursor]) {
			cursor++
		}
		nameStart := cursor
		for cursor < len(tag) && isHTMLAttrNameChar(tag[cursor]) {
			cursor++
		}
		if nameStart == cursor {
			continue
		}
		name := strings.ToLower(tag[nameStart:cursor])
		for cursor < len(tag) && isHTMLSpace(tag[cursor]) {
			cursor++
		}
		if cursor >= len(tag) || tag[cursor] != '=' {
			continue
		}
		cursor++
		for cursor < len(tag) && isHTMLSpace(tag[cursor]) {
			cursor++
		}
		valueStart := cursor
		value := ""
		if cursor < len(tag) && (tag[cursor] == '"' || tag[cursor] == '\'') {
			quote := tag[cursor]
			cursor++
			valueStart = cursor
			for cursor < len(tag) && tag[cursor] != quote {
				cursor++
			}
			value = tag[valueStart:cursor]
			if cursor < len(tag) {
				cursor++
			}
		} else {
			for cursor < len(tag) && !isHTMLSpace(tag[cursor]) && tag[cursor] != '>' {
				cursor++
			}
			value = strings.TrimRight(tag[valueStart:cursor], "/")
		}
		if name == attr {
			return stdhtml.UnescapeString(strings.TrimSpace(value))
		}
	}
	return ""
}

func addHTMLAttribute(tag string, attr string, value string) string {
	attr = strings.TrimSpace(attr)
	value = stdhtml.EscapeString(strings.TrimSpace(value))
	if attr == "" || value == "" {
		return tag
	}
	insert := fmt.Sprintf(` %s="%s"`, attr, value)
	closeIndex := strings.LastIndex(tag, ">")
	if closeIndex < 0 {
		return tag + insert
	}
	prefix := tag[:closeIndex]
	suffix := tag[closeIndex:]
	if strings.HasSuffix(strings.TrimSpace(prefix), "/") {
		trimmed := strings.TrimRight(prefix, " \t\r\n")
		prefix = strings.TrimRight(trimmed[:len(trimmed)-1], " \t\r\n")
		return prefix + insert + " /" + suffix
	}
	return prefix + insert + suffix
}

func addHTMLClasses(tag string, classes ...string) string {
	existing := strings.Fields(getHTMLAttribute(tag, "class"))
	seen := make(map[string]bool, len(existing)+len(classes))
	merged := make([]string, 0, len(existing)+len(classes))
	for _, className := range append(existing, classes...) {
		className = strings.TrimSpace(className)
		if className == "" || seen[className] {
			continue
		}
		seen[className] = true
		merged = append(merged, className)
	}
	if len(merged) == 0 {
		return tag
	}
	classValue := strings.Join(merged, " ")
	if getHTMLAttribute(tag, "class") == "" {
		return addHTMLAttribute(tag, "class", classValue)
	}
	classAttr := regexp.MustCompile(`(?is)\sclass\s*=\s*("[^"]*"|'[^']*'|[^\s"'=<>` + "`" + `]+)`)
	replaced := false
	return classAttr.ReplaceAllStringFunc(tag, func(match string) string {
		if replaced {
			return match
		}
		replaced = true
		return ` class="` + stdhtml.EscapeString(classValue) + `"`
	})
}

func htmlClassSet(tag string) map[string]bool {
	fields := strings.Fields(getHTMLAttribute(tag, "class"))
	if len(fields) == 0 {
		return nil
	}
	classes := make(map[string]bool, len(fields))
	for _, className := range fields {
		className = strings.ToLower(strings.TrimSpace(className))
		if className != "" {
			classes[className] = true
		}
	}
	return classes
}

func hasHTMLClass(classes map[string]bool, className string) bool {
	return classes[strings.ToLower(strings.TrimSpace(className))]
}

func hasHTMLClassPrefix(classes map[string]bool, prefix string) bool {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return false
	}
	for className := range classes {
		if strings.HasPrefix(className, prefix) {
			return true
		}
	}
	return false
}

func normalizeHTMLAssetKey(raw string) string {
	value := strings.TrimSpace(stdhtml.UnescapeString(raw))
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "//") ||
		strings.HasPrefix(lower, "data:") ||
		strings.HasPrefix(lower, "mailto:") ||
		strings.HasPrefix(lower, "javascript:") ||
		strings.HasPrefix(value, "#") ||
		strings.HasPrefix(value, "/") {
		return ""
	}
	if index := strings.IndexAny(value, "?#"); index >= 0 {
		value = value[:index]
	}
	value = strings.TrimPrefix(strings.ReplaceAll(value, "\\", "/"), "./")
	if decoded, err := url.PathUnescape(value); err == nil {
		value = decoded
	}
	cleaned, ok := projectcore.CleanProjectPath(value)
	if !ok {
		return ""
	}
	return cleaned
}

func isHTMLAttrNameStart(ch byte) bool {
	return ch == '_' || ch == ':' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}

func isHTMLAttrNameChar(ch byte) bool {
	return isHTMLAttrNameStart(ch) || ch == '-' || ch == '.' || ch >= '0' && ch <= '9'
}

func isHTMLSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == '\f'
}

func (s *Server) requireServiceToken(w http.ResponseWriter, r *http.Request, requestID string) bool {
	expected := strings.TrimSpace(s.cfg.ServiceToken)
	if expected == "" {
		writeError(w, http.StatusServiceUnavailable, requestID, "RIN_RENDERER_SERVICE_TOKEN is required", nil)
		return false
	}

	actual := strings.TrimSpace(r.Header.Get("X-Rin-Renderer-Token"))
	if actual == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			actual = strings.TrimSpace(auth[len("Bearer "):])
		}
	}
	if actual != expected {
		writeError(w, http.StatusUnauthorized, requestID, "missing renderer service token", nil)
		return false
	}
	return true
}

func (s *Server) versions() Versions {
	return Versions{
		RinRenderer:    s.cfg.RendererVersion,
		FinalOutput:    finaloutput.ContractVersion,
		Typst:          s.cfg.TypstVersion,
		TypstProfile:   typstProfileID(s.cfg),
		LateXML:        s.cfg.LateXMLVersion,
		LaTeXMLAdapter: s.cfg.LaTeXMLAdapterVersion,
		Perl:           s.cfg.PerlVersion,
		TeXLive:        s.cfg.TeXLiveVersion,
		Dvisvgm:        s.cfg.DvisvgmVersion,
		DiagramEngine:  s.cfg.DiagramEngineVersion,
		MathJax:        s.cfg.MathJaxVersion,
		MathJaxOutput:  s.cfg.MathJaxOutput,
		MathJaxFont:    s.cfg.MathJaxFontVersion,
		KaTeX:          s.cfg.KaTeXVersion,
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, requestID string, message string, err error) {
	if err != nil {
		message = fmt.Sprintf("%s: %s", message, err.Error())
	}
	writeJSON(w, status, ErrorResponse{
		RequestID: requestID,
		Error:     message,
		Diagnostics: []Diagnostic{{
			Severity: "error",
			Code:     "renderer.request.invalid",
			Message:  message,
		}},
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "rnd_" + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("rnd_%d", time.Now().UnixNano())
}

func cleanDocumentEngine(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "latexml", "auto":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func cleanDiagramPolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "rin", "latexml", "auto":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "rin"
	}
}

func cleanMathPolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "server", "mathml-debug", "source-debug":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "server"
	}
}

func cleanMathEngine(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "katex", "katex-legacy":
		return "katex"
	case "", "mathjax", "mathjax-chtml":
		return "mathjax-chtml"
	default:
		return "mathjax-chtml"
	}
}

func cleanMathOutputStrategy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "chtml", "mathjax", "mathjax-chtml":
		return "chtml"
	case "complex-svg", "hybrid-svg", "texsvg-complex":
		return "complex-svg"
	case "display-svg", "texsvg-display":
		return "display-svg"
	case "svg", "texsvg":
		return "svg"
	default:
		return "chtml"
	}
}

func cleanProjectPath(value string) string {
	cleaned, ok := projectcore.CleanProjectPath(value)
	if !ok {
		return ""
	}
	return cleaned
}

func supportedDiagramTypes() []string {
	return diagramservice.SupportedTypes()
}

func projectLimitsFromConfig(cfg Config) projectcore.Limits {
	return projectcore.Limits{
		ArchiveMaxBytes: cfg.ProjectArchiveMaxBytes,
		FileMaxCount:    cfg.ProjectFileMaxCount,
		FileMaxBytes:    cfg.ProjectFileMaxBytes,
	}
}

func diagnosticsFromProject(items []projectcore.Diagnostic) []Diagnostic {
	diagnostics := make([]Diagnostic, 0, len(items))
	for _, item := range items {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: item.Severity,
			Code:     item.Code,
			Message:  item.Message,
			Source:   item.Source,
		})
	}
	return diagnostics
}

func diagnosticsFromLateXML(items []latexmladapter.Diagnostic) []Diagnostic {
	diagnostics := make([]Diagnostic, 0, len(items))
	for _, item := range items {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: firstNonEmpty(item.Severity, "info"),
			Code:     firstNonEmpty(item.Code, "latexml.message"),
			Message:  item.Message,
			Engine:   "latexml",
			Source:   item.Source,
		})
	}
	return diagnostics
}

func diagnosticsWithFallbackError(items []Diagnostic, code string, message string, engine string) []Diagnostic {
	for _, item := range items {
		if item.Severity == "error" {
			return items
		}
	}
	return append(items, Diagnostic{
		Severity: "error",
		Code:     code,
		Message:  message,
		Engine:   engine,
	})
}

func assetsFromProjectFiles(files []projectcore.File) []AssetRef {
	assets := make([]AssetRef, 0)
	for _, file := range files {
		if file.Kind != "asset" {
			continue
		}
		assets = append(assets, AssetRef{
			Path: file.Path,
			MIME: file.MIME,
		})
	}
	return assets
}

func assetFilesFromProjectFiles(files []projectcore.File, manifest projectcore.Manifest) []AssetFile {
	referenced := make(map[string]bool)
	for _, asset := range manifest.AssetInventory.Assets {
		referenced[asset.Path] = asset.Referenced
	}
	assets := make([]AssetFile, 0)
	for _, file := range files {
		if file.Kind != "asset" {
			continue
		}
		var referencedPtr *bool
		if value, ok := referenced[file.Path]; ok {
			valueCopy := value
			referencedPtr = &valueCopy
		}
		assets = append(assets, AssetFile{
			Path:       file.Path,
			Filename:   path.Base(file.Path),
			MIME:       file.MIME,
			Encoding:   file.Encoding,
			Body:       file.Body,
			Bytes:      file.Bytes,
			Referenced: referencedPtr,
		})
	}
	return assets
}

func readerPayload(title string, html string) map[string]any {
	title = firstNonEmpty(title, "Untitled")
	html = strings.TrimSpace(html)
	if html == "" {
		return map[string]any{
			"version": "0.1",
			"title":   title,
			"toc":     []any{},
			"pages":   []any{},
		}
	}
	readerHTML := prepareLateXMLReaderHTML(unwrapSingleReaderArticle(html))
	readerHTML, headings := normalizeReaderHeadingHTML(readerHTML)
	if len(headings) > 0 {
		toc := make([]map[string]any, 0, len(headings))
		for _, heading := range headings {
			toc = append(toc, map[string]any{
				"id":    heading.ID,
				"text":  heading.Text,
				"level": heading.Level,
			})
		}
		pageHeadings := readerPageHeadings(headings)
		pages := make([]map[string]any, 0, len(pageHeadings))
		for index, heading := range pageHeadings {
			start := heading.Start
			if index == 0 {
				start = 0
			}
			end := len(readerHTML)
			if index+1 < len(pageHeadings) {
				end = pageHeadings[index+1].Start
			}
			if start < 0 || end < start || end > len(readerHTML) {
				continue
			}
			pageHTML := strings.TrimSpace(readerHTML[start:end])
			if pageHTML == "" {
				continue
			}
			pages = append(pages, map[string]any{
				"id":    heading.ID,
				"text":  heading.Text,
				"level": heading.Level,
				"html":  pageHTML,
			})
		}
		if len(pages) > 0 {
			return map[string]any{
				"version": "latexml-0.1",
				"title":   title,
				"toc":     toc,
				"pages":   pages,
			}
		}
	}
	return map[string]any{
		"version": "latexml-0.1",
		"title":   title,
		"toc": []map[string]any{{
			"id":    "main",
			"text":  title,
			"level": 1,
		}},
		"pages": []map[string]any{{
			"id":    "main",
			"text":  title,
			"level": 1,
			"html":  readerHTML,
		}},
	}
}

func prepareLateXMLReaderHTML(html string) string {
	return strings.TrimSpace(html)
}

func removeLateXMLGeneratedDocumentChrome(html string) string {
	html = removeHTMLBlocksByClass(html, "div", "ltx_TOC")
	html = removeHTMLBlocksByClass(html, "div", "ltx_authors")
	html = removeHTMLBlocksByClass(html, "div", "ltx_dates")
	html = removeHTMLBlocksByClass(html, "h1", "ltx_title_document")
	return html
}

func removeHTMLBlocksByClass(htmlText string, tag string, className string) string {
	if strings.TrimSpace(htmlText) == "" || !strings.Contains(strings.ToLower(htmlText), strings.ToLower(className)) {
		return htmlText
	}
	var out strings.Builder
	cursor := 0
	for cursor < len(htmlText) {
		start := nextHTMLTagStart(htmlText, tag, cursor, false)
		if start < 0 {
			break
		}
		openEnd := strings.IndexByte(htmlText[start:], '>')
		if openEnd < 0 {
			break
		}
		openEnd += start
		openTag := htmlText[start : openEnd+1]
		if !hasHTMLClass(htmlClassSet(openTag), className) {
			out.WriteString(htmlText[cursor : openEnd+1])
			cursor = openEnd + 1
			continue
		}
		end := matchingHTMLContainerEnd(htmlText, tag, start)
		if end < 0 {
			break
		}
		out.WriteString(htmlText[cursor:start])
		cursor = end
	}
	out.WriteString(htmlText[cursor:])
	return out.String()
}

func unwrapSingleReaderArticle(html string) string {
	trimmed := strings.TrimSpace(html)
	groups := regexp.MustCompile(`(?is)^<article\b[^>]*>(.*)</article>$`).FindStringSubmatch(trimmed)
	if len(groups) < 2 {
		return trimmed
	}
	return strings.TrimSpace(groups[1])
}

func readerPageHeadings(headings []readerHeading) []readerHeading {
	hasChapter := false
	hasSection := false
	for _, heading := range headings {
		hasChapter = hasChapter || heading.Kind == "chapter"
		hasSection = hasSection || heading.Kind == "section"
	}
	if hasChapter && hasSection {
		pages := make([]readerHeading, 0, len(headings))
		var chapter *readerHeading
		for _, heading := range headings {
			switch {
			case heading.Kind == "chapter":
				if chapter != nil {
					pages = append(pages, *chapter)
				}
				pending := heading
				chapter = &pending
			case heading.Kind == "section":
				if chapter != nil {
					// The first section owns its chapter heading and introduction.
					heading.Start = chapter.Start
					chapter = nil
				}
				pages = append(pages, heading)
			case heading.Level == 2:
				if chapter != nil {
					pages = append(pages, *chapter)
					chapter = nil
				}
				// Unsectioned front/back matter needs its own page too.
				pages = append(pages, heading)
			}
		}
		if chapter != nil {
			pages = append(pages, *chapter)
		}
		return pages
	}
	pages := make([]readerHeading, 0, len(headings))
	for _, heading := range headings {
		if heading.Level == 2 {
			pages = append(pages, heading)
		}
	}
	if len(pages) > 0 {
		return pages
	}
	return headings
}

func (s *Server) lateXMLAdapter() latexmladapter.Adapter {
	return latexmladapter.Adapter{
		Config: latexmladapter.Config{
			Binary:        s.cfg.LateXMLBin,
			Endpoint:      s.cfg.LateXMLWorkerEndpoint,
			Token:         s.cfg.LateXMLWorkerToken,
			Profile:       s.cfg.LateXMLProfile,
			IncludeStyles: s.cfg.LateXMLIncludeStyles,
			Timeout:       s.cfg.RenderTimeout,
			MaxHTMLBytes:  s.cfg.LateXMLMaxHTMLBytes,
		},
	}
}

func (s *Server) diagramWorker() diagramengine.HTTPWorker {
	return diagramengine.HTTPWorker{
		Endpoint: s.cfg.DiagramWorkerEndpoint,
		Token:    s.cfg.DiagramWorkerToken,
	}
}

func (s *Server) diagramStorage() diagramservice.PublicStore {
	if s.localAssets != nil {
		return s.localAssets
	}
	return renderstorage.CloudBaseClient{
		EnvID:         s.cfg.StorageEnvID,
		AccessToken:   s.cfg.StorageAccessToken,
		Bucket:        s.cfg.StorageBucket,
		BaseURL:       s.cfg.StorageBaseURL,
		PublicBaseURL: s.cfg.StoragePublicBaseURL,
	}
}

func (s *Server) storageInfo() StorageInfo {
	if s.localAssets != nil {
		return StorageInfo{Provider: "local", BaseURL: s.cfg.LocalPublicBaseURL}
	}
	return StorageInfo{Provider: "cloudbase", Bucket: s.cfg.StorageBucket, BaseURL: firstNonEmpty(s.cfg.StoragePublicBaseURL, s.cfg.StorageBaseURL)}
}

func diagnosticsFromDiagramWorker(items []diagramengine.Diagnostic) []Diagnostic {
	diagnostics := make([]Diagnostic, 0, len(items))
	for _, item := range items {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: firstNonEmpty(item.Severity, "info"),
			Code:     firstNonEmpty(item.Code, "diagram.worker.message"),
			Message:  item.Message,
			Engine:   "rin-texsvg",
			Source:   item.Source,
		})
	}
	return diagnostics
}

func cleanProjectStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "published", "published-preview":
		return "published-preview"
	default:
		return "draft"
	}
}

func cleanLegacyRenderer(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "mathjax":
		return "mathjax"
	default:
		return "katex"
	}
}

func metadataPathFromForm(r *http.Request, keys ...string) string {
	for _, field := range []string{"project", "metadata"} {
		if value := metadataStringValue(r.FormValue(field), keys...); value != "" {
			return value
		}
	}
	return ""
}

func metadataStringValue(raw string, keys ...string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	if value := stringValueForKeys(payload, keys...); value != "" {
		return value
	}
	if nested, ok := payload["project"].(map[string]any); ok {
		return stringValueForKeys(nested, keys...)
	}
	return ""
}

func stringValueForKeys(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizeDiagramType(value string) (string, bool) {
	return diagramservice.NormalizeType(value)
}
