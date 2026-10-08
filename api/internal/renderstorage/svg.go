package renderstorage

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const (
	svgNamespace   = "http://www.w3.org/2000/svg"
	xlinkNamespace = "http://www.w3.org/1999/xlink"
	xmlNamespace   = "http://www.w3.org/XML/1998/namespace"
)

type NormalizedSVGObject struct {
	Bytes    []byte
	Hash     string
	ObjectID string
}

type svgNode struct {
	Name     string
	Attr     []svgAttr
	Children []svgChild
}

type svgAttr struct {
	Name  string
	Value string
}

type svgChild struct {
	Text string
	Node *svgNode
}

func NormalizeSVGObject(raw []byte) (NormalizedSVGObject, error) {
	normalized, err := NormalizeSVG(raw)
	if err != nil {
		return NormalizedSVGObject{}, err
	}
	hash := SVGHash(normalized)
	return NormalizedSVGObject{
		Bytes:    normalized,
		Hash:     hash,
		ObjectID: svgObjectIDFromHash(hash),
	}, nil
}

func NormalizeSVG(raw []byte) ([]byte, error) {
	input := normalizeSVGInput(raw)
	if input == "" {
		return nil, errors.New("SVG is empty")
	}
	root, err := parseSVG(input)
	if err != nil {
		return nil, err
	}
	if !hasVisibleSVGContent(root) {
		return nil, errors.New("SVG has no visible content")
	}

	var out bytes.Buffer
	writeSVGNode(&out, root, true)
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func normalizeSVGInput(raw []byte) string {
	value := string(raw)
	value = strings.TrimPrefix(value, "\ufeff")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSpace(value)
}

func parseSVG(input string) (*svgNode, error) {
	decoder := xml.NewDecoder(strings.NewReader(input))
	var stack []*svgNode
	var root *svgNode
	skipDepth := 0
	seenRoot := false

	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("invalid SVG XML: %w", err)
		}

		switch typed := token.(type) {
		case xml.StartElement:
			if skipDepth > 0 {
				skipDepth++
				continue
			}
			name, ok := svgElementName(typed.Name)
			if !ok {
				skipDepth = 1
				continue
			}
			if !seenRoot {
				seenRoot = true
				if !strings.EqualFold(name, "svg") {
					return nil, errors.New("SVG root element is required")
				}
			} else if len(stack) == 0 {
				return nil, errors.New("SVG must contain a single root element")
			}
			if len(stack) > 0 && isDroppedSVGElement(name) {
				skipDepth = 1
				continue
			}
			node := &svgNode{Name: name, Attr: sanitizeSVGAttrs(typed.Attr)}
			if strings.EqualFold(name, "svg") {
				attrs, err := ensureRootViewBox(node.Attr)
				if err != nil {
					return nil, err
				}
				node.Attr = attrs
			}
			stack = append(stack, node)
		case xml.EndElement:
			if skipDepth > 0 {
				skipDepth--
				continue
			}
			if len(stack) == 0 {
				return nil, errors.New("SVG contains an unexpected closing element")
			}
			node := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if pruned := pruneSVGNode(node); pruned != nil {
				if len(stack) == 0 {
					if root != nil {
						return nil, errors.New("SVG must contain a single root element")
					}
					root = pruned
				} else {
					parent := stack[len(stack)-1]
					parent.Children = append(parent.Children, svgChild{Node: pruned})
				}
			}
		case xml.CharData:
			if skipDepth > 0 {
				continue
			}
			text := strings.ReplaceAll(string(typed), "\r\n", "\n")
			text = strings.ReplaceAll(text, "\r", "\n")
			if strings.TrimSpace(text) == "" {
				continue
			}
			if len(stack) == 0 {
				return nil, errors.New("SVG contains text outside the root element")
			}
			current := stack[len(stack)-1]
			current.Children = append(current.Children, svgChild{Text: text})
		case xml.Comment, xml.Directive, xml.ProcInst:
			continue
		}
	}

	if len(stack) != 0 {
		return nil, errors.New("SVG contains unclosed elements")
	}
	if root == nil {
		return nil, errors.New("SVG root element is required")
	}
	return root, nil
}

func svgElementName(name xml.Name) (string, bool) {
	if name.Local == "" {
		return "", false
	}
	if name.Space == "" || name.Space == svgNamespace {
		return name.Local, true
	}
	return "", false
}

