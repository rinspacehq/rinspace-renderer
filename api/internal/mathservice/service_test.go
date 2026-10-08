package mathservice

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/mathrender"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

type recordingRenderer struct {
	requests   []mathrender.Request
	batches    [][]mathrender.Request
	results    []mathrender.BatchResult
	batchError error
}

func (renderer *recordingRenderer) Render(context.Context, mathrender.Request) (mathrender.Result, error) {
	return mathrender.Result{}, errors.New("shared math service must use batch rendering")
}

func (renderer *recordingRenderer) RenderBatch(_ context.Context, requests []mathrender.Request) ([]mathrender.BatchResult, error) {
	renderer.requests = append(renderer.requests, requests...)
	renderer.batches = append(renderer.batches, append([]mathrender.Request(nil), requests...))
	if renderer.results == nil && renderer.batchError == nil {
		results := make([]mathrender.BatchResult, len(requests))
		for index, request := range requests {
			results[index].Result = mathrender.Result{
				HTML:   `<mjx-container data-latex="` + request.Source + `"></mjx-container>`,
				Engine: "mathjax-chtml", Version: "4.1.3", CSS: "mjx-container{display:inline-block}",
			}
		}
		return results, nil
	}
	return renderer.results, renderer.batchError
}

func TestResolveMathPreservesNativeMathJaxOutputAndMetadata(t *testing.T) {
	const source = `\not\exists k\in\mathbb{R}`
	const body = `<mjx-container class="MathJax" data-latex="\not\exists k\in\mathbb{R}"><mjx-math><mjx-c class="mjx-c2204"></mjx-c></mjx-math></mjx-container>`
	renderer := &recordingRenderer{results: []mathrender.BatchResult{{Result: mathrender.Result{
		HTML: body, Engine: "mathjax-chtml", Version: "4.1.3",
		CSS: "mjx-container{display:inline-block}\nmjx-c.mjx-c2204{padding:0.789em 0.556em 0.105em 0}",
	}}}}
	service := newTestService(t, renderer, nil)
	display := false
	accessibility := &contracts.MathAccessibilityContext{Language: "zh-CN", SpeechStyle: "mathspeak", Label: "不存在"}
	location := &contracts.SourceLocation{Path: "chapter.tex", Start: contracts.SourcePosition{Line: 12, Column: 3}}
	batch := testBatch(orchestration.MathOutputCHTML, orchestration.MathBatchItem{
		Unit: contracts.WorkUnit{
			Kind: contracts.WorkUnitMath, ID: "rw_00000000000000000000000000000001", Source: source,
			Display: &display, MacroContextHash: orchestration.ComputeMathMacroContextHash(nil),
			AccessibilityContext: accessibility, SourceLocation: location,
		},
		WrapperID: "book260-not-exists",
	})

	result, err := service.ResolveMath(t.Context(), batch)
	if err != nil {
		t.Fatalf("resolve math: %v", err)
	}
	if len(renderer.requests) != 1 || renderer.requests[0].Source != source {
		t.Fatalf("expected unchanged TeX source in one batch request, got %#v", renderer.requests)
	}
	unit := result.Units[0]
	for _, expected := range []string{
		`id="book260-not-exists"`, `rin-math-inline rin-math-mathjax`,
		`data-rin-math-source="\not\exists k\in\mathbb{R}"`, `aria-label="不存在"`,
		`lang="zh-CN"`, `data-rin-math-speech-style="mathspeak"`, body,
	} {
		if !strings.Contains(unit.HTML, expected) {
			t.Fatalf("expected shared wrapper to retain %q, got %s", expected, unit.HTML)
		}
	}
	if unit.Source.TeX != source || unit.Source.AccessibilityContext != accessibility || unit.Source.SourceLocation != location {
		t.Fatalf("expected source/accessibility metadata identity to be preserved: %#v", unit.Source)
	}
	if len(result.CSS) != 1 || !strings.Contains(result.CSS[0], "mjx-c.mjx-c2204") {
		t.Fatalf("expected negated-exists glyph CSS to be retained, got %#v", result.CSS)
	}
}

