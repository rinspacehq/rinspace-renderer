package publicationpdf

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	stdhtml "html"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

//go:embed print.css
var printCSS string

// ResolveImage returns the offline data URL for a verified source or renderer
// artifact. The caller binds it to the immutable publication input.
type ResolveImage func(ctx context.Context, image ImageReference) (string, error)

type ImageReference struct {
	Source          string
	RepositoryPath  string
	DiagramObjectID string
}

// MarkdownHTML assembles the final fragments in the published order. It keeps
// artifact resolution outside Chromium so printing requires no network.
func MarkdownHTML(ctx context.Context, bundle contracts.DocumentBundle, book bool, resolveImage ResolveImage) (string, error) {
	if bundle.State != contracts.DocumentBundleStateFinal || bundle.ContentKind != contracts.ContentKindMarkdown ||
		bundle.BundleHash == "" {
		return "", errors.New("print requires a final Markdown document bundle")
	}
	if err := bundle.Validate(); err != nil {
		return "", fmt.Errorf("print bundle: %w", err)
	}
	if !book && len(bundle.Pages) != 1 {
		return "", errors.New("article PDF requires exactly one finalized fragment")
	}

	pages := make(map[string]string, len(bundle.Pages))
	for index, page := range bundle.Pages {
		if _, exists := pages[page.SourcePath]; exists {
			return "", fmt.Errorf("duplicate print chapter path %q", page.SourcePath)
		}
		pages[page.SourcePath] = "chapter-" + strconv.Itoa(index+1)
	}

	var doc strings.Builder
	doc.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; font-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"><title>`)
	doc.WriteString(stdhtml.EscapeString(bundle.Title))
	doc.WriteString("</title><style>")
	doc.WriteString(printCSS)
	doc.WriteString("</style></head><body><main>")
	if book {
		doc.WriteString("<header><h1>" + stdhtml.EscapeString(bundle.Title) + "</h1></header>")
	}

	for _, page := range bundle.Pages {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		id := pages[page.SourcePath]
		nodes, err := html.ParseFragment(strings.NewReader(page.Fragment), &html.Node{
			Type: html.ElementNode, Data: "section", DataAtom: atom.Section,
		})
		if err != nil {
			return "", fmt.Errorf("parse print chapter %q: %w", page.ID, err)
		}
		section := &html.Node{Type: html.ElementNode, Data: "section", DataAtom: atom.Section}
		for _, node := range nodes {
			section.AppendChild(node)
		}
		if err := prepareFragment(ctx, section, id, pages, book, resolveImage); err != nil {
			return "", fmt.Errorf("prepare print chapter %q: %w", page.ID, err)
		}
		section.Attr = []html.Attribute{{Key: "id", Val: id}}
		class := "print-article"
		if book {
			class = "print-chapter"
		}
		section.Attr = append(section.Attr, html.Attribute{Key: "class", Val: class})
		if !hasHeading(section, book) {
			tag := atom.H1
			title := bundle.Title
			if book {
				tag, title = atom.H2, page.Title
			}
			if title != "" {
				heading := &html.Node{Type: html.ElementNode, Data: tag.String(), DataAtom: tag}
				heading.AppendChild(&html.Node{Type: html.TextNode, Data: title})
				section.InsertBefore(heading, section.FirstChild)
			}
		}
		if err := html.Render(&doc, section); err != nil {
			return "", fmt.Errorf("serialize print chapter: %w", err)
		}
	}
	doc.WriteString("</main></body></html>")
	return doc.String(), nil
}

func prepareFragment(ctx context.Context, root *html.Node, chapter string, pages map[string]string, book bool, resolveImage ResolveImage) error {
	if root.Type == html.ElementNode {
		switch root.DataAtom {
		case atom.Script, atom.Iframe, atom.Object, atom.Embed, atom.Base, atom.Link:
			return errors.New("active element in finalized print fragment")
		}
		if book && root.DataAtom == atom.H1 {
			root.DataAtom, root.Data = atom.H2, "h2"
		}
		hasImageSource := false
		image := ImageReference{}
		if root.DataAtom == atom.Img {
			for _, attr := range root.Attr {
				switch attr.Key {
				case "data-rin-asset-path":
					image.RepositoryPath = attr.Val
				case "data-rin-diagram-object-id":
					image.DiagramObjectID = attr.Val
				}
			}
		}
		for index := range root.Attr {
			attr := &root.Attr[index]
			switch attr.Key {
			case "id":
				attr.Val = chapter + "--" + attr.Val
			case "href":
				attr.Val = printLink(attr.Val, chapter, pages)
			case "aria-describedby", "aria-labelledby", "headers":
				values := strings.Fields(attr.Val)
				for i := range values {
					values[i] = chapter + "--" + values[i]
				}
				attr.Val = strings.Join(values, " ")
			case "src":
				if root.DataAtom != atom.Img {
					return errors.New("unsupported media source in finalized print fragment")
				}
				hasImageSource = true
				if attr.Val == "" {
					return errors.New("finalized print image source is missing")
				}
				if !isDataImage(attr.Val) {
					if resolveImage == nil {
						return errors.New("print image has no offline asset")
					}
					image.Source = attr.Val
					resolved, err := resolveImage(ctx, image)
					if err != nil {
						return err
					}
					if !isDataImage(resolved) {
						return errors.New("invalid offline print image")
					}
					attr.Val = resolved
				}
			case "srcset":
				return errors.New("responsive image sources must be resolved before printing")
			}
		}
		if root.DataAtom == atom.Img && !hasImageSource {
			return errors.New("finalized print image source is missing")
		}
	}
	for node := root.FirstChild; node != nil; node = node.NextSibling {
		if err := prepareFragment(ctx, node, chapter, pages, book, resolveImage); err != nil {
			return err
		}
	}
	return nil
}

func printLink(value, chapter string, pages map[string]string) string {
	target, err := url.Parse(value)
	if err != nil {
		return ""
	}
	if target.IsAbs() {
		switch target.Scheme {
		case "https", "http", "mailto":
			return value
		default:
			return ""
		}
	}
	if target.Host != "" || target.RawQuery != "" {
		return ""
	}
	if target.Path != "" {
		chapter = pages[target.Path]
		if chapter == "" {
			return ""
		}
	}
	if target.Fragment != "" {
		return "#" + chapter + "--" + target.Fragment
	}
	if target.Path != "" {
		return "#" + chapter
	}
	return ""
}

func hasHeading(node *html.Node, book bool) bool {
	want := atom.H1
	if book {
		want = atom.H2
	}
	if node.DataAtom == want {
		return true
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if hasHeading(child, book) {
			return true
		}
	}
	return false
}

func isDataImage(source string) bool {
	media, _, found := strings.Cut(source, ";base64,")
	if !found {
		return false
	}
	switch media {
	case "data:image/png", "data:image/jpeg", "data:image/gif", "data:image/webp", "data:image/svg+xml":
		return true
	default:
		return false
	}
}
