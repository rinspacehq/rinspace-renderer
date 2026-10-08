package renderapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestProjectRenderResponseFieldsAreExhaustivelyClassified(t *testing.T) {
	typeOfResponse := reflect.TypeOf(ProjectRenderResponse{})
	seen := map[string]struct{}{}
	for index := 0; index < typeOfResponse.NumField(); index++ {
		jsonName := strings.Split(typeOfResponse.Field(index).Tag.Get("json"), ",")[0]
		class, exists := ProjectRenderResponseFieldClasses[jsonName]
		if !exists || class == "" {
			t.Fatalf("response field %q is not classified", jsonName)
		}
		seen[jsonName] = struct{}{}
	}
	if len(seen) != len(ProjectRenderResponseFieldClasses) {
		t.Fatalf("classification contains stale fields: response=%d classification=%d", len(seen), len(ProjectRenderResponseFieldClasses))
	}
}

func TestCanonicalCompatibilityMappingIsDeterministicAndPrivateByDefault(t *testing.T) {
	result := compatibilityTestResult()
	compatibility := LegacyCompatibilityPayload{
		Title: "Title", HTML: "<article>ok</article>", MainFile: "main.md",
		Diagrams: []DiagramRef{}, AssetFiles: []AssetFile{}, Reader: map[string]any{"pages": 1}, Math: MathSummary{},
	}
	debug := &PrivateRenderDebug{Source: "private source", Project: map[string]any{"private": true}}
	first, err := ProjectRenderResponseFromCanonical(result, compatibility, debug, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProjectRenderResponseFromCanonical(result, compatibility, debug, false)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("mapping is not deterministic:\n%s\n%s", firstJSON, secondJSON)
	}
	if strings.Contains(string(firstJSON), "private source") || first.Source != "" || first.Project != nil {
		t.Fatal("private debug leaked through default compatibility mapping")
	}
	if len(first.Assets) != 2 || first.Assets[0].Path != "public/a.svg" || first.Assets[1].Path != "public/z.svg" {
		t.Fatalf("canonical assets were not deterministically ordered: %#v", first.Assets)
	}

	withDebug, err := ProjectRenderResponseFromCanonical(result, compatibility, debug, true)
	if err != nil {
		t.Fatal(err)
	}
	if withDebug.Source != "private source" || withDebug.Project == nil {
		t.Fatal("authorized private debug was not mapped")
	}
}

func compatibilityTestResult() contracts.RenderResult {
	bundle := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersion, ProjectHash: strings.Repeat("1", 64), BundleHash: strings.Repeat("2", 64), State: contracts.DocumentBundleStateFinal,
		ContentKind: contracts.ContentKindMarkdown, DocumentEngine: "rin-markdown", Title: "Title",
		Pages:     []contracts.DocumentPage{{ID: "page-main", SourcePath: "main.md", Fragment: "<p>ok</p>", FragmentFormat: contracts.FragmentFormatHTML, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{}}},
		WorkUnits: []contracts.WorkUnit{}, Assets: []contracts.AssetReference{}, Diagnostics: []contracts.Diagnostic{},
		Provenance: contracts.Provenance{Adapter: "rin-markdown", AdapterVersion: "test", EngineVersion: "test", ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion},
	}
	return contracts.RenderResult{
		SchemaVersion: contracts.RenderResultSchemaVersion, JobID: "job-1", RequestID: "request-1",
		ProjectHash: bundle.ProjectHash, ResultHash: strings.Repeat("2", 64), ContentKind: contracts.ContentKindMarkdown,
		Engine: "rin-markdown", Inline: &bundle,
		Assets: []contracts.ArtifactReference{
			{ArtifactID: "public/z.svg", SHA256: strings.Repeat("3", 64), Bytes: 2, MediaType: "image/svg+xml", Visibility: "public"},
			{ArtifactID: "public/a.svg", SHA256: strings.Repeat("4", 64), Bytes: 1, MediaType: "image/svg+xml", Visibility: "public"},
		},
		Diagnostics: []contracts.Diagnostic{}, Versions: map[string]string{"rinRenderer": "test"}, Cache: contracts.RenderCacheSummary{ReusedStages: []string{}},
	}
}