func sanitizeSVGAttrs(attrs []xml.Attr) []svgAttr {
	cleaned := make([]svgAttr, 0, len(attrs))
	seen := make(map[string]bool, len(attrs))
	for _, attr := range attrs {
		name := svgAttrName(attr.Name)
		if name == "" {
			continue
		}
		lowerName := strings.ToLower(name)
		if seen[lowerName] || isSVGEventAttr(lowerName) {
			continue
		}
		value := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(attr.Value, "\r\n", "\n"), "\r", "\n"))
		if dangerousSVGValue(value) {
			continue
		}
		seen[lowerName] = true
		cleaned = append(cleaned, svgAttr{Name: canonicalSVGAttrName(name), Value: value})
	}
	sort.SliceStable(cleaned, func(i, j int) bool {
		return strings.ToLower(cleaned[i].Name) < strings.ToLower(cleaned[j].Name)
	})
	return cleaned
}

func svgAttrName(name xml.Name) string {
	if name.Local == "" {
		return ""
	}
	switch name.Space {
	case "":
		return name.Local
	case xlinkNamespace, "xlink":
		return "xlink:" + name.Local
	case xmlNamespace, "xml":
		return "xml:" + name.Local
	case svgNamespace:
		return name.Local
	case "xmlns":
		return ""
	default:
		return ""
	}
}

func canonicalSVGAttrName(name string) string {
	if strings.EqualFold(name, "viewbox") {
		return "viewBox"
	}
	return name
}

func isSVGEventAttr(lowerName string) bool {
	local := lowerName
	if index := strings.LastIndex(local, ":"); index >= 0 {
		local = local[index+1:]
	}
	return strings.HasPrefix(local, "on")
}

func ensureRootViewBox(attrs []svgAttr) ([]svgAttr, error) {
	viewBox := ""
	filtered := attrs[:0]
	for _, attr := range attrs {
		if strings.EqualFold(attr.Name, "viewBox") {
			if viewBox == "" {
				viewBox = normalizeViewBox(attr.Value)
			}
			continue
		}
		filtered = append(filtered, attr)
	}
	if viewBox == "" {
		derived, ok := deriveViewBox(filtered)
		if !ok {
			return nil, errors.New("SVG root requires a non-empty viewBox")
		}
		viewBox = derived
	}
	filtered = append(filtered, svgAttr{Name: "viewBox", Value: viewBox})
	sort.SliceStable(filtered, func(i, j int) bool {
		return svgAttrSortKey(filtered[i].Name) < svgAttrSortKey(filtered[j].Name)
	})
	return filtered, nil
}

func normalizeViewBox(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, ",", " ")
	return strings.Join(strings.Fields(value), " ")
}

func deriveViewBox(attrs []svgAttr) (string, bool) {
	width, widthOK := svgAttrValue(attrs, "width")
	height, heightOK := svgAttrValue(attrs, "height")
	if !widthOK || !heightOK {
		return "", false
	}
	w, ok := parseSVGLength(width)
	if !ok {
		return "", false
	}
	h, ok := parseSVGLength(height)
	if !ok {
		return "", false
	}
	return "0 0 " + formatSVGNumber(w) + " " + formatSVGNumber(h), true
}

func svgAttrValue(attrs []svgAttr, name string) (string, bool) {
	for _, attr := range attrs {
		if strings.EqualFold(attr.Name, name) {
			return attr.Value, true
		}
	}
	return "", false
}

func parseSVGLength(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "%") {
		return 0, false
	}
	index := 0
	for index < len(value) {
		ch := value[index]
		if (ch >= '0' && ch <= '9') || ch == '+' || ch == '-' || ch == '.' || ch == 'e' || ch == 'E' {
			index++
			continue
		}
		break
	}
	if index == 0 {
		return 0, false
	}
	number, err := strconv.ParseFloat(value[:index], 64)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

func formatSVGNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func pruneSVGNode(node *svgNode) *svgNode {
	name := strings.ToLower(node.Name)
	if isHelperSVGNode(node) {
		return nil
	}
	if name == "style" && dangerousSVGValue(plainSVGText(node)) {
		return nil
	}
	return node
}

func isDroppedSVGElement(name string) bool {
	switch strings.ToLower(name) {
	case "script", "foreignobject", "iframe", "object", "embed", "audio", "video", "canvas",
		"animate", "animatetransform", "animatemotion", "set":
		return true
	default:
		return false
	}
}

func isHelperSVGNode(node *svgNode) bool {
	name := strings.ToLower(node.Name)
	switch name {
	case "text", "tspan", "title", "desc":
	default:
		return false
	}
	for _, attr := range node.Attr {
		lowerName := strings.ToLower(attr.Name)
		lowerValue := strings.ToLower(attr.Value)
		if lowerName == "data-rin-helper" || lowerName == "data-rin-renderer-helper" {
			return true
		}
		if lowerName == "id" || lowerName == "class" || strings.HasPrefix(lowerName, "data-") {
			if containsSVGHelperMarker(lowerValue) {
				return true
			}
		}
	}
	return containsSVGHelperMarker(strings.ToLower(plainSVGText(node)))
}

