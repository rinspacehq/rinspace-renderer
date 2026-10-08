package diagramservice

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

// DirectiveInput is the source-format-neutral, non-executable boundary used by the Markdown
// adapter. Options are typed key/value data rather than a raw TeX option string; only the
// allowlisted keys below are serialized for the isolated TeX SVG engine.
type DirectiveInput struct {
	Type           string
	Source         string
	Options        map[string]string
	Alignment      string
	SourceLocation *contracts.SourceLocation
}

var safeDimensionPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,3})(?:\.[0-9]{1,4})?(?:pt|pc|in|bp|cm|mm|dd|cc|sp|em|ex)$`)

func NewDirectiveItem(input DirectiveInput) (orchestration.DiagramBatchItem, error) {
	kind, ok := NormalizeType(input.Type)
	if !ok {
		return orchestration.DiagramBatchItem{}, fmt.Errorf("unsupported diagram directive type %q", input.Type)
	}
	source := normalizeText(input.Source)
	if source == "" {
		return orchestration.DiagramBatchItem{}, errors.New("diagram directive source is required")
	}
	options, err := directiveOptions(kind, input.Options)
	if err != nil {
		return orchestration.DiagramBatchItem{}, err
	}
	alignment := strings.ToLower(strings.TrimSpace(input.Alignment))
	if alignment != "" && alignment != "center" && alignment != "flushleft" && alignment != "flushright" {
		return orchestration.DiagramBatchItem{}, fmt.Errorf("unsupported diagram directive alignment %q", input.Alignment)
	}
	id, err := contracts.NewWorkID()
	if err != nil {
		return orchestration.DiagramBatchItem{}, err
	}
	var layout *contracts.DiagramLayout
	if alignment != "" {
		layout = &contracts.DiagramLayout{Alignment: alignment}
	}
	item := orchestration.DiagramBatchItem{
		Unit: contracts.WorkUnit{
			Kind: contracts.WorkUnitDiagram, ID: id, DiagramType: kind, Source: source,
			Options: options, Layout: layout, SourceLocation: input.SourceLocation,
		},
		SourceMode: orchestration.DiagramSourceBody,
		Body:       source,
	}
	if err := item.Unit.Validate(); err != nil {
		return orchestration.DiagramBatchItem{}, fmt.Errorf("diagram directive: %w", err)
	}
	return item, nil
}

func directiveOptions(kind string, values map[string]string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	type option struct{ engineKey, value string }
	options := make([]option, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for rawKey, rawValue := range values {
		key := canonicalOptionKey(rawKey)
		if key == "" {
			return "", fmt.Errorf("diagram directive option %q is not allowlisted", rawKey)
		}
		if _, duplicate := seen[key]; duplicate {
			return "", fmt.Errorf("duplicate diagram directive option %q", rawKey)
		}
		seen[key] = struct{}{}
		engineKey, value, err := normalizeDirectiveOption(kind, key, rawValue)
		if err != nil {
			return "", err
		}
		options = append(options, option{engineKey: engineKey, value: value})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].engineKey < options[j].engineKey })
	serialized := make([]string, len(options))
	for index, option := range options {
		serialized[index] = option.engineKey + "=" + option.value
	}
	return strings.Join(serialized, ","), nil
}

func canonicalOptionKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	switch value {
	case "scale", "xscale", "yscale", "rotate", "width", "height", "grid":
		return value
	case "row-sep", "rowsep":
		return "row-sep"
	case "column-sep", "columnsep", "col-sep", "colsep":
		return "column-sep"
	default:
		return ""
	}
}

func normalizeDirectiveOption(kind, key, rawValue string) (string, string, error) {
	value := strings.ToLower(strings.TrimSpace(rawValue))
	switch key {
	case "scale", "xscale", "yscale":
		if kind != "tikzpicture" && kind != "axis" {
			return "", "", unsupportedDirectiveOption(kind, key)
		}
		number, err := boundedNumber(value, 0.05, 20)
		return key, number, err
	case "rotate":
		if kind != "tikzpicture" {
			return "", "", unsupportedDirectiveOption(kind, key)
		}
		number, err := boundedNumber(value, -360, 360)
		return key, number, err
	case "row-sep", "column-sep":
		if kind != "tikzcd" {
			return "", "", unsupportedDirectiveOption(kind, key)
		}
		if !oneOf(value, "tiny", "small", "normal", "large", "huge") {
			return "", "", fmt.Errorf("diagram directive option %q has an unsupported spacing", key)
		}
		return strings.ReplaceAll(key, "-", " "), value, nil
	case "width", "height":
		if kind != "axis" || !safeDimensionPattern.MatchString(value) {
			return "", "", fmt.Errorf("diagram directive option %q requires a safe axis dimension", key)
		}
		return key, value, nil
	case "grid":
		if kind != "axis" || !oneOf(value, "none", "major", "minor", "both") {
			return "", "", fmt.Errorf("diagram directive option %q has an unsupported axis grid", key)
		}
		return key, value, nil
	default:
		return "", "", unsupportedDirectiveOption(kind, key)
	}
}

func boundedNumber(value string, minimum, maximum float64) (string, error) {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < minimum || parsed > maximum {
		return "", errors.New("diagram directive numeric option is outside the allowed range")
	}
	return strconv.FormatFloat(parsed, 'f', -1, 64), nil
}

func unsupportedDirectiveOption(kind, key string) error {
	return fmt.Errorf("diagram directive option %q is not supported for %q", key, kind)
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
