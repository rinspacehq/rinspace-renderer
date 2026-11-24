package publicationpdf

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

func TestBrowserPrinter(t *testing.T) {
	chromium := os.Getenv("RIN_RENDERER_MARKDOWN_PDF_CHROMIUM")
	if chromium == "" {
		t.Skip("set RIN_RENDERER_MARKDOWN_PDF_CHROMIUM to the locked headless browser")
	}
	script, err := filepath.Abs("../../../engines/markdown-pdf/print.mjs")
	if err != nil {
		t.Fatal(err)
	}
	printer := BrowserPrinter{Node: os.Getenv("RIN_RENDERER_MARKDOWN_NODE_BIN"), Script: script, Chromium: chromium}
	if os.Getenv("RIN_RENDERER_TEST_PDF_ISOLATION") == "1" {
		printer.Isolate = true
		if err := printer.CheckIsolation(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	const formula = `<style>@font-face { font-family: MarkdownMath; src: url('/fonts/mathjax-newcm/woff2/mjx-ncm-n.woff2') format('woff2'); }</style><p style="font-family: MarkdownMath">π</p>`
	pdf, inspection, err := printer.Print(context.Background(), markdownBundle(
		`<h1 id="intro">First chapter</h1><a href="chapters/last.md#intro">Next chapter</a>`+formula,
		`<h1 id="intro">样书终章</h1><p>终章正文</p><a href="chapters/first.md#intro">Back</a>`,
	), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.PageCount != 2 || inspection.SourceBytes != int64(len(pdf)) {
		t.Fatalf("unexpected inspection: %+v", inspection)
	}
	bookmarks, err := api.Bookmarks(bytes.NewReader(pdf), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bookmarks) != 1 || len(bookmarks[0].Kids) != 2 ||
		bookmarks[0].Kids[0].PageFrom != 1 || bookmarks[0].Kids[1].PageFrom != 2 ||
		bookmarks[0].Kids[1].Title != "样书终章" {
		t.Fatalf("unexpected bookmarks: %+v", bookmarks)
	}
	for name, fragment := range map[string]string{
		"undecodable image": `<img alt="fixture" src="data:image/png;base64,iVBORw0KGgo=">`,
		"unavailable font":  strings.Replace(formula, "mjx-ncm-n.woff2", "mjx-ncm-missing.woff2", 1),
		"external font":     strings.Replace(formula, "/fonts/", "https://example.invalid/fonts/", 1),
	} {
		t.Run(name, func(t *testing.T) {
			pdf, _, err := printer.Print(t.Context(), markdownBundle(fragment), false, nil)
			if err == nil || len(pdf) != 0 {
				t.Fatal("unavailable asset produced a PDF")
			}
		})
	}
}
