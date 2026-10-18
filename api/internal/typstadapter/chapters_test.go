package typstadapter

import (
	"strings"
	"testing"
)

func TestAutomaticChaptersPreservePrefaceHierarchyAndAnchors(t *testing.T) {
	source := []byte(`<html><body><p>Preface</p><h2 id="chapter">First</h2><p>One</p><h3 id="section">Section</h3><p>Two</p><h2>Last</h2><p>Three</p></body></html>`)
	book, err := AdaptHTML(source, "book", nil)
	if err != nil || len(book.Pages) != 3 {
		t.Fatalf("automatic chapters: %#v %v", book.Pages, err)
	}
	if book.Pages[0].ID != "chapter" || book.Pages[1].ID != "section" || book.Pages[1].Level != 3 ||
		!strings.Contains(book.Pages[0].HTML, "Preface") || strings.Contains(book.Pages[0].HTML, "Two") {
		t.Fatalf("lost preface, hierarchy or boundaries: %#v", book.Pages)
	}
	again, err := AdaptHTML(source, "book", nil)
	if err != nil || book.Pages[2].ID != again.Pages[2].ID {
		t.Fatalf("generated chapter identity is not deterministic: %v", err)
	}
	article, err := AdaptHTML(source, "article", nil)
	if err != nil || len(article.Pages) != 1 || !strings.Contains(article.Pages[0].HTML, "Three") {
		t.Fatalf("articles must not be paginated: %#v %v", article.Pages, err)
	}
}

func TestAutomaticChaptersHandleRepeatedTitlesAndPlainBooks(t *testing.T) {
	result, err := AdaptHTML([]byte(`<html><body><h2>Repeated</h2><p>one</p><h2>Repeated</h2><p>two</p></body></html>`), "book", nil)
	if err != nil || len(result.Pages) != 2 || result.Pages[0].ID == result.Pages[1].ID {
		t.Fatalf("repeated unlabelled chapter titles: %#v %v", result.Pages, err)
	}
	plain, err := AdaptHTML([]byte(`<html><body><p>All of the text.</p></body></html>`), "book", nil)
	if err != nil || len(plain.Pages) != 1 || plain.Pages[0].HTML == "" {
		t.Fatalf("plain book content was lost: %#v %v", plain.Pages, err)
	}
}
