package knowledgeindex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

var testIdentity = Identity{ProjectID: "tag-wiki:288", SourceCommit: strings.Repeat("a", 40), ProjectHash: strings.Repeat("b", 64)}

func TestExtractPermanentKnowledgeCommandsDeterministically(t *testing.T) {
	files := []projectcore.File{
		{Path: "z.tex", Body: "\\rinanchor{same-name-two}{definition}{同名概念}\n\\rintagref{120}{同名目标} 与 \\rintagref{121}{同名目标}"},
		{Path: "a.tex", Body: "% \\rinanchor{ignored-comment}{note}{no}\n\\label{local-only}\n\\rinanchor{def-formal-deformation}{definition}{形式形变}\n\\rinanchorref{120}{def-scheme}{概形的定义}\n\\rinmissing{形变函子}"},
		{Path: "rinspace.sty", Body: "\\newcommand{\\rinanchor}[3]{}"},
	}
	first, err := Extract(files, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Extract([]projectcore.File{files[2], files[0], files[1]}, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(first)
	right, _ := json.Marshal(second)
	if string(left) != string(right) {
		t.Fatalf("output changed with archive order:\n%s\n%s", left, right)
	}
	if len(first.Anchors) != 2 || first.Anchors[0].ID != "def-formal-deformation" || first.Anchors[1].ID != "same-name-two" {
		t.Fatalf("unexpected anchors: %#v", first.Anchors)
	}
	if len(first.References) != 3 || first.References[0].SourceAnchorID != "def-formal-deformation" || first.References[0].Target.AnchorID != "def-scheme" || first.References[1].SourceAnchorID != "same-name-two" || first.References[1].Target.ID == first.References[2].Target.ID {
		t.Fatalf("unexpected references: %#v", first.References)
	}
	if len(first.Unresolved) != 1 || first.Unresolved[0].Label != "形变函子" || first.Unresolved[0].SourceLocator.Line != 5 {
		t.Fatalf("unexpected unresolved: %#v", first.Unresolved)
	}
}

func TestMovedAnchorKeepsPermanentIdentityAndContentHash(t *testing.T) {
	one, err := Extract([]projectcore.File{{Path: "old/heading.tex", Body: "\\section{Old}\n\\rinanchor{stable-anchor}{theorem}{不变量}"}}, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Extract([]projectcore.File{{Path: "new/location.tex", Body: "\\section{New}\n\n\\rinanchor{stable-anchor}{theorem}{不变量}"}}, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if one.Anchors[0].ID != two.Anchors[0].ID || one.Anchors[0].ContentHash != two.Anchors[0].ContentHash {
		t.Fatal("move changed permanent identity or semantic content hash")
	}
	if one.Anchors[0].SourceLocator == two.Anchors[0].SourceLocator {
		t.Fatal("move did not update source locator")
	}
}

func TestExtractRejectsDuplicateMalformedAndOversizedInput(t *testing.T) {
	tests := []struct{ name, body, code string }{
		{"duplicate", "\\rinanchor{same-anchor}{note}{one}\n\\rinanchor{same-anchor}{note}{two}", "knowledge_index.duplicate_anchor"},
		{"bad id", "\\rinanchor{Bad ID}{note}{one}", "knowledge_index.invalid_anchor"},
		{"missing brace", "\\rintagref{12}{broken", "knowledge_index.malformed_command"},
		{"bad target", "\\rintagref{not-id}{label}", "knowledge_index.invalid_reference"},
		{"long label", "\\rinmissing{" + strings.Repeat("界", 241) + "}", "knowledge_index.invalid_unresolved"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Extract([]projectcore.File{{Path: "main.tex", Body: test.body}}, testIdentity)
			extraction, ok := err.(*Error)
			if !ok || extraction.Code != test.code {
				t.Fatalf("error = %#v, want %s", err, test.code)
			}
		})
	}
	_, err := Extract([]projectcore.File{{Path: "main.tex", Body: strings.Repeat("x", maxSourceBytes+1)}}, testIdentity)
	if extraction, ok := err.(*Error); !ok || extraction.Code != "knowledge_index.source_too_large" {
		t.Fatalf("oversized error = %#v", err)
	}
}
