package renderapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const fakeTypstMarker = "RIN_TYPST_STUB_EOF"

// writeFakeTypstCompiler writes a stand-in for the pinned Typst binary that
// emits a fixed native HTML document to its output argument. The real
// Compiler still verifies the pinned sha256 of both the binary and the font, so
// these tests exercise the executor's admission, sandbox and sanitizer path.
func writeFakeTypstCompiler(t *testing.T, nativeHTML string) (binary, binaryHash, font, fontHash string) {
	t.Helper()
	root := t.TempDir()
	binary = filepath.Join(root, "typst")
	script := "#!/bin/sh\nout=\"\"\nfor arg in \"$@\"; do out=\"$arg\"; done\ncat > \"$out\" <<'" + fakeTypstMarker + "'\n" + nativeHTML + "\n" + fakeTypstMarker + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	font = filepath.Join(root, "font.ttf")
	if err := os.WriteFile(font, []byte("pinned-font-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return binary, fileSHA256(t, binary), font, fileSHA256(t, font)
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func typstTestConfig(t *testing.T, nativeHTML string) Config {
	t.Helper()
	cfg := testConfig()
	binary, binaryHash, font, fontHash := writeFakeTypstCompiler(t, nativeHTML)
	cfg.TypstBin = binary
	cfg.TypstBinHash = binaryHash
	cfg.TypstFontPath = font
	cfg.TypstFontHash = fontHash
	cfg.TypstPackageHash = strings.Repeat("b", 64)
	cfg.TypstVersion = "0.15.1"
	cfg.TypstTimeout = 20 * 1000 * 1000 * 1000
	cfg.TypstMaxOutputBytes = 32 << 20
	return cfg
}

func zipTypstProject(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func runTypstExecutor(t *testing.T, cfg Config, archive []byte, mode string) ProjectExecutionResult {
	t.Helper()
	executor := NewProjectExecutor(cfg)
	t.Cleanup(func() { _ = executor.Close() })
	return executor.Execute(context.Background(), ProjectExecutionRequest{
		ContentKind: "typst", JobID: "job-typst-1", RequestID: "request-typst-1",
		ArchiveName: "project.zip", Archive: archive, Title: "Typst 书", ExplicitMainFile: "main.typ",
		DocumentMode: mode,
	})
}

const typstBookHTML = `<!DOCTYPE html><html><head><title>Book</title></head><body>` +
	`<p><span id="rin-page-intro"></span></p>` +
	`<h1 id="intro">引言</h1>` +
	`<p>正文 <math display="inline"><mi>x</mi></math> <a href="https://example.com/page">外链</a> ` +
	`<a href="#chapter-two">内链</a></p>` +
	`<p><span id="rin-page-chapter-two"></span></p>` +
	`<h1 id="chapter-two">第二章</h1>` +
	`<p>第二章内容</p>` +
	`<svg width="10" height="10"><circle cx="5" cy="5" r="4"></circle></svg>` +
	`</body></html>`

func TestTypstProjectExecutorBuildsBookBundleWithProfile(t *testing.T) {
	archive := zipTypstProject(t, map[string][]byte{
		"main.typ":        []byte("#include \"chapter.typ\""),
		"chapter.typ":     []byte("= 第二章"),
		"assets/logo.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="4" height="4"></rect></svg>`),
	})
	result := runTypstExecutor(t, typstTestConfig(t, typstBookHTML), archive, "book")
	if result.Err != nil {
		t.Fatalf("Typst book render failed: %v", result.Err)
	}
	canonical := result.Canonical
	if canonical == nil || canonical.Inline == nil {
		t.Fatal("Typst book render produced no canonical bundle")
	}
	if canonical.ContentKind != "typst" || canonical.Engine != "typst" {
		t.Fatalf("canonical identity = %s/%s", canonical.ContentKind, canonical.Engine)
	}
	if canonical.Inline.SchemaVersion != "rin-document-bundle/v1" {
		t.Fatalf("bundle schema = %s", canonical.Inline.SchemaVersion)
	}
	if got := canonical.Versions["typst"]; got != "0.15.1" {
		t.Fatalf("typst version provenance = %q", got)
	}
	profile := canonical.Versions["typstProfile"]
	if !strings.HasPrefix(profile, "typst-html-") {
		t.Fatalf("render profile provenance = %q", profile)
	}
	if len(canonical.Inline.Pages) != 2 || canonical.Inline.Pages[1].ID != "chapter-two" {
		t.Fatalf("book pages = %#v", canonical.Inline.Pages)
	}
	if strings.Contains(canonical.Inline.Pages[1].Fragment, "引言") {
		t.Fatal("second page leaked first-page content")
	}
	if !strings.Contains(canonical.Inline.Pages[1].Fragment, `data-rin-asset-path="typst-assets/`) {
		t.Fatal("inline Typst drawing was not delivered as a declared asset")
	}
	if !strings.Contains(canonical.Inline.Pages[0].Fragment, `<math display="inline">`) {
		t.Fatal("native Typst MathML was dropped from the reader fragment")
	}
	if !strings.Contains(canonical.Inline.Pages[0].Fragment, `class="rin-reader-ref"`) {
		t.Fatal("internal Typst reference was not marked for the reader")
	}
	if len(canonical.Inline.Assets) != 1 {
		t.Fatalf("declared assets = %#v", canonical.Inline.Assets)
	}
}

func TestTypstProjectExecutorFailsClosedWithoutPinnedProfile(t *testing.T) {
	archive := zipTypstProject(t, map[string][]byte{"main.typ": []byte("Hello")})
	cfg := testConfig()
	cfg.TypstBin = ""
	cfg.TypstBinHash = ""
	cfg.TypstFontPath = ""
	cfg.TypstFontHash = ""
	cfg.TypstPackageHash = ""
	cfg.TypstVersion = ""
	result := runTypstExecutor(t, cfg, archive, "article")
	if result.Err == nil || result.Canonical != nil {
		t.Fatalf("unconfigured Typst render was accepted: %#v", result)
	}
	if code := typstDiagnosticCode(result); code != "typst.unavailable" {
		t.Fatalf("failure code = %q", code)
	}
}

func TestTypstProfileIDFoldsSealedPackageSet(t *testing.T) {
	base := typstTestConfig(t, `<html><body><p>ok</p></body></html>`)
	base.TypstPackageHash = strings.Repeat("1", 64)
	first := TypstHTMLProfileID(base)
	if !strings.HasPrefix(first, "typst-html-") {
		t.Fatalf("profile id = %q", first)
	}
	changed := base
	changed.TypstPackageHash = strings.Repeat("2", 64)
	if second := TypstHTMLProfileID(changed); second == first {
		t.Fatalf("sealed package digest did not change the profile id: %q", first)
	}

	missing := base
	missing.TypstPackageHash = ""
	if id := TypstHTMLProfileID(missing); id != "" {
		t.Fatalf("profile without a sealed package digest was advertised: %q", id)
	}
}

func TestTypstProfileIDFollowsMathPresentation(t *testing.T) {
	base := typstTestConfig(t, `<html><body><p>ok</p></body></html>`)
	native := TypstHTMLProfileID(base)
	if !strings.HasPrefix(native, "typst-html-") {
		t.Fatalf("profile id = %q", native)
	}

	mathjax := base
	mathjax.TypstMathRender = true
	if advertised := TypstHTMLProfileID(mathjax); advertised == native {
		t.Fatalf("math presentation did not change the advertised profile id: %q", native)
	}

	// The bundle a render publishes must carry the profile that was advertised
	// for the same configuration, otherwise a job admitted for one profile would
	// record another.
	result := runTypstExecutor(t, base, zipTypstProject(t, map[string][]byte{"main.typ": []byte("Hello")}), "article")
	if result.Err != nil {
		t.Fatalf("Typst render failed: %v", result.Err)
	}
	if got := result.Canonical.Versions["typstProfile"]; got != native {
		t.Fatalf("rendered profile = %q, advertised profile = %q", got, native)
	}
}

func TestTypstProjectExecutorRejectsMismatchedCompilerHash(t *testing.T) {
	archive := zipTypstProject(t, map[string][]byte{"main.typ": []byte("Hello")})
	cfg := typstTestConfig(t, `<html><body><p>ok</p></body></html>`)
	cfg.TypstFontHash = strings.Repeat("a", 64)
	result := runTypstExecutor(t, cfg, archive, "article")
	if result.Err == nil || result.Canonical != nil {
		t.Fatalf("unpinned compiler profile was accepted: %#v", result)
	}
	if code := typstDiagnosticCode(result); code != "typst.compile.failed" {
		t.Fatalf("failure code = %q", code)
	}
}

func TestTypstProjectExecutorRejectsUnsafeNativeHTML(t *testing.T) {
	cases := map[string]string{
		"external script":     `<html><body><p>hi</p><script src="https://evil.example/x.js"></script></body></html>`,
		"inline script":       `<html><body><p>hi</p><script>alert(1)</script></body></html>`,
		"external stylesheet": `<html><body><link rel="stylesheet" href="https://evil.example/x.css"><p>hi</p></body></html>`,
		"inline style block":  `<html><body><style>body{background:url(https://evil.example/x)}</style><p>hi</p></body></html>`,
		"event handler":       `<html><body><p onclick="alert(1)">hi</p></body></html>`,
		"javascript link":     `<html><body><p><a href="javascript:alert(1)">x</a></p></body></html>`,
		"undeclared image":    `<html><body><p><img src="https://evil.example/track.png"></p></body></html>`,
		"iframe":              `<html><body><p><iframe src="https://evil.example"></iframe></p></body></html>`,
	}
	for name, native := range cases {
		t.Run(name, func(t *testing.T) {
			archive := zipTypstProject(t, map[string][]byte{"main.typ": []byte("Hello")})
			result := runTypstExecutor(t, typstTestConfig(t, native), archive, "article")
			if result.Err == nil || result.Canonical != nil {
				t.Fatalf("unsafe Typst HTML was published: %#v", result)
			}
			if code := typstDiagnosticCode(result); code != "typst.html.invalid" {
				t.Fatalf("failure code = %q", code)
			}
		})
	}
}

func TestTypstProjectExecutorRejectsForgedPageMarkers(t *testing.T) {
	cases := map[string]string{
		"marker inside content": `<html><body><p><span id="rin-page-a"></span><span id="rin-page-b"></span></p></body></html>`,
		"duplicate markers":     `<html><body><p><span id="rin-page-a"></span></p><p>x</p><p><span id="rin-page-a"></span></p></body></html>`,
		"content before marker": `<html><body><h1>前言</h1><p><span id="rin-page-a"></span></p></body></html>`,
	}
	for name, native := range cases {
		t.Run(name, func(t *testing.T) {
			archive := zipTypstProject(t, map[string][]byte{"main.typ": []byte("Hello")})
			result := runTypstExecutor(t, typstTestConfig(t, native), archive, "book")
			if result.Err == nil || result.Canonical != nil {
				t.Fatalf("forged page marker was published: %#v", result)
			}
			if code := typstDiagnosticCode(result); code != "typst.html.invalid" {
				t.Fatalf("failure code = %q", code)
			}
		})
	}
}

func TestTypstProjectExecutorRejectsEscapingArchive(t *testing.T) {
	archive := zipTypstProject(t, map[string][]byte{"../escape.typ": []byte("bad"), "main.typ": []byte("Hello")})
	result := runTypstExecutor(t, typstTestConfig(t, `<html><body><p>ok</p></body></html>`), archive, "article")
	if result.Err == nil || result.Canonical != nil {
		t.Fatalf("path-escaping Typst archive was accepted: %#v", result)
	}
	if code := typstDiagnosticCode(result); code != "typst.project.invalid" {
		t.Fatalf("failure code = %q", code)
	}
}

func TestCapabilitiesAdvertiseTypstOnlyWhenPinned(t *testing.T) {
	cfg := testConfig()
	server := NewServer(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/render/capabilities", nil)
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d", resp.Code)
	}
	var unpinned map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &unpinned); err != nil {
		t.Fatal(err)
	}
	if engines := capabilityEngines(t, unpinned); containsString(engines, "typst") {
		t.Fatalf("unpinned renderer advertised typst: %#v", engines)
	}

	pinned := typstTestConfig(t, `<html><body><p>ok</p></body></html>`)
	server = NewServer(pinned)
	resp = httptest.NewRecorder()
	server.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/render/capabilities", nil))
	var advertised map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &advertised); err != nil {
		t.Fatal(err)
	}
	if engines := capabilityEngines(t, advertised); !containsString(engines, "typst") {
		t.Fatalf("pinned renderer did not advertise typst: %#v", engines)
	}
}

// A book whose cross-chapter reference no longer resolves must fail closed: a
// reader would otherwise be routed to the wrong page or to nothing at all.
const typstDanglingBookHTML = `<!DOCTYPE html><html><head><title>Book</title></head><body>` +
	`<p><span id="rin-page-intro"></span></p>` +
	`<h1 id="intro">引言</h1>` +
	`<p>引用丢失 <a href="#missing-section">不存在的章节</a></p>` +
	`<p><span id="rin-page-second"></span></p>` +
	`<h1 id="second">第二章</h1>` +
	`</body></html>`

func TestTypstProjectExecutorFailsClosedOnBrokenBookReferences(t *testing.T) {
	archive := zipTypstProject(t, map[string][]byte{
		"main.typ":   []byte("= 引言"),
		"second.typ": []byte("= 第二章"),
	})
	result := runTypstExecutor(t, typstTestConfig(t, typstDanglingBookHTML), archive, "book")
	if result.Status != http.StatusUnprocessableEntity || result.Err == nil ||
		typstDiagnosticCode(result) != "typst.book.link.unresolved" {
		t.Fatalf("dangling cross-chapter reference was not rejected: %#v", result)
	}
}

func capabilityEngines(t *testing.T, body map[string]any) []string {
	t.Helper()
	engines, ok := body["engines"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities has no engines map: %#v", body)
	}
	document, ok := engines["document"].([]any)
	if !ok {
		t.Fatalf("capabilities has no document engines: %#v", engines)
	}
	result := make([]string, 0, len(document))
	for _, item := range document {
		if value, ok := item.(string); ok {
			result = append(result, value)
		}
	}
	return result
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func typstDiagnosticCode(result ProjectExecutionResult) string {
	if len(result.Diagnostics) == 0 {
		return ""
	}
	return result.Diagnostics[0].Code
}
