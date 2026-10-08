package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDocumentBundleFixturesValidateAndRoundTrip(t *testing.T) {
	_, sourceFile, _, _ := runtime.Caller(0)
	fixturePattern := filepath.Join(filepath.Dir(sourceFile), "../../../packages/document-contract/fixtures/document-bundle/*.json")
	fixtures, err := filepath.Glob(fixturePattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 4 {
		t.Fatalf("expected four document bundle fixtures, got %d", len(fixtures))
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			body, readErr := os.ReadFile(fixture)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var bundle DocumentBundle
			if unmarshalErr := json.Unmarshal(body, &bundle); unmarshalErr != nil {
				t.Fatalf("unmarshal fixture: %v", unmarshalErr)
			}
			if validateErr := bundle.Validate(); validateErr != nil {
				t.Fatalf("validate fixture: %v", validateErr)
			}
			roundTrip, marshalErr := json.Marshal(bundle)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var decoded DocumentBundle
			if unmarshalErr := json.Unmarshal(roundTrip, &decoded); unmarshalErr != nil {
				t.Fatal(unmarshalErr)
			}
			if !reflect.DeepEqual(bundle, decoded) {
				t.Fatal("document bundle changed during JSON round trip")
			}
		})
	}
}

func TestOpaqueWorkPlaceholderGeneration(t *testing.T) {
	first, err := NewWorkID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewWorkID()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !workIDPattern.MatchString(first) || !workIDPattern.MatchString(second) {
		t.Fatalf("unexpected generated work ids: %q %q", first, second)
	}
	placeholder, err := WorkPlaceholder(first)
	if err != nil {
		t.Fatal(err)
	}
	if placeholder != `<rin-work data-id="`+first+`"></rin-work>` {
		t.Fatalf("unexpected placeholder %q", placeholder)
	}
	if _, err := WorkPlaceholder("author-chosen"); err == nil {
		t.Fatal("expected author-chosen work id to fail")
	}
	if err := ValidateSourceHasNoWorkElements("before <rin-work data-id=\"author\"></rin-work> after"); err == nil {
		t.Fatal("expected reserved source element to fail")
	}
	if err := ValidateSourceHasNoWorkElements("ordinary <em>source</em>"); err != nil {
		t.Fatal(err)
	}
}

func TestPageIDContractPreservesSafeUnicodeAndRejectsUnsafeValues(t *testing.T) {
	for _, value := range []string{"md-003-啊啊啊", "章节-１２３", "épisode-1", "e\u0301pisode-1"} {
		if !ValidPageID(value) {
			t.Fatalf("expected safe Unicode page ID %q", value)
		}
	}
	for _, value := range []string{"", "-page", "md/page", "md page", "md\u200dpage", "md<script>", strings.Repeat("页", 129)} {
		if ValidPageID(value) {
			t.Fatalf("expected unsafe page ID %q to be rejected", value)
		}
	}
}

func TestDocumentBundleRejectsUnknownMalformedDuplicateAndUnusedPlaceholders(t *testing.T) {
	bundle := minimalDocumentBundle()
	if err := bundle.Validate(); err != nil {
		t.Fatalf("minimal bundle: %v", err)
	}

	unknown := bundle
	unknown.Pages = clonePages(bundle.Pages)
	unknown.Pages[0].Fragment = `<rin-work data-id="rw_22222222222222222222222222222222"></rin-work>`
	if err := unknown.Validate(); err == nil || !strings.Contains(err.Error(), "unknown work id") {
		t.Fatalf("expected unknown placeholder error, got %v", err)
	}

	malformed := bundle
	malformed.Pages = clonePages(bundle.Pages)
	malformed.Pages[0].Fragment = `<rin-work data-id="rw_11111111111111111111111111111111" />`
	if err := malformed.Validate(); err == nil || !strings.Contains(err.Error(), "malformed or source-created") {
		t.Fatalf("expected malformed placeholder error, got %v", err)
	}

	duplicate := bundle
	duplicate.Pages = clonePages(bundle.Pages)
	duplicate.Pages[0].Fragment += duplicate.Pages[0].Fragment
	if err := duplicate.Validate(); err == nil || !strings.Contains(err.Error(), "exactly one placeholder") {
		t.Fatalf("expected duplicate placeholder error, got %v", err)
	}

	unused := bundle
	unused.Pages = clonePages(bundle.Pages)
	unused.Pages[0].Fragment = "<p>no placeholder</p>"
	if err := unused.Validate(); err == nil || !strings.Contains(err.Error(), "exactly one placeholder") {
		t.Fatalf("expected unused work error, got %v", err)
	}
}

