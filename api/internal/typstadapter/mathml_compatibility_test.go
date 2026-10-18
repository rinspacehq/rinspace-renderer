package typstadapter

import (
	"os"
	"strings"
	"testing"
)

// This checked-in output is generated from our synthetic .typ fixture using
// the pinned compiler. Unlike the optional executable test, CI cannot skip it.
func TestPinnedMathMLOutputIsAlwaysAccepted(t *testing.T) {
	source, err := os.ReadFile("testdata/mathml-compatibility.html")
	if err != nil {
		t.Fatal(err)
	}
	checkMathMLCompatibility(t, source)
}

func TestPinnedCompilerMathMLCompatibility(t *testing.T) {
	compiler := pinnedCorpusCompiler(t)
	source, err := os.ReadFile("testdata/mathml-compatibility.typ")
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(t.Context(), map[string][]byte{"main.typ": source}, "main.typ", "html")
	if err != nil {
		t.Fatalf("pinned MathML compilation failed: %v %s", err, result.Log)
	}
	checkMathMLCompatibility(t, result.Output)
}

func checkMathMLCompatibility(t *testing.T, source []byte) {
	t.Helper()
	attributes := []string{`fence="false"`, `separator="true"`, `symmetric="true"`, `largeop="true"`,
		`movablelimits="false"`, `minsize="150%"`, `scriptlevel="1"`, `scriptlevel="2"`,
		`displaystyle="false"`, `displaystyle="true"`}
	attributes = append(attributes, "<mmultiscripts>", "<mprescripts")
	for _, attribute := range attributes {
		if !strings.Contains(string(source), attribute) {
			t.Fatalf("fixture does not exercise %s", attribute)
		}
	}
	for _, mode := range []string{"article", "book"} {
		result, err := AdaptHTML(source, mode, nil)
		if err != nil || len(result.Pages) != 1 {
			t.Fatalf("real compiler output rejected in %s: %v", mode, err)
		}
		for _, attribute := range attributes {
			if !strings.Contains(result.Pages[0].HTML, attribute) {
				t.Fatalf("lost MathML semantics in %s: %s", mode, attribute)
			}
		}
	}
}

func TestMathMLNewAttributesRemainFailClosed(t *testing.T) {
	for _, fragment := range []string{
		`<p fence="false">x</p>`, `<mo fence="url(evil)">x</mo>`, `<mi separator="true">x</mi>`,
		`<mo symmetric="yes">x</mo>`, `<mo largeop="1">x</mo>`, `<mo movablelimits="no">x</mo>`,
		`<mo minsize="-1em">x</mo>`, `<mo minsize="url(x)">x</mo>`, `<mi minsize="1em">x</mi>`,
		`<p displaystyle="true">x</p>`, `<mi displaystyle="1">x</mi>`, `<p scriptlevel="0">x</p>`,
		`<mi scriptlevel="-1">x</mi>`, `<mi scriptlevel="3">x</mi>`, `<mo fence="false" fence="true">x</mo>`,
		`<mo fence="false" onload="evil()">x</mo>`, `<mo maxsize="infinity">x</mo>`,
	} {
		if _, err := AdaptHTML([]byte(`<html><body><math>`+fragment+`</math></body></html>`), "article", nil); err == nil {
			t.Fatalf("unsafe or unsupported MathML was accepted: %s", fragment)
		}
	}
}

func TestNewAdapterInvalidatesPreviousRenderProfile(t *testing.T) {
	profile := Profile{CompilerVersion: "0.15.1", CompilerHash: strings.Repeat("a", 64), FontHash: strings.Repeat("b", 64),
		PackageHash: strings.Repeat("c", 64), FinalOutputVersion: "rin-final-output/v1"}
	current := profile.ID()
	profile.AdapterVersion = "rin-typst-html-adapter/v1"
	if current == profile.ID() {
		t.Fatal("changed adapter must not reuse previous cached renderings")
	}
}
