package jobresult

import (
	"encoding/json"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestStoredOutputRejectsInvalidOrMissingCompatibility(t *testing.T) {
	output := StoredOutput{SchemaVersion: StoredOutputSchemaVersion, Result: testResult()}
	if err := output.Validate(); err == nil {
		t.Fatal("Validate() accepted missing compatibility response")
	}
	output.Compatibility = json.RawMessage(`{"requestId":"request-a"}`)
	if err := output.Validate(); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(output)
	decoded, err := Decode(body)
	if err != nil || decoded.Result.JobID != output.Result.JobID {
		t.Fatalf("Decode() = %#v, %v", decoded, err)
	}
}

func testResult() contracts.RenderResult {
	bundle := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersion, ProjectHash: hash("1"), BundleHash: hash("2"),
		State: contracts.DocumentBundleStateFinal, ContentKind: contracts.ContentKindLaTeX,
		DocumentEngine: "latexml", Title: "Title",
		Pages:     []contracts.DocumentPage{{ID: "page-main", SourcePath: "main.tex", Fragment: "<p>ok</p>", FragmentFormat: contracts.FragmentFormatHTML, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{}}},
		WorkUnits: []contracts.WorkUnit{}, Assets: []contracts.AssetReference{}, Diagnostics: []contracts.Diagnostic{},
		Provenance: contracts.Provenance{Adapter: "latexml", AdapterVersion: "test", EngineVersion: "test", ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion},
	}
	return contracts.RenderResult{
		SchemaVersion: contracts.RenderResultSchemaVersion, JobID: "job-a", RequestID: "request-a",
		ProjectHash: hash("1"), ResultHash: hash("2"), ContentKind: contracts.ContentKindLaTeX,
		Engine: "latexml", Inline: &bundle, Assets: []contracts.ArtifactReference{},
		Diagnostics: []contracts.Diagnostic{}, Versions: map[string]string{"rinRenderer": "test"},
		Cache: contracts.RenderCacheSummary{ReusedStages: []string{}},
	}
}

func hash(value string) string {
	result := ""
	for len(result) < 64 {
		result += value
	}
	return result[:64]
}
