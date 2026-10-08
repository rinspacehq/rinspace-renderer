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

func TestMathJaxRendererRendersDisplayMath(t *testing.T) {
	renderer := NewMathJaxRenderer(testMathJaxScriptPath(t), 5*time.Second, 32<<10)
	result, err := renderer.Render(context.Background(), Request{
		Source:      `\not\exists x`,
		DisplayMode: true,
	})
	if err != nil {
		t.Fatalf("render mathjax: %v", err)
	}
	if result.Engine != "mathjax-chtml" || result.Version == "" {
		t.Fatalf("expected MathJax metadata, got %#v", result)
	}
	for _, expected := range []string{`mjx-container`, `display="true"`, `∄`} {
		if !strings.Contains(result.HTML, expected) {
			t.Fatalf("expected MathJax HTML to contain %q, got %s", expected, result.HTML)
		}
	}
	if !strings.Contains(result.CSS, `/fonts/mathjax-newcm/woff2`) {
		t.Fatalf("expected MathJax CSS with local font URL, got %s", result.CSS)
	}
}

func TestMathJaxRendererRendersBatch(t *testing.T) {
	renderer := NewMathJaxRenderer(testMathJaxScriptPath(t), 5*time.Second, 32<<10)
	results, err := renderer.RenderBatch(context.Background(), []Request{
		{Source: `x^2`, DisplayMode: false},
		{Source: `\begin{bmatrix}1\\2\end{bmatrix}`, DisplayMode: true},
	})
	if err != nil {
		t.Fatalf("render mathjax batch: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for index, result := range results {
		if result.Err != nil {
			t.Fatalf("result %d error: %v", index, result.Err)
		}
		if result.Result.Engine != "mathjax-chtml" || !strings.Contains(result.Result.HTML, "mjx-container") {
			t.Fatalf("unexpected result %d: %#v", index, result.Result)
		}
		if !strings.Contains(result.Result.CSS, `/fonts/mathjax-newcm/woff2`) {
			t.Fatalf("expected batch CSS on result %d, got %s", index, result.Result.CSS)
		}
	}
}

func TestMathJaxRendererDoesNotBreakInlineMatrices(t *testing.T) {
	renderer := NewMathJaxRenderer(testMathJaxScriptPath(t), 5*time.Second, 32<<10)
	result, err := renderer.Render(context.Background(), Request{
		Source:      `\mathbf{x}={\begin{bmatrix}a_{1}\\a_{2}\\\cdots\\a_{n}\end{bmatrix}}=\ket{x}`,
		DisplayMode: false,
	})
	if err != nil {
		t.Fatalf("render mathjax inline matrix: %v", err)
	}
	for _, unexpected := range []string{`mjx-break`, `breakable="true"`} {
		if strings.Contains(result.HTML, unexpected) {
			t.Fatalf("expected inline matrix to render without %q, got %s", unexpected, result.HTML)
		}
	}
}

func TestMathJaxRendererDoesNotBreakDisplayMatrices(t *testing.T) {
	renderer := NewMathJaxRenderer(testMathJaxScriptPath(t), 5*time.Second, 32<<10)
	result, err := renderer.Render(context.Background(), Request{
		Source:      `RS=\begin{pmatrix}\cos\frac{\pi}{6}&-\sin\frac{\pi}{6}\\[2pt]\sin\frac{\pi}{6}&\cos\frac{\pi}{6}\end{pmatrix}`,
		DisplayMode: true,
	})
	if err != nil {
		t.Fatalf("render mathjax display matrix: %v", err)
	}
	if !strings.Contains(result.HTML, `display="true"`) {
		t.Fatalf("expected display MathJax HTML, got %s", result.HTML)
	}
	for _, unexpected := range []string{`mjx-break`, `breakable="true"`} {
		if strings.Contains(result.HTML, unexpected) {
			t.Fatalf("expected display matrix to render without %q, got %s", unexpected, result.HTML)
		}
	}
}

func TestMathJaxRendererReturnsStructuredRenderError(t *testing.T) {
	renderer := NewMathJaxRenderer(testMathJaxScriptPath(t), 5*time.Second, 32<<10)
	_, err := renderer.Render(context.Background(), Request{
		Source:      `\definitelyunsupportedcommand{x}`,
		DisplayMode: false,
	})
	if err == nil {
		t.Fatal("expected render error")
	}
	var renderErr *RenderError
	if !errors.As(err, &renderErr) {
		t.Fatalf("expected RenderError, got %T: %v", err, err)
	}
	if renderErr.Code == "" || !strings.Contains(renderErr.Error(), `definitelyunsupportedcommand`) {
		t.Fatalf("expected structured MathJax error, got %#v", renderErr)
	}
}

// LaTeX works are resolved by LaTeXML from the author's own preamble. Notation
// the document never declared must therefore fail loudly here instead of being
// served from an engine-side recovery pack: silently inventing a definition
// publishes math the source cannot compile and disagrees with the TeX SVG
// fallback and the reader.
func TestMathJaxRendererFailsOnUndeclaredNotation(t *testing.T) {
	renderer := NewMathJaxRenderer(testMathJaxScriptPath(t), 5*time.Second, 32<<10)
	for _, source := range []string{
		`\R \subset \C`,
		`\Hom(T,\bbG_{m}) = \bbZ^{n}`,
		`\PGL_{3}`,
		`\Spec k`,
	} {
		_, err := renderer.Render(context.Background(), Request{Source: source, DisplayMode: false})
		if err == nil {
			t.Fatalf("expected %q to fail until the source declares it", source)
		}
		var renderErr *RenderError
		if !errors.As(err, &renderErr) || renderErr.Code == "" {
			t.Fatalf("expected a structured render error for %q, got %T: %v", source, err, err)
		}
	}
}

// The supported route is the author's own declaration: a macro the project or
// request supplies renders, and no engine default can shadow it.
func TestMathJaxRendererHonoursDeclaredMacros(t *testing.T) {
	renderer := NewMathJaxRenderer(testMathJaxScriptPath(t), 5*time.Second, 32<<10)
	result, err := renderer.Render(context.Background(), Request{
		Source:      `\R`,
		DisplayMode: false,
		Macros:      map[string]string{`\R`: `\mathcal{R}`},
	})
	if err != nil {
		t.Fatalf("render declared macro: %v", err)
	}
	if !strings.Contains(result.HTML, "NCM-C") {
		t.Fatalf("expected the declared \\R definition to be used, got %s", result.HTML)
	}
}

// amsmath defines \boldsymbol, so an expanded LaTeXML formula may legitimately
// carry it and the TeX input has to be able to render it.
func TestMathJaxRendererRendersAmsmathBoldsymbol(t *testing.T) {
	renderer := NewMathJaxRenderer(testMathJaxScriptPath(t), 5*time.Second, 32<<10)
	result, err := renderer.Render(context.Background(), Request{Source: `\boldsymbol{x}^2`, DisplayMode: false})
	if err != nil {
		t.Fatalf("render boldsymbol: %v", err)
	}
	if !strings.Contains(result.HTML, "mjx-container") || strings.Contains(result.HTML, "mjx-merror") {
		t.Fatalf("expected boldsymbol to render, got %s", result.HTML)
	}
}

func testMathJaxScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../../engines/mathjax/render-mathjax.mjs"))
}
