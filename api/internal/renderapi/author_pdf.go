package renderapi

import (
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/knowledgeindex"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfinspect"
)

const authorPDFMaxBytes = 4 << 20

var (
	authorPDFCommit  = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
	authorPDFProject = regexp.MustCompile(`^(article|book):[1-9][0-9]*$`)
	authorPDFHash    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type authorPDF struct {
	Path       string
	Body       []byte
	Inspection pdfinspect.Result
}

// Only the PDF beside the declared TeX entrypoint is eligible. Never select an
// arbitrary generated file or retrieve content from another source revision.
func availableAuthorPDF(project projectRenderContext) *authorPDF {
	if !authorPDFCommit.MatchString(project.SourceCommit) || !authorPDFProject.MatchString(project.ProjectID) || !authorPDFHash.MatchString(project.ProjectHash) {
		return nil
	}
	extension := path.Ext(project.MainFile)
	if extension != ".tex" && extension != ".ltx" {
		return nil
	}
	candidate := strings.TrimSuffix(project.MainFile, extension) + ".pdf"
	for _, file := range project.Files {
		if file.Path != candidate {
			continue
		}
		if file.Bytes <= 0 || file.Bytes > authorPDFMaxBytes || len(file.Body) > (authorPDFMaxBytes+2)/3*4 {
			return nil
		}
		body := []byte(file.Body)
		if file.Encoding == "base64" {
			var err error
			body, err = base64.StdEncoding.DecodeString(file.Body)
			if err != nil {
				return nil
			}
		}
		if int64(len(body)) != file.Bytes {
			return nil
		}
		inspected, err := pdfinspect.Inspect(body)
		if err != nil {
			return nil
		}
		return &authorPDF{Path: candidate, Body: body, Inspection: inspected}
	}
	return nil
}

func (s *Server) authorPDFOutcome(project projectRenderContext, failed []Diagnostic) (projectEngineOutcome, bool) {
	pdf := availableAuthorPDF(project)
	if pdf == nil {
		return projectEngineOutcome{}, false
	}
	index, err := knowledgeindex.Extract(project.Files, knowledgeindex.Identity{ProjectID: project.ProjectID, SourceCommit: project.SourceCommit, ProjectHash: project.ProjectHash})
	if err != nil {
		return projectEngineOutcome{}, false
	}
	diagnostics := append([]Diagnostic(nil), failed...)
	diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Code: "document.author_pdf_reading", Engine: "author-pdf", Message: "HTML rendering failed; reading uses the author-provided PDF from the same source commit."})
	fragment := fmt.Sprintf(`<div class="rin-author-pdf" data-rin-pdf="%s" data-rin-pdf-sha256="%s" data-rin-pdf-pages="%d" data-rin-pdf-commit="%s" data-rin-pdf-path="%s"><p>PDF</p></div>`,
		base64.StdEncoding.EncodeToString(pdf.Body), pdf.Inspection.SourceSHA256, pdf.Inspection.PageCount, project.SourceCommit, html.EscapeString(pdf.Path))
	response := ProjectRenderResponse{RequestID: project.RequestID, Title: project.Title, MainFile: project.MainFile,
		Engine: "author-pdf", Fallback: true, PrimaryEngine: "latexml", FallbackEngine: "author-pdf", HTML: fragment,
		Diagrams: []DiagramRef{}, Assets: []AssetRef{}, GeneratedArtifacts: []contracts.ArtifactReference{},
		Versions: s.versions(), Diagnostics: diagnostics, KnowledgeIndex: &index,
		Reader: map[string]any{
			"toc":   []map[string]any{{"id": "page-pdf-reading", "text": "PDF", "level": 2}},
			"pages": []map[string]any{{"id": "page-pdf-reading", "text": "PDF", "level": 2, "html": fragment}},
		},
	}
	return projectEngineOutcome{Response: response, Status: http.StatusOK, Diagnostics: diagnostics}, true
}
