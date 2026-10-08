package typstadapter

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// readerPageAttribute records the reader page that owns a cross-page link
// target. The sanitizer rejects every unrecognized data- attribute from
// compiler output, so only this pass, which knows the whole-book anchor map,
// may author it.
const readerPageAttribute = "data-rin-page"

// BookError reports a book structure violation that must block publication
// instead of producing a truncated "single page" book.
type BookError struct {
	Code    string
	Message string
}

func (err *BookError) Error() string { return err.Message }

func bookReject(code string, format string, args ...any) *BookError {
	return &BookError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// resolveBookLinks builds the whole-book anchor map, proves that each reader
// page begins with an explicit heading, rejects duplicate or unresolvable
// anchors, and marks cross-page references. Anchor identity is only ever the id
// the compiler emitted; it is never regenerated from a heading slug, an array
// index or a PDF page number.
func resolveBookLinks(pages []Page) error {
	pageByAnchor := make(map[string]string)
	for index := range pages {
		page := pages[index]
		if page.ID == "" || strings.TrimSpace(page.Title) == "" || page.Level <= 0 {
			return bookReject("typst.book.page.heading", "Typst book page %q does not begin with a heading", page.ID)
		}
		body := pageBody(page)
		if body == nil {
			return bookReject("typst.book.page.invalid", "Typst book page %q has no usable body", page.ID)
		}
		ids, duplicate := collectPageAnchors(body)
		if duplicate != "" {
			return bookReject("typst.book.anchor.duplicate", "duplicate Typst book anchor %q on page %q", duplicate, page.ID)
		}
		for _, id := range ids {
			if owner, exists := pageByAnchor[id]; exists {
				return bookReject("typst.book.anchor.duplicate", "duplicate Typst book anchor %q in pages %q and %q", id, owner, page.ID)
			}
			pageByAnchor[id] = page.ID
		}
	}
	for index := range pages {
		page := &pages[index]
		body := pageBody(*page)
		if body == nil {
			return bookReject("typst.book.page.invalid", "Typst book page %q has no usable body", page.ID)
		}
		if err := rewriteBookLinks(body, page.ID, pageByAnchor); err != nil {
			return err
		}
		var fragment bytes.Buffer
		for child := body.FirstChild; child != nil; child = child.NextSibling {
			if err := html.Render(&fragment, child); err != nil {
				return fmt.Errorf("render Typst book page %q: %w", page.ID, err)
			}
		}
		page.HTML = fragment.String()
	}
	return nil
}

func pageBody(page Page) *html.Node {
	doc, err := html.Parse(strings.NewReader(page.HTML))
	if err != nil {
		return nil
	}
	return findElement(doc, "body")
}

// collectPageAnchors returns the ids this page owns in document order, plus the
// first id that appears twice so callers can report an ambiguous page.
func collectPageAnchors(node *html.Node) ([]string, string) {
	ids := make([]string, 0, 8)
	seen := make(map[string]struct{})
	duplicate := ""
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if duplicate == "" && current.Type == html.ElementNode {
			for _, attr := range current.Attr {
				if attr.Key != "id" {
					continue
				}
				id := strings.TrimSpace(attr.Val)
				if id == "" {
					continue
				}
				if _, exists := seen[id]; exists {
					duplicate = id
					return
				}
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return ids, duplicate
}

func rewriteBookLinks(node *html.Node, pageID string, pageByAnchor map[string]string) error {
	if node.Type == html.ElementNode && node.Data == "a" {
		attrs := make([]html.Attribute, 0, len(node.Attr)+1)
		for _, attr := range node.Attr {
			attrs = append(attrs, attr)
			if attr.Key != "href" || !strings.HasPrefix(attr.Val, "#") {
				continue
			}
			owner, ok := resolveAnchorOwner(strings.TrimPrefix(attr.Val, "#"), pageByAnchor)
			if !ok {
				return bookReject("typst.book.link.unresolved", "Typst book page %q references missing anchor %q", pageID, attr.Val)
			}
			if owner != pageID {
				attrs = append(attrs, html.Attribute{Key: readerPageAttribute, Val: owner})
			}
		}
		node.Attr = attrs
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if err := rewriteBookLinks(child, pageID, pageByAnchor); err != nil {
			return err
		}
	}
	return nil
}

func resolveAnchorOwner(raw string, pageByAnchor map[string]string) (string, bool) {
	if owner, ok := pageByAnchor[raw]; ok {
		return owner, true
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || decoded == raw {
		return "", false
	}
	owner, ok := pageByAnchor[decoded]
	return owner, ok
}
