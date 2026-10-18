package typstadapter

import (
	"strings"

	"golang.org/x/net/html"
)

// The pinned Typst backend expresses table alignment through private CSS
// classes. MathJax consumes MathML, not Typst's page stylesheet. Translate the
// compiler's known layout hints into equivalent MathML attributes before
// handing them to the shared engine; never forward an arbitrary stylesheet.
func mathMLLayout(node *html.Node) {
	classes := func(node *html.Node) map[string]bool {
		result := map[string]bool{}
		for _, attr := range node.Attr {
			if attr.Key == "class" {
				for _, class := range strings.Fields(attr.Val) {
					result[class] = true
				}
			}
		}
		return result
	}
	if node.Type == html.ElementNode && node.Data == "mtable" {
		table := classes(node)
		columns := 0
		for row := node.FirstChild; row != nil; row = row.NextSibling {
			if row.Type != html.ElementNode || row.Data != "mtr" {
				continue
			}
			count := 0
			for cell := row.FirstChild; cell != nil; cell = cell.NextSibling {
				if cell.Type != html.ElementNode || cell.Data != "mtd" {
					continue
				}
				count++
				cellClasses := classes(cell)
				if cellClasses["left-align"] {
					cell.Attr = append(cell.Attr, html.Attribute{Key: "columnalign", Val: "left"})
				} else if cellClasses["right-align"] {
					cell.Attr = append(cell.Attr, html.Attribute{Key: "columnalign", Val: "right"})
				}
			}
			columns = max(columns, count)
		}
		alignment := ""
		if table["cases"] || table["left-align"] {
			alignment = "left"
		}
		if table["right-align"] {
			alignment = "right"
		}
		if table["aligned"] {
			values := make([]string, columns)
			for i := range values {
				if i%2 == 0 {
					values[i] = "right"
				} else {
					values[i] = "left"
				}
			}
			alignment = strings.Join(values, " ")
		}
		if alignment != "" {
			node.Attr = append(node.Attr, html.Attribute{Key: "columnalign", Val: alignment})
		}
		if table["cases"] || table["aligned"] || table["multiline-equation"] {
			node.Attr = append(node.Attr, html.Attribute{Key: "columnspacing", Val: "0em"})
		}
		if table["multiline-equation"] {
			node.Attr = append(node.Attr, html.Attribute{Key: "rowspacing", Val: "0.5em"})
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		mathMLLayout(child)
	}
}
