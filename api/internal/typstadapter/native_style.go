package typstadapter

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Policy follows the pinned 0.15.1 native emitters, not individual documents:
// typst-html/{rules,convert}.rs and css/encode.rs at 9dfd3a08500b7896045f907433cf7b4b02434fad.
// html.elem can also supply arbitrary CSS, so compiler provenance alone is not
// permission to execute it. These grammars admit only native, non-fetching CSS.
var imageCSSLength = regexp.MustCompile(`^(?:0|(?:[0-9]{1,9}(?:\.[0-9]{1,20})?|\.[0-9]{1,20})(?:pt|px|pc|cm|mm|in|em|rem|ex|%))$`)
var nativeCSSHexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}(?:[0-9a-fA-F]{2})?$`)
var nativeCSSColorNumber = regexp.MustCompile(`^-?(?:[0-9]{1,9}(?:\.[0-9]{1,20})?|\.[0-9]{1,20})(?:%|deg)?$`)

func nativeCSSLength(value string) bool {
	if value == "auto" || imageCSSLength.MatchString(value) {
		return true
	}
	if !strings.HasPrefix(value, "calc(") || !strings.HasSuffix(value, ")") {
		return false
	}
	// Typst Rel serializes at most three terms: percentage, em and pt.
	terms := strings.Fields(value[5 : len(value)-1])
	if len(terms) != 3 && len(terms) != 5 {
		return false
	}
	for i, term := range terms {
		if i%2 == 1 {
			if term != "+" && term != "-" {
				return false
			}
		} else if !imageCSSLength.MatchString(strings.TrimPrefix(term, "-")) {
			return false
		}
	}
	return true
}

func nativeCSSColor(value string) bool {
	if nativeCSSHexColor.MatchString(value) {
		return true
	}
	name, args, ok := strings.Cut(value, "(")
	if !ok || !strings.HasSuffix(args, ")") {
		return false
	}
	switch name {
	case "rgb", "hsl", "oklab", "oklch":
	case "color":
		if !strings.HasPrefix(args, "srgb-linear ") {
			return false
		}
		args = strings.TrimPrefix(args, "srgb-linear ")
	default:
		return false
	}
	fields := strings.Fields(strings.TrimSuffix(args, ")"))
	if len(fields) != 3 && (len(fields) != 5 || fields[3] != "/") {
		return false
	}
	for i, field := range fields {
		if i != 3 && !nativeCSSColorNumber.MatchString(field) {
			return false
		}
	}
	return true
}

func sanitizeImageStyle(raw string) (string, error) {
	return sanitizeNativeStyle("img", raw)
}

func sanitizeNativeStyle(tag, raw string) (string, error) {
	if len(raw) > 1024 {
		return "", fmt.Errorf("Typst native style is too long")
	}
	values := map[string]string{}
	for _, declaration := range strings.Split(raw, ";") {
		if strings.TrimSpace(declaration) == "" {
			continue
		}
		name, value, ok := strings.Cut(declaration, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || values[name] != "" {
			return "", fmt.Errorf("Typst native style contains an invalid or duplicate declaration")
		}
		valid := false
		switch name {
		case "width", "height":
			valid = (tag == "img" || tag == "svg") && nativeCSSLength(value)
		case "display":
			valid = value == "inline" || value == "block" || value == "inline-block" || value == "inline math" || value == "block math"
		case "object-fit":
			valid = tag == "img" && (value == "contain" || value == "cover" || value == "fill" || value == "none" || value == "scale-down")
		case "image-rendering":
			valid = (tag == "img" || tag == "svg") && (value == "auto" || value == "smooth" || value == "crisp-edges" || value == "pixelated")
		case "overflow":
			valid = tag == "svg" && value == "visible"
		case "text-decoration":
			valid = tag == "span" && (value == "underline" || value == "overline")
		case "font-variant-caps":
			valid = tag == "span" && (value == "small-caps" || value == "all-small-caps")
		case "white-space":
			valid = tag == "span" && value == "pre-wrap"
		case "list-style-type":
			valid = (tag == "ol" || tag == "ul") && value == "none"
		case "color":
			valid = tag == "span" && nativeCSSColor(value)
		case "math-shift":
			valid = mathPresentationElement(tag) && (value == "normal" || value == "compact")
		}
		if !valid {
			return "", fmt.Errorf("Typst native style property %q or value is unsupported", name)
		}
		values[name] = value
	}
	// An intrinsic HTML height must not stretch an image whose CSS sets only
	// its width (and conversely). Both explicit dimensions remain authoritative.
	if tag == "img" && values["width"] != "" && values["height"] == "" {
		values["height"] = "auto"
	} else if tag == "img" && values["height"] != "" && values["width"] == "" {
		values["width"] = "auto"
	}
	var result []string
	var names []string
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if value := values[name]; value != "" {
			result = append(result, name+": "+value)
		}
	}
	return strings.Join(result, "; "), nil
}
