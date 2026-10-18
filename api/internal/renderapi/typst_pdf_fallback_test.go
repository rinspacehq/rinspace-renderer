package renderapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestTypstNativeCapabilityLossCompilesInspectedPDF(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		t.Run(fmt.Sprint(exitCode), func(t *testing.T) {
			log := "warning: cancel was ignored during MathML export"
			if exitCode != 0 {
				log = "error: custom content is not supported in HTML export"
			}
			cfg := typstFallbackTestConfig(t, log, exitCode)
			result := runTypstPublication(t, cfg)
			if result.Err != nil || result.Canonical == nil || !result.Response.Fallback || result.Response.FallbackEngine != "typst-pdf" {
				t.Fatalf("PDF fallback failed: %v %#v", result.Err, result.Diagnostics)
			}
			if err := result.Canonical.Validate(); err != nil {
				t.Fatal(err)
			}
			if result.Canonical.ContentKind != "typst" || result.Canonical.Versions["typstReading"] != "pdf" ||
				len(result.Canonical.Inline.Pages) != 1 || !strings.Contains(result.Response.HTML, `data-rin-pdf-pages="1"`) ||
				!strings.Contains(result.Response.HTML, `data-rin-pdf-commit="`+strings.Repeat("a", 40)+`"`) {
				t.Fatal("PDF reading lost source identity or reader metadata")
			}
		})
	}
}

func TestTypstSourceErrorsAndSecurityFailuresNeverTriggerPDF(t *testing.T) {
	for _, log := range []string{"error: unknown variable: x", "error: failed to download package", "warning: cancel was ignored during MathML export\nerror: unclosed delimiter"} {
		result := runTypstPublication(t, typstFallbackTestConfig(t, log, 1))
		if result.Err == nil || result.Response.Fallback || typstDiagnosticCode(result) != "typst.compile.failed" {
			t.Fatalf("source error masked by PDF: %s %#v", log, result)
		}
	}
	cfg := typstTestConfig(t, `<html><body><script>evil()</script></body></html>`)
	result := runTypstPublication(t, cfg)
	if result.Err == nil || result.Response.Fallback {
		t.Fatal("sanitizer failure masked by PDF")
	}
}

func TestTypstMathJaxIsDefaultAndNativeIsExplicit(t *testing.T) {
	t.Setenv("RIN_RENDERER_TYPST_MATH_RENDER", "")
	if !ConfigFromEnv().TypstMathRender {
		t.Fatal("Typst must use the shared MathJax by default")
	}
	t.Setenv("RIN_RENDERER_TYPST_MATH_RENDER", "false")
	if ConfigFromEnv().TypstMathRender {
		t.Fatal("explicit native mode ignored")
	}
}

func TestPinnedTypstHTMLLossFallsBackToRealPDF(t *testing.T) {
	binary, font := os.Getenv("RIN_TYPST_TEST_BINARY"), os.Getenv("RIN_TYPST_TEST_FONT")
	if binary == "" || font == "" {
		t.Skip("pinned Typst compiler/font not configured")
	}
	cfg := typstTestConfig(t, "unused")
	cfg.TypstBin, cfg.TypstFontPath = binary, font
	cfg.TypstBinHash = "29273eaa04f6d00edd0c2bec578f565fc9c65be856bfbffc894567c68ed0b237"
	cfg.TypstFontHash = "79c18ebe7b811951e8311bad7103ebeae8c337ed9988ea69e8a78a66cfe029b9"
	executor := NewProjectExecutor(cfg)
	t.Cleanup(func() { _ = executor.Close() })
	result := executor.Execute(t.Context(), ProjectExecutionRequest{ContentKind: "typst", JobID: "job-real-typst-fallback", RequestID: "request-real-typst-fallback",
		ArchiveName: "project.zip", Archive: zipTypstProject(t, map[string][]byte{"main.typ": []byte("= Source\n$ cancel(x) $\n")}),
		Title: "Fallback", ExplicitMainFile: "main.typ", DocumentMode: "article", ProjectID: "article:42", SourceCommit: strings.Repeat("a", 40), ControlProjectHash: strings.Repeat("b", 64)})
	if result.Err != nil || result.Canonical == nil || !result.Response.Fallback || !strings.Contains(result.Response.HTML, `data-rin-pdf="`) {
		t.Fatalf("real compiler did not recover from unsupported HTML: %v %#v", result.Err, result.Diagnostics)
	}
	if err := result.Canonical.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTypstProfileIncludesMathJaxAndFontVersions(t *testing.T) {
	cfg := typstTestConfig(t, "unused")
	cfg.TypstMathRender = true
	first := TypstHTMLProfileID(cfg)
	changed := cfg
	changed.MathJaxVersion = "different-version"
	if TypstHTMLProfileID(changed) == first {
		t.Fatal("MathJax change reused the old profile")
	}
	changed = cfg
	changed.MathJaxFontVersion = "different-font"
	if TypstHTMLProfileID(changed) == first {
		t.Fatal("font change reused the old profile")
	}
}

func runTypstPublication(t *testing.T, cfg Config) ProjectExecutionResult {
	t.Helper()
	executor := NewProjectExecutor(cfg)
	t.Cleanup(func() { _ = executor.Close() })
	return executor.Execute(context.Background(), ProjectExecutionRequest{ContentKind: "typst", JobID: "job-typst-fallback", RequestID: "request-typst-fallback",
		ArchiveName: "project.zip", Archive: zipTypstProject(t, map[string][]byte{"main.typ": []byte("= Source\nBody\n")}),
		Title: "Reading", ExplicitMainFile: "main.typ", DocumentMode: "book", ProjectID: "book:42", SourceCommit: strings.Repeat("a", 40), ControlProjectHash: strings.Repeat("b", 64)})
}

func typstFallbackTestConfig(t *testing.T, log string, code int) Config {
	t.Helper()
	cfg := typstTestConfig(t, "unused")
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	script := fmt.Sprintf("#!/bin/sh\nprevious=''\nformat=''\nfor arg in \"$@\"; do\n if [ \"$previous\" = '--format' ]; then format=\"$arg\"; fi\n previous=\"$arg\"\n out=\"$arg\"\ndone\nif [ \"$format\" = 'html' ]; then\n printf '%%s' '%s' | base64 -d >&2\n printf '%%s' '%s' | base64 -d > \"$out\"\n exit %d\nfi\nprintf '%%s' '%s' | base64 -d > \"$out\"\n",
		encode(log), encode("<html><body><p>Partial HTML</p></body></html>"), code, base64.StdEncoding.EncodeToString(authorPDFTestBytes()))
	if err := os.WriteFile(cfg.TypstBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	cfg.TypstBinHash = fileSHA256(t, cfg.TypstBin)
	return cfg
}
