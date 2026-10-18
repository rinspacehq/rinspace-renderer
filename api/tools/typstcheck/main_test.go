package main

import (
	"os"
	"testing"
)

func TestPublicationProbeUsesCanonicalIdentityAndNormalCompilerBudget(t *testing.T) {
	binary, font := os.Getenv("RIN_TYPST_TEST_BINARY"), os.Getenv("RIN_TYPST_TEST_FONT")
	if binary == "" || font == "" {
		t.Skip("pinned native compiler/font not configured")
	}
	t.Setenv("RIN_RENDERER_TYPST_TIMEOUT_SECONDS", "120")
	for _, source := range []string{"= Synthetic local publication\n", "= Official HTML loss\n$ cancel(x) $\n"} {
		result, err := checkPublication(map[string][]byte{"main.typ": []byte(source)}, "main.typ", "article", binary, font, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if result["passed"] != true || result["compileTimeoutSeconds"] != float64(120) {
			t.Fatalf("probe bypassed normal publication configuration: %#v", result)
		}
	}
}
