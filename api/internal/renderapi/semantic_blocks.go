package renderapi

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

type semanticBlockState struct {
	sourcePath        string
	headingStack      []semanticHeading
	duplicateOrdinals map[string]int
	blocks            []contracts.DocumentBlock
	existingIDs       map[string]struct{}
	err               error
}

type semanticHeading struct {
	depth int
	id    string
}

type semanticBlockContext struct {
	directListItem bool
	directQuote    bool
}

func addLateXMLSemanticBlocks(fragment, sourcePath string) (string, []contracts.DocumentBlock, error) {
	contextNode := &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"}
	nodes, err := html.ParseFragment(strings.NewReader(fragment), contextNode)
	if err != nil {
		return "", nil, fmt.Errorf("parse final LaTeXML fragment for semantic blocks: %w", err)
	}
	root := &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"}
	for _, node := range nodes {
		root.AppendChild(node)
	}
	state := semanticBlockState{
		sourcePath:        sourcePath,
		duplicateOrdinals: map[string]int{},
		existingIDs:       map[string]struct{}{},
	}
	walkSemanticHTML(root, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		removeHTMLAttribute(node, "data-rin-block-id")
		removeHTMLAttribute(node, "data-rin-block-kind")
		if id := htmlNodeAttribute(node, "id"); id != "" {
			state.existingIDs[id] = struct{}{}
		}
	})
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		state.visit(child, semanticBlockContext{})
	}
	if len(state.blocks) == 0 {
		if fallback := firstSemanticBlockFallback(root); fallback != nil {
			state.add(fallback, contracts.DocumentBlockParagraph, semanticHTMLText(fallback), explicitSemanticAnchor(fallback))
		}
	}
	if state.err != nil {
		return "", nil, state.err
	}
	if len(state.blocks) == 0 {
		return "", nil, errors.New("final LaTeXML fragment has no supported semantic blocks")
	}
	var output bytes.Buffer
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if err := html.Render(&output, child); err != nil {
			return "", nil, fmt.Errorf("serialize final LaTeXML semantic blocks: %w", err)
		}
	}
	return output.String(), state.blocks, nil
}

func firstSemanticBlockFallback(root *html.Node) *html.Node {
	var fallback *html.Node
	walkSemanticHTML(root, func(node *html.Node) {
		if fallback != nil || node == root || node.Type != html.ElementNode ||
			strings.EqualFold(node.Data, "style") || strings.EqualFold(node.Data, "script") || semanticHTMLText(node) == "" {
			return
		}
		fallback = node
	})
	return fallback
}

func (state *semanticBlockState) visit(node *html.Node, context semanticBlockContext) {
	if node.Type != html.ElementNode {
		return
	}
	tag := strings.ToLower(node.Data)
	if depth := semanticHeadingDepth(tag); depth > 0 {
		for len(state.headingStack) > 0 && state.headingStack[len(state.headingStack)-1].depth >= depth {
			state.headingStack = state.headingStack[:len(state.headingStack)-1]
		}
		anchor := state.ensureHeadingID(node)
		state.add(node, contracts.DocumentBlockHeading, semanticHTMLText(node), anchor)
		state.headingStack = append(state.headingStack, semanticHeading{depth: depth, id: anchor})
		return
	}
	classes := semanticHTMLClassSet(node)
	switch {
	case classes["rin-env"]:
		state.add(node, contracts.DocumentBlockTheorem, semanticHTMLText(node), explicitSemanticAnchor(node))
		return
	case tag == "figure":
		kind := contracts.DocumentBlockFigure
		if classes["rin-float-table"] || hasSemanticDescendant(node, "table", "rin-table") {
			kind = contracts.DocumentBlockTable
		}
		state.add(node, kind, semanticHTMLText(node), explicitSemanticAnchor(node))
		return
	case tag == "table":
		kind := contracts.DocumentBlockTable
		if classes["rin-equation"] || classes["rin-equation-table"] {
			kind = contracts.DocumentBlockMath
		}
		state.add(node, kind, semanticHTMLTextOrSource(node), explicitSemanticAnchor(node))
		return
	case classes["rin-math-display"]:
		state.add(node, contracts.DocumentBlockMath, semanticHTMLTextOrSource(node), explicitSemanticAnchor(node))
		return
	case tag == "pre":
		state.add(node, contracts.DocumentBlockCode, semanticHTMLText(node), explicitSemanticAnchor(node))
		return
	case tag == "li":
		if !hasSemanticDescendant(node, "li", "") {
			state.add(node, contracts.DocumentBlockListItem, semanticHTMLText(node), explicitSemanticAnchor(node))
			return
		}
		state.visitChildren(node, semanticBlockContext{directListItem: true})
		return
	case tag == "blockquote":
		state.visitChildren(node, semanticBlockContext{directQuote: true})
		return
	case tag == "p":
		kind := contracts.DocumentBlockParagraph
		if context.directListItem {
			kind = contracts.DocumentBlockListItem
		} else if context.directQuote {
			kind = contracts.DocumentBlockQuote
		}
		state.add(node, kind, semanticHTMLText(node), explicitSemanticAnchor(node))
		return
	case tag == "img":
		state.add(node, contracts.DocumentBlockFigure, firstNonEmpty(htmlNodeAttribute(node, "alt"), htmlNodeAttribute(node, "title")), explicitSemanticAnchor(node))
		return
	}
	state.visitChildren(node, context)
}

