package projectidentity

import (
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
	"testing"
)

func TestMarkdownCatalogHierarchyAndStableIdentity(t *testing.T) {
	files := map[string][]byte{"intro.md": []byte("# Introduction\n"), "chapters/中文.md": []byte("# 章节\n"), "end.md": []byte("End")}
	pages, err := ParseMarkdownCatalog("- intro.md\n  - chapters/中文.md\n- end.md\n", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 || pages[1].Level != 2 || pages[1].Title != "章节" {
		t.Fatalf("pages: %#v", pages)
	}
	reordered, err := ParseMarkdownCatalog("- end.md\n- intro.md\n  - chapters/中文.md\n", files)
	if err != nil || pages[0].ID != reordered[1].ID {
		t.Fatalf("identity changed after reorder: %v", err)
	}
	for _, source := range []string{"", "# Old book", "- main.md", "- ../intro.md", "- ./intro.md", "- /intro.md", "- missing.md", "- intro.md\n- intro.md", "  - intro.md", "- intro.md\n    - end.md", "- intro.md\n\t- end.md", "- [Intro](intro.md)"} {
		if _, err := ParseMarkdownCatalog(source, files); err == nil {
			t.Errorf("accepted invalid catalog %q", source)
		}
	}
}

func TestMarkdownCatalogGraphIncludesCatalogAndSingleChapter(t *testing.T) {
	limits := projectcore.Limits{ArchiveMaxBytes: 1 << 20, FileMaxCount: 10, FileMaxBytes: 1 << 20}
	makeProject := func(catalog string) Project {
		archive := identityArchive(t, []identityArchiveFile{{name: "main.md", body: catalog}, {name: "intro.md", body: "# Introduction"}})
		project, err := ImportMarkdownCatalogBook("book.zip", archive, "Book", limits)
		if err != nil {
			t.Fatal(err)
		}
		return project
	}
	first, changed := makeProject("- intro.md\n"), makeProject("- intro.md\n\n")
	if len(first.Graph.Entrypoints) != 1 || len(first.Graph.Files) != 2 || first.Graph.ProjectHash == changed.Graph.ProjectHash {
		t.Fatal("catalog was rendered as a chapter or excluded from project identity")
	}
}
