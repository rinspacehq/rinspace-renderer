package typstadapter

import (
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/mathrender"
)

// This keeps the alternative MathJax presentation path measurable without
// making it the default Typst publication profile.
func TestMathMLAlternativeUsesPinnedMathJaxInput(t *testing.T) {
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
