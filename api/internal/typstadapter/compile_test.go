package typstadapter

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompilerRejectsUnpinnedOrEscapingInputs(t *testing.T) {
	compiler := Compiler{}
	if err := compiler.Validate(); err == nil {
		t.Fatal("unconfigured Typst compiler was accepted")
	}
	if _, err := compiler.Compile(t.Context(), map[string][]byte{"main.typ": []byte("Hello")}, "main.typ", "html"); err == nil {
		t.Fatal("unconfigured Typst compiler executed")
	}
}

func TestPinnedCompilerBuildsBothTargetsFromCorpus(t *testing.T) {
	binary := os.Getenv("RIN_TYPST_TEST_BINARY")
	font := os.Getenv("RIN_TYPST_TEST_FONT")
	if binary == "" || font == "" {
		t.Skip("pinned Typst compiler and font are not configured")
	}
	compiler := Compiler{
		BinaryPath: binary, BinaryHash: "29273eaa04f6d00edd0c2bec578f565fc9c65be856bfbffc894567c68ed0b237",
		FontPath: font, FontHash: "79c18ebe7b811951e8311bad7103ebeae8c337ed9988ea69e8a78a66cfe029b9",
		Timeout: 90 * time.Second, MaxOutput: 32 << 20,
	}
	root := filepath.Join("..", "..", "..", "corpus", "fixtures", "typst", "book")
	files := map[string][]byte{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)], err = os.ReadFile(path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"html", "pdf"} {
		result, err := compiler.Compile(context.Background(), files, "main.typ", format)
		if err != nil {
			t.Fatalf("pinned %s compile failed: %v: %s", format, err, result.Log)
		}
		if format == "html" && (!bytes.Contains(result.Output, []byte("rin-page-group-theory")) || !bytes.Contains(result.Output, []byte("<math>"))) {
			t.Fatal("Typst HTML lost book pages or MathML")
		}
		if format == "pdf" && !bytes.HasPrefix(result.Output, []byte("%PDF-")) {
			t.Fatal("Typst PDF header is missing")
		}
	}
	frame, err := compiler.Compile(context.Background(), map[string][]byte{
		"main.typ": []byte(`#html.frame(circle(radius: 10pt, fill: blue))`),
	}, "main.typ", "html")
	if err != nil {
		t.Fatalf("pinned html.frame compile failed: %v: %s", err, frame.Log)
	}
	frameResult, err := AdaptHTML(frame.Output, "article", nil)
	if err != nil {
		t.Fatalf("pinned html.frame SVG adaptation failed: %v", err)
	}
	if len(frameResult.Assets) != 1 || !strings.Contains(frameResult.Pages[0].HTML, `data-rin-asset-path="typst-assets/`) {
		t.Fatalf("pinned html.frame drawing was not delivered as SVG asset: %#v", frameResult)
	}
	if _, err := compiler.Compile(context.Background(), map[string][]byte{"../escape.typ": []byte("bad"), "main.typ": []byte("Hello")}, "main.typ", "html"); err == nil || !strings.Contains(err.Error(), "project file") {
		t.Fatalf("Typst path escape was accepted: %v", err)
	}
}
