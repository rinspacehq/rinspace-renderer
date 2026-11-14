package typstadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// FragmentTemplateVersion is an additional cache-key input for this entrypoint.
// The existing article/book adapter and publication profiles are unchanged.
const FragmentTemplateVersion = "typst-fragment-template/v1"

const MaxFragmentSourceBytes = 4 << 20
const MaxFragmentBlocks = 128

var fragmentID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type FragmentBlock struct {
	ID     string `json:"id"`
	Source string `json:"source"`
}

type FragmentResult struct {
	Document        Result
	BlockIDs        []string
	TemplateVersion string
	Log             string
	CompilerCPU     time.Duration
}

// CompileFragments compiles complete content blocks as ONE Typst document, so
// equation numbering and label resolution belong to Typst. Individual sources
// are included as files under a trusted wrapper, not concatenated into an
// executable string literal or converted using regular expressions.
func CompileFragments(ctx context.Context, compiler Compiler, blocks []FragmentBlock) (FragmentResult, error) {
	files, ids, err := BuildFragmentProject(blocks)
	if err != nil {
		return FragmentResult{}, err
	}
	compiled, err := compiler.Compile(ctx, files, "main.typ", "html")
	if err != nil {
		return FragmentResult{Log: compiled.Log}, err
	}
	adapted, err := AdaptHTML(compiled.Output, "article", files)
	if err != nil {
		return FragmentResult{Log: compiled.Log}, err
	}
	if len(adapted.Pages) != 1 {
		return FragmentResult{}, errors.New("fragment result must be one semantic document")
	}
	if err := ValidateFragmentRoots(adapted.Pages[0].HTML, ids); err != nil {
		return FragmentResult{}, err
	}
	return FragmentResult{Document: adapted, BlockIDs: ids, TemplateVersion: FragmentTemplateVersion, Log: compiled.Log, CompilerCPU: compiled.UserCPU + compiled.SystemCPU}, nil
}

// BuildFragmentProject only constructs bounded files; it never invokes a
// compiler and can safely be used by the queue submission adapter.
func BuildFragmentProject(blocks []FragmentBlock) (map[string][]byte, []string, error) {
	if len(blocks) == 0 || len(blocks) > MaxFragmentBlocks {
		return nil, nil, errors.New("invalid fragment block count")
	}
	files := make(map[string][]byte, len(blocks)+1)
	ids := make([]string, 0, len(blocks))
	seen := map[string]bool{}
	var main strings.Builder
	main.WriteString("#set math.equation(numbering: \"(1)\")\n")
	total := 0
	for _, block := range blocks {
		if !fragmentID.MatchString(block.ID) || seen[block.ID] || strings.TrimSpace(block.Source) == "" || !utf8.ValidString(block.Source) {
			return nil, nil, errors.New("invalid or duplicate fragment block")
		}
		if len(block.Source) > MaxFragmentSourceBytes-total {
			return nil, nil, errors.New("fragment sources exceed limit")
		}
		total += len(block.Source)
		seen[block.ID] = true
		ids = append(ids, block.ID)
		path := "blocks/" + block.ID + ".typ"
		files[path] = []byte(block.Source)
		fmt.Fprintf(&main, "#html.elem(\"div\", attrs: (id: \"fragment-%s\"), [#include \"%s\"])\n", block.ID, path)
	}
	files["main.typ"] = []byte(main.String())
	return files, ids, nil
}

type FragmentOptions struct {
	TemplateVersion string   `json:"templateVersion"`
	BlockIDs        []string `json:"blockIds"`
}

func ParseFragmentOptions(source string) (*FragmentOptions, error) {
	if strings.TrimSpace(source) == "" {
		return nil, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(source), &root); err != nil {
		// Legacy jobs allow opaque options. Only the explicit new namespace
		// opts into this contract and its stricter decoding.
		if !strings.Contains(source, `"typstFragments"`) {
			return nil, nil
		}
		return nil, err
	}
	body, present := root["typstFragments"]
	if !present {
		return nil, nil
	}
	var options FragmentOptions
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&options); err != nil {
		return nil, err
	}
	if options.TemplateVersion != FragmentTemplateVersion || len(options.BlockIDs) == 0 || len(options.BlockIDs) > MaxFragmentBlocks {
		return nil, errors.New("invalid fragment options")
	}
	seen := map[string]bool{}
	for _, id := range options.BlockIDs {
		if !fragmentID.MatchString(id) || seen[id] {
			return nil, errors.New("invalid fragment identity")
		}
		seen[id] = true
	}
	return &options, nil
}

func ValidateFragmentProject(files map[string][]byte, ids []string) error {
	blocks := make([]FragmentBlock, 0, len(ids))
	for _, id := range ids {
		blocks = append(blocks, FragmentBlock{ID: id, Source: string(files["blocks/"+id+".typ"])})
	}
	expected, _, err := BuildFragmentProject(blocks)
	if err != nil {
		return err
	}
	if len(expected) != len(files) {
		return errors.New("fragment project contains unexpected files")
	}
	for name, body := range expected {
		if !bytes.Equal(body, files[name]) {
			return errors.New("fragment project wrapper differs from trusted template")
		}
	}
	return nil
}

func ValidateFragmentRoots(source string, ids []string) error {
	return validateFragmentRoots(source, ids)
}

func validateFragmentRoots(source string, ids []string) error {
	root := &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"}
	nodes, err := html.ParseFragment(strings.NewReader(source), root)
	if err != nil {
		return err
	}
	allIDs := map[string]bool{}
	var visit func(*html.Node) error
	visit = func(node *html.Node) error {
		for _, attr := range node.Attr {
			if attr.Key == "id" {
				if allIDs[attr.Val] {
					return fmt.Errorf("fragment has duplicate anchor %q", attr.Val)
				}
				allIDs[attr.Val] = true
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	position := 0
	for _, node := range nodes {
		if node.Type == html.TextNode && strings.TrimSpace(node.Data) == "" {
			continue
		}
		if position >= len(ids) || node.Type != html.ElementNode || node.Data != "div" {
			return errors.New("fragment output escaped its block wrapper")
		}
		id := ""
		for _, attr := range node.Attr {
			if attr.Key == "id" {
				id = attr.Val
			}
		}
		if id != "fragment-"+ids[position] {
			return errors.New("fragment root identity or order changed")
		}
		if err := visit(node); err != nil {
			return err
		}
		position++
	}
	if position != len(ids) {
		return errors.New("fragment output omitted blocks")
	}
	return nil
}
