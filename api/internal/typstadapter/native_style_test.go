package typstadapter

import (
	"os"
	"strings"
	"testing"
)

// This is actual pinned compiler output of the adjacent, entirely synthetic
// source, not a user's work. CI always checks it, even without a local compiler.
func TestNativePresentationGolden(t *testing.T) {
	source, err := os.ReadFile("testdata/native-presentation.html")
	if err != nil {
		t.Fatal(err)
	}
	checkNativePresentation(t, source)
}

func TestPinnedCompilerNativePresentation(t *testing.T) {
	compiler := pinnedCorpusCompiler(t)
	files := map[string][]byte{}
	for _, name := range []string{"native-presentation.typ", "native-presentation.svg"} {
		body, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = body
	}
	result, err := compiler.Compile(t.Context(), files, "native-presentation.typ", "html")
	if err != nil {
		t.Fatalf("pinned presentation compilation: %v %s", err, result.Log)
	}
	checkNativePresentation(t, result.Output)
}

func checkNativePresentation(t *testing.T, source []byte) {
	t.Helper()
	for _, mode := range []string{"article", "book"} {
		result, err := AdaptHTML(source, mode, nil)
		if err != nil {
			t.Fatal(err)
		}
		var page strings.Builder
		for _, part := range result.Pages {
			page.WriteString(part.HTML)
		}
		for _, expected := range []string{
			`text-decoration: underline`, `text-decoration: overline`,
			`font-variant-caps: small-caps`, `font-variant-caps: all-small-caps`,
			`display: inline-block`, `color: #d73948`, `list-style-type: none`,
			`height: auto; width: 24pt`, `height: auto; width: 25%`,
			`height: 34.0157pt; width: auto`, `width: calc(50% + 1em - 12pt)`,
			`display: block; height: 1.090909091em; width: 2.181818182em`,
		} {
			if !strings.Contains(page.String(), expected) {
				t.Fatalf("%s lost native presentation %q: %s", mode, expected, page.String())
			}
		}
		if strings.Count(page.String(), `height: auto; width: 24pt`) != 2 {
			t.Fatal("inline image row lost dimensions")
		}
	}
}

func TestNativeStylePolicyRejectsArbitraryCSSAndWrongElements(t *testing.T) {
	for _, tc := range []struct{ tag, raw string }{
		{"p", "position: fixed"}, {"span", "background: url(https://evil.invalid)"},
		{"span", "color: red; width: 24pt"}, {"div", "list-style-type: none"},
		{"p", "math-shift: compact"}, {"span", "color: rgb(url(evil) 0 0)"},
	} {
		if _, err := sanitizeNativeStyle(tc.tag, tc.raw); err == nil {
			t.Fatalf("accepted unsupported native CSS on %s: %s", tc.tag, tc.raw)
		}
	}
	for _, color := range []string{"#aabbcc", "#aabbcc00", "rgb(10% 20% 30% / 50%)", "hsl(20deg 10% 30%)", "oklab(20% -0.2 0.1)", "oklch(20% 0.2 40deg)", "color(srgb-linear 10% 20% 30%)"} {
		if _, err := sanitizeNativeStyle("span", "color: "+color); err != nil {
			t.Fatalf("rejected native color %s: %v", color, err)
		}
	}
}

func TestNativePresentationInvalidatesPreviousAdapterCache(t *testing.T) {
	profile := Profile{CompilerVersion: "0.15.1", CompilerHash: strings.Repeat("a", 64), FontHash: strings.Repeat("b", 64)}
	current := profile.ID()
	profile.AdapterVersion = "rin-typst-html-adapter/v3"
	if current == profile.ID() {
		t.Fatal("native presentation changes must invalidate v3 cached HTML")
	}
}
