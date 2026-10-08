package renderapi

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestAddLateXMLSemanticBlocksIsStableNonOverlappingAndStrict(t *testing.T) {
	fragment := strings.Join([]string{
		`<article data-rin-block-id="rb_ffffffffffffffffffffffffffffffff">`,
		`<h2 id="sec:intro">向量空间</h2>`,
		`<p>重复段落</p><p>重复段落</p>`,
		`<ol class="rin-list"><li class="rin-list-item">外层<ol><li class="rin-list-item">内层</li></ol></li></ol>`,
		`<div id="thm:one" class="rin-env rin-env-theorem"><h6 class="rin-env-title">Theorem 1</h6><p>Statement.</p></div>`,
		`<div class="rin-math rin-math-display" data-rin-math-source="x^2">rendered math</div>`,
		`<pre><code>const x = 1</code></pre>`,
		`<figure class="rin-float rin-float-figure"><a id="fig:one" class="rin-label"></a><figcaption>Figure one</figcaption></figure>`,
		`<table class="rin-table"><tr><td>A</td></tr></table>`,
		`</article>`,
	}, "")
	firstHTML, firstBlocks, err := addLateXMLSemanticBlocks(fragment, "main.tex")
	if err != nil {
		t.Fatalf("add semantic blocks: %v", err)
	}
	secondHTML, secondBlocks, err := addLateXMLSemanticBlocks(fragment, "main.tex")
	if err != nil || firstHTML != secondHTML || !equalDocumentBlocks(firstBlocks, secondBlocks) {
		t.Fatalf("identical LaTeXML finalization was not stable: err=%v", err)
	}
	if strings.Contains(firstHTML, `data-rin-block-id="rb_ffffffffffffffffffffffffffffffff"`) {
		t.Fatal("source-created reserved block id survived finalization")
	}
	kinds := map[contracts.DocumentBlockKind]bool{}
	ids := map[string]bool{}
	for _, block := range firstBlocks {
		kinds[block.Kind] = true
		if ids[block.ID] || !strings.HasPrefix(block.ID, "rb_") || len(block.ID) != 35 {
			t.Fatalf("invalid or duplicate block id: %#v", block)
		}
		ids[block.ID] = true
		if block.Kind != contracts.DocumentBlockHeading && len(block.HeadingPath) != 1 ||
			block.Kind != contracts.DocumentBlockHeading && block.HeadingPath[0] != "sec:intro" {
			t.Fatalf("block lost ancestor heading path: %#v", block)
		}
	}
	for _, kind := range []contracts.DocumentBlockKind{
		contracts.DocumentBlockHeading, contracts.DocumentBlockParagraph, contracts.DocumentBlockListItem,
		contracts.DocumentBlockTheorem, contracts.DocumentBlockMath, contracts.DocumentBlockCode,
		contracts.DocumentBlockFigure, contracts.DocumentBlockTable,
	} {
		if !kinds[kind] {
			t.Fatalf("missing LaTeXML semantic block kind %q", kind)
		}
	}
	mathBlock := findDocumentBlock(firstBlocks, contracts.DocumentBlockMath, "x^2")
	if mathBlock == nil {
		t.Fatal("display math block did not preserve its canonical TeX source")
	}
	if strings.Count(firstHTML, "data-rin-block-id=") != len(firstBlocks) || hasNestedSemanticBlock(firstHTML) {
		t.Fatal("semantic block manifest is not one-to-one and non-overlapping")
	}

	inserted := strings.Replace(fragment, `<p>重复段落</p>`, `<p>相邻新增</p><p>重复段落</p>`, 1)
	_, insertedBlocks, err := addLateXMLSemanticBlocks(inserted, "main.tex")
	if err != nil {
		t.Fatalf("add inserted semantic blocks: %v", err)
	}
	if blockIDsForText(firstBlocks, "重复段落") != blockIDsForText(insertedBlocks, "重复段落") {
		t.Fatal("adjacent insertion changed duplicate paragraph identities")
	}
	if findDocumentBlock(insertedBlocks, contracts.DocumentBlockParagraph, "相邻新增") == nil {
		t.Fatal("inserted paragraph did not receive a semantic block")
	}
}

func equalDocumentBlocks(left, right []contracts.DocumentBlock) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].ID != right[index].ID || left[index].Kind != right[index].Kind || left[index].Text != right[index].Text ||
			left[index].TextHash != right[index].TextHash || strings.Join(left[index].HeadingPath, "\x00") != strings.Join(right[index].HeadingPath, "\x00") {
			return false
		}
	}
	return true
}

func findDocumentBlock(blocks []contracts.DocumentBlock, kind contracts.DocumentBlockKind, text string) *contracts.DocumentBlock {
	for index := range blocks {
		if blocks[index].Kind == kind && blocks[index].Text == text {
			return &blocks[index]
		}
	}
	return nil
}

func blockIDsForText(blocks []contracts.DocumentBlock, text string) string {
	ids := make([]string, 0)
	for _, block := range blocks {
		if block.Text == text {
			ids = append(ids, block.ID)
		}
	}
	return strings.Join(ids, ",")
}

func hasNestedSemanticBlock(fragment string) bool {
	// Every supported container candidate returns before its descendants are visited. These known
	// fixture patterns catch a regression without depending on browser-side DOM inference.
	for _, pattern := range []string{
		`class="rin-env rin-env-theorem" data-rin-block-id=`,
		`<figure class="rin-float rin-float-figure" data-rin-block-id=`,
		`<table class="rin-table" data-rin-block-id=`,
	} {
		start := strings.Index(fragment, pattern)
		if start < 0 {
			continue
		}
		end := strings.Index(fragment[start:], map[string]string{
			`class="rin-env rin-env-theorem" data-rin-block-id=`:            `</div>`,
			`<figure class="rin-float rin-float-figure" data-rin-block-id=`: `</figure>`,
			`<table class="rin-table" data-rin-block-id=`:                   `</table>`,
		}[pattern])
		if end >= 0 && strings.Count(fragment[start:start+end], "data-rin-block-id=") > 1 {
			return true
		}
	}
	return false
}
