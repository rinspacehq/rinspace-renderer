package renderapi

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfinspect"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
)

func (executor *typstProjectExecutor) pdfFallback(ctx context.Context, request ProjectExecutionRequest, project projectidentity.Project,
	files map[string][]byte, entrypoint, mode, reason string) ProjectExecutionResult {
	if ctx.Err() != nil {
		return typstCompileFailure(ctx, ctx.Err(), "")
	}
	// The existing browser PDF reader requires an actual publication commit.
	// Do not fabricate a Git commit from a source hash for a local-only render.
	if !authorPDFCommit.MatchString(request.SourceCommit) || !authorPDFProject.MatchString(request.ProjectID) || !authorPDFHash.MatchString(request.ControlProjectHash) {
		return typstExecutionFailure("typst.html.unsupported", "Typst HTML export is unsupported; PDF reading requires a publication identity", errors.New(reason))
	}
	knowledge, failure := executor.knowledgeIndex(request, project)
	if failure != nil {
		return *failure
	}
	compiled, err := executor.compiler().Compile(ctx, files, entrypoint, "pdf")
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return typstCompileFailure(ctx, err, compiled.Log)
		}
		return typstExecutionFailure("typst.pdf.compile.failed", "Typst PDF fallback compilation failed", errors.New(boundedDiagnostic(reason+"\n"+compiled.Log+"\n"+err.Error())))
	}
	if len(compiled.Output) > authorPDFMaxBytes {
		return typstExecutionFailure("typst.pdf.delivery.too_large", "Typst fallback PDF exceeds the current authenticated inline reader limit", errors.New(reason))
	}
	inspected, err := pdfinspect.Inspect(compiled.Output)
	if err != nil {
		return typstExecutionFailure("typst.pdf.invalid", "Typst fallback PDF failed inspection", err)
	}
	fragment := pdfReadingFragment(compiled.Output, inspected, request.SourceCommit, strings.TrimSuffix(entrypoint, path.Ext(entrypoint))+".pdf")
	diagnostics := []Diagnostic{{Severity: "warning", Code: "typst.html.unsupported", Engine: "typst", Message: boundedDiagnostic(reason)},
		{Severity: "warning", Code: "typst.pdf.reading", Engine: "typst", Message: "Reading uses a PDF compiled from the same source commit because the native HTML backend cannot represent this document completely."}}
	response := ProjectRenderResponse{RequestID: request.RequestID, Title: firstNonEmpty(strings.TrimSpace(request.Title), "Document"),
		MainFile: entrypoint, Engine: "typst", DocumentMode: mode, Fallback: true, PrimaryEngine: "typst", FallbackEngine: "typst-pdf",
		HTML: fragment, Diagnostics: diagnostics, KnowledgeIndex: knowledge,
		Assets: []AssetRef{}, AssetFiles: []AssetFile{}, Diagrams: []DiagramRef{}, GeneratedArtifacts: []contracts.ArtifactReference{},
		Reader: map[string]any{"toc": []map[string]any{{"id": "page-pdf-reading", "text": "PDF", "level": 2}},
			"pages": []map[string]any{{"id": "page-pdf-reading", "text": "PDF", "level": 2, "html": fragment}}},
		Versions: Versions{RinRenderer: executor.cfg.RendererVersion, Typst: executor.cfg.TypstVersion,
			TypstProfile: executor.profile(typstMathPresentation(executor.cfg)).ID(), FinalOutput: finaloutput.ContractVersion},
	}
	canonical, err := CanonicalResult(request.JobID, request.ControlProjectHash, "typst", response)
	if err != nil {
		return typstExecutionFailure("typst.pdf.result.invalid", "Typst PDF reading result is invalid", err)
	}
	canonical.Versions["typstReading"] = "pdf"
	return ProjectExecutionResult{Response: response, Canonical: &canonical, Status: http.StatusOK, Diagnostics: diagnostics}
}
