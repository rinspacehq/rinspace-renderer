package renderapi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/codeservice"
	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/markdownadapter"
	"github.com/rinspacehq/rinspace-renderer/api/internal/nodeworker"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
)

type markdownProjectExecutor struct {
	cfg     Config
	server  *Server
	worker  *nodeworker.Supervisor
	adapter markdownadapter.Adapter
	code    *codeservice.Service
}

type markdownCodeRenderer struct {
	worker *nodeworker.Supervisor
}

type markdownShikiRequest struct {
	ContractVersion string                      `json:"contractVersion"`
	Theme           string                      `json:"theme"`
	Items           []codeservice.RenderRequest `json:"items"`
}

type markdownShikiResponse struct {
	ContractVersion string                     `json:"contractVersion"`
	Engine          string                     `json:"engine"`
	EngineVersion   string                     `json:"engineVersion"`
	Theme           string                     `json:"theme"`
	Items           []codeservice.RenderResult `json:"items"`
}

type markdownProjectFileReader struct {
	files map[string][]byte
}

func newMarkdownProjectExecutor(cfg Config, server *Server, cache orchestration.Cache, store orchestration.ArtifactStore) (*markdownProjectExecutor, error) {
	if server == nil {
		return nil, errors.New("Markdown executor requires shared Renderer services")
	}
	scriptPath, err := filepath.Abs(cfg.MarkdownScript)
	if err != nil {
		return nil, fmt.Errorf("resolve Markdown worker script: %w", err)
	}
	worker, err := nodeworker.New(nodeworker.Config{
		Command: cfg.MarkdownNodeBin, Args: []string{scriptPath, "--ndjson-worker"},
		Dir: filepath.Dir(scriptPath), Workers: cfg.MarkdownWorkerCount,
		MaxRequestBytes: cfg.MarkdownMaxRequestBytes, MaxResponseBytes: cfg.MarkdownMaxResponseBytes,
		MaxTasks: cfg.MarkdownMaxTasks, MaxRSSBytes: cfg.MarkdownMaxRSSBytes,
		StartTimeout: cfg.MarkdownStartTimeout, StopGrace: cfg.MarkdownStopGrace,
		Environment: markdownWorkerEnvironment(cfg),
	})
	if err != nil {
		return nil, err
	}
	code, err := codeservice.New(codeservice.Config{
		Renderer: markdownCodeRenderer{worker: worker}, Cache: cache, Store: store,
		EngineVersion: codeservice.ShikiVersion, Theme: codeservice.DefaultTheme,
	})
	if err != nil {
		_ = worker.Close()
		return nil, err
	}
	return &markdownProjectExecutor{
		cfg: cfg, server: server, worker: worker, code: code,
		adapter: markdownadapter.Adapter{
			Worker: worker, MaxFileBytes: cfg.ProjectFileMaxBytes, Cache: cache, Store: store,
			MathFontVersion: cfg.MathJaxFontVersion, BookCacheCompatibility: "rin-markdown-book-cache-compat/v1",
		},
	}, nil
}

func markdownWorkerEnvironment(cfg Config) map[string]string {
	return map[string]string{
		"RIN_NODE_WORKER_MAX_REQUEST_BYTES":  strconv.FormatInt(cfg.MarkdownMaxRequestBytes, 10),
		"RIN_NODE_WORKER_MAX_RESPONSE_BYTES": strconv.FormatInt(cfg.MarkdownMaxResponseBytes, 10),
	}
}

