package diagramservice

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

func TestNewDirectiveItemBuildsCanonicalRestrictedUnit(t *testing.T) {
	item, err := NewDirectiveItem(DirectiveInput{
		Type: "tikz-cd", Source: " A \\arrow[r] & B ", Alignment: " CENTER ",
		Options:        map[string]string{"columnSep": "Large", "row_sep": "small"},
		SourceLocation: &contracts.SourceLocation{Path: "chapter.md", Start: contracts.SourcePosition{Line: 4, Column: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.Unit.Kind != contracts.WorkUnitDiagram || item.Unit.DiagramType != "tikzcd" ||
		item.SourceMode != orchestration.DiagramSourceBody || item.Unit.Source != item.Body ||
		item.Unit.Options != "column sep=large,row sep=small" || item.Unit.Layout.Alignment != "center" ||
		!strings.HasPrefix(item.Unit.ID, "rw_") || item.Unit.SourceLocation.Path != "chapter.md" {
		t.Fatalf("directive item = %#v", item)
	}
}

func TestNewDirectiveItemRejectsArbitraryExecutableOptions(t *testing.T) {
	tests := []DirectiveInput{
		{Type: "tikzpicture", Source: `\draw (0,0)--(1,1);`, Options: map[string]string{"execute": `\input{/etc/passwd}`}},
		{Type: "tikzpicture", Source: `\draw (0,0)--(1,1);`, Options: map[string]string{"scale": `1,execute at begin picture={\write18{bad}}`}},
		{Type: "axis", Source: `\addplot coordinates {(0,0)};`, Options: map[string]string{"width": `8cm,after end axis=\input{x}`}},
		{Type: "tikzcd", Source: `A & B`, Options: map[string]string{"row-sep": `large,execute at end picture={bad}`}},
	}
	for _, input := range tests {
		if _, err := NewDirectiveItem(input); err == nil {
			t.Fatalf("unsafe directive options accepted: %#v", input.Options)
		}
	}
}

func TestNewDirectiveItemRejectsInvalidTypeAlignmentAndTypeSpecificOption(t *testing.T) {
	tests := []DirectiveInput{
		{Type: "unknown", Source: "body"},
		{Type: "tikzpicture", Source: "body", Alignment: "left"},
		{Type: "chemfig", Source: "body", Options: map[string]string{"scale": "2"}},
		{Type: "axis", Source: "body", Options: map[string]string{"grid": "everything"}},
	}
	for _, input := range tests {
		if _, err := NewDirectiveItem(input); err == nil {
			t.Fatalf("invalid directive accepted: %#v", input)
		}
	}
}

func TestDirectiveOptionsAreDeterministic(t *testing.T) {
	first, err := directiveOptions("axis", map[string]string{"grid": "major", "height": "6CM", "width": "8cm", "scale": "1.0"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := directiveOptions("axis", map[string]string{"scale": "1", "width": "8cm", "height": "6cm", "grid": "major"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first != "grid=major,height=6cm,scale=1,width=8cm" {
		t.Fatalf("canonical options = %q / %q", first, second)
	}
}
