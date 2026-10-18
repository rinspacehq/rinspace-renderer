package typstadapter

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/mathrender"
)

// The default presentation uses the same pinned MathJax runtime as LaTeX.
func TestMathMLUsesSharedPinnedMathJaxInput(t *testing.T) {
	renderer := mathrender.NewMathJaxRenderer("../../../engines/mathjax/render-mathjax.mjs", 10*time.Second, 32<<10)
	adapted, err := AdaptHTML([]byte(`<html><body><h2>公式</h2><p>令 <math><msup><mi>x</mi><mn>2</mn></msup></math> 等于一。</p></body></html>`), "article", nil)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := RenderMathML(t.Context(), adapted, renderer)
	if err != nil {
		t.Fatal(err)
	}
	fragment := converted.Pages[0].HTML
	for _, expected := range []string{
		"<mjx-container",
		`data-rin-math-engine="mathjax-chtml"`,
		`data-rin-math-source="&lt;math&gt;`,
		`<mjx-assistive-mml`,
		`<math xmlns="http://www.w3.org/1998/Math/MathML">`,
		`aria-hidden="true"`,
		"mjx-assistive-mml {",
	} {
		if !strings.Contains(fragment, expected) {
			t.Fatalf("MathML alternative did not produce %q: %s", expected, fragment)
		}
	}
	// The visible CHTML layer must stay free of raw MathML; the only MathML in
	// the fragment is the assistive mirror that screen readers consume.
	assistive := strings.Index(fragment, "<mjx-assistive-mml")
	visible := fragment
	if assistive >= 0 {
		visible = fragment[:assistive]
	}
	if assistive < 0 || strings.Contains(visible, "<math") {
		t.Fatalf("MathML alternative leaked unstyled MathML outside the assistive mirror: %s", fragment)
	}
}

type countingMathRenderer struct {
	batches []int
	sources []mathrender.Request
}

func (renderer *countingMathRenderer) Render(context.Context, mathrender.Request) (mathrender.Result, error) {
	panic("batch only")
}
func (renderer *countingMathRenderer) RenderBatch(_ context.Context, requests []mathrender.Request) ([]mathrender.BatchResult, error) {
	renderer.batches = append(renderer.batches, len(requests))
	renderer.sources = append(renderer.sources, requests...)
	results := make([]mathrender.BatchResult, len(requests))
	for index := range results {
		results[index].Result = mathrender.Result{HTML: `<mjx-container><mjx-math>x</mjx-math></mjx-container>`, Engine: "mathjax-chtml", CSS: "mjx-container{display:inline-block}"}
	}
	return results, nil
}

func TestMathMLTranslatesNativeAlignmentBeforeMathJax(t *testing.T) {
	source := `<html><body><math><mtable class="multiline-equation aligned"><mtr><mtd><mi>x</mi></mtd><mtd><mi>y</mi></mtd></mtr></mtable></math></body></html>`
	adapted, err := AdaptHTML([]byte(source), "article", nil)
	if err != nil {
		t.Fatal(err)
	}
	renderer := &countingMathRenderer{}
	if _, err := RenderMathML(t.Context(), adapted, renderer); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`columnalign="right left"`, `columnspacing="0em"`, `rowspacing="0.5em"`} {
		if !strings.Contains(renderer.sources[0].Source, expected) {
			t.Fatalf("lost native MathML layout: %s", expected)
		}
	}
}

func TestMathMLDeduplicatesFormulasAndPreservesEquationAnchors(t *testing.T) {
	adapted, err := AdaptHTML([]byte(`<html><body><h2 id="one">One</h2><math id="equation-one"><mi>x</mi></math><p><a href="#equation-two">Second</a></p><h2 id="two">Two</h2><math id="equation-two"><mi>x</mi></math></body></html>`), "book", nil)
	if err != nil {
		t.Fatal(err)
	}
	renderer := &countingMathRenderer{}
	converted, err := RenderMathML(t.Context(), adapted, renderer)
	if err != nil {
		t.Fatal(err)
	}
	if len(renderer.batches) != 1 || renderer.batches[0] != 1 {
		t.Fatalf("identical formulas were not reused: %v", renderer.batches)
	}
	for i, id := range []string{"equation-one", "equation-two"} {
		if strings.Count(converted.Pages[i].HTML, `id="`+id+`"`) != 1 {
			t.Fatalf("lost or duplicated equation anchor %s", id)
		}
	}
	if !strings.Contains(converted.Pages[0].HTML, `href="#equation-two"`) || !strings.Contains(converted.Pages[0].HTML, `data-rin-page="two"`) {
		t.Fatalf("lost cross-page equation link: %s", converted.Pages[0].HTML)
	}
}

func TestMathMLBoundsLongBookBatches(t *testing.T) {
	var source strings.Builder
	source.WriteString("<html><body><h2>Book</h2>")
	for i := 0; i < 260; i++ {
		source.WriteString(fmt.Sprintf("<math><mn>%d</mn></math>", i))
	}
	source.WriteString("</body></html>")
	adapted, err := AdaptHTML([]byte(source.String()), "book", nil)
	if err != nil {
		t.Fatal(err)
	}
	renderer := &countingMathRenderer{}
	if _, err := RenderMathML(t.Context(), adapted, renderer); err != nil {
		t.Fatal(err)
	}
	if len(renderer.batches) != 3 || renderer.batches[0] != 128 || renderer.batches[2] != 4 {
		t.Fatalf("unbounded math operation: %v", renderer.batches)
	}
}
