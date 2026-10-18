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

func TestMarkdownCatalogBookTitleIsNotAChapter(t *testing.T) {
	files := map[string][]byte{"intro.md": []byte("# Introduction\n")}
	for _, source := range []string{"# 书名\n\n- intro.md\n", "\r\n# 书名\r\n\r\n- intro.md\r\n"} {
		pages, err := ParseMarkdownCatalog(source, files)
		if err != nil || len(pages) != 1 || pages[0].Path != "intro.md" || MarkdownCatalogTitle(source) != "书名" {
			t.Fatalf("title became a page or was lost: %#v, %v", pages, err)
		}
		archive := identityArchive(t, []identityArchiveFile{{name: "main.md", body: source}, {name: "intro.md", body: "# Introduction"}})
		project, err := ImportMarkdownCatalogBook("book.zip", archive, "Old project title", projectcore.Limits{ArchiveMaxBytes: 1 << 20, FileMaxCount: 10, FileMaxBytes: 1 << 20})
		if err != nil || project.Graph.Options["title"] != "书名" || len(project.Graph.Entrypoints) != 1 {
			t.Fatalf("entrypoint title was not authoritative: %#v, %v", project.Graph.Options, err)
		}
	}
	for _, source := range []string{"# \n- intro.md", "# One\n# Two\n- intro.md", "- intro.md\n# Late", "## Title\n- intro.md", "# Book\nBody\n- intro.md", "# Book\tName\n- intro.md"} {
		if _, err := ParseMarkdownCatalog(source, files); err == nil {
			t.Errorf("accepted invalid heading: %q", source)
		}
	}
}