func TestResolveMathBatchesAndDeduplicatesByExactCacheContract(t *testing.T) {
	renderer := &recordingRenderer{results: []mathrender.BatchResult{
		{Result: mathrender.Result{HTML: `<mjx-container data-latex="x"></mjx-container>`, Engine: "mathjax-chtml", Version: "4.1.3", CSS: "mjx-container{display:inline-block}"}},
		{Result: mathrender.Result{HTML: `<mjx-container data-latex="\\left( x \\right)"></mjx-container>`, Engine: "mathjax-chtml", Version: "4.1.3", CSS: "mjx-container{display:inline-block}\n.mjx-stretchy-v{height:2em}"}},
	}}
	service := newTestService(t, renderer, nil)
	inline := false
	display := true
	hash := orchestration.ComputeMathMacroContextHash(map[string]string{`\R`: `\mathbb{R}`})
	batch := orchestration.MathBatch{
		ContractVersion: orchestration.MathBatchContractVersion,
		OutputStrategy:  orchestration.MathOutputCHTML,
		MacroContexts:   map[string]map[string]string{hash: {`\R`: `\mathbb{R}`}},
		Items: []orchestration.MathBatchItem{
			{Unit: testUnit("rw_00000000000000000000000000000001", "x", inline, hash)},
			{Unit: testUnit("rw_00000000000000000000000000000002", "x", inline, hash)},
			{Unit: testUnit("rw_00000000000000000000000000000003", `\left( x \right)`, display, hash)},
		},
	}

	result, err := service.ResolveMath(t.Context(), batch)
	if err != nil {
		t.Fatalf("resolve batch: %v", err)
	}
	if len(renderer.requests) != 2 {
		t.Fatalf("expected identical units to share one primary request, got %d", len(renderer.requests))
	}
	if got := renderer.requests[0].Macros[`\R`]; got != `\mathbb{R}` {
		t.Fatalf("expected macro context to reach renderer, got %#v", renderer.requests[0].Macros)
	}
	if result.Units[0].Cache.Status != "miss" || result.Units[1].Cache.Status != "hit" {
		t.Fatalf("expected batch-local cache reuse, got %#v", result.Units)
	}
	if len(result.CSS) != 1 || strings.Count(result.CSS[0], "mjx-container{display:inline-block}") != 1 || !strings.Contains(result.CSS[0], ".mjx-stretchy-v") {
		t.Fatalf("expected exact-rule CSS merge including stretchy rule, got %#v", result.CSS)
	}
}

