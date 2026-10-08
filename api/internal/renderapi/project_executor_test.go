package renderapi

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestCanonicalResultPreservesHTMLDiagnosticsAndAssetMetadata(t *testing.T) {
	response := ProjectRenderResponse{
		RequestID: "request-a", Title: "Title", HTML: "<article>ok</article>", Engine: "latexml",
		MainFile: "main.tex", AssetFiles: []AssetFile{{Path: "images/a.png", MIME: "image/png", Encoding: "base64", Body: base64.StdEncoding.EncodeToString([]byte("png"))}},
		Versions:    Versions{RinRenderer: "test", LateXML: "0.8.8"},
		Diagnostics: []Diagnostic{{Severity: "warning", Code: "test.warning", Message: "warning", Engine: "latexml"}},
	}
	result, err := CanonicalResult("job-a", strings.Repeat("a", 64), "latex", response)
	if err != nil {
		t.Fatal(err)
	}
	if result.Inline == nil || result.Inline.SchemaVersion != contracts.DocumentBundleSchemaVersionV2 ||
		!strings.Contains(result.Inline.Pages[0].Fragment, ">ok</article>") || len(result.Inline.Pages[0].Blocks) != 1 ||
		len(result.Inline.Assets) != 1 || result.Inline.Assets[0].Bytes != 3 {
		t.Fatalf("canonical result = %#v", result)
	}
	if result.Diagnostics[0].Stage != "latexml" || result.Versions["latexml"] != "0.8.8" {
		t.Fatalf("canonical diagnostics/versions = %#v / %#v", result.Diagnostics, result.Versions)
	}
}

func TestCanonicalResultUsesRendererBookPages(t *testing.T) {
	response := ProjectRenderResponse{
		RequestID: "request-book", Title: "Book", HTML: "<article>whole book</article>", Engine: "latexml",
		MainFile: "main.tex", DocumentMode: "book", Versions: Versions{RinRenderer: "test"},
		Reader: map[string]any{
			"toc": []map[string]any{
				{"id": "chapter-1", "text": "Chapter 1", "level": 2},
				{"id": "section-1-1", "text": "Section 1.1", "level": 3},
				{"id": "section-1-2", "text": "Section 1.2", "level": 3},
			},
			"pages": []map[string]any{
				{"id": "section-1-1", "text": "Section 1.1", "level": 3, "html": "<section>one</section>"},
				{"id": "section-1-2", "text": "Section 1.2", "level": 3, "html": "<section>two</section>"},
			},
		},
	}
	result, err := CanonicalResult("job-book", strings.Repeat("a", 64), "latex", response)
	if err != nil {
		t.Fatal(err)
	}
	if result.Inline == nil || len(result.Inline.Pages) != 2 || result.Inline.Pages[0].ID != "section-1-1" ||
		!strings.Contains(result.Inline.Pages[1].Fragment, ">two</section>") || len(result.Inline.Pages[1].Blocks) != 1 {
		t.Fatalf("canonical book pages = %#v", result.Inline)
	}
	if len(result.Inline.Pages[0].TOC) != 3 || result.Inline.Pages[0].TOC[1].Depth != 3 {
		t.Fatalf("canonical book toc = %#v", result.Inline.Pages[0].TOC)
	}
}

func TestCanonicalResultRejectsErrorsInReaderPageEvenWhenBodyIsClean(t *testing.T) {
	response := ProjectRenderResponse{
		RequestID: "request-book", Title: "Book", HTML: "<article>clean cover</article>", Engine: "latexml",
		MainFile: "main.tex", DocumentMode: "book", Versions: Versions{RinRenderer: "test"},
		Reader: map[string]any{"pages": []map[string]any{
			{"id": "section-1", "text": "Section 1", "level": 3, "html": `<span class="ltx_ERROR undefined">\Needspace</span>`},
		}},
	}
	if _, err := CanonicalResult("job-book", strings.Repeat("a", 64), "latex", response); err == nil || !strings.Contains(err.Error(), "rendering error") {
		t.Fatalf("incomplete book reader page was accepted: %v", err)
	}
}

