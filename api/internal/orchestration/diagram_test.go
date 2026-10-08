package orchestration

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestDiagramBatchAndResultContractsPreserveSourceAndArtifactMetadata(t *testing.T) {
	batch := validDiagramBatch()
	if err := batch.Validate(); err != nil {
		t.Fatal(err)
	}
	item := batch.Items[0]
	hash := strings.Repeat("a", 64)
	objectID := "diagrams/v1/svg-sha256/aa/" + hash + ".svg"
	result := DiagramBatchResult{
		ContractVersion: DiagramResultContractVersion,
		Units: []DiagramResolvedUnit{{
			ID: item.Unit.ID, State: "succeeded", DiagramID: "svg-sha256-aaaaaaaaaaaa",
			Engine: "rin-texsvg", EngineVersion: "texsvg-v1", URL: "https://storage.example/" + objectID,
			SVGHash: hash, ObjectID: objectID, SVGBytes: 128, Uploaded: true,
			Artifact: &contracts.ArtifactReference{ArtifactID: objectID, SHA256: hash, Bytes: 128, MediaType: "image/svg+xml; charset=utf-8", Visibility: "public"},
			Source: DiagramSourceMetadata{
				DiagramType: item.Unit.DiagramType, Source: item.Unit.Source, Options: item.Unit.Options,
				Layout: item.Unit.Layout, SourceMode: item.SourceMode, SourceLocation: item.Unit.SourceLocation,
			},
			Cache:       DiagramCacheResult{Status: "miss", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("b", 64)},
			Diagnostics: []contracts.Diagnostic{},
		}},
		Versions: validDiagramVersions(),
	}
	if err := result.Validate(batch); err != nil {
		t.Fatal(err)
	}
	if result.Units[0].Source.Layout.Alignment != "center" || result.Units[0].Source.SourceLocation.Path != "chapter.tex" {
		t.Fatalf("diagram metadata was not preserved: %#v", result.Units[0].Source)
	}
}

func TestDiagramBatchRejectsInvalidModesDuplicatesAndWrapperIDs(t *testing.T) {
	for name, mutate := range map[string]func(*DiagramBatch){
		"contract":    func(batch *DiagramBatch) { batch.ContractVersion = "rin-diagram-batch/v0" },
		"strategy":    func(batch *DiagramBatch) { batch.OutputStrategy = "html" },
		"kind":        func(batch *DiagramBatch) { batch.Items[0].Unit.Kind = contracts.WorkUnitCode },
		"mode":        func(batch *DiagramBatch) { batch.Items[0].SourceMode = "raw" },
		"empty-body":  func(batch *DiagramBatch) { batch.Items[0].Body = "" },
		"body-source": func(batch *DiagramBatch) { batch.Items[0].Body = "different" },
		"duplicate":   func(batch *DiagramBatch) { batch.Items = append(batch.Items, batch.Items[0]) },
		"wrapper":     func(batch *DiagramBatch) { batch.Items[0].WrapperID = `bad" id` },
	} {
		t.Run(name, func(t *testing.T) {
			batch := validDiagramBatch()
			mutate(&batch)
			if err := batch.Validate(); err == nil {
				t.Fatalf("invalid diagram batch was accepted: %#v", batch)
			}
		})
	}
}

func TestDiagramResultRejectsSourceMutationAndIncompleteArtifacts(t *testing.T) {
	batch := validDiagramBatch()
	base := func() DiagramBatchResult {
		hash := strings.Repeat("a", 64)
		item := batch.Items[0]
		objectID := "diagrams/v1/svg-sha256/aa/" + hash + ".svg"
		return DiagramBatchResult{
			ContractVersion: DiagramResultContractVersion,
			Units: []DiagramResolvedUnit{{
				ID: item.Unit.ID, State: "succeeded", DiagramID: "diagram", Engine: "rin-texsvg", EngineVersion: "texsvg-v1",
				URL: "https://storage.example/object", SVGHash: hash, ObjectID: objectID, SVGBytes: 10,
				Artifact: &contracts.ArtifactReference{ArtifactID: objectID, SHA256: hash, Bytes: 10, MediaType: "image/svg+xml; charset=utf-8", Visibility: "public"},
				Source:   DiagramSourceMetadata{DiagramType: item.Unit.DiagramType, Source: item.Unit.Source, Options: item.Unit.Options, Layout: item.Unit.Layout, SourceMode: item.SourceMode, SourceLocation: item.Unit.SourceLocation},
				Cache:    DiagramCacheResult{Status: "hit", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("c", 64)}, Diagnostics: []contracts.Diagnostic{},
			}},
			Versions: validDiagramVersions(),
		}
	}
	if err := base().Validate(batch); err != nil {
		t.Fatalf("valid diagram result: %v", err)
	}
	mutated := base()
	mutated.Units[0].Source.Options = "scale=2"
	if err := mutated.Validate(batch); err == nil {
		t.Fatal("mutated source metadata was accepted")
	}
	incomplete := base()
	incomplete.Units[0].Artifact = nil
	if err := incomplete.Validate(batch); err == nil {
		t.Fatal("missing immutable artifact was accepted")
	}
}

func TestDiagramCacheResultRequiresVersionedStatus(t *testing.T) {
	for _, cache := range []DiagramCacheResult{
		{Status: "hit", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("a", 64)},
		{Status: "miss", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("b", 64)},
		{Status: "bypass"},
	} {
		if err := cache.Validate(); err != nil {
			t.Fatalf("valid cache result %#v: %v", cache, err)
		}
	}
	for _, cache := range []DiagramCacheResult{
		{Status: "hit"}, {Status: "miss", KeyVersion: "v0", Digest: strings.Repeat("a", 64)},
		{Status: "bypass", Digest: strings.Repeat("a", 64)}, {Status: "unknown"},
	} {
		if err := cache.Validate(); err == nil {
			t.Fatalf("invalid cache result was accepted: %#v", cache)
		}
	}
}

func validDiagramBatch() DiagramBatch {
	return DiagramBatch{
		ContractVersion: DiagramBatchContractVersion,
		Items: []DiagramBatchItem{{
			Unit: contracts.WorkUnit{
				Kind: contracts.WorkUnitDiagram, ID: "rw_11111111111111111111111111111111",
				DiagramType: "tikzpicture", Source: `\draw (0,0)--(1,1);`, Options: "scale=1",
				Layout:         &contracts.DiagramLayout{Alignment: "center"},
				SourceLocation: &contracts.SourceLocation{Path: "chapter.tex", Start: contracts.SourcePosition{Line: 3, Column: 1}},
			},
			SourceMode: DiagramSourceBody, Body: `\draw (0,0)--(1,1);`, WrapperID: "diagram-000001",
		}},
		OutputStrategy: "svg",
	}
}

func validDiagramVersions() map[string]string {
	return map[string]string{
		"engine": "rin-texsvg", "engine-version": "texsvg-v1", "plugin": "diagram-input/v1",
		"font": "texlive+dvisvgm", "sanitizer": "svg/v1", "compatibility": "reader/v1",
		"batch-contract": DiagramBatchContractVersion, "result-contract": DiagramResultContractVersion,
		"artifact-contract": DiagramArtifactContract, "cache-schema": DiagramCacheSchemaVersion,
	}
}
