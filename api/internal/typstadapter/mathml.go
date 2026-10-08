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
	page int
	node *html.Node
	text string
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
			var source bytes.Buffer
			if err := html.Render(&source, math); err != nil {
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
			locations = append(locations, mathLocation{page: index, node: math, text: source.String()})
			requests = append(requests, mathrender.Request{Source: source.String(), InputFormat: "mathml", DisplayMode: display})
		}
	}
	if len(requests) == 0 {
		return result, nil
	}
	batch, err := renderer.RenderBatch(ctx, requests)
	if err != nil {
		return Result{}, fmt.Errorf("Typst MathML rendering failed: %w", err)
	}
	if len(batch) != len(requests) {
		return Result{}, fmt.Errorf("Typst MathML returned %d results for %d formulas", len(batch), len(requests))
	}
	css := ""
	mathPages := map[int]struct{}{}
	for index, item := range batch {
		if item.Err != nil || strings.TrimSpace(item.Result.HTML) == "" || item.Result.Engine != "mathjax-chtml" {
			return Result{}, fmt.Errorf("Typst MathML formula %d failed: %v", index+1, item.Err)
		}
		if css == "" {
			css = item.Result.CSS
		}
		location := locations[index]
		wrapper := &html.Node{Type: html.ElementNode, DataAtom: atom.Span, Data: "span", Attr: []html.Attribute{
			{Key: "class", Val: "rin-math rin-math-mathjax"},
			{Key: "data-rin-math-engine", Val: "mathjax-chtml"},
			{Key: "data-rin-math-source", Val: location.text},
		}}
		if requests[index].DisplayMode {
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