func TestCanonicalResultPreservesUnicodeTOCAnchorAndNormalizesNumericPageID(t *testing.T) {
	response := ProjectRenderResponse{
		RequestID: "request-book-unicode", Title: "Book", HTML: "<article>whole book</article>", Engine: "latexml",
		MainFile: "main.tex", DocumentMode: "book", Versions: Versions{RinRenderer: "test"},
		Reader: map[string]any{
			"toc": []map[string]any{
				{"id": "1-1-定义", "text": "定义", "level": 3},
			},
			"pages": []map[string]any{
				{"id": "1-1-定义", "text": "定义", "level": 3, "html": `<h3 id="1-1-定义">定义</h3>`},
			},
		},
	}
	result, err := CanonicalResult("job-book-unicode", strings.Repeat("a", 64), "latex", response)
	if err != nil {
		t.Fatal(err)
	}
	page := result.Inline.Pages[0]
	if page.ID != "page-1-1-定义" || page.TOC[0].ID != "1-1-定义" || !strings.Contains(page.Fragment, `id="1-1-定义"`) {
		t.Fatalf("canonical Unicode book page = %#v", page)
	}
}

func TestCanonicalResultRejectsInvalidContentKind(t *testing.T) {
	_, err := CanonicalResult("job-a", strings.Repeat("a", 64), "unknown", ProjectRenderResponse{RequestID: "request-a"})
	if err == nil {
		t.Fatal("CanonicalResult() accepted unknown content kind")
	}
}

func TestCanonicalResultPreservesVerifiedGeneratedArtifacts(t *testing.T) {
	hash := strings.Repeat("b", 64)
	artifact := contracts.ArtifactReference{
		ArtifactID: "diagrams/v1/svg-sha256/bb/" + hash + ".svg", SHA256: hash, Bytes: 128,
		MediaType: "image/svg+xml; charset=utf-8", Visibility: "public",
	}
	response := ProjectRenderResponse{
		RequestID: "request-artifact", Engine: "latexml", MainFile: "main.tex",
		HTML:               `<img src="https://cdn.example/math.svg" data-rin-math-object-id="` + artifact.ArtifactID + `">`,
		GeneratedArtifacts: []contracts.ArtifactReference{artifact}, Versions: Versions{RinRenderer: "test"},
	}
	result, err := CanonicalResult("job-artifact", strings.Repeat("a", 64), "latex", response)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Assets) != 1 || result.Assets[0] != artifact {
		t.Fatalf("canonical generated artifacts = %#v", result.Assets)
	}
}

func TestCanonicalResultBindsKnowledgeIndexIntoHashedBundle(t *testing.T) {
	projectHash := strings.Repeat("a", 64)
	index := &contracts.KnowledgeIndex{
		SchemaVersion: contracts.KnowledgeIndexSchemaVersion, ProjectID: "tag-wiki:288",
		SourceCommit: strings.Repeat("b", 40), ProjectHash: projectHash,
		Anchors:    []contracts.KnowledgeAnchor{{ID: "stable-anchor", Kind: "definition", Label: "定义", SourceLocator: contracts.KnowledgeLocator{Path: "main.tex", Line: 2}, ContentHash: strings.Repeat("c", 64)}},
		References: []contracts.KnowledgeReference{}, Unresolved: []contracts.KnowledgeUnresolved{},
	}
	response := ProjectRenderResponse{RequestID: "request-index", Title: "Tag", HTML: "<p>ok</p>", Engine: "latexml", MainFile: "main.tex", Versions: Versions{RinRenderer: "test"}, KnowledgeIndex: index}
	result, err := CanonicalResult("job-index", projectHash, "latex", response)
	if err != nil {
		t.Fatal(err)
	}
	if result.Inline == nil || result.Inline.KnowledgeIndex == nil || result.Inline.KnowledgeIndex.ProjectID != "tag-wiki:288" {
		t.Fatalf("knowledge index missing from canonical bundle: %#v", result.Inline)
	}
	changed := *index
	changed.Unresolved = []contracts.KnowledgeUnresolved{{Label: "未解析", SourceLocator: contracts.KnowledgeLocator{Path: "main.tex", Line: 3}}}
	response.KnowledgeIndex = &changed
	changedResult, err := CanonicalResult("job-index", projectHash, "latex", response)
	if err != nil {
		t.Fatal(err)
	}
	if changedResult.ResultHash == result.ResultHash {
		t.Fatal("knowledge index change did not change canonical result hash")
	}
	changed.ProjectHash = strings.Repeat("d", 64)
	response.KnowledgeIndex = &changed
	if _, err := CanonicalResult("job-index", projectHash, "latex", response); err == nil {
		t.Fatal("mismatched knowledge project hash was accepted")
	}
}

func TestCanonicalResultCannotBypassFinalOutputValidation(t *testing.T) {
	_, err := CanonicalResult("job-a", strings.Repeat("a", 64), "latex", ProjectRenderResponse{
		RequestID: "request-a", Engine: "latexml", MainFile: "main.tex",
		HTML: `<rin-work data-id="rw_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"></rin-work>`, Versions: Versions{RinRenderer: "test"},
	})
	if err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("CanonicalResult() = %v", err)
	}
}
