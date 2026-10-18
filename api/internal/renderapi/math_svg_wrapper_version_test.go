package renderapi

import (
	"os"
	"strings"
	"testing"
)

func TestMathSVGWrapperVersionBindsFallbackCache(t *testing.T) {
	const old = "diagram+texlive+dvisvgm"
	got := mathSVGFallbackVersion(Config{DiagramEngineVersion: "diagram", TeXLiveVersion: "texlive", DvisvgmVersion: "dvisvgm"})
	if got != old+"+"+mathSVGWrapperVersion || got == old {
		t.Fatalf("fallback cache omitted new wrapper policy: %q", got)
	}
	source, err := os.ReadFile("../../../engines/texsvg/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `mathWrapperVersion`) || !strings.Contains(string(source), `"`+mathSVGWrapperVersion+`"`) {
		t.Fatal("worker and API fallback wrapper identities disagree")
	}
}
