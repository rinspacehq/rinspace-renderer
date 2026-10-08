package renderapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
	"github.com/rinspacehq/rinspace-renderer/api/internal/knowledgeindex"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstadapter"
)

// typstProjectExecutor renders a committed Typst project into the shared
// semantic HTML reader bundle. It runs the pinned compiler directly; there is
// no TeX toolchain and no network access in this path, and an incomplete
// compiler/sanitizer profile fails closed instead of reporting success.
type typstProjectExecutor struct {
	cfg    Config
	server *Server
}

func newTypstProjectExecutor(cfg Config, server *Server) *typstProjectExecutor {
	return &typstProjectExecutor{cfg: cfg, server: server}
}

func (executor *typstProjectExecutor) compiler() typstadapter.Compiler {
	return typstadapter.Compiler{
		BinaryPath: strings.TrimSpace(executor.cfg.TypstBin),
		BinaryHash: strings.ToLower(strings.TrimSpace(executor.cfg.TypstBinHash)),
		FontPath:   strings.TrimSpace(executor.cfg.TypstFontPath),
		FontHash:   strings.ToLower(strings.TrimSpace(executor.cfg.TypstFontHash)),
		Timeout:    executor.cfg.TypstTimeout,
		MaxOutput:  executor.cfg.TypstMaxOutputBytes,
	}
}

// Configured reports whether a complete pinned Typst compiler profile is
// present. An incomplete profile must never be reported as a successful HTML
// publication.
func (executor *typstProjectExecutor) Configured() bool {
	cfg := executor.cfg
	return strings.TrimSpace(cfg.TypstBin) != "" &&
		len(strings.ToLower(strings.TrimSpace(cfg.TypstBinHash))) == 64 &&
		strings.TrimSpace(cfg.TypstFontPath) != "" &&
		len(strings.ToLower(strings.TrimSpace(cfg.TypstFontHash))) == 64 &&
		len(strings.ToLower(strings.TrimSpace(cfg.TypstPackageHash))) == 64 &&
		strings.TrimSpace(cfg.TypstVersion) != "" &&
		cfg.TypstTimeout > 0 && cfg.TypstMaxOutputBytes > 0
}

func (executor *typstProjectExecutor) profile(mathPresentation string) typstadapter.Profile {
	return typstadapter.Profile{
		CompilerVersion:    strings.TrimSpace(executor.cfg.TypstVersion),
		CompilerHash:       strings.ToLower(strings.TrimSpace(executor.cfg.TypstBinHash)),
		FontHash:           strings.ToLower(strings.TrimSpace(executor.cfg.TypstFontHash)),
		PackageHash:        strings.ToLower(strings.TrimSpace(executor.cfg.TypstPackageHash)),
		AdapterVersion:     typstadapter.AdapterVersion,
		FinalOutputVersion: finaloutput.ContractVersion,
		MathPresentation:   mathPresentation,
	}
}

func (executor *typstProjectExecutor) Execute(ctx context.Context, request ProjectExecutionRequest) ProjectExecutionResult {
	mode := strings.TrimSpace(request.DocumentMode)
	if mode == "" {
		mode = "article"
	}
	if mode != "article" && mode != "book" {
		return typstExecutionFailure("typst.project.invalid", "unsupported Typst document mode", errors.New("invalid document mode"))
	}
	if !executor.Configured() {
		return typstExecutionFailure("typst.unavailable", "Typst HTML rendering is not configured", errors.New("pinned Typst compiler profile is incomplete"))
	}
	project, err := projectidentity.Import(request.ArchiveName, request.Archive, contracts.ContentKindTypst, request.ExplicitMainFile, projectLimitsFromConfig(executor.cfg))
	if err != nil {
		return typstExecutionFailure("typst.project.invalid", "invalid Typst project archive", err)
	}
	files := make(map[string][]byte, len(project.Files))
	for _, file := range project.Files {
		body, readErr := projectidentity.FileBytes(file)
		if readErr != nil {
			return typstExecutionFailure("typst.project.invalid", "invalid Typst project file", readErr)
		}
		files[file.Path] = body
	}
	entrypoint := project.Graph.Entrypoints[0].Path
	compiled, err := executor.compiler().Compile(ctx, files, entrypoint, "html")
	if err != nil {
		return typstCompileFailure(ctx, err, compiled.Log)
	}
	adapted, err := typstadapter.AdaptHTML(compiled.Output, mode, files)
	if err != nil {
		return typstAdaptFailure(err)
	}
	mathPresentation := typstMathPresentation(executor.cfg)
	if executor.cfg.TypstMathRender {
		adapted, err = typstadapter.RenderMathML(ctx, adapted, executor.server.primaryMathRenderer())
		if err != nil {
			return typstExecutionFailure("typst.math.failed", "Typst MathML presentation failed", err)
		}
	}
	knowledge, failure := executor.knowledgeIndex(request, project)
	if failure != nil {
		return *failure
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = projectcore.TitleFromArchiveName(request.ArchiveName)
	}
	return typstAdaptedResult(typstAdaptedInput{
		JobID: request.JobID, RequestID: request.RequestID,
		ProjectHash: firstNonEmpty(request.ControlProjectHash, project.Graph.ProjectHash),
		Entrypoint:  entrypoint, Title: title, Mode: mode,
		Profile:         executor.profile(mathPresentation),
		RendererVersion: executor.cfg.RendererVersion, CompilerVersion: executor.cfg.TypstVersion,
		SourceCommit: request.SourceCommit, ProjectID: request.ProjectID,
		Adapted: adapted, ProjectFiles: project.Graph.Files, Knowledge: knowledge,
	})
}