func containsSVGHelperMarker(value string) bool {
	for _, marker := range []string{
		"rin_renderer_helper",
		"rin-renderer-helper",
		"rin_diagram_helper",
		"rin-diagram-helper",
		"rin-tikz-helper",
		"rin_texsvg_helper",
		"rin-texsvg-helper",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func hasVisibleSVGContent(node *svgNode) bool {
	if isHiddenSVGNode(node) {
		return false
	}
	name := strings.ToLower(node.Name)
	if isNonRenderingSVGElement(name) {
		return false
	}
	switch name {
	case "path", "rect", "circle", "ellipse", "line", "polyline", "polygon", "use", "image":
		return true
	case "text", "tspan", "textpath":
		if strings.TrimSpace(plainSVGText(node)) != "" {
			return true
		}
	}
	for _, child := range node.Children {
		if child.Node != nil && hasVisibleSVGContent(child.Node) {
			return true
		}
	}
	return false
}

func isNonRenderingSVGElement(name string) bool {
	switch name {
	case "defs", "metadata", "style", "title", "desc", "clippath", "mask", "pattern",
		"lineargradient", "radialgradient", "marker", "symbol", "filter":
		return true
	default:
		return false
	}
}

func isHiddenSVGNode(node *svgNode) bool {
	for _, attr := range node.Attr {
		value := strings.ToLower(strings.TrimSpace(attr.Value))
		switch strings.ToLower(attr.Name) {
		case "display":
			if value == "none" {
				return true
			}
		case "visibility":
			if value == "hidden" || value == "collapse" {
				return true
			}
		case "opacity":
			if value == "0" || value == "0.0" || value == ".0" {
				return true
			}
		case "style":
			style := compactSVGValue(value)
			if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") || strings.Contains(style, "opacity:0") {
				return true
			}
		}
	}
	return false
}

func plainSVGText(node *svgNode) string {
	var builder strings.Builder
	var walk func(*svgNode)
	walk = func(current *svgNode) {
		for _, child := range current.Children {
			if child.Node != nil {
				walk(child.Node)
			} else {
				builder.WriteString(child.Text)
			}
		}
	}
	walk(node)
	return builder.String()
}

func dangerousSVGValue(value string) bool {
	compact := compactSVGValue(value)
	for _, marker := range []string{
		"javascript:",
		"vbscript:",
		"data:text/html",
		"data:application/xhtml+xml",
		"data:image/svg+xml",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

func compactSVGValue(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func writeSVGNode(out *bytes.Buffer, node *svgNode, root bool) {
	out.WriteByte('<')
	out.WriteString(node.Name)
	attrs := append([]svgAttr(nil), node.Attr...)
	if root {
		attrs = rootSVGAttrs(attrs, usesXLink(node))
	}
	for _, attr := range attrs {
		out.WriteByte(' ')
		out.WriteString(attr.Name)
		out.WriteString(`="`)
		out.WriteString(escapeSVGAttr(attr.Value))
		out.WriteByte('"')
	}
	if len(node.Children) == 0 {
		out.WriteString("/>")
		return
	}
	out.WriteByte('>')
	for _, child := range node.Children {
		if child.Node != nil {
			writeSVGNode(out, child.Node, false)
		} else {
			_ = xml.EscapeText(out, []byte(child.Text))
		}
	}
	out.WriteString("</")
	out.WriteString(node.Name)
	out.WriteByte('>')
}

func rootSVGAttrs(attrs []svgAttr, xlink bool) []svgAttr {
	filtered := make([]svgAttr, 0, len(attrs)+2)
	for _, attr := range attrs {
		lower := strings.ToLower(attr.Name)
		if lower == "xmlns" || strings.HasPrefix(lower, "xmlns:") {
			continue
		}
		filtered = append(filtered, attr)
	}
	filtered = append(filtered, svgAttr{Name: "xmlns", Value: svgNamespace})
	if xlink {
		filtered = append(filtered, svgAttr{Name: "xmlns:xlink", Value: xlinkNamespace})
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return svgAttrSortKey(filtered[i].Name) < svgAttrSortKey(filtered[j].Name)
	})
	return filtered
}

func usesXLink(node *svgNode) bool {
	for _, attr := range node.Attr {
		if strings.HasPrefix(strings.ToLower(attr.Name), "xlink:") {
			return true
		}
	}
	for _, child := range node.Children {
		if child.Node != nil && usesXLink(child.Node) {
			return true
		}
	}
	return false
}

func svgAttrSortKey(name string) string {
	lower := strings.ToLower(name)
	switch lower {
	case "xmlns":
		return "00:" + lower
	case "xmlns:xlink":
		return "01:" + lower
	case "viewbox":
		return "02:" + lower
	default:
		return "10:" + lower
	}
}

func escapeSVGAttr(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return replacer.Replace(value)
}
