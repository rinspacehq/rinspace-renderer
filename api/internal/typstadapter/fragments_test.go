package typstadapter

import (
	"strings"
	"testing"
	"time"
)

func TestFragmentInputRejectsIdentityInjectionBeforeCompile(t *testing.T) {
	for _, test := range []struct {
		blocks []FragmentBlock
		want   string
	}{
		{nil, "invalid fragment block count"},
		{[]FragmentBlock{{ID: `x"); evil`, Source: "text"}}, "invalid or duplicate fragment block"},
		{[]FragmentBlock{{ID: "one", Source: "text"}, {ID: "one", Source: "other"}}, "invalid or duplicate fragment block"},
		{[]FragmentBlock{{ID: "one", Source: ""}}, "invalid or duplicate fragment block"},
		{[]FragmentBlock{{ID: "one", Source: strings.Repeat("x", MaxFragmentSourceBytes+1)}}, "fragment sources exceed limit"},
	} {
		if _, err := CompileFragments(t.Context(), Compiler{}, test.blocks); err == nil || err.Error() != test.want {
			t.Fatalf("input was not rejected before compiler validation: want %q got %v", test.want, err)
		}
	}
}

func TestFragmentOutputRejectsMissingReorderedAndDuplicateAnchors(t *testing.T) {
	for _, source := range []string{
		`<div id="fragment-b">b</div><div id="fragment-a">a</div>`,
		`<div id="fragment-a"><span id="fragment-a">a</span></div><div id="fragment-b">b</div>`,
		`<p>escaped</p><div id="fragment-a">a</div><div id="fragment-b">b</div>`,
		`<div id="fragment-a">a</div>`,
	} {
		if err := validateFragmentRoots(source, []string{"a", "b"}); err == nil {
			t.Fatalf("ambiguous output admitted: %s", source)
		}
	}
	if err := validateFragmentRoots(`<div id="fragment-a">a</div><div id="fragment-b"><span id="eq-b">b</span></div>`, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedFragmentsPreserveCrossBlockForwardReferences(t *testing.T) {
	compiler := pinnedCorpusCompiler(t)
	blocks := []FragmentBlock{
		{ID: "first", Source: "= First step\nSee @eq-later.\n$ x = 1 $ <eq-first>\n"},
		{ID: "second", Source: "= Second step\n$ y = x + 1 $ <eq-later>\nSee @eq-first.\n"},
	}
	result, err := CompileFragments(t.Context(), compiler, blocks)
	if err != nil {
		t.Fatalf("pinned fragments failed: %v: %s", err, result.Log)
	}
	fragment := result.Document.Pages[0].HTML
	for _, expected := range []string{`id="fragment-first"`, `id="fragment-second"`, `href="#eq-later"`, `href="#eq-first"`, `id="eq-later"`, `id="eq-first"`, "<math"} {
		if !strings.Contains(fragment, expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	if result.TemplateVersion != FragmentTemplateVersion || len(result.BlockIDs) != 2 || len(result.Document.Assets) != 0 {
		t.Fatal("fragment identity or semantic output changed")
	}
}

func TestPinnedFragmentsRejectForgedWrapperAndActiveHTML(t *testing.T) {
	compiler := pinnedCorpusCompiler(t)
	for _, source := range []string{
		`#html.elem("div", attrs: (id: "fragment-one"), [forged])`,
		`#html.elem("script", [alert(1)])`,
	} {
		if _, err := CompileFragments(t.Context(), compiler, []FragmentBlock{{ID: "one", Source: source}}); err == nil {
			t.Fatal("forged or active fragment admitted")
		}
	}
}

func TestPinnedFragmentPrefixCost(t *testing.T) {
	compiler := pinnedCorpusCompiler(t)
	for _, count := range []int{1, 8, 32} {
		blocks := make([]FragmentBlock, count)
		for i := range blocks {
			blocks[i] = FragmentBlock{ID: "step-" + strings.Repeat("a", i+1), Source: "A complete step: $ x^2 + y^2 = 1 $.\n"}
		}
		started := time.Now()
		result, err := CompileFragments(t.Context(), compiler, blocks)
		if err != nil {
			t.Fatalf("%d-block prefix: %v: %s", count, err, result.Log)
		}
		t.Logf("blocks=%d bytes=%d elapsed=%s", count, len(result.Document.Pages[0].HTML), time.Since(started))
	}
}
