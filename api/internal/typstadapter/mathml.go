package typstadapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
	"github.com/rinspacehq/rinspace-renderer/api/internal/mathrender"
)

type mathLocation struct {
	page    int
	node    *html.Node
	text    string
	request int
}

// RenderMathML sends Typst's semantic MathML through the same pinned MathJax
// CHTML engine used by the LaTeX reader. Failure is fatal for the publication;
// it never silently ships an unstyled or partially converted formula.
func RenderMathML(ctx context.Context, adapted Result, renderer mathrender.Renderer) (Result, error) {
	if renderer == nil {
		return Result{}, errors.New("Typst MathML requires a managed MathJax renderer")
	}
	result := adapted
	result.Pages = append([]Page(nil), adapted.Pages...)
	locations := []mathLocation{}
	requests := []mathrender.Request{}
	requestBySource := map[string]int{}
	roots := make([]*html.Node, len(result.Pages))
	for index, page := range result.Pages {
		root := &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"}
		nodes, err := html.ParseFragment(strings.NewReader(page.HTML), root)
		if err != nil {
			return Result{}, err
		}
		for _, node := range nodes {
			root.AppendChild(node)
		}
		roots[index] = root
		for _, math := range collectMathNodes(root) {
			mathMLLayout(math)
			// Transfer the equation anchor to the reader-owned wrapper. Passing
			// it through MathJax as well would produce a duplicate DOM id.
			originalAttrs := math.Attr
			math.Attr = nil
			for _, attr := range originalAttrs {
				if attr.Key != "id" {
					math.Attr = append(math.Attr, attr)
				}
			}
			var source bytes.Buffer
			err := html.Render(&source, math)
			math.Attr = originalAttrs
			if err != nil {
				return Result{}, err
			}
			if source.Len() > 32<<10 {
				return Result{}, errors.New("Typst MathML formula exceeds the source limit")
			}
			display := false
			for _, attr := range math.Attr {
				if attr.Key == "display" {
					display = attr.Val == "block"
				}
			}
			key := fmt.Sprintf("%t\x00%s", display, source.String())
			request, exists := requestBySource[key]
			if !exists {
				request = len(requests)
				requestBySource[key] = request
				requests = append(requests, mathrender.Request{Source: source.String(), InputFormat: "mathml", DisplayMode: display})
			}
			locations = append(locations, mathLocation{page: index, node: math, text: source.String(), request: request})
		}
	}
	if len(requests) == 0 {
		return result, nil
	}
	// Bound each shared-engine operation; a long book must not turn the
	// per-operation math timeout into a whole-book timeout or one huge batch.
	batch := make([]mathrender.BatchResult, 0, len(requests))
	for start := 0; start < len(requests); start += 128 {
		end := min(start+128, len(requests))
		chunk, err := renderer.RenderBatch(ctx, requests[start:end])
		if err != nil {
			return Result{}, fmt.Errorf("Typst MathML rendering failed: %w", err)
		}
		if len(chunk) != end-start {
			return Result{}, errors.New("Typst MathML returned an incomplete batch")
		}
		batch = append(batch, chunk...)
	}
	cssParts := []string{}
	seenCSS := map[string]bool{}
	for _, item := range batch {
		if item.Result.CSS != "" && !seenCSS[item.Result.CSS] {
			seenCSS[item.Result.CSS] = true
			cssParts = append(cssParts, item.Result.CSS)
		}
	}
	css := strings.Join(cssParts, "\n")
	mathPages := map[int]struct{}{}
	for index, location := range locations {
		item := batch[location.request]
		if item.Err != nil || strings.TrimSpace(item.Result.HTML) == "" || item.Result.Engine != "mathjax-chtml" {
			return Result{}, fmt.Errorf("Typst MathML formula %d failed: %v", index+1, item.Err)
		}
		wrapper := &html.Node{Type: html.ElementNode, DataAtom: atom.Span, Data: "span", Attr: []html.Attribute{
			{Key: "class", Val: "rin-math rin-math-mathjax"},
			{Key: "data-rin-math-engine", Val: "mathjax-chtml"},
			{Key: "data-rin-math-source", Val: location.text},
		}}
		for _, attr := range location.node.Attr {
			if attr.Key == "id" {
				wrapper.Attr = append(wrapper.Attr, attr)
			}
		}
		if requests[location.request].DisplayMode {
			wrapper.Attr[0].Val += " rin-math-display"
		} else {
			wrapper.Attr[0].Val += " rin-math-inline"
		}
		fragment, err := html.ParseFragment(strings.NewReader(item.Result.HTML), wrapper)
		if err != nil || len(fragment) != 1 || fragment[0].Data != "mjx-container" {
			return Result{}, errors.New("MathJax returned an invalid CHTML root")
		}
		wrapper.AppendChild(fragment[0])
		parent := location.node.Parent
		parent.InsertBefore(wrapper, location.node)
		parent.RemoveChild(location.node)
		mathPages[location.page] = struct{}{}
	}
	if css == "" {
		return Result{}, errors.New("MathJax omitted required CHTML CSS")
	}
	css = strings.ReplaceAll(css, "</", "<\\/")
	for index, root := range roots {
		var output bytes.Buffer
		if _, hasMath := mathPages[index]; hasMath {
			output.WriteString(`<style class="rin-mathjax-chtml-style" data-rin-math-engine="mathjax-chtml">`)
			output.WriteString(css)
			output.WriteString(`</style>`)
		}
		for child := root.FirstChild; child != nil; child = child.NextSibling {
			if err := html.Render(&output, child); err != nil {
				return Result{}, err
			}
		}
		result.Pages[index].HTML = output.String()
		if err := finaloutput.Validate(finaloutput.Input{Adapter: "typst", Fragment: output.String(), ClientOwnedAssets: result.AssetPaths}); err != nil {
			return Result{}, fmt.Errorf("Typst MathJax final output: %w", err)
		}
	}
	return result, nil
}

func collectMathNodes(root *html.Node) []*html.Node {
	result := []*html.Node{}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "math" {
			result = append(result, node)
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return result
}
