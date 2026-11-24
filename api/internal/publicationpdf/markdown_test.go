package publicationpdf

import (
	"context"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func markdownBundle(fragments ...string) contracts.DocumentBundle {
	bundle := contracts.DocumentBundle{
		SchemaVersion:  contracts.DocumentBundleSchemaVersionV1,
		ProjectHash:    strings.Repeat("a", 64),
		BundleHash:     strings.Repeat("b", 64),
		State:          contracts.DocumentBundleStateFinal,
		DocumentEngine: "markdown",
		ContentKind:    contracts.ContentKindMarkdown,
		Title:          "The & book",
		Provenance: contracts.Provenance{
			Adapter: "markdown", AdapterVersion: "1", EngineVersion: "1",
			ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion,
		},
	}
	for i, fragment := range fragments {
		name := []string{"first", "last"}[i]
		bundle.Pages = append(bundle.Pages, contracts.DocumentPage{
			ID: name, SourcePath: "chapters/" + name + ".md", Title: name,
			Fragment: fragment, FragmentFormat: contracts.FragmentFormatHTML,
		})
	}
	return bundle
}

func TestMarkdownHTMLBookKeepsOrderAndDistinctLinks(t *testing.T) {
	bundle := markdownBundle(
		`<h1 id="intro">First chapter</h1><p><a href="chapters/last.md#intro">next</a></p><a href="#intro" aria-describedby="note">note</a><p id="note">text</p>`,
		`<h1 id="intro">Last chapter</h1><p><a href="chapters/first.md">back</a> and final content.</p>`,
	)
	doc, err := MarkdownHTML(context.Background(), bundle, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<h1>The &amp; book</h1>`,
		`<h2 id="chapter-1--intro">First chapter</h2>`,
		`<h2 id="chapter-2--intro">Last chapter</h2>`,
		`href="#chapter-2--intro"`,
		`href="#chapter-1--intro"`, `aria-describedby="chapter-1--note"`,
		`href="#chapter-1"`, `and final content`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing %s in %s", want, doc[strings.Index(doc, "<body>"):])
		}
	}
	if strings.Index(doc, "First chapter") >= strings.Index(doc, "Last chapter") {
		t.Fatal("book chapters changed order")
	}
}

func TestMarkdownHTMLRequiresOfflineImages(t *testing.T) {
	bundle := markdownBundle(`<p>body</p><img src="https://example.test/chart.png" alt="plot"><img src="https://example.test/second.png" alt="second">`)
	if _, err := MarkdownHTML(context.Background(), bundle, false, nil); err == nil {
		t.Fatal("accepted network image without a verified offline copy")
	}
	doc, err := MarkdownHTML(context.Background(), bundle, false, func(_ context.Context, image ImageReference) (string, error) {
		return "data:image/png;base64,iVBORw0KGgo=", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, "https://example.test") || strings.Count(doc, "data:image/png;") != 2 {
		t.Fatal("offline images were not substituted")
	}
	if _, err := MarkdownHTML(context.Background(), markdownBundle(`<img alt="missing">`), false, nil); err == nil {
		t.Fatal("accepted missing image source")
	}
}
