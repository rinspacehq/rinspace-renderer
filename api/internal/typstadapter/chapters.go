package typstadapter

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"golang.org/x/net/html"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

// Pagination is a post-compile reading concern. Source files, PDF page numbers
// and custom author macros do not define HTML reading boundaries.
func automaticBookPages(body *html.Node) (map[*html.Node]Page, error) {
	boundaries := map[*html.Node]Page{}
	var first *html.Node
	counts := map[string]int{}
	seen := map[string]bool{}
	for node := body.FirstChild; node != nil; node = node.NextSibling {
		if node.Type == html.TextNode && strings.TrimSpace(node.Data) == "" {
			continue
		}
		if first == nil {
			first = node
		}
		if node.Type != html.ElementNode || !isHeading(node.Data) {
			continue
		}
		title := strings.Join(strings.Fields(textContent(node)), " ")
		if title == "" {
			return nil, bookReject("typst.book.page.heading", "Typst book has an empty chapter heading")
		}
		id := ""
		for _, attr := range node.Attr {
			if attr.Key == "id" && contracts.ValidPageID(attr.Val) {
				id = attr.Val
				break
			}
		}
		if id == "" {
			counts[title]++
			hash := sha256.Sum256([]byte(title))
			id = fmt.Sprintf("chapter-%x", hash[:8])
			if counts[title] > 1 {
				id += fmt.Sprintf("-%d", counts[title])
			}
		}
		if seen[id] {
			return nil, bookReject("typst.book.anchor.duplicate", "duplicate Typst chapter anchor %q", id)
		}
		seen[id] = true
		start := node
		if len(boundaries) == 0 {
			// Keep any preface, title or introductory content with the first
			// chapter instead of losing content before its heading.
			start = first
		}
		boundaries[start] = Page{ID: id, Title: title, Level: int(node.Data[1] - '0')}
	}
	if first == nil {
		return nil, bookReject("typst.book.page.empty", "Typst book HTML has no content")
	}
	if len(boundaries) == 0 {
		boundaries[first] = Page{ID: "page-main", Title: "Document", Level: 1}
	}
	return boundaries, nil
}
