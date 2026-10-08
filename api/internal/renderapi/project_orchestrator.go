package renderapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

type projectRenderInput struct {
	RequestID        string
	ArchiveName      string
	Archive          []byte
	Engine           string
	Title            string
	ExplicitMainFile string
	MetadataMainFile string
	ActiveFile       string
	ProjectStatus    string
	MathPolicy       string
	Renderer         string
	ProjectID        string
	SourceCommit     string
	ProjectHash      string
}

type projectPreparationError struct {
	Status  int
	Message string
	Cause   error
}

type projectRenderContext struct {
	RequestID     string
	Title         string
	MainFile      string
	ProjectStatus string
	MathPolicy    string
	Renderer      string
	Files         []projectcore.File
	Manifest      projectcore.Manifest
	Diagnostics   []Diagnostic
	ProjectID     string
	SourceCommit  string
	ProjectHash   string
}

// orchestrateProject is source-format orchestration, not an HTTP protocol handler. It owns project
// import, manifest construction, normalized render options, and registered engine execution.
func (s *Server) orchestrateProject(ctx context.Context, input projectRenderInput) (projectEngineOutcome, *projectPreparationError) {
	engine := cleanDocumentEngine(input.Engine)
	if engine == "" {
		engine = s.cfg.DefaultDocumentEngine
	}
	title := strings.TrimSpace(input.Title)
	if title == "" {
		title = projectcore.TitleFromArchiveName(input.ArchiveName)
	}
	files, importDiagnostics, err := projectcore.ImportArchive(input.ArchiveName, input.Archive, projectLimitsFromConfig(s.cfg))
	if err != nil {
		return projectEngineOutcome{}, &projectPreparationError{Status: http.StatusBadRequest, Message: "invalid project archive", Cause: err}
	}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{
		Title:            title,
		ExplicitMainFile: input.ExplicitMainFile,
		MetadataMainFile: input.MetadataMainFile,
		ActiveFile:       input.ActiveFile,
	})
	manifest.Diagnostics = append(importDiagnostics, manifest.Diagnostics...)
	for _, diagnostic := range manifest.Diagnostics {
		if diagnostic.Severity == "error" && strings.HasPrefix(diagnostic.Code, "project.") {
			return projectEngineOutcome{}, &projectPreparationError{
				Status: http.StatusBadRequest, Message: "project dependencies are incomplete",
				Cause: fmt.Errorf("%s: %s", diagnostic.Code, diagnostic.Message),
			}
		}
	}
	mathPolicy := cleanMathPolicy(input.MathPolicy)
	if mathPolicy == "" {
		mathPolicy = s.cfg.DefaultMathPolicy
	}
	project := projectRenderContext{
		RequestID:     input.RequestID,
		Title:         title,
		MainFile:      manifest.MainFile,
		ProjectStatus: cleanProjectStatus(input.ProjectStatus),
		MathPolicy:    mathPolicy,
		Renderer:      cleanLegacyRenderer(input.Renderer),
		Files:         files,
		Manifest:      manifest,
		Diagnostics:   diagnosticsFromProject(manifest.Diagnostics),
		ProjectID:     input.ProjectID, SourceCommit: input.SourceCommit, ProjectHash: input.ProjectHash,
	}
	return s.renderProjectUsingEngine(ctx, engine, project), nil
}