// knowledgeIndex extracts the reader search index only when the Control Plane
// supplied a complete publication identity. A partial identity is a hard
// failure rather than a silently unindexed publication.
func (executor *typstProjectExecutor) knowledgeIndex(request ProjectExecutionRequest, project projectidentity.Project) (*contracts.KnowledgeIndex, *ProjectExecutionResult) {
	if request.ProjectID == "" && request.SourceCommit == "" && request.ControlProjectHash == "" {
		return nil, nil
	}
	if request.ProjectID == "" || request.SourceCommit == "" || request.ControlProjectHash == "" {
		failure := typstExecutionFailure("typst.identity.invalid", "Typst publication identity is incomplete", errors.New("knowledge index publication identity is incomplete"))
		return nil, &failure
	}
	index, err := knowledgeindex.Extract(project.Files, knowledgeindex.Identity{
		ProjectID: request.ProjectID, SourceCommit: request.SourceCommit, ProjectHash: request.ControlProjectHash,
	})
	if err != nil {
		code := "knowledge_index.invalid_source"
		var extractionErr *knowledgeindex.Error
		if errors.As(err, &extractionErr) {
			code = extractionErr.Code
		}
		failure := typstExecutionFailure(code, "Typst knowledge index extraction failed", err)
		return nil, &failure
	}
	return &index, nil
}

type typstAdaptedInput struct {
	JobID           string
	RequestID       string
	ProjectHash     string
	Entrypoint      string
	Title           string
	Mode            string
	Profile         typstadapter.Profile
	RendererVersion string
	CompilerVersion string
	SourceCommit    string
	ProjectID       string
	Adapted         typstadapter.Result
	ProjectFiles    []contracts.ProjectGraphFile
	Knowledge       *contracts.KnowledgeIndex
}

// typstAdaptedResult turns one sanitized adapter result into the shared v1
// semantic reader bundle. It claims no v2 annotation blocks and carries the
// compiler/font/adapter profile so cache and publication decisions can reject
// stale results.
func typstAdaptedResult(input typstAdaptedInput) ProjectExecutionResult {
	readerPages := make([]map[string]any, 0, len(input.Adapted.Pages))
	toc := make([]map[string]any, 0, len(input.Adapted.Pages))
	fragments := make([]string, 0, len(input.Adapted.Pages))
	for _, page := range input.Adapted.Pages {
		fragments = append(fragments, page.HTML)
		if input.Mode == "book" {
			readerPages = append(readerPages, map[string]any{"id": page.ID, "text": page.Title, "level": page.Level, "html": page.HTML})
			if page.Title != "" && page.Level > 0 {
				toc = append(toc, map[string]any{"id": page.ID, "text": page.Title, "level": page.Level})
			}
		}
	}
	files := make([]AssetFile, 0, len(input.Adapted.Assets))
	for _, asset := range input.Adapted.Assets {
		files = append(files, AssetFile{
			Path: asset.Path, Filename: asset.Path, MIME: "image/svg+xml",
			Encoding: "base64", Body: base64.StdEncoding.EncodeToString(asset.Body), Bytes: int64(len(asset.Body)),
		})
	}
	projectFiles := make([]map[string]any, 0, len(input.ProjectFiles))
	for _, item := range input.ProjectFiles {
		projectFiles = append(projectFiles, map[string]any{"path": item.Path, "sha256": item.SHA256, "bytes": item.Bytes})
	}
	response := ProjectRenderResponse{
		RequestID: input.RequestID, Title: input.Title, HTML: strings.Join(fragments, ""), Engine: "typst",
		MainFile: input.Entrypoint, DocumentMode: input.Mode,
		Reader: map[string]any{"toc": toc, "pages": readerPages},
		Assets: []AssetRef{}, AssetFiles: files, Diagrams: []DiagramRef{}, Diagnostics: []Diagnostic{},
		Project:            map[string]any{"files": projectFiles, "sourceCommit": input.SourceCommit},
		KnowledgeIndex:     input.Knowledge,
		GeneratedArtifacts: []contracts.ArtifactReference{},
		Versions: Versions{
			RinRenderer: input.RendererVersion, Typst: input.CompilerVersion,
			TypstProfile: input.Profile.ID(), FinalOutput: finaloutput.ContractVersion,
		},
	}
	canonical, err := CanonicalResult(input.JobID, input.ProjectHash, string(contracts.ContentKindTypst), response)
	if err != nil {
		return typstExecutionFailure("typst.result.invalid", "Typst result contract is invalid", err)
	}
	return ProjectExecutionResult{Response: response, Canonical: &canonical, Status: http.StatusOK}
}

