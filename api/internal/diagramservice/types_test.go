package diagramservice

import "testing"

func TestNormalizeTypePreservesPublicTeXSVGAliases(t *testing.T) {
	want := map[string]string{
		"tikz": "tikzpicture", "tikzpicture": "tikzpicture", "tikz-cd": "tikzcd",
		"pgfplots": "axis", "xy": "xymatrix", "cd": "amscd", "scheme": "chemfig-scheme",
	}
	for alias, canonical := range want {
		if got, ok := NormalizeType(alias); !ok || got != canonical {
			t.Fatalf("NormalizeType(%q) = %q, %v; want %q", alias, got, ok, canonical)
		}
	}
	if _, ok := NormalizeType("shell"); ok {
		t.Fatal("unsupported diagram type was accepted")
	}
}

func TestSupportedTypesPreservePublishedPgfplotsAlias(t *testing.T) {
	for _, kind := range SupportedTypes() {
		if kind == "pgfplots" {
			return
		}
	}
	t.Fatal("published pgfplots alias is missing")
}
