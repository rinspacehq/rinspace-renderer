package typstadapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/mathrender"
)

// This optional integration test reads original synthetic HTML produced with
// the pinned compiler. The corpus is supplied by a consumer, not copied from
// user works into this public repository.
func TestLearningCorpusPreservesSemanticMathAndReferences(t *testing.T) {
	directory := os.Getenv("RIN_TYPST_LEARNING_CORPUS_DIR")
	if directory == "" {
		t.Skip("learning corpus is not configured")
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.html"))
	if err != nil || len(files) < 12 {
		t.Fatalf("expected at least 12 generated learning documents: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			result, err := AdaptHTML(source, "article", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Pages) != 1 || len(result.Assets) != 0 {
				t.Fatalf("text/math lesson became a visual asset: pages=%d assets=%d", len(result.Pages), len(result.Assets))
			}
			fragment := result.Pages[0].HTML
			if strings.Count(fragment, "<math") != strings.Count(string(source), "<math") {
				t.Fatal("semantic formulas were lost by the adapter")
			}
			if strings.Contains(fragment, "<script") || strings.Contains(fragment, "<svg") {
				t.Fatal("unexpected active content or whole-document SVG")
			}
			if filepath.Base(file) == "kernel.html" &&
				(!strings.Contains(fragment, `id="eq-kernel"`) || !strings.Contains(fragment, `href="#eq-kernel"`)) {
				t.Fatal("equation identity or reference was lost")
			}
			t.Logf("native=%d bytes final=%d bytes formulas=%d", len(source), len(fragment), strings.Count(fragment, "<math"))
		})
	}
}

func TestLearningCorpusMathJaxPreservesEquationsAndAnchors(t *testing.T) {
	directory := os.Getenv("RIN_TYPST_LEARNING_CORPUS_DIR")
	if directory == "" {
		t.Skip("learning corpus is not configured")
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.html"))
	if err != nil || len(files) < 12 {
		t.Fatalf("expected at least 12 generated learning documents: %v", err)
	}
	renderer := mathrender.NewMathJaxRenderer("../../../engines/mathjax/render-mathjax.mjs", 10*time.Second, 32<<10)
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			native, err := AdaptHTML(source, "article", nil)
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			converted, err := RenderMathML(t.Context(), native, renderer)
			if err != nil {
				t.Fatal(err)
			}
			fragment := converted.Pages[0].HTML
			if strings.Count(fragment, "<mjx-assistive-mml") != strings.Count(string(source), "<math") {
				t.Fatal("MathJax conversion lost accessible equations")
			}
			if filepath.Base(file) == "kernel.html" &&
				(!strings.Contains(fragment, `id="eq-kernel"`) || !strings.Contains(fragment, `href="#eq-kernel"`)) {
				t.Fatal("MathJax conversion lost equation identity or reference")
			}
			if output := os.Getenv("RIN_TYPST_LEARNING_CHTML_OUTPUT_DIR"); output != "" {
				if !filepath.IsAbs(output) {
					t.Fatal("CHTML output directory must be absolute")
				}
				if err := os.MkdirAll(output, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(output, filepath.Base(file)), []byte(fragment), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("final=%d bytes formulas=%d conversion=%s", len(fragment), strings.Count(fragment, "<mjx-assistive-mml"), time.Since(started))
		})
	}
}

func TestPinnedLearningPrefixWaitsForForwardReference(t *testing.T) {
	compiler := pinnedCorpusCompiler(t)
	prefix := "#set math.equation(numbering: \"(1)\")\n= First step\nSee @later.\n"
	partial, err := compiler.Compile(t.Context(), map[string][]byte{"main.typ": []byte(prefix)}, "main.typ", "html")
	if err == nil || !strings.Contains(partial.Log, "later") {
		t.Fatal("an unresolved forward reference must not be treated as ready output")
	}
	complete := prefix + "\n= Second step\n$ x = 2 $ <later>\n"
	compiled, err := compiler.Compile(t.Context(), map[string][]byte{"main.typ": []byte(complete)}, "main.typ", "html")
	if err != nil {
		t.Fatalf("complete dependency prefix failed: %v: %s", err, compiled.Log)
	}
	adapted, err := AdaptHTML(compiled.Output, "article", nil)
	if err != nil || len(adapted.Pages) != 1 {
		t.Fatalf("complete prefix failed adaptation: %v", err)
	}
	if !strings.Contains(adapted.Pages[0].HTML, `id="later"`) || !strings.Contains(adapted.Pages[0].HTML, `href="#later"`) {
		t.Fatal("resolved forward reference or equation anchor missing")
	}
}
