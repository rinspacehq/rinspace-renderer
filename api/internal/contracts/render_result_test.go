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

func TestRenderResultFixturesValidateAndRoundTrip(t *testing.T) {
	_, sourceFile, _, _ := runtime.Caller(0)
	fixturePattern := filepath.Join(filepath.Dir(sourceFile), "../../../packages/protocol/fixtures/render-result/*.json")
	fixtures, err := filepath.Glob(fixturePattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 2 {
		t.Fatalf("expected two render result fixtures, got %d", len(fixtures))
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			body, readErr := os.ReadFile(fixture)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var result RenderResult
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatal(err)
			}
			if err := result.Validate(); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var decoded RenderResult
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result, decoded) {
				t.Fatal("render result changed during JSON round trip")
			}
		})
	}
}

func TestRenderResultRequiresExactlyOneRepresentation(t *testing.T) {
	result := minimalRenderResult()
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	result.ResultArtifact = &ArtifactReference{
		ArtifactID: "private/result.json", SHA256: strings.Repeat("2", 64), MediaType: "application/json", Visibility: "private", ExpiresAt: "2026-08-16T07:00:00Z",
	}
	if err := result.Validate(); err == nil {
		t.Fatal("expected two result representations to fail")
	}
	result.Inline = nil
	if err := result.Validate(); err != nil {
		t.Fatalf("valid artifact result: %v", err)
	}
	result.ResultArtifact.Visibility = "public"
	if err := result.Validate(); err == nil {
		t.Fatal("expected public large result to fail")
	}
}

func TestRenderResultRequiresArrayValuedCollections(t *testing.T) {
	tests := map[string]func(*RenderResult){
		"assets":       func(result *RenderResult) { result.Assets = nil },
		"diagnostics":  func(result *RenderResult) { result.Diagnostics = nil },
		"reusedStages": func(result *RenderResult) { result.Cache.ReusedStages = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			result := minimalRenderResult()
			mutate(&result)
			if err := result.Validate(); err == nil {
				t.Fatal("expected nil collection to fail validation")
			}
		})
	}
}

func minimalRenderResult() RenderResult {
	bundle := minimalDocumentBundle()
	bundle.State = DocumentBundleStateFinal
	bundle.BundleHash = strings.Repeat("2", 64)
	bundle.Pages[0].Fragment = "<p>resolved</p>"
	bundle.Pages[0].FragmentFormat = FragmentFormatHTML
	return RenderResult{
		SchemaVersion: RenderResultSchemaVersion,
		JobID:         "job-1",
		RequestID:     "request-1",
		ProjectHash:   bundle.ProjectHash,
		ResultHash:    strings.Repeat("2", 64),
		ContentKind:   bundle.ContentKind,
		Engine:        "rin-markdown",
		Inline:        &bundle,
		Assets:        []ArtifactReference{},
		Diagnostics:   []Diagnostic{},
		Versions:      map[string]string{"rinRenderer": "test"},
		Cache:         RenderCacheSummary{ReusedStages: []string{}},
	}
}