func (state *semanticBlockState) visitChildren(node *html.Node, context semanticBlockContext) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		state.visit(child, context)
	}
}

func (state *semanticBlockState) add(node *html.Node, kind contracts.DocumentBlockKind, value, explicitAnchor string) {
	text := contracts.NormalizeDocumentBlockText(value)
	headingPath := make([]string, len(state.headingStack))
	for index, heading := range state.headingStack {
		headingPath[index] = heading.id
	}
	identity := []string{"anchor", state.sourcePath, string(kind), explicitAnchor}
	if explicitAnchor == "" {
		identity = append([]string{"content", state.sourcePath}, headingPath...)
		identity = append(identity, string(kind), text)
		key := strings.Join(identity, "\x00")
		ordinal := state.duplicateOrdinals[key]
		state.duplicateOrdinals[key] = ordinal + 1
		identity = append(identity, fmt.Sprintf("%d", ordinal))
	}
	id, err := contracts.NewDocumentBlockID(strings.Join(identity, "\x00"))
	if err != nil {
		state.err = fmt.Errorf("generate final LaTeXML semantic block id: %w", err)
		return
	}
	setHTMLAttribute(node, "data-rin-block-id", id)
	setHTMLAttribute(node, "data-rin-block-kind", string(kind))
	state.blocks = append(state.blocks, contracts.DocumentBlock{
		ID: id, Kind: kind, Text: text, TextHash: contracts.DocumentBlockTextHash(text), HeadingPath: headingPath,
	})
}

func (state *semanticBlockState) ensureHeadingID(node *html.Node) string {
	if id := htmlNodeAttribute(node, "id"); contracts.ValidTOCID(id) {
		return id
	}
	seed, _ := contracts.NewDocumentBlockID(strings.Join([]string{"heading-anchor", state.sourcePath, semanticHTMLText(node)}, "\x00"))
	base := "rin-block-heading-" + strings.TrimPrefix(seed, "rb_")
	id := base
	for index := 2; ; index++ {
		if _, exists := state.existingIDs[id]; !exists {
			break
		}
		id = fmt.Sprintf("%s-%d", base, index)
	}
	setHTMLAttribute(node, "id", id)
	state.existingIDs[id] = struct{}{}
	return id
}

func semanticHeadingDepth(tag string) int {
	if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
		return int(tag[1] - '0')
	}
	return 0
}

func semanticHTMLTextOrSource(node *html.Node) string {
	if source := htmlNodeAttribute(node, "data-rin-math-source"); source != "" {
		return source
	}
	var found string
	walkSemanticHTML(node, func(child *html.Node) {
		if found == "" && child.Type == html.ElementNode {
			found = htmlNodeAttribute(child, "data-rin-math-source")
		}
	})
	if found != "" {
		return found
	}
	return semanticHTMLText(node)
}

func semanticHTMLText(node *html.Node) string {
	var text strings.Builder
	walkSemanticHTML(node, func(child *html.Node) {
		if child.Type == html.TextNode {
			text.WriteString(child.Data)
			text.WriteByte(' ')
		}
	})
	return contracts.NormalizeDocumentBlockText(text.String())
}

func explicitSemanticAnchor(node *html.Node) string {
	if id := htmlNodeAttribute(node, "id"); contracts.ValidTOCID(id) {
		return id
	}
	var anchor string
	walkSemanticHTML(node, func(child *html.Node) {
		if anchor != "" || child.Type != html.ElementNode {
			return
		}
		classes := semanticHTMLClassSet(child)
		if !classes["rin-label"] && !classes["ltx_label"] {
			return
		}
		if id := htmlNodeAttribute(child, "id"); contracts.ValidTOCID(id) {
			anchor = id
		}
	})
	return anchor
}

func hasSemanticDescendant(node *html.Node, tag, class string) bool {
	found := false
	for child := node.FirstChild; child != nil && !found; child = child.NextSibling {
		walkSemanticHTML(child, func(candidate *html.Node) {
			if found || candidate.Type != html.ElementNode || strings.ToLower(candidate.Data) != tag {
				return
			}
			found = class == "" || semanticHTMLClassSet(candidate)[class]
		})
	}
	return found
}

func semanticHTMLClassSet(node *html.Node) map[string]bool {
	classes := map[string]bool{}
	for _, class := range strings.Fields(htmlNodeAttribute(node, "class")) {
		classes[class] = true
	}
	return classes
}

func walkSemanticHTML(node *html.Node, visit func(*html.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walkSemanticHTML(child, visit)
	}
}

func htmlNodeAttribute(node *html.Node, key string) string {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, key) {
			return strings.TrimSpace(attribute.Val)
		}
	}
	return ""
}

func setHTMLAttribute(node *html.Node, key, value string) {
	removeHTMLAttribute(node, key)
	node.Attr = append(node.Attr, html.Attribute{Key: key, Val: value})
}

func removeHTMLAttribute(node *html.Node, key string) {
	attributes := node.Attr[:0]
	for _, attribute := range node.Attr {
		if !strings.EqualFold(attribute.Key, key) {
			attributes = append(attributes, attribute)
		}
	}
	node.Attr = attributes
}