func TestResolveMathRoutesComplexMatrixToSanitizedSVGFallbackContract(t *testing.T) {
	renderer := &recordingRenderer{}
	fallbackCalls := 0
	service := newTestService(t, renderer, func(_ context.Context, item orchestration.MathBatchItem) (FallbackResult, error) {
		fallbackCalls++
		return FallbackResult{
			URL:           "https://assets.example/rin/math.svg",
			SVGHash:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Artifact:      testPublicSVGArtifact("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			EngineVersion: "texlive-2025",
		}, nil
	})
	display := false
	source := `\mathbf{x}=\begin{bmatrix}a_1\\a_2\end{bmatrix}`
	batch := testBatch(orchestration.MathOutputComplexSVG, orchestration.MathBatchItem{
		Unit:      testUnit("rw_00000000000000000000000000000001", source, display, orchestration.ComputeMathMacroContextHash(nil)),
		WrapperID: "matrix-1",
	})

	result, err := service.ResolveMath(t.Context(), batch)
	if err != nil {
		t.Fatalf("resolve matrix fallback: %v", err)
	}
	if fallbackCalls != 1 || len(renderer.requests) != 0 {
		t.Fatalf("expected complex matrix to route directly to fallback, fallback=%d primary=%d", fallbackCalls, len(renderer.requests))
	}
	for _, expected := range []string{`rin-math-svg`, `rin-math-tall-inline`, `data-rin-math-svg-hash="aaaaaaaa`, `alt="` + source + `"`} {
		if !strings.Contains(result.Units[0].HTML, expected) {
			t.Fatalf("expected SVG wrapper to contain %q, got %s", expected, result.Units[0].HTML)
		}
	}
}

func TestResolveMathKeepsLegacyKaTeXClassInsideSharedWrapper(t *testing.T) {
	renderer := &recordingRenderer{results: []mathrender.BatchResult{{Result: mathrender.Result{
		HTML: `<span class="katex"></span>`, Engine: "katex", Version: "0.16.22",
	}}}}
	service, err := New(Config{
		Primary: renderer, PrimaryEngine: "katex", PrimaryVersion: "0.16.22",
		FontVersion: "mathjax-newcm-4.1.3", PluginVersion: "tex-input/v1",
		SanitizerVersion: "math-output/v1", CompatibilityVersion: "reader/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	display := false
	source := `\mathbf{x}=\begin{bmatrix}1\\2\end{bmatrix}`
	result, err := service.ResolveMath(t.Context(), testBatch(orchestration.MathOutputCHTML, orchestration.MathBatchItem{
		Unit: testUnit("rw_00000000000000000000000000000001", source, display, orchestration.ComputeMathMacroContextHash(nil)),
	}))
	if err != nil {
		t.Fatalf("resolve KaTeX compatibility unit: %v", err)
	}
	for _, expected := range []string{`rin-math-katex`, `rin-math-tall-inline`, `data-rin-math-engine="katex"`, `<span class="katex"></span>`} {
		if !strings.Contains(result.Units[0].HTML, expected) {
			t.Fatalf("expected shared KaTeX wrapper to contain %q, got %s", expected, result.Units[0].HTML)
		}
	}
}

func TestResolveMathFallsBackAfterPrimaryFailure(t *testing.T) {
	renderer := &recordingRenderer{results: []mathrender.BatchResult{{Err: errors.New("unsupported command")}}}
	service := newTestService(t, renderer, func(context.Context, orchestration.MathBatchItem) (FallbackResult, error) {
		return FallbackResult{
			URL:      "https://assets.example/rin/math.svg",
			SVGHash:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Artifact: testPublicSVGArtifact("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		}, nil
	})
	display := false
	result, err := service.ResolveMath(t.Context(), testBatch(orchestration.MathOutputCHTML, orchestration.MathBatchItem{
		Unit: testUnit("rw_00000000000000000000000000000001", `\unsupported{x}`, display, orchestration.ComputeMathMacroContextHash(nil)),
	}))
	if err != nil {
		t.Fatalf("resolve failed primary: %v", err)
	}
	if result.Units[0].Engine != "rin-texsvg" || len(result.Units[0].Diagnostics) == 0 || result.Units[0].Diagnostics[0].Code != "math.primary_fallback" {
		t.Fatalf("expected explicit successful fallback diagnostic, got %#v", result.Units[0])
	}
}

func TestResolveMathReturnsEscapedSourceWhenAllEnginesFail(t *testing.T) {
	renderer := &recordingRenderer{results: []mathrender.BatchResult{{Err: errors.New("unsupported command")}}}
	service := newTestService(t, renderer, nil)
	display := false
	source := `<script>alert(1)</script>\not\exists`
	result, err := service.ResolveMath(t.Context(), testBatch(orchestration.MathOutputCHTML, orchestration.MathBatchItem{
		Unit: testUnit("rw_00000000000000000000000000000001", source, display, orchestration.ComputeMathMacroContextHash(nil)),
	}))
	if err != nil {
		t.Fatalf("resolve total failure: %v", err)
	}
	unit := result.Units[0]
	if unit.State != "failed" || !strings.Contains(unit.HTML, `&lt;script&gt;alert(1)&lt;/script&gt;\not\exists`) || strings.Contains(unit.HTML, `<script>`) {
		t.Fatalf("expected safe escaped TeX fallback, got %#v", unit)
	}
	if len(unit.Diagnostics) < 2 || unit.Diagnostics[0].Code != "math.primary_fallback" || unit.Diagnostics[1].Code != "math.fallback.unavailable" {
		t.Fatalf("expected primary and unavailable fallback diagnostics, got %#v", unit.Diagnostics)
	}
}

func TestResolveMathBoundsPrimaryBatches(t *testing.T) {
	renderer := &recordingRenderer{}
	service := newTestService(t, renderer, nil)
	service.config.MaxBatchSize = 2
	hash := orchestration.ComputeMathMacroContextHash(nil)
	items := make([]orchestration.MathBatchItem, 5)
	for index := range items {
		display := false
		items[index].Unit = testUnit(
			"rw_0000000000000000000000000000000"+string(rune('1'+index)),
			string(rune('a'+index)), display, hash,
		)
	}
	result, err := service.ResolveMath(t.Context(), testBatch(orchestration.MathOutputCHTML, items...))
	if err != nil {
		t.Fatalf("resolve bounded batches: %v", err)
	}
	if len(result.Units) != 5 || len(renderer.batches) != 3 || len(renderer.batches[0]) != 2 || len(renderer.batches[1]) != 2 || len(renderer.batches[2]) != 1 {
		t.Fatalf("expected bounded 2/2/1 batches, batches=%#v result=%#v", renderer.batches, result)
	}
}

func TestResolveMathDeduplicatesPreferredFallback(t *testing.T) {
	renderer := &recordingRenderer{}
	fallbackCalls := 0
	service := newTestService(t, renderer, func(context.Context, orchestration.MathBatchItem) (FallbackResult, error) {
		fallbackCalls++
		return FallbackResult{
			URL:      "https://assets.example/rin/math.svg",
			SVGHash:  "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			Artifact: testPublicSVGArtifact("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		}, nil
	})
	hash := orchestration.ComputeMathMacroContextHash(nil)
	display := true
	source := `\begin{bmatrix}1\\2\end{bmatrix}`
	result, err := service.ResolveMath(t.Context(), testBatch(orchestration.MathOutputComplexSVG,
		orchestration.MathBatchItem{Unit: testUnit("rw_00000000000000000000000000000001", source, display, hash), WrapperID: "matrix-a"},
		orchestration.MathBatchItem{Unit: testUnit("rw_00000000000000000000000000000002", source, display, hash), WrapperID: "matrix-b"},
	))
	if err != nil {
		t.Fatalf("resolve repeated preferred fallback: %v", err)
	}
	if fallbackCalls != 1 || result.Units[0].Cache.Status != "miss" || result.Units[1].Cache.Status != "hit" {
		t.Fatalf("expected one fallback render and explicit reuse, calls=%d units=%#v", fallbackCalls, result.Units)
	}
	if !strings.Contains(result.Units[0].HTML, `id="matrix-a"`) || !strings.Contains(result.Units[1].HTML, `id="matrix-b"`) {
		t.Fatalf("expected reused artifact with per-occurrence wrappers, got %#v", result.Units)
	}
}

func testPublicSVGArtifact(hash string) *contracts.ArtifactReference {
	return &contracts.ArtifactReference{
		ArtifactID: "diagrams/v1/svg-sha256/" + hash[:2] + "/" + hash + ".svg",
		SHA256:     hash, Bytes: 128, MediaType: "image/svg+xml; charset=utf-8", Visibility: "public",
	}
}

func TestCacheKeyChangesForBehaviorAffectingMathInputs(t *testing.T) {
	service := newTestService(t, &recordingRenderer{}, nil)
	display := false
	hash := orchestration.ComputeMathMacroContextHash(nil)
	unit := testUnit("rw_00000000000000000000000000000001", "x", display, hash)
	base, err := service.cacheKey(unit, orchestration.MathOutputCHTML)
	if err != nil {
		t.Fatal(err)
	}
	changed := unit
	changed.Source = "y"
	changedSource, _ := service.cacheKey(changed, orchestration.MathOutputCHTML)
	changedStrategy, _ := service.cacheKey(unit, orchestration.MathOutputSVG)
	if base.Digest == changedSource.Digest || base.Digest == changedStrategy.Digest {
		t.Fatalf("expected source and strategy to change cache digest: %#v %#v %#v", base, changedSource, changedStrategy)
	}
	serviceWithNewFont, err := New(Config{
		Primary: &recordingRenderer{}, PrimaryEngine: "mathjax-chtml", PrimaryVersion: "4.1.3",
		FontVersion: "newcm-next", PluginVersion: "tex-input/v1", SanitizerVersion: "math-output/v1",
		CompatibilityVersion: "reader/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	changedFont, _ := serviceWithNewFont.cacheKey(unit, orchestration.MathOutputCHTML)
	if base.Digest == changedFont.Digest {
		t.Fatal("expected font contract to change cache digest")
	}
}

func TestMergeCSSKeepsLaterGlyphRulesAndNestedAtRules(t *testing.T) {
	blocks := MergeCSS(
		"mjx-container{display:inline-block}\n@font-face{font-family:MJX;src:url(a)}",
		"mjx-container{display:inline-block}\nmjx-c.mjx-c2204{padding:1em}",
		"@font-face{font-family:MJX;src:url(a)}\n.mjx-stretchy-v{height:2em}",
	)
	if len(blocks) != 1 {
		t.Fatalf("expected a single deterministic CSS aggregate, got %#v", blocks)
	}
	css := blocks[0]
	if strings.Count(css, "mjx-container{display:inline-block}") != 1 ||
		strings.Count(css, "@font-face{font-family:MJX;src:url(a)}") != 1 ||
		!strings.Contains(css, "mjx-c.mjx-c2204") || !strings.Contains(css, ".mjx-stretchy-v") {
		t.Fatalf("expected exact-rule aggregation without lost glyph rules, got %q", css)
	}
}

func newTestService(t *testing.T, renderer mathrender.Renderer, fallback FallbackFunc) *Service {
	t.Helper()
	config := Config{
		Primary: renderer, PrimaryEngine: "mathjax-chtml", PrimaryVersion: "4.1.3",
		FontVersion: "mathjax-newcm-4.1.3", PluginVersion: "tex-input/v1",
		SanitizerVersion: "math-output/v1", CompatibilityVersion: "reader/v1",
		Fallback: fallback,
	}
	if fallback != nil {
		config.FallbackVersion = "texlive-2025"
	}
	service, err := New(config)
	if err != nil {
		t.Fatalf("create test math service: %v", err)
	}
	return service
}

func testBatch(strategy orchestration.MathOutputStrategy, items ...orchestration.MathBatchItem) orchestration.MathBatch {
	hash := orchestration.ComputeMathMacroContextHash(nil)
	return orchestration.MathBatch{
		ContractVersion: orchestration.MathBatchContractVersion,
		Items:           items, MacroContexts: map[string]map[string]string{hash: {}}, OutputStrategy: strategy,
	}
}

func testUnit(id, source string, display bool, macroHash string) contracts.WorkUnit {
	return contracts.WorkUnit{
		Kind: contracts.WorkUnitMath, ID: id, Source: source, Display: &display, MacroContextHash: macroHash,
	}
}
