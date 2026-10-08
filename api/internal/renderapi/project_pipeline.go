package renderapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/knowledgeindex"
	"github.com/rinspacehq/rinspace-renderer/api/internal/latexmladapter"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectdiagrams"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectmath"
)

func (s *Server) renderLateXMLProject(ctx context.Context, project projectRenderContext) (ProjectRenderResponse, int, []Diagnostic, string, error) {
	diagnostics := append([]Diagnostic(nil), project.Diagnostics...)
	var knowledge *contracts.KnowledgeIndex
	if project.ProjectID != "" || project.SourceCommit != "" || project.ProjectHash != "" {
		if project.ProjectID == "" || project.SourceCommit == "" || project.ProjectHash == "" {
			err := errors.New("knowledge index publication identity is incomplete")
			return ProjectRenderResponse{}, http.StatusBadRequest, knowledgeDiagnostics(diagnostics, "knowledge_index.invalid_identity", err), "knowledge-index", err
		}
		index, err := knowledgeindex.Extract(project.Files, knowledgeindex.Identity{ProjectID: project.ProjectID, SourceCommit: project.SourceCommit, ProjectHash: project.ProjectHash})
		if err != nil {
			code := "knowledge_index.invalid_source"
			var extractionErr *knowledgeindex.Error
			if errors.As(err, &extractionErr) {
				code = extractionErr.Code
			}
			return ProjectRenderResponse{}, http.StatusBadRequest, knowledgeDiagnostics(diagnostics, code, err), "knowledge-index", err
		}
		knowledge = &index
	}
	lateXMLFiles := project.Files
	lateXMLManifest := project.Manifest
	extracted := projectdiagrams.Extract(project.Files)
	if len(extracted.Diagrams) > 0 {
		lateXMLFiles = extracted.Files
		lateXMLManifest = projectcore.RebuildManifestFiles(project.Manifest, extracted.Files)
		lateXMLManifest.DiagramPlaceholders = diagramPlaceholdersFromExtracted(extracted.Diagrams)
	}
	extractedMath := projectmath.Result{}
	if project.MathPolicy == "server" {
		extractedMath = projectmath.Extract(lateXMLFiles)
		if len(extractedMath.Math) > 0 {
			lateXMLFiles = extractedMath.Files
			lateXMLManifest = projectcore.RebuildManifestFiles(lateXMLManifest, extractedMath.Files)
		}
	}
	rendered, err := s.lateXMLAdapter().Render(ctx, latexmladapter.Request{
		Files:    lateXMLFiles,
		Manifest: lateXMLManifest,
		Title:    project.Title,
	})
	diagnostics = append(diagnostics, diagnosticsFromLateXML(rendered.Diagnostics)...)
	if err != nil {
		return ProjectRenderResponse{}, lateXMLHTTPStatus(err), diagnosticsWithFallbackError(diagnostics, "latexml.failed", err.Error(), "latexml"), "latexml", err
	}

	diagramRefs := []DiagramRef{}
	generatedArtifacts := []contracts.ArtifactReference{}
	diagramReplacements := map[string]diagramHTMLReplacement{}
	if len(extracted.Diagrams) > 0 {
		items := make([]orchestration.DiagramBatchItem, len(extracted.Diagrams))
		for index, diagram := range extracted.Diagrams {
			item, itemErr := newDiagramBatchItem(diagram.Type, DiagramRequest{
				Type: diagram.Type, Options: diagram.Options, Body: diagram.Body, Source: diagram.RenderSource,
			}, diagram.ID, projectDiagramSourceLocation(diagram))
			if itemErr != nil {
				return ProjectRenderResponse{}, http.StatusBadGateway, diagnosticsWithFallbackError(diagnostics, "diagram.unit.invalid", itemErr.Error(), "rin-texsvg"), "rin-texsvg", itemErr
			}
			if diagram.Layout.Alignment != "" {
				item.Unit.Layout = &contracts.DiagramLayout{Alignment: diagram.Layout.Alignment}
			}
			items[index] = item
		}
		resolved, resolveErr := s.diagrams.ResolveDiagrams(ctx, orchestration.DiagramBatch{
			ContractVersion: orchestration.DiagramBatchContractVersion,
			Items:           items, OutputStrategy: "svg",
		})
		if resolveErr != nil {
			return ProjectRenderResponse{}, http.StatusBadGateway, diagnosticsWithFallbackError(diagnostics, "diagram.render.failed", resolveErr.Error(), "rin-texsvg"), "rin-texsvg", resolveErr
		}
		for index, unit := range resolved.Units {
			diagram := extracted.Diagrams[index]
			renderedDiagram := diagramResponseFromResolved(project.RequestID, unit, s.versions())
			diagnostics = append(diagnostics, renderedDiagram.Diagnostics...)
			s.operations.Count("cache_requests_total", "diagram", unit.Cache.Status)
			if unit.State != "succeeded" {
				err := errors.New(firstDiagramFailure(unit))
				return ProjectRenderResponse{}, http.StatusBadGateway, diagnosticsWithFallbackError(diagnostics, "diagram.render.failed", err.Error(), "rin-texsvg"), "rin-texsvg", err
			}
			cloudBaseURL := firstNonEmpty(renderedDiagram.CloudBaseURL, renderedDiagram.URL)
			diagramRefs = append(diagramRefs, DiagramRef{
				Type: firstNonEmpty(renderedDiagram.Type, diagram.Type), ObjectID: renderedDiagram.ObjectID,
				CloudBaseURL: cloudBaseURL,
			})
			generatedArtifacts = append(generatedArtifacts, *unit.Artifact)
			diagramReplacements[diagram.Placeholder] = diagramReplacementHTML(diagram, renderedDiagram)
		}
	}
	html := normalizeLateXMLHTML(rendered.HTML, lateXMLManifest)
	mathSummary := MathSummary{}
	if project.MathPolicy == "server" {
		var mathDiagnostics []Diagnostic
		var mathArtifacts []contracts.ArtifactReference
		var sourceMathSummary MathSummary
		html, sourceMathSummary, mathArtifacts, mathDiagnostics = s.replaceSourceMathPlaceholdersHTML(ctx, project.RequestID, html, extractedMath.Math, extractedMath.Macros)
		generatedArtifacts = append(generatedArtifacts, mathArtifacts...)
		mathSummary = mergeMathSummary(mathSummary, sourceMathSummary)
		diagnostics = append(diagnostics, mathDiagnostics...)
		mathDiagnostics = nil
		html, mathSummary, mathArtifacts, mathDiagnostics = s.renderLateXMLMathHTML(ctx, project.RequestID, html, extractedMath.Macros)
		generatedArtifacts = append(generatedArtifacts, mathArtifacts...)
		mathSummary = mergeMathSummary(sourceMathSummary, mathSummary)
		diagnostics = append(diagnostics, mathDiagnostics...)
	}
	html = replaceDiagramPlaceholders(html, diagramReplacements)
	response := s.projectRenderResponse(project.RequestID, project.Title, html, "latexml", false, project.MainFile, project.ProjectStatus, project.MathPolicy, mathSummary, project.Renderer, project.Files, lateXMLManifest, diagnostics, diagramRefs, generatedArtifacts)
	response.KnowledgeIndex = knowledge
	return response, http.StatusOK, diagnostics, "", nil
}

