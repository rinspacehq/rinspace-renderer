package latexmladapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

func TestRenderInvokesLateXMLCAndReturnsBodyFragment(t *testing.T) {
	tempDir := t.TempDir()
	argsPath := filepath.Join(tempDir, "args.txt")
	bin := fakeLateXMLC(t, argsPath, 0, `<!doctype html><html><head><title>Ignored</title></head><body><article><h1>Converted</h1><p>OK</p></article></body></html>`)
	files := []projectcore.File{{
		Path:  "main.tex",
		Kind:  "tex",
		Body:  `\documentclass{article}\begin{document}OK\end{document}`,
		Bytes: 53,
	}}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "Test"})
	adapter := Adapter{Config: Config{
		Binary:       bin,
		Timeout:      5 * time.Second,
		MaxHTMLBytes: 1 << 20,
	}}

	result, err := adapter.Render(context.Background(), Request{
		Files:    files,
		Manifest: manifest,
		Title:    "Test",
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if strings.Contains(strings.ToLower(result.HTML), "<body") || !strings.Contains(result.HTML, "<article><h1>Converted</h1><p>OK</p></article>") {
		t.Fatalf("expected body fragment, got %s", result.HTML)
	}
	args := string(mustReadFile(t, argsPath))
	for _, expected := range []string{"--expire=-1", "--format=html5", "--destination=", "--log=", "--timeout=5", "main.tex"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("expected args to contain %q, got %s", expected, args)
		}
	}
	if len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != "latexml.info" {
		t.Fatalf("expected parsed diagnostics, got %#v", result.Diagnostics)
	}
}

func TestRenderInvokesLateXMLCWithCompatSource(t *testing.T) {
	tempDir := t.TempDir()
	argsPath := filepath.Join(tempDir, "args.txt")
	bin := fakeLateXMLC(t, argsPath, 0, `<!doctype html><html><head><title>Ignored</title></head><body><article><h1>Converted</h1><p>OK</p></article></body></html>`)
	files := []projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `\documentclass{article}
\usepackage{xeCJK}
\begin{document}OK\end{document}`,
		Bytes: 72,
	}}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "Test"})
	adapter := Adapter{Config: Config{
		Binary:       bin,
		Timeout:      5 * time.Second,
		MaxHTMLBytes: 1 << 20,
	}}

	if _, err := adapter.Render(context.Background(), Request{
		Files:    files,
		Manifest: manifest,
		Title:    "Test",
	}); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	args := string(mustReadFile(t, argsPath))
	if strings.Contains(args, "--preamble=") {
		t.Fatalf("expected compat shim to be injected into source, not --preamble args; got %s", args)
	}
	source := string(mustReadFile(t, argsPath+".main"))
	if !strings.Contains(source, xecjkCompatibilitySentinel) {
		t.Fatalf("expected compatibility shim in main source, got %s", source)
	}
	if strings.Contains(source, `\usepackage{xeCJK`) {
		t.Fatalf("expected xeCJK package to be stripped from main source, got %s", source)
	}
}

