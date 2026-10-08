package renderapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
	"strings"
	"testing"
	"time"
)

func authorPDFTestBytes() []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	offsets := make([]int, 5)
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>", "<< /Length 0 >>\nstream\n\nendstream"}
	for i, obj := range objects {
		offsets[i+1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := body.Len()
	body.WriteString("xref\n0 5\n0000000000 65535 f \n")
	for i := 1; i <= 4; i++ {
		fmt.Fprintf(&body, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&body, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return body.Bytes()
}
func authorPDFTestProject() projectRenderContext {
	source := "\\documentclass{book}\n\\begin{document}\nReading\n\\end{document}"
	pdf := authorPDFTestBytes()
	return projectRenderContext{RequestID: "pdf-recovery", Title: "Reading", MainFile: "main.tex", ProjectID: "book:340", SourceCommit: strings.Repeat("a", 40), ProjectHash: strings.Repeat("b", 64), Files: []projectcore.File{{Path: "main.tex", Kind: "tex", Body: source, Bytes: int64(len(source))}, {Path: "main.pdf", Kind: "asset", MIME: "application/pdf", Encoding: "base64", Body: base64.StdEncoding.EncodeToString(pdf), Bytes: int64(len(pdf))}}}
}
func TestAuthorPDFRequiresExactIdentityAndValidatedSibling(t *testing.T) {
	candidate := availableAuthorPDF(authorPDFTestProject())
	if candidate == nil || candidate.Inspection.PageCount != 1 || !bytes.Equal(candidate.Body, authorPDFTestBytes()) {
		t.Fatal("valid author PDF not preserved")
	}
	cases := []struct {
		name   string
		change func(*projectRenderContext)
	}{{"no commit", func(p *projectRenderContext) { p.SourceCommit = "" }}, {"short commit", func(p *projectRenderContext) { p.SourceCommit = "abcdefa" }}, {"wrong filename", func(p *projectRenderContext) { p.Files[1].Path = "older.pdf" }}, {"damaged", func(p *projectRenderContext) { p.Files[1].Body = "!!!!" }}, {"size mismatch", func(p *projectRenderContext) { p.Files[1].Bytes++ }}, {"oversize", func(p *projectRenderContext) { p.Files[1].Bytes = authorPDFMaxBytes + 1 }}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			p := authorPDFTestProject()
			test.change(&p)
			if availableAuthorPDF(p) != nil {
				t.Fatal("invalid candidate accepted")
			}
		})
	}
}
func TestAutoRecoversPDFOnlyAfterHTMLFailureOrQualityRejection(t *testing.T) {
	for _, quality := range []bool{false, true} {
		t.Run(fmt.Sprint(quality), func(t *testing.T) {
			server := &Server{cfg: testConfig(), projectEngines: newProjectEngineRegistry()}
			_ = server.projectEngines.register(fakeProjectEngineAdapter{engine: "auto", render: func(context.Context, projectEngineRequest) projectEngineOutcome {
				if quality {
					return projectEngineOutcome{Status: http.StatusOK, Response: ProjectRenderResponse{Engine: "latexml", HTML: `<span class="ltx_ERROR">broken</span>`}}
				}
				return projectEngineOutcome{Status: 502, FailedEngine: "latexml", Err: errors.New("unsupported"), Diagnostics: []Diagnostic{{Severity: "error", Code: "latexml.failed", Message: "unsupported", Engine: "latexml"}}}
			}})
			result := server.renderProjectUsingEngine(t.Context(), "auto", authorPDFTestProject())
			if result.Err != nil || result.Response.Engine != "author-pdf" || !result.Response.Fallback || len(result.Diagnostics) != 2 || result.Diagnostics[0].Severity != "error" {
				t.Fatalf("lost failure evidence: %#v", result)
			}
			result.Response.DocumentMode = "book"
			canonical, err := CanonicalResult("pdf-job", strings.Repeat("b", 64), "latex", result.Response)
			if err != nil {
				t.Fatal(err)
			}
			if canonical.Inline.SchemaVersion != "rin-document-bundle/v1" || canonical.Inline.KnowledgeIndex == nil || len(canonical.Inline.Pages) != 1 || len(canonical.Inline.Pages[0].Blocks) != 0 || !strings.Contains(canonical.Inline.Pages[0].Fragment, "data-rin-pdf=") {
				t.Fatal("invalid PDF bundle")
			}
			page := canonical.Inline.Pages[0]
			if page.ID != "page-pdf-reading" || len(page.TOC) != 1 || page.TOC[0].ID != page.ID || page.TOC[0].Depth != 2 {
				t.Fatal("PDF book page is incompatible with the deployed reader contract")
			}
		})
	}
}
func TestPDFRecoveryDoesNotMaskCancellationOrSuccessfulHTML(t *testing.T) {
	server := &Server{cfg: testConfig()}
	project := authorPDFTestProject()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	failure := projectEngineOutcome{Status: 502, FailedEngine: "latexml", Err: context.Canceled}
	if result := server.validateAndRecoverProjectOutcome(ctx, "auto", project, failure); result.Err == nil {
		t.Fatal("cancel recovered")
	}
	if result := server.validateAndRecoverProjectOutcome(t.Context(), "latexml", project, failure); result.Err == nil {
		t.Fatal("explicit latexml recovered")
	}
	good := projectEngineOutcome{Status: 200, Response: ProjectRenderResponse{Engine: "latexml", HTML: "<p>Complete</p>"}}
	if result := server.validateAndRecoverProjectOutcome(t.Context(), "auto", project, good); result.Err != nil || result.Response.Engine != "latexml" {
		t.Fatal("HTML replaced")
	}
}

func TestAuthorPDFBoundsHTMLStageOnlyWithEligiblePDF(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		t.Run(fmt.Sprint(eligible), func(t *testing.T) {
			server := &Server{cfg: testConfig(), projectEngines: newProjectEngineRegistry()}
			project := authorPDFTestProject()
			if !eligible {
				project.Files = project.Files[:1]
			}
			_ = server.projectEngines.register(fakeProjectEngineAdapter{engine: "auto", render: func(ctx context.Context, _ projectEngineRequest) projectEngineOutcome {
				deadline, bounded := ctx.Deadline()
				if bounded != eligible {
					t.Fatal("HTML time budget changed without an eligible PDF")
				}
				if bounded && (time.Until(deadline) > 90*time.Second || time.Until(deadline) < 89*time.Second) {
					t.Fatal("incorrect PDF HTML time budget")
				}
				return projectEngineOutcome{Status: http.StatusOK, Response: ProjectRenderResponse{Engine: "latexml", HTML: "<p>Complete</p>"}}
			}})
			result := server.renderProjectUsingEngine(t.Context(), "auto", project)
			if result.Err != nil || result.Response.Engine != "latexml" {
				t.Fatal("valid HTML did not remain primary")
			}
		})
	}
}
