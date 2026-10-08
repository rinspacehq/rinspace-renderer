package mathrender

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestKaTeXRendererRendersDisplayMath(t *testing.T) {
	renderer := NewKaTeXRenderer(testKaTeXScriptPath(t), 5*time.Second, 32<<10)

	result, err := renderer.Render(context.Background(), Request{
		Source:      `\not\exists x`,
		DisplayMode: true,
	})
	if err != nil {
		t.Fatalf("render katex: %v", err)
	}
	if result.Engine != "katex" || result.Version == "" {
		t.Fatalf("expected katex result metadata, got %#v", result)
	}
	for _, expected := range []string{`katex-display`, `aria-hidden="true"`, `∃`} {
		if !strings.Contains(result.HTML, expected) {
			t.Fatalf("expected KaTeX HTML to contain %q, got %s", expected, result.HTML)
		}
	}
	if strings.Contains(result.HTML, `<math`) {
		t.Fatalf("expected KaTeX engine to omit MathML output, got %s", result.HTML)
	}
}

func TestKaTeXRendererRendersBook260CommonMath(t *testing.T) {
	renderer := NewKaTeXRenderer(testKaTeXScriptPath(t), 5*time.Second, 32<<10)
	cases := []struct {
		name     string
		source   string
		contains []string
	}{
		{
			name:     "dirac ket",
			source:   `\ket{x_{1}}\pm\ket{x_{2}}`,
			contains: []string{`katex-display`, `⟩`, `±`},
		},
		{
			name:     "rotation matrix",
			source:   `R S = \begin{pmatrix} \cos \frac{\pi}{6}&-\sin \frac{\pi}{6}\\[2pt] \sin \frac{\pi}{6}&\cos \frac{\pi}{6} \end{pmatrix}`,
			contains: []string{`katex-display`, `mtable`, `cos`, `sin`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := renderer.Render(context.Background(), Request{
				Source:      tc.source,
				DisplayMode: true,
			})
			if err != nil {
				t.Fatalf("render katex: %v", err)
			}
			for _, expected := range tc.contains {
				if !strings.Contains(result.HTML, expected) {
					t.Fatalf("expected KaTeX HTML to contain %q, got %s", expected, result.HTML)
				}
			}
			if strings.Contains(result.HTML, `<math`) {
				t.Fatalf("expected KaTeX engine to omit MathML output, got %s", result.HTML)
			}
		})
	}
}

func TestKaTeXRendererReturnsStructuredRenderError(t *testing.T) {
	renderer := NewKaTeXRenderer(testKaTeXScriptPath(t), 5*time.Second, 32<<10)

	_, err := renderer.Render(context.Background(), Request{
		Source:      `\definitelyunsupportedcommand{x}`,
		DisplayMode: true,
	})
	var renderErr *RenderError
	if !errors.As(err, &renderErr) {
		t.Fatalf("expected RenderError, got %T: %v", err, err)
	}
	if renderErr.Code == "" || renderErr.Message == "" {
		t.Fatalf("expected structured KaTeX error, got %#v", renderErr)
	}
}

func TestKaTeXRendererRendersBatch(t *testing.T) {
	renderer := NewKaTeXRenderer(testKaTeXScriptPath(t), 5*time.Second, 32<<10)

	results, err := renderer.RenderBatch(context.Background(), []Request{
		{Source: `x+y`, DisplayMode: false},
		{Source: `\not\exists x`, DisplayMode: true},
		{Source: `\definitelyunsupportedcommand{x}`, DisplayMode: true},
	})
	if err != nil {
		t.Fatalf("render katex batch: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 batch results, got %d", len(results))
	}
	if results[0].Err != nil || !strings.Contains(results[0].Result.HTML, `katex`) {
		t.Fatalf("expected first batch result to render, got %#v", results[0])
	}
	if results[1].Err != nil || !strings.Contains(results[1].Result.HTML, `katex-display`) {
		t.Fatalf("expected display batch result to render, got %#v", results[1])
	}
	var renderErr *RenderError
	if !errors.As(results[2].Err, &renderErr) || renderErr.Code == "" {
		t.Fatalf("expected structured batch render error, got %#v", results[2].Err)
	}
}

func TestKaTeXRendererRejectsOversizedSource(t *testing.T) {
	renderer := NewKaTeXRenderer(testKaTeXScriptPath(t), 5*time.Second, 4)

	_, err := renderer.Render(context.Background(), Request{
		Source:      `x + y`,
		DisplayMode: true,
	})
	var renderErr *RenderError
	if !errors.As(err, &renderErr) || renderErr.Code != "math.source.too_large" {
		t.Fatalf("expected oversized source error, got %T: %v", err, err)
	}
}

func testKaTeXScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../../engines/katex/render-katex.mjs"))
}
