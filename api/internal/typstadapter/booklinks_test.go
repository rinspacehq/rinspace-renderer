package typstadapter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBookLinksMarksOnlyCrossPageReferences(t *testing.T) {
	source := `<!doctype html><html><body>` +
		`<p><span id="rin-page-first"></span></p><h2 id="first">第一章</h2>` +
		`<p>见 <a href="#second">第二章</a> 与 <a href="#first">本页</a>。</p>` +
		`<p><span id="rin-page-second"></span></p><h2 id="second">第二章</h2>` +
		`<p><a href="#first">回第一章</a></p></body></html>`
	result, err := AdaptHTML([]byte(source), "book", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pages) != 2 {
		t.Fatalf("expected two reader pages, got %#v", result.Pages)
	}
	first, second := result.Pages[0].HTML, result.Pages[1].HTML
	if !strings.Contains(first, `<a class="rin-reader-ref" href="#second" data-rin-page="second">`) {
		t.Fatalf("cross-page reference was not routed to its owning page: %s", first)
	}
	if strings.Contains(first, `href="#first" data-rin-page`) || !strings.Contains(first, `href="#first"`) {
		t.Fatalf("same-page reference must not be marked as cross-page: %s", first)
	}
	if !strings.Contains(second, `href="#first" data-rin-page="first"`) {
		t.Fatalf("back reference must be routed to the first page: %s", second)
	}
}

func TestResolveBookLinksRejectsDanglingDuplicateAndUnboundedPages(t *testing.T) {
	cases := map[string]struct {
		source string
		code   string
	}{
		"dangling": {
			source: `<html><body><p><span id="rin-page-a"></span></p><h2>A</h2>` +
				`<p><a href="#missing">loss</a></p></body></html>`,
			code: "typst.book.link.unresolved",
		},
		"duplicate": {
			source: `<html><body><p><span id="rin-page-a"></span></p><h2>A</h2>` +
				`<p><span id="shared">x</span></p><p><span id="rin-page-b"></span></p><h2>B</h2>` +
				`<p><span id="shared">y</span></p></body></html>`,
			code: "typst.book.anchor.duplicate",
		},
		"no heading": {
			source: `<html><body><p><span id="rin-page-a"></span></p><h2>A</h2><p>正文</p>` +
				`<p><span id="rin-page-b"></span></p><p>缺少标题</p></body></html>`,
			code: "typst.book.page.heading",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := AdaptHTML([]byte(testCase.source), "book", nil)
			var bookErr *BookError
			if !errors.As(err, &bookErr) || bookErr.Code != testCase.code {
				t.Fatalf("expected %s, got %v", testCase.code, err)
			}
		})
	}
}

func TestResolveBookLinksLeavesArticleOutputAlone(t *testing.T) {
	source := `<html><body><h1>Article</h1><p><a href="#anywhere">anchor</a></p></body></html>`
	if _, err := AdaptHTML([]byte(source), "article", nil); err != nil {
		t.Fatal(err)
	}
}

// The pinned compiler corpus proves the real Typst footnote, bibliography and
// cross-chapter links all resolve to exactly one reader page. Set
// RIN_TYPST_CORPUS_OUTPUT_DIR to the pinned experiment output directory.
func TestPinnedCompilerCorpusResolvesBookAnchors(t *testing.T) {
	directory := os.Getenv("RIN_TYPST_CORPUS_OUTPUT_DIR")
	if directory == "" {
		t.Skip("pinned compiler corpus output is not configured")
	}
	book, err := os.ReadFile(filepath.Join(directory, "book.html"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := AdaptHTML(book, "book", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pages) != 9 {
		t.Fatalf("pinned Typst book pagination drift: %#v", result.Pages)
	}
	page := func(id string) string {
		for _, candidate := range result.Pages {
			if candidate.ID == id {
				return candidate.HTML
			}
		}
		t.Fatalf("pinned Typst book lost reader page %q", id)
		return ""
	}
	if definition := page("definition"); !strings.Contains(definition, `href="#loc-4" data-rin-page="appendix"`) {
		t.Fatalf("footnote reference did not resolve to the endnotes page: %s", definition)
	}
	if subgroups := page("subgroups"); !strings.Contains(subgroups, `href="#loc-3" data-rin-page="appendix"`) ||
		!strings.Contains(subgroups, `href="#definition" data-rin-page="definition"`) ||
		!strings.Contains(subgroups, `href="#subgroups"`) || strings.Contains(subgroups, `href="#subgroups" data-rin-page`) {
		t.Fatalf("cross-chapter references were misrouted: %s", subgroups)
	}
	if appendix := page("appendix"); !strings.Contains(appendix, `href="#loc-2" data-rin-page="subgroups"`) ||
		!strings.Contains(appendix, `href="#group-theory" data-rin-page="group-theory"`) {
		t.Fatalf("bibliography or appendix back references were misrouted: %s", appendix)
	}
}
