package admission

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/json"
	"testing"
)

func TestObserveAdmissionSourceUsesBoundedArchiveInventory(t *testing.T) {
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	mainFile, _ := writer.Create("main.tex")
	_, _ = mainFile.Write([]byte(`\documentclass[11pt]{book}\begin{document}x\end{document}`))
	chapter, _ := writer.Create("chapter.tex")
	_, _ = chapter.Write([]byte("chapter"))
	asset, _ := writer.Create("image.png")
	_, _ = asset.Write([]byte("image"))
	_ = writer.Close()
	metadata, _ := json.Marshal(map[string]string{"entrypoint": "main.tex"})
	observation := observeAdmissionSource(body.Bytes(), "latex", metadata)
	if observation.FileCount == nil || *observation.FileCount != 3 || observation.DocumentClass != "book" {
		t.Fatalf("archive admission observation = %#v", observation)
	}
}

func TestObserveAdmissionSourceInventoriesImmutablePreviewTar(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	files := map[string]string{
		"main.tex":          `\documentclass{article}\begin{document}preview\end{document}`,
		"chapter.tex":       "chapter",
		"figures/image.png": "image",
	}
	for name, value := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(value)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]string{"entrypoint": "main.tex"})
	observation := observeAdmissionSource(body.Bytes(), "latex", metadata)
	if observation.FileCount == nil || *observation.FileCount != 3 || observation.DocumentClass != "article" {
		t.Fatalf("preview tar admission observation = %#v", observation)
	}
}

func TestObserveAdmissionSourceHandlesRawAndMarkdownBooks(t *testing.T) {
	raw := observeAdmissionSource([]byte(`\documentclass {article}`), "latex", nil)
	if raw.FileCount == nil || *raw.FileCount != 1 || raw.DocumentClass != "article" {
		t.Fatalf("raw admission observation = %#v", raw)
	}

	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	first, _ := writer.Create("one.md")
	_, _ = first.Write([]byte("# One"))
	second, _ := writer.Create("two.md")
	_, _ = second.Write([]byte("# Two"))
	_ = writer.Close()
	markdown := observeAdmissionSource(body.Bytes(), "markdown", nil)
	if markdown.FileCount == nil || *markdown.FileCount != 2 || markdown.DocumentClass != "book" {
		t.Fatalf("Markdown admission observation = %#v", markdown)
	}
}

func TestObserveAdmissionSourceInventoriesTypstArchive(t *testing.T) {
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	main, _ := writer.Create("main.typ")
	_, _ = main.Write([]byte(`#set text(font: "WenQuanYi Zen Hei")` + "\n= Title\n"))
	chapter, _ := writer.Create("chapter.typ")
	_, _ = chapter.Write([]byte("= Chapter"))
	template, _ := writer.Create("template.typ")
	_, _ = template.Write([]byte("#let project(body) = body"))
	manifest, _ := writer.Create("rinspace.yaml")
	_, _ = manifest.Write([]byte("format: typst"))
	_ = writer.Close()
	observation := observeAdmissionSource(body.Bytes(), "typst", nil)
	if observation.FileCount == nil || *observation.FileCount != 4 || observation.DocumentClass != "article" {
		t.Fatalf("typst archive admission observation = %#v", observation)
	}
	raw := observeAdmissionSource([]byte("= Raw"), "typst", nil)
	if raw.FileCount == nil || *raw.FileCount != 1 || raw.DocumentClass != "article" {
		t.Fatalf("raw typst admission observation = %#v", raw)
	}
}
