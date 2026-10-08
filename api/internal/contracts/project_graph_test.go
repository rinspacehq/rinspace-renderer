package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestComputeProjectHashIgnoresFileOrderAndMetadata(t *testing.T) {
	files := []ProjectGraphFile{
		{Path: "chapters/one.md", SHA256: strings.Repeat("1", 64), Bytes: 12, MediaType: "text/markdown", Role: ProjectFileRoleSource},
		{Path: "images/plot.svg", SHA256: strings.Repeat("2", 64), Bytes: 99, MediaType: "image/svg+xml", Role: ProjectFileRoleAsset},
	}
	want, err := ComputeProjectHash(ContentKindMarkdown, files)
	if err != nil {
		t.Fatalf("compute project hash: %v", err)
	}
	changedOrderAndMetadata := []ProjectGraphFile{
		{Path: "images/plot.svg", SHA256: strings.Repeat("2", 64), Bytes: 500, MediaType: "application/octet-stream", Role: ProjectFileRoleGenerated},
		{Path: "chapters/one.md", SHA256: strings.Repeat("1", 64), Bytes: 1, Role: ProjectFileRoleAsset},
	}
	got, err := ComputeProjectHash(ContentKindMarkdown, changedOrderAndMetadata)
	if err != nil {
		t.Fatalf("compute reordered project hash: %v", err)
	}
	if got != want {
		t.Fatalf("project hash changed with order/metadata: got %s want %s", got, want)
	}
}

func TestComputeProjectHashChangesWithPathContentOrKind(t *testing.T) {
	base := []ProjectGraphFile{{Path: "main.md", SHA256: strings.Repeat("a", 64), Bytes: 1, Role: ProjectFileRoleSource}}
	want, err := ComputeProjectHash(ContentKindMarkdown, base)
	if err != nil {
		t.Fatal(err)
	}
	for name, testCase := range map[string]struct {
		kind  ContentKind
		files []ProjectGraphFile
	}{
		"path":    {ContentKindMarkdown, []ProjectGraphFile{{Path: "other.md", SHA256: strings.Repeat("a", 64), Role: ProjectFileRoleSource}}},
		"content": {ContentKindMarkdown, []ProjectGraphFile{{Path: "main.md", SHA256: strings.Repeat("b", 64), Role: ProjectFileRoleSource}}},
		"kind":    {ContentKindLaTeX, base},
	} {
		t.Run(name, func(t *testing.T) {
			got, hashErr := ComputeProjectHash(testCase.kind, testCase.files)
			if hashErr != nil {
				t.Fatal(hashErr)
			}
			if got == want {
				t.Fatalf("project hash did not change for %s", name)
			}
		})
	}
}

func TestProjectGraphFixturesValidate(t *testing.T) {
	_, sourceFile, _, _ := runtime.Caller(0)
	fixturePattern := filepath.Join(filepath.Dir(sourceFile), "../../../packages/document-contract/fixtures/project-graph/*.json")
	fixtures, err := filepath.Glob(fixturePattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 4 {
		t.Fatalf("expected four project graph fixtures, got %d", len(fixtures))
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			body, readErr := os.ReadFile(fixture)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var graph ProjectGraph
			if unmarshalErr := json.Unmarshal(body, &graph); unmarshalErr != nil {
				t.Fatalf("unmarshal fixture: %v", unmarshalErr)
			}
			if validateErr := graph.Validate(); validateErr != nil {
				t.Fatalf("validate fixture: %v", validateErr)
			}
		})
	}
}

func TestProjectGraphRejectsUnsafeOrInconsistentInput(t *testing.T) {
	files := []ProjectGraphFile{{Path: "main.md", SHA256: strings.Repeat("a", 64), Bytes: 1, Role: ProjectFileRoleSource}}
	hash, err := ComputeProjectHash(ContentKindMarkdown, files)
	if err != nil {
		t.Fatal(err)
	}
	valid := ProjectGraph{
		SchemaVersion: ProjectGraphSchemaVersion,
		ProjectHash:   hash,
		ContentKind:   ContentKindMarkdown,
		Entrypoints:   []ProjectEntrypoint{{Path: "main.md", Role: EntrypointRoleDocument}},
		Files:         files,
		References:    []ProjectReference{},
		Options:       map[string]any{},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid graph: %v", err)
	}

	unsafe := valid
	unsafe.Files = []ProjectGraphFile{{Path: "../main.md", SHA256: strings.Repeat("a", 64), Role: ProjectFileRoleSource}}
	if err := unsafe.Validate(); err == nil {
		t.Fatal("expected unsafe path to fail")
	}

	wrongHash := valid
	wrongHash.ProjectHash = strings.Repeat("0", 64)
	if err := wrongHash.Validate(); err == nil {
		t.Fatal("expected mismatched project hash to fail")
	}

	missingEntrypoint := valid
	missingEntrypoint.Entrypoints = []ProjectEntrypoint{{Path: "missing.md", Role: EntrypointRoleDocument}}
	if err := missingEntrypoint.Validate(); err == nil {
		t.Fatal("expected missing entrypoint to fail")
	}
}