// typstCompileFailure surfaces the pinned compiler log as a bounded diagnostic
// so authors can locate the error, without leaking host paths or credentials.
func typstCompileFailure(ctx context.Context, err error, log string) ProjectExecutionResult {
	code := "typst.compile.failed"
	message := "Typst HTML compilation failed"
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		code = "typst.compile.timeout"
		message = "Typst HTML compilation timed out or was canceled"
	}
	detail := boundedDiagnostic(log)
	if detail == "" {
		detail = boundedDiagnostic(err.Error())
	}
	return ProjectExecutionResult{
		Status: http.StatusUnprocessableEntity, Message: message,
		Err:         errors.New(code + ": " + detail),
		Diagnostics: []Diagnostic{{Severity: "error", Code: code, Message: detail, Engine: "typst"}},
	}
}

func typstExecutionFailure(code string, message string, err error) ProjectExecutionResult {
	detail := boundedDiagnostic(err.Error())
	return ProjectExecutionResult{
		Status: http.StatusUnprocessableEntity, Message: message,
		Err:         errors.New(code + ": " + detail),
		Diagnostics: []Diagnostic{{Severity: "error", Code: code, Message: detail, Engine: "typst"}},
	}
}

// typstAdaptFailure reports a rejected HTML output. Book structure problems keep
// their own code so authors can tell a dangling cross-chapter reference apart
// from a sanitizer rejection.
func typstAdaptFailure(err error) ProjectExecutionResult {
	var bookErr *typstadapter.BookError
	if errors.As(err, &bookErr) {
		return typstExecutionFailure(bookErr.Code, "Typst book structure was rejected", err)
	}
	return typstExecutionFailure("typst.html.invalid", "Typst HTML output was rejected by the sanitizer", err)
}

func boundedDiagnostic(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	characters := []rune(value)
	if len(characters) > 512 {
		characters = characters[:512]
	}
	return string(characters)
}

// typstProfileConfigured reports whether the pinned Typst compiler profile is
// complete enough to advertise and run Typst HTML rendering.
func typstProfileConfigured(cfg Config) bool {
	executor := typstProjectExecutor{cfg: cfg}
	return executor.Configured()
}

// typstProfileID is the profile identifier advertised for the configured
// compiler/font/adapter combination. It is empty when no compiler is pinned so
// capabilities never claim a profile that cannot run.
func typstProfileID(cfg Config) string {
	if !typstProfileConfigured(cfg) {
		return ""
	}
	executor := typstProjectExecutor{cfg: cfg}
	return executor.profile(typstMathPresentation(cfg)).ID()
}

// typstMathPresentation is the math presentation a render under cfg records in
// its profile. The advertised profile id is derived from the same value, so a
// job admitted for the advertised profile can only publish a bundle that carries
// that exact profile and the two never drift apart when
// RIN_RENDERER_TYPST_MATH_RENDER changes.
func typstMathPresentation(cfg Config) string {
	if cfg.TypstMathRender {
		return typstadapter.MathPresentationMathJax
	}
	return typstadapter.MathPresentationNative
}

// TypstHTMLProfileID exposes the advertised Typst HTML render profile to the
// job API. The Control Plane resolves it before submitting a document job so
// that the request, Renderer cache key and publication record all agree on one
// renderProfileId, and a stale Control Plane requesting an older profile is
// rejected instead of silently reusing an outdated result.
func TypstHTMLProfileID(cfg Config) string {
	return typstProfileID(cfg)
}