func TestNormalizeXeCJKSourcesInjectsCompatShimAndStripsPackages(t *testing.T) {
	files := []projectcore.File{
		{
			Path: "main.tex",
			Kind: "tex",
			Body: `\documentclass{article}
\usepackage{xeCJK,graphicx}
\begin{document}
\setCJKmainfont[BoldFont={Kai}]{SimSun}
\end{document}`,
			Bytes: 122,
		},
		{
			Path: "styles/ptmt.sty",
			Kind: "tex",
			Body: `\NeedsTeXFormat{LaTeX2e}
\usepackage{xeCJK, xecjkfntef}
`,
			Bytes: 50,
		},
	}
	normalized, err := normalizeXeCJKSources(files, "main.tex")
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if !strings.Contains(normalized[0].Body, xecjkCompatibilitySentinel) {
		t.Fatalf("expected compatibility shim injected into main file, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, "\\scrollmode") {
		t.Fatalf("expected xeCJK compatibility shim to inject \\\\scrollmode, got %q", normalized[0].Body)
	}
	classPos := strings.Index(normalized[0].Body, `\documentclass{article}`)
	compatPos := strings.Index(normalized[0].Body, xecjkCompatibilitySentinel)
	if classPos < 0 || compatPos < 0 {
		t.Fatalf("expected both documentclass and shim in normalized body, got class=%d compat=%d", classPos, compatPos)
	}
	if compatPos < classPos {
		t.Fatalf("expected compatibility shim to be injected after documentclass, got compat=%d class=%d", compatPos, classPos)
	}
	if strings.Contains(normalized[0].Body, `\usepackage{xeCJK`) {
		t.Fatalf("expected xeCJK stripped from main.tex, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\usepackage{graphicx}`) {
		t.Fatalf("expected remaining package import preserved, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[1].Body, "% xeCJK package removed by renderer compatibility shim for latexml") {
		t.Fatalf("expected xeCJK stripped from style file, got %q", normalized[1].Body)
	}
}

func TestRewriteXeCJKPackageLineStripsCompatPackages(t *testing.T) {
	line := `\usepackage[UTF8]{fontspec, xecjkfntef, luatexja-fontspec, graphicx, xetex, tabularray, algorithm2e, mdframed}`
	nextLine, changed := rewriteXeCJKPackageLine(line)
	if !changed {
		t.Fatal("expected line to be rewritten")
	}
	if nextLine != `\usepackage[UTF8]{graphicx}` {
		t.Fatalf("expected only compatible package to remain, got %q", nextLine)
	}
}

func TestNormalizeXeCJKSourcesInjectsTabularrayCompat(t *testing.T) {
	files := []projectcore.File{
		{
			Path: "main.tex",
			Kind: "tex",
			Body: `\documentclass{article}
\usepackage{tabularray}
\begin{document}
\begin{tblr}{l|l}
A & B \\
\end{tblr}
\end{document}`,
			Bytes: 112,
		},
	}
	normalized, err := normalizeXeCJKSources(files, "main.tex")
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if !strings.Contains(normalized[0].Body, xecjkCompatibilitySentinel) {
		t.Fatalf("expected compatibility shim injected into main file, got %q", normalized[0].Body)
	}
	if strings.Contains(normalized[0].Body, `\usepackage{tabularray}`) {
		t.Fatalf("expected tabularray package stripped from main file, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\newenvironment{tblr}`) {
		t.Fatalf("expected tblr compatibility environment in shim, got %q", normalized[0].Body)
	}
}

func TestNormalizeXeCJKSourcesInjectsAlgorithm2eCompat(t *testing.T) {
	files := []projectcore.File{
		{
			Path: "main.tex",
			Kind: "tex",
			Body: `\documentclass{book}
\usepackage{ptmt}
\begin{document}
\begin{algorithm}
\KwData{input}
\end{algorithm}
\[
A \coloneqq B
\]
\end{document}`,
			Bytes: 132,
		},
		{
			Path: "ptmt.sty",
			Kind: "tex",
			Body: `\usepackage[algoruled]{algorithm2e}
\newcommand{\undersign}{signed}
`,
			Bytes: 67,
		},
	}
	normalized, err := normalizeXeCJKSources(files, "main.tex")
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if !strings.Contains(normalized[0].Body, xecjkCompatibilitySentinel) {
		t.Fatalf("expected compatibility shim injected into main file, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\newenvironment{algorithm}`) {
		t.Fatalf("expected algorithm compatibility environment in shim, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\KwData`) {
		t.Fatalf("expected algorithm2e keyword compatibility macro in shim, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\coloneqq`) {
		t.Fatalf("expected coloneqq compatibility macro in shim, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\undersign`) {
		t.Fatalf("expected undersign compatibility macro in shim, got %q", normalized[0].Body)
	}
	if strings.Contains(normalized[1].Body, `\usepackage[algoruled]{algorithm2e}`) {
		t.Fatalf("expected algorithm2e package stripped from style file, got %q", normalized[1].Body)
	}
	if !strings.Contains(normalized[1].Body, `\newcommand{\undersign}`) {
		t.Fatalf("expected later style definitions preserved after stripping algorithm2e, got %q", normalized[1].Body)
	}
}

func TestShouldStripLateXMLCompatPackage(t *testing.T) {
	cases := map[string]bool{
		"xeCJK":             true,
		"xecjkfntef":        true,
		"fontspec":          true,
		"xparse":            true,
		"luatex":            true,
		"luatexja":          true,
		"luatexja-fontspec": true,
		"xunicode":          true,
		"xetex":             true,
		"xetex-inputenc":    true,
		"ctex":              true,
		"tabularray":        true,
		"algorithm2e":       true,
		"mdframed":          true,
		"expl3":             true,
		"extarrows":         true,
		"cmupint":           true,
		"graphicx":          false,
		"amsthm":            false,
		"":                  false,
	}
	for name, expected := range cases {
		if got := shouldStripLateXMLCompatPackage(name); got != expected {
			t.Fatalf("package %q strip expected=%v got=%v", name, expected, got)
		}
	}
}

func TestFailureDiagnosticsFromLogsIncludesLateErrorsAndTail(t *testing.T) {
	var builder strings.Builder
	for index := 0; index < 55; index++ {
		builder.WriteString("Info: early line\n")
	}
	builder.WriteString("Warning: late warning\n")
	builder.WriteString("Error: late fatal detail\n")
	builder.WriteString("Final context line\n")
	logs := builder.String()

	diagnostics := diagnosticsFromLogs(logs)
	if len(diagnostics) != 50 {
		t.Fatalf("expected first-pass diagnostics limit, got %d", len(diagnostics))
	}
	if diagnostics[0].Code != "latexml.error" || diagnostics[1].Code != "latexml.warning" {
		t.Fatalf("expected late errors and warnings before progress messages, got %#v", diagnostics)
	}

	diagnostics = appendMissingDiagnostics(diagnostics, failureDiagnosticsFromLogs(logs)...)
	if !hasLateXMLDiagnostic(diagnostics, "latexml.warning", "Warning: late warning") {
		t.Fatalf("expected late warning diagnostic, got %#v", diagnostics)
	}
	if !hasLateXMLDiagnostic(diagnostics, "latexml.error", "Error: late fatal detail") {
		t.Fatalf("expected late error diagnostic, got %#v", diagnostics)
	}
	if !hasLateXMLDiagnostic(diagnostics, "latexml.log.tail", "Final context line") {
		t.Fatalf("expected log tail diagnostic, got %#v", diagnostics)
	}
}

func hasLateXMLDiagnostic(diagnostics []Diagnostic, code string, text string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code && strings.Contains(diagnostic.Message, text) {
			return true
		}
	}
	return false
}

func TestRewriteElegantTheoremLine(t *testing.T) {
	source := `\elegantnewtheorem{proposition}{Proposition}{plain}{exfancy}`
	got, changed := rewriteElegantTheoremLine(source)
	if !changed {
		t.Fatal("expected theorem line to be rewritten")
	}
	if got != `\newtheorem{proposition}{Proposition}` {
		t.Fatalf("unexpected theorem rewrite, got %q", got)
	}
}

func TestNormalizeXeCJKSourcesPreservesBase64Body(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(`\documentclass{article}
\usepackage{xeCJK}
\begin{document}A\end{document}`))
	files := []projectcore.File{
		{
			Path:     "main.tex",
			Kind:     "tex",
			Body:     encoded,
			Bytes:    64,
			Encoding: "base64",
		},
	}
	normalized, err := normalizeXeCJKSources(files, "main.tex")
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if normalized[0].Encoding != "base64" {
		t.Fatalf("expected base64 encoding to be preserved, got %q", normalized[0].Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(normalized[0].Body)
	if err != nil {
		t.Fatalf("decode normalized body: %v", err)
	}
	if !strings.Contains(string(raw), xecjkCompatibilitySentinel) {
		t.Fatalf("expected compatibility shim in decoded base64 body, got %q", string(raw))
	}
}

func TestNormalizeXeCJKSourcesFallbacksMainFileToDocumentClassEntry(t *testing.T) {
	files := []projectcore.File{
		{
			Path: "main.tex",
			Kind: "tex",
			Body: `\theoremstyle{plain}
\newtheorem{lemma}{Lemma}
`,
			Bytes: 48,
		},
		{
			Path: "book.tex",
			Kind: "tex",
			Body: `\documentclass{book}
\usepackage{amsthm}
\begin{document}
\end{document}`,
			Bytes: 66,
		},
	}
	mainFile := resolveLateXMLMainFile(files, "main.tex")
	if mainFile != "book.tex" {
		t.Fatalf("expected resolveLateXMLMainFile fallback to book.tex, got %s", mainFile)
	}
}

func TestResolveLateXMLMainFileFallbacksWhenRequestedTexIsNotLegacyName(t *testing.T) {
	files := []projectcore.File{
		{
			Path: "main/chapter.tex",
			Kind: "tex",
			Body: `\theoremstyle{plain}
\newtheorem{lemma}{Lemma}
`,
			Bytes: 48,
		},
		{
			Path: "book.tex",
			Kind: "tex",
			Body: `\documentclass{book}
\begin{document}
\end{document}`,
			Bytes: 52,
		},
	}
	mainFile := resolveLateXMLMainFile(files, "main/chapter.tex")
	if mainFile != "book.tex" {
		t.Fatalf("expected resolveLateXMLMainFile fallback to book.tex, got %s", mainFile)
	}
}

func TestNormalizeXeCJKSourcesInjectsElegantBookCompatAndRewritesClass(t *testing.T) {
	files := []projectcore.File{
		{
			Path: "main.tex",
			Kind: "tex",
			Body: `\documentclass{elegantbook}
\usepackage{xeCJK}
\elegantnewtheorem{orange}{}{thmstyle}{exfancy}
\cover{cover.jpg}
\protectProposition
\protectTheorem
\extrainfo{note}
\begin{document}
\end{document}`,
			Bytes: 130,
		},
	}
	normalized, err := normalizeXeCJKSources(files, "main.tex")
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if !strings.Contains(normalized[0].Body, xecjkCompatibilitySentinel) {
		t.Fatalf("expected xecjk compatibility shim, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, elegantBookCompatibilitySentinel) {
		t.Fatalf("expected elegantbook compatibility shim, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, elegantBookEnvironmentCompatibilitySentinel) {
		t.Fatalf("expected elegantbook environment compatibility shim, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\documentclass{book}`) {
		t.Fatalf("expected documentclass rewritten to book, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\newtheorem{orange}{}`) {
		t.Fatalf("expected elegant theorem macro to be rewritten, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\newtheoremstyle`) {
		t.Fatalf("expected newtheoremstyle compatibility macro, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\theoremstyle`) {
		t.Fatalf("expected theoremstyle compatibility macro, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\newtheorem`) {
		t.Fatalf("expected newtheorem compatibility macro, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\rinpc@define@theorem{definition}`) {
		t.Fatalf("expected default elegant theorem environments, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\rinpc@define@simpleenv{proof}`) {
		t.Fatalf("expected default elegant proof environment, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\rinpc@define@simpleenv{introduction}`) {
		t.Fatalf("expected introduction compatibility environment, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\protectProposition`) || !strings.Contains(normalized[0].Body, `\protectTheorem`) {
		t.Fatalf("expected protect style theorem name commands preserved, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\setlength`) {
		t.Fatalf("expected setlength compatibility macro, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\xlongequal`) || !strings.Contains(normalized[0].Body, `\def\xlongrightarrow`) {
		t.Fatalf("expected extarrows compatibility macros, got %q", normalized[0].Body)
	}
}

func TestNormalizeXeCJKSourcesInjectsCompatForTheoremCommands(t *testing.T) {
	files := []projectcore.File{
		{
			Path: "main.tex",
			Kind: "tex",
			Body: `\theoremstyle{plain}
\newtheorem{lemma}{Lemma}
\setlength{\parskip}{1em}
\begin{document}
\end{document}`,
			Bytes: 100,
		},
	}
	normalized, err := normalizeXeCJKSources(files, "main.tex")
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if !strings.Contains(normalized[0].Body, elegantBookCompatibilitySentinel) {
		t.Fatalf("expected elegant theorem compatibility shim for theorem commands, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\theoremstyle`) {
		t.Fatalf("expected theoremstyle compatibility macro, got %q", normalized[0].Body)
	}
	if !strings.Contains(normalized[0].Body, `\def\newtheorem`) {
		t.Fatalf("expected newtheorem compatibility macro, got %q", normalized[0].Body)
	}
	if strings.Contains(normalized[0].Body, elegantBookEnvironmentCompatibilitySentinel) {
		t.Fatalf("did not expect elegantbook environment shim for generic theorem commands, got %q", normalized[0].Body)
	}
}

func TestRenderUsesWorkerEndpointWhenConfigured(t *testing.T) {
	var workerAuth string
	var workerPayload workerRenderRequest
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/render" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&workerPayload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "html": "<!doctype html><html><body><article><h1>Worker Converted</h1><p>OK</p></article></body></html>",
  "diagnostics": [{"severity":"warning","code":"latexml.warning","message":"Warning: worker diagnostic"}]
}`))
	}))
	defer worker.Close()

	files := []projectcore.File{{
		Path:  "main.tex",
		Kind:  "tex",
		Body:  `\documentclass{article}\begin{document}OK\end{document}`,
		Bytes: 53,
	}}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "Worker Test"})
	adapter := Adapter{Config: Config{
		Binary:       filepath.Join(t.TempDir(), "missing-latexmlc"),
		Endpoint:     worker.URL + "/render",
		Token:        "worker-token",
		Profile:      "html5",
		Timeout:      5 * time.Second,
		MaxHTMLBytes: 1 << 20,
	}}

	result, err := adapter.Render(context.Background(), Request{
		Files:    files,
		Manifest: manifest,
		Title:    "Worker Test",
	})
	if err != nil {
		t.Fatalf("worker render failed: %v", err)
	}
	if workerAuth != "Bearer worker-token" {
		t.Fatalf("expected worker auth header, got %q", workerAuth)
	}
	if workerPayload.MainFile != "main.tex" || workerPayload.Manifest.MainFile != "main.tex" || workerPayload.Title != "Worker Test" || workerPayload.Profile != "html5" || workerPayload.MaxHTMLBytes != 1<<20 {
		t.Fatalf("unexpected worker payload: %#v", workerPayload)
	}
	if strings.Contains(strings.ToLower(result.HTML), "<body") || !strings.Contains(result.HTML, "<article><h1>Worker Converted</h1><p>OK</p></article>") {
		t.Fatalf("expected worker body fragment, got %s", result.HTML)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "latexml.warning" || result.Diagnostics[0].Source["endpoint"] != worker.URL+"/render" {
		t.Fatalf("expected structured worker diagnostics, got %#v", result.Diagnostics)
	}
}

func TestRenderWorkerReportsHTTPErrorDiagnostics(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{
  "error": "worker warming up",
  "diagnostics": [{"severity":"warning","code":"latexml.worker.warming","message":"warming"}]
}`))
	}))
	defer worker.Close()

	files := []projectcore.File{{
		Path:  "main.tex",
		Kind:  "tex",
		Body:  `\documentclass{article}\begin{document}OK\end{document}`,
		Bytes: 53,
	}}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "Worker Error"})
	adapter := Adapter{Config: Config{
		Endpoint:     worker.URL,
		MaxHTMLBytes: 1 << 20,
	}}

	result, err := adapter.Render(context.Background(), Request{
		Files:    files,
		Manifest: manifest,
		Title:    "Worker Error",
	})
	if err == nil {
		t.Fatal("expected worker error")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected unavailable error, got %v", err)
	}
	if len(result.Diagnostics) != 2 || result.Diagnostics[0].Code != "latexml.worker.warming" || result.Diagnostics[1].Code != "latexml.unavailable" {
		t.Fatalf("expected worker diagnostics plus unavailable error, got %#v", result.Diagnostics)
	}
}

func TestRenderReportsUnavailableBinary(t *testing.T) {
	files := []projectcore.File{{
		Path:  "main.tex",
		Kind:  "tex",
		Body:  `\documentclass{article}\begin{document}OK\end{document}`,
		Bytes: 53,
	}}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "Test"})
	adapter := Adapter{Config: Config{
		Binary: filepath.Join(t.TempDir(), "missing-latexmlc"),
	}}

	result, err := adapter.Render(context.Background(), Request{
		Files:    files,
		Manifest: manifest,
		Title:    "Test",
	})
	if err == nil {
		t.Fatal("expected missing binary error")
	}
	if !strings.Contains(err.Error(), "latexml adapter unavailable") || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "latexml.unavailable" {
		t.Fatalf("expected unavailable diagnostic, got err=%v diagnostics=%#v", err, result.Diagnostics)
	}
}

func TestRenderWithRealLateXMLC(t *testing.T) {
	if os.Getenv("RIN_RENDERER_LATEXML_INTEGRATION") != "1" {
		t.Skip("set RIN_RENDERER_LATEXML_INTEGRATION=1 to run against a real latexmlc runtime")
	}
	bin := strings.TrimSpace(os.Getenv("RIN_RENDERER_LATEXML_BIN"))
	if bin == "" {
		bin = "latexmlc"
	}
	files := []projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `\documentclass{article}
\begin{document}
\section{Smoke}
Inline math \(x^2\).
\end{document}`,
		Bytes: 92,
	}}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "Real LaTeXML Smoke"})
	adapter := Adapter{Config: Config{
		Binary:       bin,
		Timeout:      30 * time.Second,
		MaxHTMLBytes: 1 << 20,
	}}

	result, err := adapter.Render(context.Background(), Request{
		Files:    files,
		Manifest: manifest,
		Title:    "Real LaTeXML Smoke",
	})
	if err != nil {
		t.Fatalf("real latexml render failed: %v diagnostics=%#v", err, result.Diagnostics)
	}
	if !strings.Contains(result.HTML, "Smoke") || !strings.Contains(result.HTML, "Inline math") {
		t.Fatalf("expected real latexml HTML content, got %s", result.HTML)
	}
}

func TestRenderCTeXArticleWithRealLateXMLC(t *testing.T) {
	if os.Getenv("RIN_RENDERER_LATEXML_INTEGRATION") != "1" {
		t.Skip("set RIN_RENDERER_LATEXML_INTEGRATION=1 to run against a real latexmlc runtime")
	}
	bin := strings.TrimSpace(os.Getenv("RIN_RENDERER_LATEXML_BIN"))
	if bin == "" {
		bin = "latexmlc"
	}
	for _, tc := range []struct {
		name      string
		body      string
		wantError bool
	}{
		{
			name: "kernel hook before class and author-declared theorem",
			body: `\AddToHook{package/xeCJK/after}{\defaultCJKfontfeatures{}}
\documentclass[UTF8,11pt,fontset=fandol]{ctexart}
\usepackage{amsmath,amsthm}
\newtheorem{definition}{定义}[section]
\begin{document}
\section{中文结构}
\begin{definition}\label{def:test}中文定义与公式 \(x^2\)。\end{definition}
引用定义~\ref{def:test}。
\begin{figure}\caption{中文图题}\end{figure}
\end{document}`,
		},
		{
			name: "undeclared environment remains an error",
			body: `\documentclass[UTF8,fontset=fandol]{ctexart}
\begin{document}
\begin{undeclaredTheorem}This declaration is missing.\end{undeclaredTheorem}
\end{document}`,
			wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []projectcore.File{{Path: "main.tex", Kind: "tex", Body: tc.body, Bytes: int64(len(tc.body))}}
			manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "CTeX article"})
			adapter := Adapter{Config: Config{Binary: bin, IncludeStyles: true, Timeout: 30 * time.Second, MaxHTMLBytes: 1 << 20}}
			result, err := adapter.Render(context.Background(), Request{Files: files, Manifest: manifest, Title: "CTeX article"})
			hasError := false
			for _, diagnostic := range result.Diagnostics {
				hasError = hasError || diagnostic.Severity == "error"
			}
			if tc.wantError {
				if !hasError {
					t.Fatalf("missing declaration must produce error diagnostics: err=%v diagnostics=%#v", err, result.Diagnostics)
				}
				return
			}
			if err != nil || hasError {
				t.Fatalf("CTeX article failed: err=%v diagnostics=%#v", err, result.Diagnostics)
			}
			for _, expected := range []string{"中文结构", "中文定义与公式", "中文图题", "定义", "1.1"} {
				if !strings.Contains(result.HTML, expected) {
					t.Fatalf("CTeX article lost %q: %s", expected, result.HTML)
				}
			}
			if files[0].Body != tc.body {
				t.Fatal("renderer modified caller-owned CTeX source")
			}
		})
	}
}

func fakeLateXMLC(t *testing.T, argsPath string, exitCode int, html string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "latexmlc")
	script := `#!/bin/sh
set -eu
dest=""
log=""
main=""
preamble=""
for arg in "$@"; do
  case "$arg" in
    --destination=*) dest="${arg#--destination=}" ;;
    --log=*) log="${arg#--log=}" ;;
    --preamble=*) preamble="${arg#--preamble=}" ;;
    *.tex) main="$arg" ;;
  esac
done
printf '%s\n' "$@" > ` + shellQuote(argsPath) + `
test -n "$dest"
test -n "$log"
test -n "$main"
test -z "$preamble" || test -f "$preamble"
test -f "$main"
cp "$main" ` + shellQuote(argsPath+".main") + `
mkdir -p "$(dirname "$dest")"
printf '%s\n' 'Info: fake latexml conversion complete' > "$log"
cat > "$dest" <<'HTML'
` + html + `
HTML
exit ` + shellExitCode(exitCode) + `
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake latexmlc: %v", err)
	}
	return bin
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func shellExitCode(value int) string {
	if value < 0 || value > 255 {
		return "1"
	}
	return strconv.Itoa(value)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return body
}

func TestRenderPassesIncludeStylesToLateXMLC(t *testing.T) {
	files := []projectcore.File{{
		Path:  "main.tex",
		Kind:  "tex",
		Body:  `\documentclass{article}\begin{document}OK\end{document}`,
		Bytes: 53,
	}}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "Test"})

	for _, tc := range []struct {
		name          string
		includeStyles bool
		wantFlag      bool
	}{
		{name: "enabled keeps project style files", includeStyles: true, wantFlag: true},
		{name: "disabled falls back to bundled bindings only", includeStyles: false, wantFlag: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			argsPath := filepath.Join(tempDir, "args.txt")
			bin := fakeLateXMLC(t, argsPath, 0, `<!doctype html><html><body><article><h1>Converted</h1><p>OK</p></article></body></html>`)
			adapter := Adapter{Config: Config{
				Binary:        bin,
				Timeout:       5 * time.Second,
				MaxHTMLBytes:  1 << 20,
				IncludeStyles: tc.includeStyles,
			}}
			if _, err := adapter.Render(context.Background(), Request{Files: files, Manifest: manifest, Title: "Test"}); err != nil {
				t.Fatalf("render failed: %v", err)
			}
			args := string(mustReadFile(t, argsPath))
			if got := strings.Contains(args, "--includestyles"); got != tc.wantFlag {
				t.Fatalf("expected --includestyles=%t, got args %s", tc.wantFlag, args)
			}
		})
	}
}
