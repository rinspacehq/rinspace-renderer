package projectcore

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func testLimits() Limits {
	return Limits{
		ArchiveMaxBytes: 64 << 20,
		FileMaxCount:    700,
		FileMaxBytes:    16 << 20,
	}
}

func TestImportArchiveSupportsDirectTexSource(t *testing.T) {
	files, diagnostics, err := ImportArchive("main.tex", []byte(`\documentclass{article}
\begin{document}
Direct source file.
\end{document}`), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %#v", diagnostics)
	}
	if len(files) != 1 || files[0].Path != "main.tex" || files[0].Kind != "tex" {
		t.Fatalf("unexpected files: %#v", files)
	}
}

func TestImportArchiveStripsCommonTopDirectory(t *testing.T) {
	body := tarGzipBody(t, map[string]string{
		"paper/main.tex":           `\documentclass{article}`,
		"paper/sections/intro.tex": `Intro`,
		"paper/refs.bib":           `@book{knuth}`,
	})

	files, diagnostics, err := ImportArchive("paper.tar.gz", body, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %#v", diagnostics)
	}
	got := filePaths(files)
	want := "main.tex,refs.bib,sections/intro.tex"
	if got != want {
		t.Fatalf("expected paths %s, got %s", want, got)
	}
}

func TestImportArchiveRejectsTraversalPath(t *testing.T) {
	body := zipBody(t, map[string]string{
		"../main.tex": `\documentclass{article}`,
	})

	_, _, err := ImportArchive("bad.zip", body, testLimits())
	if err == nil || !strings.Contains(err.Error(), "unsafe archive path") {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
}

func TestImportArchiveSkipsUnsupportedFiles(t *testing.T) {
	body := zipBody(t, map[string]string{
		"main.tex":   `\documentclass{article}`,
		"secret.exe": "binary",
	})

	files, diagnostics, err := ImportArchive("paper.zip", body, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "main.tex" {
		t.Fatalf("unexpected files: %#v", files)
	}
	if len(diagnostics) != 1 || diagnostics[0].Code != "project.file.unsupported" {
		t.Fatalf("expected unsupported diagnostic, got %#v", diagnostics)
	}
}

func TestImportArchiveReportsOversizedFiles(t *testing.T) {
	limits := testLimits()
	limits.FileMaxBytes = 8
	body := zipBody(t, map[string]string{
		"main.tex": strings.Repeat("x", 16),
	})

	_, diagnostics, err := ImportArchive("paper.zip", body, limits)
	if err == nil || !strings.Contains(err.Error(), "archive contains no supported project files") {
		t.Fatalf("expected empty archive error after oversized skip, got %v", err)
	}
	if len(diagnostics) != 1 || diagnostics[0].Code != "project.file.oversized" {
		t.Fatalf("expected oversized diagnostic, got %#v", diagnostics)
	}
}

func TestImportArchiveRejectsEmptyProject(t *testing.T) {
	_, _, err := ImportArchive("empty.zip", zipBody(t, map[string]string{}), testLimits())
	if err == nil || !strings.Contains(err.Error(), "archive contains no supported project files") {
		t.Fatalf("expected empty project error, got %v", err)
	}
}

func TestImportArchiveKeepsSupportedFileTypes(t *testing.T) {
	body := zipBody(t, map[string]string{
		"main.tex":        `\documentclass{article}`,
		"refs.bib":        `@book{knuth}`,
		"styles/rin.cls":  `\ProvidesClass{rin}`,
		"figures/a.svg":   `<svg></svg>`,
		"notes/readme.md": "# Notes",
	})

	files, diagnostics, err := ImportArchive("paper.zip", body, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %#v", diagnostics)
	}
	got := map[string]string{}
	for _, file := range files {
		got[file.Path] = file.Kind
	}
	for path, kind := range map[string]string{
		"main.tex":        "tex",
		"refs.bib":        "bib",
		"styles/rin.cls":  "style",
		"figures/a.svg":   "asset",
		"notes/readme.md": "text",
	} {
		if got[path] != kind {
			t.Fatalf("expected %s to be %s, got %#v", path, kind, got)
		}
	}
}

func TestChooseMainFileHonorsExplicitPath(t *testing.T) {
	main, diagnostics := ChooseMainFile([]File{
		{Path: "main.tex", Kind: "tex", Body: `\documentclass{article}`},
		{Path: "chapters/one.tex", Kind: "tex", Body: `\documentclass{book}`},
	}, MainFileCandidates{Explicit: "chapters/one.tex"})
	if main != "chapters/one.tex" {
		t.Fatalf("expected explicit main file, got %s", main)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %#v", diagnostics)
	}
}

func TestChooseMainFileReportsMissingExplicitPath(t *testing.T) {
	main, diagnostics := ChooseMainFile([]File{
		{Path: "main.tex", Kind: "tex", Body: `\documentclass{article}`},
	}, MainFileCandidates{Explicit: "paper.tex"})
	if main != "main.tex" {
		t.Fatalf("expected fallback main.tex, got %s", main)
	}
	if len(diagnostics) != 1 || diagnostics[0].Code != "project.mainfile.missing" {
		t.Fatalf("expected missing main diagnostic, got %#v", diagnostics)
	}
}

func TestChooseMainFileUsesMetadataBeforeActivePath(t *testing.T) {
	main, diagnostics := ChooseMainFile([]File{
		{Path: "chapters/one.tex", Kind: "tex", Body: `\documentclass{book}`},
		{Path: "chapters/two.tex", Kind: "tex", Body: `\documentclass{book}`},
	}, MainFileCandidates{Metadata: "chapters/two.tex", Active: "chapters/one.tex"})
	if main != "chapters/two.tex" {
		t.Fatalf("expected metadata main file, got %s", main)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %#v", diagnostics)
	}
}

func zipBody(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, body := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func tarGzipBody(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		content := []byte(body)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write tar entry: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func filePaths(files []File) string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return strings.Join(paths, ",")
}