func knowledgeDiagnostics(diagnostics []Diagnostic, code string, err error) []Diagnostic {
	diagnostic := Diagnostic{Severity: "error", Code: code, Message: err.Error(), Engine: "knowledge-index"}
	var extractionErr *knowledgeindex.Error
	if errors.As(err, &extractionErr) {
		diagnostic.Source = map[string]string{"path": extractionErr.Path, "line": fmt.Sprintf("%d", extractionErr.Line)}
	}
	return append(append([]Diagnostic(nil), diagnostics...), diagnostic)
}

func projectDiagramSourceLocation(diagram projectdiagrams.Diagram) *contracts.SourceLocation {
	if diagram.SourceFile == "" || diagram.SourceLine <= 0 || diagram.SourceColumn <= 0 {
		return nil
	}
	return &contracts.SourceLocation{
		Path:  diagram.SourceFile,
		Start: contracts.SourcePosition{Line: diagram.SourceLine, Column: diagram.SourceColumn},
	}
}

func lateXMLHTTPStatus(err error) int {
	if errors.Is(err, latexmladapter.ErrUnavailable) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func (s *Server) projectRenderResponse(requestID string, title string, html string, engine string, fallback bool, mainFile string, projectStatus string, mathPolicy string, mathSummary MathSummary, renderer string, files []projectcore.File, manifest projectcore.Manifest, diagnostics []Diagnostic, diagrams []DiagramRef, generatedArtifacts []contracts.ArtifactReference) ProjectRenderResponse {
	if diagrams == nil {
		diagrams = []DiagramRef{}
	}
	assetFiles := assetFilesFromProjectFiles(files, manifest)
	return ProjectRenderResponse{
		RequestID:          requestID,
		Title:              title,
		HTML:               strings.TrimSpace(html),
		Engine:             engine,
		Fallback:           fallback,
		MainFile:           mainFile,
		Diagrams:           diagrams,
		GeneratedArtifacts: generatedArtifacts,
		Assets:             assetsFromProjectFiles(files),
		AssetFiles:         assetFiles,
		AssetManifest:      assetManifestFromProjectManifest(manifest, assetFiles),
		Reader:             readerPayload(title, html),
		Math:               mathSummary,
		Versions:           s.versions(),
		Diagnostics:        diagnostics,
		TexSource:          manifest.AnalysisSource,
		Source:             manifest.AnalysisSource,
		AnalysisSource:     manifest.AnalysisSource,
		ResolvedSource:     manifest.ResolvedSource,
		AssetInventory:     manifest.AssetInventory,
		Project: map[string]any{
			"title":      title,
			"status":     projectStatus,
			"mode":       "book",
			"renderer":   renderer,
			"mathPolicy": mathPolicy,
			"mainFile":   mainFile,
			"activePath": mainFile,
			"files":      files,
			"manifest":   manifest,
		},
	}
}

func assetManifestFromProjectManifest(manifest projectcore.Manifest, files []AssetFile) map[string]any {
	return map[string]any{
		"version":            "0.1",
		"files":              files,
		"references":         manifest.AssetInventory.References,
		"assets":             manifest.AssetInventory.Assets,
		"missing":            manifest.AssetInventory.Missing,
		"unused":             manifest.AssetInventory.Unused,
		"graphicsPaths":      manifest.AssetInventory.GraphicsPaths,
		"graphicsExtensions": manifest.AssetInventory.GraphicsExtensions,
	}
}
