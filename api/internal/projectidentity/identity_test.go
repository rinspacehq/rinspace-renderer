package projectidentity

import (
	"archive/zip"
	"bytes"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

func TestMarkdownProjectIdentityIgnoresArchiveOrderAndTimestamps(t *testing.T) {
	first := identityArchive(t, []identityArchiveFile{
		{name: "article.md", body: "# Article", modified: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "image.png", body: "png-bytes", modified: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	second := identityArchive(t, []identityArchiveFile{
		{name: "image.png", body: "png-bytes", modified: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)},
		{name: "article.md", body: "# Article", modified: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	limits := projectcore.Limits{ArchiveMaxBytes: 1 << 20, FileMaxCount: 10, FileMaxBytes: 1 << 20}
	a, err := Import("first.zip", first, contracts.ContentKindMarkdown, "article.md", limits)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Import("second.zip", second, contracts.ContentKindMarkdown, "article.md", limits)
	if err != nil {
		t.Fatal(err)
	}
	if a.Graph.ProjectHash != b.Graph.ProjectHash {
		t.Fatalf("archive metadata changed canonical project hash: %s != %s", a.Graph.ProjectHash, b.Graph.ProjectHash)
	}
	if len(a.Graph.Files) != 2 || a.Graph.Files[0].Path != "article.md" || a.Graph.Files[1].Role != contracts.ProjectFileRoleAsset {
		t.Fatalf("unexpected project graph: %#v", a.Graph)
	}
}

func TestMarkdownProjectIdentityRecognizesRepositorySVGAsset(t *testing.T) {
	archive := identityArchive(t, []identityArchiveFile{
		{name: "content.md", body: "![quiver](assets/quiver/diagram.svg)"},
		{name: "assets/quiver/diagram.svg", body: `<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h1"/></svg>`},
		{name: "rinspace.yaml", body: "status: draft\nsourceVisibility: private\n"},
	})
	project, err := Import("article.zip", archive, contracts.ContentKindMarkdown, "content.md", projectcore.Limits{
		ArchiveMaxBytes: 1 << 20, FileMaxCount: 10, FileMaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	foundManifest := false
	foundSVG := false
	for _, file := range project.Graph.Files {
		if file.Path == "rinspace.yaml" {
			foundManifest = true
		}
		if file.Path == "assets/quiver/diagram.svg" {
			if file.Role != contracts.ProjectFileRoleAsset || file.MediaType != "image/svg+xml" {
				t.Fatalf("Markdown SVG graph identity = %#v", file)
			}
			foundSVG = true
		}
	}
	if !foundManifest || !foundSVG {
		t.Fatalf("Markdown project lost manifest or SVG: %#v", project.Graph.Files)
	}
}

func TestTypstProjectIncludesManagedSourcesWithoutChangingMarkdownImport(t *testing.T) {
	archive := identityArchive(t, []identityArchiveFile{
		{name: "main.typ", body: `#include "chapters/one.typ"`},
		{name: "chapters/one.typ", body: `= 第一章`},
		{name: "rinspace.yaml", body: "build:\n  format: typst\n"},
		{name: "rinspace.typst.lock.json", body: `{"profile":"typst-web/v1"}`},
		{name: "assets/figure.svg", body: `<svg/>`},
	})
	limits := projectcore.Limits{ArchiveMaxBytes: 1 << 20, FileMaxCount: 10, FileMaxBytes: 1 << 20}
	typst, err := Import("project.zip", archive, contracts.ContentKindTypst, "main.typ", limits)
	if err != nil {
		t.Fatal(err)
	}
	if typst.Graph.ContentKind != contracts.ContentKindTypst || len(typst.Graph.Files) != 5 {
		t.Fatalf("Typst graph did not include all reproducible inputs: %#v", typst.Graph)
	}
	if _, err := Import("project.zip", archive, contracts.ContentKindMarkdown, "main.typ", limits); err == nil {
		t.Fatal("a Typst entrypoint was accepted as Markdown")
	}
	if _, err := Import("project.zip", archive, contracts.ContentKindTypst, "main.tex", limits); err == nil {
		t.Fatal("a TeX entrypoint was accepted as Typst")
	}
}

func TestImportMarkdownBookPreservesLegacyPageIdentityAndOrder(t *testing.T) {
	archive := identityArchive(t, []identityArchiveFile{
		{name: "01-introduction.md", body: "# Introduction"},
		{name: "01-introduction/01-example.md", body: "# Example"},
	})
	limits := projectcore.Limits{ArchiveMaxBytes: 1 << 20, FileMaxCount: 10, FileMaxBytes: 1 << 20}
	project, err := ImportMarkdownBook("book.zip", archive, "Synthetic Markdown Book", []MarkdownBookPage{
		{Path: "01-introduction.md", ID: "md-001-introduction", Title: "Introduction"},
		{Path: "01-introduction/01-example.md", ID: "md-001-introduction-sec-001-example", Title: "Example"},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Graph.Entrypoints) != 2 || project.Graph.Entrypoints[0].Path != "01-introduction.md" ||
		project.Graph.Entrypoints[1].Path != "01-introduction/01-example.md" ||
		project.Graph.Entrypoints[0].Role != contracts.EntrypointRoleBookPage {
		t.Fatalf("unexpected ordered book entrypoints: %#v", project.Graph.Entrypoints)
	}
	pageIDs := project.Graph.Options["pageIds"].(map[string]any)
	if pageIDs["01-introduction.md"] != "md-001-introduction" ||
		pageIDs["01-introduction/01-example.md"] != "md-001-introduction-sec-001-example" {
		t.Fatalf("legacy stable page IDs were not preserved: %#v", pageIDs)
	}
}

func TestImportMarkdownBookPreservesUnicodeLegacyPageIDs(t *testing.T) {
	archive := identityArchive(t, []identityArchiveFile{
		{name: "03-啊啊啊.md", body: "# 啊啊啊"},
		{name: "01-可爱捏/01-非常可爱.md", body: "# 非常可爱"},
	})
	limits := projectcore.Limits{ArchiveMaxBytes: 1 << 20, FileMaxCount: 10, FileMaxBytes: 1 << 20}
	pages := []MarkdownBookPage{
		{Path: "03-啊啊啊.md", ID: "md-003-啊啊啊", Title: "啊啊啊"},
		{Path: "01-可爱捏/01-非常可爱.md", ID: "md-001-可爱捏-sec-001-非常可爱", Title: "非常可爱"},
	}
	project, err := ImportMarkdownBook("book.zip", archive, "测试 markdown 书籍", pages, limits)
	if err != nil {
		t.Fatal(err)
	}
	pageIDs := project.Graph.Options["pageIds"].(map[string]any)
	for _, page := range pages {
		if pageIDs[page.Path] != page.ID {
			t.Fatalf("Unicode stable page ID was not preserved for %q: %#v", page.Path, pageIDs)
		}
	}
}

func TestImportMarkdownBookRejectsUnsafeUnicodePageIDs(t *testing.T) {
	archive := identityArchive(t, []identityArchiveFile{
		{name: "one.md", body: "# One"},
		{name: "two.md", body: "# Two"},
	})
	limits := projectcore.Limits{ArchiveMaxBytes: 1 << 20, FileMaxCount: 10, FileMaxBytes: 1 << 20}
	for _, id := range []string{"-starts-with-separator", "md/escape", "md has-space", "md-控制\n符", "m<d", "m\u200dd"} {
		_, err := ImportMarkdownBook("book.zip", archive, "Book", []MarkdownBookPage{
			{Path: "one.md", ID: id}, {Path: "two.md", ID: "md-safe"},
		}, limits)
		if err == nil {
			t.Fatalf("expected unsafe page ID %q to be rejected", id)
		}
	}
}

type identityArchiveFile struct {
	name     string
	body     string
	modified time.Time
}

func identityArchive(t *testing.T, files []identityArchiveFile) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	for _, file := range files {
		header := &zip.FileHeader{Name: file.name, Method: zip.Store}
		header.SetModTime(file.modified)
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte(file.body))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}
