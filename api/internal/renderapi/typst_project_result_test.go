package renderapi

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestTypstHTMLResultBuildsV1BookBundle(t *testing.T) {
	html := []byte(`<html><body><p><span id="rin-page-second"></span></p><h2>第二章</h2><p>第二章正文</p>` +
		`<p><span id="rin-page-group-theory"></span></p><h2 id="group-theory">第四章</h2><p>本章引言</p>` +
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
