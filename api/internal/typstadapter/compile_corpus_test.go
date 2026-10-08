package typstadapter

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// pinnedCorpusCompiler returns the real pinned Typst compiler when the local
// corpus environment is configured. These cases cannot be simulated with a fake
// executor: they prove how the sealed spool resolves nested includes and how an
// unavailable offline package fails closed.
func pinnedCorpusCompiler(t *testing.T) Compiler {
	t.Helper()
	binary := os.Getenv("RIN_TYPST_TEST_BINARY")
	font := os.Getenv("RIN_TYPST_TEST_FONT")
	if binary == "" || font == "" {
		t.Skip("pinned Typst compiler and font are not configured")
	}
	return Compiler{
		BinaryPath: binary, BinaryHash: "29273eaa04f6d00edd0c2bec578f565fc9c65be856bfbffc894567c68ed0b237",
		FontPath: font, FontHash: "79c18ebe7b811951e8311bad7103ebeae8c337ed9988ea69e8a78a66cfe029b9",
		Timeout: 90 * time.Second, MaxOutput: 32 << 20,
	}
}

func TestPinnedCompilerResolvesNestedIncludesInBothTargets(t *testing.T) {
	compiler := pinnedCorpusCompiler(t)
	files := map[string][]byte{
		"main.typ": []byte("= Root\n\n#include \"chapters/one.typ\"\n"),
		"chapters/one.typ": []byte(
			"== Chapter one\n\n#include \"two.typ\"\n"),
		"chapters/two.typ": []byte(
			"NESTED_DEPTH_TWO_MARKER\n"),
		"assets/notes/three.typ": []byte(
			"NESTED_DEPTH_THREE_MARKER\n"),
	}
	for _, format := range []string{"html", "pdf"} {
		result, err := compiler.Compile(context.Background(), files, "main.typ", format)
		if err != nil {
			t.Fatalf("nested %s compile failed: %v: %s", format, err, result.Log)
		}
		if format == "html" {
			if !bytes.Contains(result.Output, []byte("NESTED_DEPTH_TWO_MARKER")) {
				t.Fatal("Typst HTML lost the chapter that main.typ includes")
			}
			if !bytes.Contains(result.Output, []byte("Chapter one")) {
				t.Fatal("Typst HTML lost the nested heading")
			}
		} else if !bytes.HasPrefix(result.Output, []byte("%PDF-")) {
			t.Fatal("nested Typst PDF header is missing")
		}
	}

	// A deeper relative include from a sibling directory must resolve too.
	deeper := map[string][]byte{
		"main.typ":               []byte("= Root\n\n#include \"assets/notes/three.typ\"\n"),
		"assets/notes/three.typ": []byte("NESTED_DEPTH_THREE_MARKER\n"),
	}
	result, err := compiler.Compile(context.Background(), deeper, "main.typ", "html")
	if err != nil {
		t.Fatalf("deeper nested html compile failed: %v: %s", err, result.Log)
	}
	if !bytes.Contains(result.Output, []byte("NESTED_DEPTH_THREE_MARKER")) {
		t.Fatal("Typst HTML lost a doubly nested include")
	}

	// An include that escapes the project root must stay rejected even though
	// the file exists on the host.
	escaping := map[string][]byte{
		"main.typ": []byte("= Root\n\n#include \"../../etc/hostname\"\n"),
	}
	if _, err := compiler.Compile(context.Background(), escaping, "main.typ", "html"); err == nil {
		t.Fatal("Typst accepted an include that escapes the project root")
	}
}

func TestPinnedCompilerMissingOfflinePackageFailsClosed(t *testing.T) {
	compiler := pinnedCorpusCompiler(t)
	files := map[string][]byte{
		"main.typ": []byte(
			"#import \"@preview/rin-not-installed:0.1.0\": *\n\n= Root\n"),
	}
	started := time.Now()
	result, err := compiler.Compile(context.Background(), files, "main.typ", "html")
	if err == nil {
		t.Fatalf("an offline package that is not in the pinned cache compiled: %s", result.Log)
	}
	if !strings.Contains(result.Log, "rin-not-installed") {
		t.Fatalf("missing package diagnostic does not name the package: %q", result.Log)
	}
	// The sealed package path must fail immediately instead of reaching the
	// network and waiting for a download.
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("missing offline package waited %s before failing", elapsed)
	}
}
