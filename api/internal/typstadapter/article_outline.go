package typstadapter

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"golang.org/x/net/html"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

// Unreferenced Typst headings need not have native IDs. The final renderer,
// rather than the browser, owns the extra anchors required for reader navigation.
type articleOutline struct {
	ids     map[string]int
	entries []contracts.TOCEntry
}

func newArticleOutline(body *html.Node) *articleOutline {
	outline := &articleOutline{ids: map[string]int{}, entries: []contracts.TOCEntry{}}
	var reserve func(*html.Node)
	reserve = func(node *html.Node) {
		for _, attr := range node.Attr {
			if attr.Key == "id" && attr.Val != "" {
				outline.ids[attr.Val]++
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			reserve(child)
		}
	}
	reserve(body)
	return outline
}

// Only visit already-sanitized nodes. Generated outline/endnote headings are
// reader apparatus, not article sections; their original HTML stays untouched.
func (outline *articleOutline) visit(node *html.Node) error {
	if node.Type != html.ElementNode {
		return nil
	}
	if node.Data == "nav" {
		return nil
	}
	for _, attr := range node.Attr {
		if attr.Key == "role" && (attr.Val == "doc-endnotes" || attr.Val == "doc-footnote") {
			return nil
		}
	}
	if isHeading(node.Data) {
		title := strings.Join(strings.Fields(textContent(node)), " ")
		if title != "" {
			id := ""
			for _, attr := range node.Attr {
				if attr.Key == "id" {
					id = attr.Val
				}
			}
			if id == "" {
				hash := sha256.Sum256([]byte(title))
				base := fmt.Sprintf("typst-heading-%x", hash[:12])
				id = base
				for ordinal := 2; outline.ids[id] != 0; ordinal++ {
					id = fmt.Sprintf("%s-%d", base, ordinal)
				}
				outline.ids[id] = 1
				// An explicit empty id is replaced, not duplicated.
				attrs := node.Attr[:0]
				for _, attr := range node.Attr {
					if attr.Key != "id" {
						attrs = append(attrs, attr)
					}
				}
				node.Attr = append(attrs, html.Attribute{Key: "id", Val: id})
			}
			if !contracts.ValidTOCID(id) || outline.ids[id] != 1 {
				return fmt.Errorf("Typst article heading has an invalid or ambiguous anchor %q", id)
			}
			outline.entries = append(outline.entries, contracts.TOCEntry{ID: id, Text: title, Depth: int(node.Data[1] - '0')})
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if err := outline.visit(child); err != nil {
			return err
		}
	}
	return nil
}