func (executor *markdownProjectExecutor) Execute(ctx context.Context, request ProjectExecutionRequest) ProjectExecutionResult {
	mode := strings.TrimSpace(request.DocumentMode)
	if mode == "" {
		mode = "article"
	}
	var project projectidentity.Project
	var err error
	if mode == "book" {
		project, err = projectidentity.ImportMarkdownBook(request.ArchiveName, request.Archive, request.Title,
			request.MarkdownBookPages, projectLimitsFromConfig(executor.cfg))
	} else if mode == "article" && len(request.MarkdownBookPages) == 0 {
		project, err = projectidentity.Import(request.ArchiveName, request.Archive, contracts.ContentKindMarkdown,
			request.ExplicitMainFile, projectLimitsFromConfig(executor.cfg))
	} else {
		err = errors.New("unsupported Markdown document mode or Book page manifest")
	}
	if err != nil {
		return markdownExecutionFailure("markdown.project.invalid", "invalid Markdown project archive", err)
	}
	project.Graph.Options["documentMode"] = mode
	if title := strings.TrimSpace(request.Title); title != "" {
		project.Graph.Options["title"] = title
	}
	files := make(map[string][]byte, len(project.Files))
	for _, file := range project.Files {
		body, readErr := projectidentity.FileBytes(file)
		if readErr != nil {
			return markdownExecutionFailure("markdown.project.invalid", "invalid Markdown project file", readErr)
		}
		files[file.Path] = body
	}
	snapshot := orchestration.ProjectSnapshot{Graph: project.Graph, Files: markdownProjectFileReader{files: files}}
	analysis, err := executor.adapter.Analyze(ctx, snapshot, orchestration.RenderOptions{Mode: mode, Status: request.ProjectStatus})
	if err != nil {
		return markdownExecutionFailure("markdown.analyze.failed", "Markdown analysis failed", err)
	}
	draft, err := executor.adapter.Compile(ctx, snapshot, analysis)
	if err != nil {
		return markdownExecutionFailure("markdown.compile.failed", "Markdown compilation failed", err)
	}
	math, err := executor.server.sharedMathService(request.RequestID, newMathRenderCache())
	if err != nil {
		return markdownExecutionFailure("markdown.math.unavailable", "shared math service is unavailable", err)
	}
	macroHash := orchestration.ComputeMathMacroContextHash(nil)
	resolver := orchestration.WorkResolver{Math: math, Diagrams: executor.server.diagrams, Code: executor.code}
	resolved, err := resolver.Resolve(ctx, draft, orchestration.WorkResolutionOptions{
		MacroContexts: map[string]map[string]string{macroHash: {}},
		MathStrategy:  orchestration.MathOutputCHTML, CodeTheme: codeservice.DefaultTheme,
	})
	if err != nil {
		return markdownExecutionFailure("markdown.resolve.failed", "Markdown shared work resolution failed", err)
	}
	finalization, err := executor.adapter.FinalizeDetailedWithBookReuse(ctx, draft, resolved, !request.DisableMarkdownBookReuse)
	if err != nil {
		return markdownExecutionFailure("markdown.finalize.failed", "Markdown finalization failed", err)
	}
	finalBundle := finalization.Bundle
	artifacts := markdownResolvedArtifacts(resolved)
	promotedAssets, err := executor.promoteRepositoryAssets(ctx, request, &finalBundle, files)
	if err != nil {
		return markdownExecutionFailure("markdown.assets.failed", "Markdown repository asset promotion failed", err)
	}
	artifacts = uniqueArtifactReferences(append(artifacts, promotedAssets...))
	versions := map[string]string{
		"rinRenderer":              executor.cfg.RendererVersion,
		"markdownAdapter":          finalBundle.Provenance.AdapterVersion,
		"markdownPipeline":         finalBundle.Provenance.EngineVersion,
		"markdownSanitizer":        "rin-markdown-final-sanitizer/v2",
		"shiki":                    codeservice.ShikiVersion,
		"mathJax":                  executor.cfg.MathJaxVersion,
		"mathJaxFont":              executor.cfg.MathJaxFontVersion,
		"diagramEngine":            executor.cfg.DiagramEngineVersion,
		"markdownCacheDecision":    finalization.CacheDecision,
		"markdownRepositoryAssets": markdownRepositoryAssetPromotionVersion,
	}
	reusedStages := []string{}
	if len(finalization.ReusedPageIDs) > 0 {
		reusedStages = append(reusedStages, string(orchestration.CacheStagePageFinalizer))
	}
	result := contracts.RenderResult{
		SchemaVersion: contracts.RenderResultSchemaVersion, JobID: request.JobID, RequestID: request.RequestID,
		ProjectHash: finalBundle.ProjectHash, ResultHash: finalBundle.BundleHash,
		ContentKind: contracts.ContentKindMarkdown, Engine: markdownadapter.Engine,
		Inline: &finalBundle, Assets: artifacts, Diagnostics: append([]contracts.Diagnostic{}, finalBundle.Diagnostics...),
		Versions: versions, Cache: contracts.RenderCacheSummary{Hit: len(reusedStages) > 0, ReusedStages: reusedStages},
	}
	if err := result.Validate(); err != nil {
		return markdownExecutionFailure("markdown.result.invalid", "Markdown result contract is invalid", err)
	}
	page := finalBundle.Pages[0]
	readerPages := make([]map[string]any, 0, len(finalBundle.Pages))
	for _, item := range finalBundle.Pages {
		readerPages = append(readerPages, map[string]any{"id": item.ID, "sourcePath": item.SourcePath, "title": item.Title})
	}
	projectFiles := make([]map[string]any, 0, len(project.Graph.Files))
	for _, item := range project.Graph.Files {
		projectFiles = append(projectFiles, map[string]any{"path": item.Path, "sha256": item.SHA256, "bytes": item.Bytes})
	}
	response := ProjectRenderResponse{
		RequestID: request.RequestID, Title: finalBundle.Title, HTML: page.Fragment,
		Engine: markdownadapter.Engine, MainFile: page.SourcePath,
		GeneratedArtifacts: artifacts, Assets: []AssetRef{}, AssetFiles: []AssetFile{},
		Diagrams: []DiagramRef{}, Reader: map[string]any{"documentMode": mode, "pages": readerPages},
		Project: map[string]any{"files": projectFiles}, Diagnostics: markdownResponseDiagnostics(finalBundle.Diagnostics),
		Versions: Versions{RinRenderer: executor.cfg.RendererVersion, FinalOutput: "rin-final-output/v1",
			MathJax: executor.cfg.MathJaxVersion, MathJaxFont: executor.cfg.MathJaxFontVersion,
			DiagramEngine: executor.cfg.DiagramEngineVersion},
	}
	return ProjectExecutionResult{Response: response, Canonical: &result, Status: 200}
}