func TestDocumentBundleRejectsCrossKindFieldsAndUnsafePaths(t *testing.T) {
	bundle := minimalDocumentBundle()
	bundle.WorkUnits[0].DiagramType = "tikzpicture"
	if err := bundle.Validate(); err == nil || !strings.Contains(err.Error(), "another work-unit kind") {
		t.Fatalf("expected cross-kind field error, got %v", err)
	}

	bundle = minimalDocumentBundle()
	bundle.Pages[0].SourcePath = "../main.md"
	if err := bundle.Validate(); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("expected unsafe page path error, got %v", err)
	}
}

func TestFinalDocumentBundleRequiresResolvedHTML(t *testing.T) {
	bundle := minimalDocumentBundle()
	bundle.State = DocumentBundleStateFinal
	bundle.Pages[0].FragmentFormat = FragmentFormatHTML
	bundle.Pages[0].Fragment = "<p>resolved math</p>"
	if err := bundle.Validate(); err != nil {
		t.Fatalf("valid final bundle: %v", err)
	}

	bundle.Pages[0].Fragment, _ = WorkPlaceholder(bundle.WorkUnits[0].ID)
	if err := bundle.Validate(); err == nil || !strings.Contains(err.Error(), "final page") {
		t.Fatalf("expected final placeholder rejection, got %v", err)
	}
}

func TestDocumentBundleV2RequiresCanonicalBlocksAndMatchingDOM(t *testing.T) {
	blockID, err := NewDocumentBlockID("main.md\x00intro\x00paragraph\x00Hello world\x000")
	if err != nil {
		t.Fatal(err)
	}
	bundle := DocumentBundle{
		SchemaVersion:  DocumentBundleSchemaVersionV2,
		ProjectHash:    strings.Repeat("1", 64),
		State:          DocumentBundleStateFinal,
		ContentKind:    ContentKindMarkdown,
		DocumentEngine: "rin-markdown",
		Title:          "test",
		Pages: []DocumentPage{{
			ID:               "page-main",
			SourcePath:       "main.md",
			Fragment:         `<p data-rin-block-id="` + blockID + `" data-rin-block-kind="paragraph">Hello world</p>`,
			FragmentFormat:   FragmentFormatHTML,
			TOC:              []TOCEntry{},
			DependencyHashes: []string{},
			Blocks: []DocumentBlock{{
				ID: blockID, Kind: DocumentBlockParagraph, Text: "Hello world",
				TextHash: DocumentBlockTextHash("Hello world"), HeadingPath: []string{},
			}},
		}},
		WorkUnits:   []WorkUnit{},
		Assets:      []AssetReference{},
		Diagnostics: []Diagnostic{},
		Provenance: Provenance{
			Adapter: "rin-markdown", AdapterVersion: "0.1.0", EngineVersion: "test",
			ProjectGraphSchemaVersion: ProjectGraphSchemaVersion,
		},
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("valid v2 bundle: %v", err)
	}

	badHash := bundle
	badHash.Pages = clonePages(bundle.Pages)
	badHash.Pages[0].Blocks = append([]DocumentBlock(nil), bundle.Pages[0].Blocks...)
	badHash.Pages[0].Blocks[0].TextHash = strings.Repeat("f", 64)
	if err := badHash.Validate(); err == nil || !strings.Contains(err.Error(), "text hash") {
		t.Fatalf("expected block text hash rejection, got %v", err)
	}

	missingDOM := bundle
	missingDOM.Pages = clonePages(bundle.Pages)
	missingDOM.Pages[0].Fragment = "<p>Hello world</p>"
	if err := missingDOM.Validate(); err == nil || !strings.Contains(err.Error(), "fragment has 0 block ids") {
		t.Fatalf("expected missing DOM block rejection, got %v", err)
	}

	duplicateDOM := bundle
	duplicateDOM.Pages = clonePages(bundle.Pages)
	duplicateDOM.Pages[0].Fragment += duplicateDOM.Pages[0].Fragment
	if err := duplicateDOM.Validate(); err == nil || !strings.Contains(err.Error(), "fragment has 2 block ids") {
		t.Fatalf("expected duplicate DOM block rejection, got %v", err)
	}

	malformedDOM := bundle
	malformedDOM.Pages = clonePages(bundle.Pages)
	malformedDOM.Pages[0].Fragment = strings.Replace(bundle.Pages[0].Fragment, blockID, "not-a-block-id", 1)
	if err := malformedDOM.Validate(); err == nil || !strings.Contains(err.Error(), "malformed block id attribute") {
		t.Fatalf("expected malformed DOM block rejection, got %v", err)
	}

}

