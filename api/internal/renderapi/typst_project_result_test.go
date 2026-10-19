package renderapi

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestTypstHTMLResultBuildsV1BookBundle(t *testing.T) {
	html := []byte(`<html><body><h2 id="second">第二章</h2><p>第二章正文</p>` +
		`<h2 id="group-theory">第四章</h2><p>本章引言</p>` +
		`<p><a href="#second">回到第二章</a></p></body></html>`)
	result, err := TypstHTMLResult("job-typst", "request-typst", strings.Repeat("a", 64), "main.typ", "书名", "book", "test-renderer", "0.15.1", html, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Canonical == nil || result.Canonical.Inline == nil ||
		result.Canonical.ContentKind != contracts.ContentKindTypst || result.Canonical.Engine != "typst" ||
		result.Canonical.Inline.SchemaVersion != contracts.DocumentBundleSchemaVersionV1 ||
		result.Canonical.Inline.Provenance.EngineVersion != "0.15.1" ||
		len(result.Canonical.Inline.Pages) != 2 || result.Canonical.Inline.Pages[1].ID != "group-theory" ||
		strings.Contains(result.Canonical.Inline.Pages[1].Fragment, "第二章正文") ||
		!strings.Contains(result.Canonical.Inline.Pages[1].Fragment, "本章引言") {
		t.Fatalf("Typst canonical book result = %#v", result.Canonical)
	}
}

func TestTypstHTMLArticlePublishesHeadingOutlineWithoutAuthorMarkers(t *testing.T) {
	source := []byte(`<html><body><h2>Introduction</h2><section><h3 id="native-section">Nested</h3>` +
		`<h4>Details</h4></section><nav role="doc-toc"><h2>Contents</h2></nav>` +
		`<section role="doc-endnotes"><h2>Notes</h2></section></body></html>`)
	result, err := TypstHTMLResult("article-toc", "request-toc", strings.Repeat("a", 64), "main.typ", "Title", "article", "test", "0.15.1", source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Canonical == nil || result.Canonical.Inline == nil || len(result.Canonical.Inline.Pages) != 1 {
		t.Fatal("Typst article must remain a single canonical page")
	}
	page := result.Canonical.Inline.Pages[0]
	if len(page.TOC) != 3 {
		t.Fatalf("article outline is missing or includes generated navigation: %#v", page.TOC)
	}
	for index, title := range []string{"Introduction", "Nested", "Details"} {
		entry := page.TOC[index]
		if entry.Text != title || entry.Depth != index+2 || !strings.Contains(page.Fragment, `id="`+entry.ID+`"`) {
			t.Fatalf("TOC entry cannot resolve to its native heading: %#v", entry)
		}
	}
	if page.TOC[1].ID != "native-section" {
		t.Fatal("native Typst heading anchor was rewritten")
	}
}

func TestTypstHTMLArticleWithoutHeadingsHasEmptyOutline(t *testing.T) {
	result, err := TypstHTMLResult("article-none", "request-none", strings.Repeat("a", 64), "main.typ", "Title", "article", "test", "0.15.1", []byte(`<html><body><p>Body only.</p></body></html>`), nil)
	if err != nil || result.Canonical == nil || len(result.Canonical.Inline.Pages) != 1 || len(result.Canonical.Inline.Pages[0].TOC) != 0 {
		t.Fatalf("heading-free article became invalid: %v", err)
	}
}