func (executor *markdownProjectExecutor) Close() error {
	if executor == nil || executor.worker == nil {
		return nil
	}
	return executor.worker.Close()
}

func (renderer markdownCodeRenderer) RenderBatch(ctx context.Context, theme string, requests []codeservice.RenderRequest) ([]codeservice.RenderResult, error) {
	if renderer.worker == nil {
		return nil, errors.New("Markdown worker is unavailable")
	}
	var response markdownShikiResponse
	if err := renderer.worker.Do(ctx, "shiki.render-batch", markdownShikiRequest{
		ContractVersion: "rin-shiki-batch/v1", Theme: theme, Items: requests,
	}, &response); err != nil {
		return nil, err
	}
	if response.ContractVersion != "rin-shiki-result/v1" || response.Engine != "shiki" ||
		response.EngineVersion != codeservice.ShikiVersion || response.Theme != theme || len(response.Items) != len(requests) {
		return nil, errors.New("Markdown worker returned an invalid Shiki result")
	}
	return response.Items, nil
}

func (reader markdownProjectFileReader) ReadFile(_ context.Context, path string, maxBytes int64) ([]byte, error) {
	body, ok := reader.files[path]
	if !ok {
		return nil, errors.New("project file not found")
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("project file exceeds read limit")
	}
	return append([]byte(nil), body...), nil
}

func markdownResolvedArtifacts(resolved orchestration.ResolvedWork) []contracts.ArtifactReference {
	byID := map[string]contracts.ArtifactReference{}
	for _, unit := range resolved.Units {
		if unit.Artifact != nil {
			byID[unit.Artifact.ArtifactID] = *unit.Artifact
		}
	}
	result := make([]contracts.ArtifactReference, 0, len(byID))
	for _, artifact := range byID {
		result = append(result, artifact)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ArtifactID < result[j].ArtifactID })
	return result
}

func markdownResponseDiagnostics(items []contracts.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, 0, len(items))
	for _, item := range items {
		result = append(result, Diagnostic{Severity: item.Severity, Code: item.Code, Message: item.Message, Engine: item.Stage})
	}
	return result
}

func markdownExecutionFailure(code string, message string, err error) ProjectExecutionResult {
	diagnosticCode := code
	diagnosticMessage := message
	var remote *nodeworker.RemoteError
	if errors.As(err, &remote) && strings.HasPrefix(remote.Code, "markdown.") {
		diagnosticCode = remote.Code
		diagnosticMessage = boundedWorkerDiagnosticMessage(remote.Message, message)
	} else if code == "markdown.finalize.failed" && strings.Contains(err.Error(), "Markdown Book") {
		diagnosticCode = "markdown.book_cache.failed"
		diagnosticMessage = boundedWorkerDiagnosticMessage(err.Error(), message)
	} else if code == "markdown.finalize.failed" && strings.HasPrefix(err.Error(), "validate final Markdown bundle:") {
		diagnosticCode = "markdown.bundle_contract.failed"
		diagnosticMessage = boundedWorkerDiagnosticMessage(err.Error(), message)
	} else if code == "markdown.finalize.failed" && strings.HasPrefix(err.Error(), "finalize Markdown:") {
		diagnosticCode = "markdown.worker_contract.failed"
		diagnosticMessage = boundedWorkerDiagnosticMessage(err.Error(), message)
	} else if code == "markdown.finalize.failed" && err.Error() == "Markdown finalizer returned a non-publishable result" {
		diagnosticCode = "markdown.worker_contract.failed"
		diagnosticMessage = err.Error()
	} else if code == "markdown.assets.failed" {
		var assetFailure *markdownRepositoryAssetFailure
		if errors.As(err, &assetFailure) {
			diagnosticCode = assetFailure.Code
			diagnosticMessage = assetFailure.Message
		}
	}
	return ProjectExecutionResult{
		Status: 422, Message: message, Err: fmt.Errorf("%s: %w", code, err),
		Diagnostics: []Diagnostic{{Severity: "error", Code: diagnosticCode, Message: diagnosticMessage, Engine: "rin-markdown"}},
	}
}

func boundedWorkerDiagnosticMessage(value string, fallback string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return fallback
	}
	characters := []rune(value)
	if len(characters) > 256 {
		characters = characters[:256]
	}
	return string(characters)
}

var _ codeservice.Renderer = markdownCodeRenderer{}
var _ orchestration.ProjectFileReader = markdownProjectFileReader{}