func TestDocumentBundleV2AcceptsGreaterThanInsideAttributeBeforeBlockID(t *testing.T) {
	blockID, err := NewDocumentBlockID("main.md\x00math\x00P(X>N)\x000")
	if err != nil {
		t.Fatal(err)
	}
	bundle := DocumentBundle{
		SchemaVersion:  DocumentBundleSchemaVersionV2,
		ProjectHash:    strings.Repeat("1", 64),
		State:          DocumentBundleStateFinal,
		ContentKind:    ContentKindMarkdown,
		DocumentEngine: "rin-markdown",
		Title:          "test",
		Pages: []DocumentPage{{
			ID:         "page-main",
			SourcePath: "main.md",
			Fragment: `<span class="rin-math-inline" data-rin-math-source="P(X>N)" data-rin-block-id="` + blockID +
				`" data-rin-block-kind="math"><mjx-container></mjx-container></span>`,
			FragmentFormat:   FragmentFormatHTML,
			TOC:              []TOCEntry{},
			DependencyHashes: []string{},
			Blocks: []DocumentBlock{{
				ID: blockID, Kind: DocumentBlockMath, Text: "P(X>N)",
				TextHash: DocumentBlockTextHash("P(X>N)"), HeadingPath: []string{},
			}},
		}},
		WorkUnits:   []WorkUnit{},
		Assets:      []AssetReference{},
		Diagnostics: []Diagnostic{},
		Provenance: Provenance{
			Adapter: "rin-markdown", AdapterVersion: "0.1.0", EngineVersion: "test",
			ProjectGraphSchemaVersion: ProjectGraphSchemaVersion,
		},
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("valid v2 bundle with greater-than sign in preceding attribute: %v", err)
	}
}

