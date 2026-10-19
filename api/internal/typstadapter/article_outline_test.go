package typstadapter

import (
	"reflect"
	"strings"
	"testing"
)

func TestArticleHeadingAnchorsAreStableAndPreserveNativeLinks(t *testing.T) {
	source := []byte(`<html><body><h2>引言</h2><section><h3 id="loc-2">Nested</h3>` +
		`<h3>Repeated</h3><h3>Repeated</h3></section><p><a href="#loc-2">Reference</a></p></body></html>`)
	first, err := AdaptHTML(source, "article", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AdaptHTML(source, "article", nil)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("article output is not deterministic: %v", err)
	}
	if len(first.Pages) != 1 {
		t.Fatalf("article was paginated: %#v", first.Pages)
	}
	root := pageBody(first.Pages[0])
	ids, duplicate := collectPageAnchors(root)
	if len(ids) != 4 || duplicate != "" || !strings.Contains(first.Pages[0].HTML, `href="#loc-2"`) {
		t.Fatalf("missing or ambiguous heading anchors: ids=%v duplicate=%q", ids, duplicate)
	}
	if ids[1] != "loc-2" || !strings.HasPrefix(ids[0], "typst-heading-") || ids[2] == ids[3] {
		t.Fatalf("native or repeated heading identity changed: %v", ids)
	}
}

func TestArticleHeadingAnchorAllocationReservesLaterNativeIDs(t *testing.T) {
	first, err := AdaptHTML([]byte(`<html><body><h2>Repeated</h2></body></html>`), "article", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := collectPageAnchors(pageBody(first.Pages[0]))
	if len(ids) != 1 {
		t.Fatal("unreferenced heading has no reader anchor")
	}
	second, err := AdaptHTML([]byte(`<html><body><h2>Repeated</h2><p id="`+ids[0]+`">Native anchor</p></body></html>`), "article", nil)
	if err != nil {
		t.Fatal(err)
	}
	all, duplicate := collectPageAnchors(pageBody(second.Pages[0]))
	if len(all) != 2 || duplicate != "" || all[1] != ids[0] || all[0] == ids[0] {
		t.Fatalf("generated heading shadows a native target: %v %q", all, duplicate)
	}
}

func TestArticleOutlineRejectsAmbiguousNativeHeadingAnchors(t *testing.T) {
	_, err := AdaptHTML([]byte(`<html><body><h2 id="same">Heading</h2><p id="same">Other target</p></body></html>`), "article", nil)
	if err == nil || !strings.Contains(err.Error(), "ambiguous anchor") {
		t.Fatalf("reader navigation accepted an ambiguous heading target: %v", err)
	}
}

func TestArticleOutlineReplacesEmptyIDWithoutDuplicateAttribute(t *testing.T) {
	result, err := AdaptHTML([]byte(`<html><body><h2 id="">Heading</h2></body></html>`), "article", nil)
	if err != nil || len(result.ArticleTOC) != 1 || strings.Count(result.Pages[0].HTML, `id="`) != 1 {
		t.Fatalf("empty heading anchor was not safely filled: %v", err)
	}
}
