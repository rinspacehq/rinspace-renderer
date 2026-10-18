package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestPublicationProbeUsesCanonicalIdentityAndNormalCompilerBudget(t *testing.T) {
	binary, font := os.Getenv("RIN_TYPST_TEST_BINARY"), os.Getenv("RIN_TYPST_TEST_FONT")
	if binary == "" || font == "" {
		t.Skip("pinned native compiler/font not configured")
	}
	t.Setenv("RIN_RENDERER_TYPST_TIMEOUT_SECONDS", "120")
	for _, source := range []string{"= Synthetic local publication\n", "= Official HTML loss\n$ cancel(x) $\n"} {
		result, err := checkPublication(map[string][]byte{"main.typ": []byte(source)}, "main.typ", "article", binary, font, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if result["passed"] != true || result["compileTimeoutSeconds"] != float64(120) {
			t.Fatalf("probe bypassed normal publication configuration: %#v", result)
		}
	}
}

func TestCanonicalOutputIsCompletePrivateAndExclusive(t *testing.T) {
	binary, font := os.Getenv("RIN_TYPST_TEST_BINARY"), os.Getenv("RIN_TYPST_TEST_FONT")
	if binary == "" || font == "" {
		t.Skip("pinned native compiler/font not configured")
	}
	files := map[string][]byte{"main.typ": []byte("= Figure\n#image(\"figure.svg\")\n"), "figure.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><path d="M0 0L10 10"/></svg>`)}
	output := filepath.Join(t.TempDir(), "canonical.json")
	report, err := checkPublication(files, "main.typ", "article", binary, font, "", "", output)
	if err != nil || report["embeddedImages"] != 1 {
		t.Fatalf("canonical probe: %#v %v", report, err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var result contracts.RenderResult
	if err := json.Unmarshal(before, &result); err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("canonical output is not private")
	}
	if _, err := checkPublication(files, "main.typ", "article", binary, font, "", "", output); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing output was not protected: %v", err)
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing output bytes were overwritten")
	}
}