func TestDocumentBundleV2AcceptsOnlyCanonicalBlockAnnotatedWorkPlaceholders(t *testing.T) {
	bundle := minimalDocumentBundle()
	bundle.SchemaVersion = DocumentBundleSchemaVersionV2
	blockID, err := NewDocumentBlockID("main.md\x00math\x00x\x000")
	if err != nil {
		t.Fatal(err)
	}
	bundle.Pages[0].Blocks = []DocumentBlock{{
		ID: blockID, Kind: DocumentBlockMath, Text: "x", TextHash: DocumentBlockTextHash("x"), HeadingPath: []string{},
	}}
	bundle.Pages[0].Fragment = `<rin-work data-id="` + bundle.WorkUnits[0].ID + `" data-rin-block-id="` + blockID + `" data-rin-block-kind="math"></rin-work>`
	if err := bundle.Validate(); err != nil {
		t.Fatalf("valid v2 block-annotated work placeholder: %v", err)
	}

	v1 := minimalDocumentBundle()
	v1.Pages[0].Fragment = bundle.Pages[0].Fragment
	if err := v1.Validate(); err == nil || !strings.Contains(err.Error(), "malformed or source-created") {
		t.Fatalf("expected v1 annotated placeholder rejection, got %v", err)
	}

	mismatched := bundle
	mismatched.Pages = clonePages(bundle.Pages)
	mismatched.Pages[0].Blocks = append([]DocumentBlock(nil), bundle.Pages[0].Blocks...)
	mismatched.Pages[0].Fragment = strings.Replace(bundle.Pages[0].Fragment, `data-rin-block-kind="math"`, `data-rin-block-kind="code"`, 1)
	if err := mismatched.Validate(); err == nil || !strings.Contains(err.Error(), "unknown or mismatched block") {
		t.Fatalf("expected mismatched block placeholder rejection, got %v", err)
	}

	malformed := bundle
	malformed.Pages = clonePages(bundle.Pages)
	malformed.Pages[0].Blocks = append([]DocumentBlock(nil), bundle.Pages[0].Blocks...)
	malformed.Pages[0].Fragment = strings.Replace(bundle.Pages[0].Fragment, ` data-rin-block-kind="math"`, "", 1)
	if err := malformed.Validate(); err == nil || !strings.Contains(err.Error(), "malformed or source-created") {
		t.Fatalf("expected malformed v2 annotated placeholder rejection, got %v", err)
	}
}

func TestDocumentBundleV1RejectsBlocks(t *testing.T) {
	bundle := minimalDocumentBundle()
	bundle.Pages = clonePages(bundle.Pages)
	bundle.Pages[0].Blocks = []DocumentBlock{}
	if err := bundle.Validate(); err == nil || !strings.Contains(err.Error(), "v1 page cannot contain blocks") {
		t.Fatalf("expected v1 block rejection, got %v", err)
	}
}

func TestDocumentBlockIDIsDeterministicAndNamespaced(t *testing.T) {
	first, err := NewDocumentBlockID("main.md\x00intro\x00paragraph\x00Hello world\x000")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDocumentBlockID("main.md\x00intro\x00paragraph\x00Hello world\x000")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewDocumentBlockID("main.md\x00intro\x00paragraph\x00Hello world\x001")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == other || !blockIDPattern.MatchString(first) {
		t.Fatalf("unexpected deterministic ids: %q %q %q", first, second, other)
	}
	if _, err := NewDocumentBlockID("  "); err == nil {
		t.Fatal("expected empty block seed to fail")
	}
}

func minimalDocumentBundle() DocumentBundle {
	display := false
	id := "rw_11111111111111111111111111111111"
	placeholder, _ := WorkPlaceholder(id)
	return DocumentBundle{
		SchemaVersion:  DocumentBundleSchemaVersion,
		ProjectHash:    strings.Repeat("1", 64),
		State:          DocumentBundleStateDraft,
		ContentKind:    ContentKindMarkdown,
		DocumentEngine: "rin-markdown",
		Title:          "test",
		Pages: []DocumentPage{{
			ID:               "page-main",
			SourcePath:       "main.md",
			Fragment:         placeholder,
			FragmentFormat:   FragmentFormatPlaceholders,
			TOC:              []TOCEntry{},
			DependencyHashes: []string{},
		}},
		WorkUnits: []WorkUnit{{
			Kind:             WorkUnitMath,
			ID:               id,
			Source:           "x",
			Display:          &display,
			MacroContextHash: strings.Repeat("2", 64),
		}},
		Assets:      []AssetReference{},
		Diagnostics: []Diagnostic{},
		Provenance: Provenance{
			Adapter:                   "rin-markdown",
			AdapterVersion:            "0.1.0",
			EngineVersion:             "test",
			ProjectGraphSchemaVersion: ProjectGraphSchemaVersion,
		},
	}
}

func clonePages(pages []DocumentPage) []DocumentPage {
	cloned := make([]DocumentPage, len(pages))
	copy(cloned, pages)
	return cloned
}
