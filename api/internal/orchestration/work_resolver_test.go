package orchestration

import (
	"context"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

type resolverServices struct {
	mathBatch    MathBatch
	diagramBatch DiagramBatch
	codeBatch    CodeBatch
}

func (services *resolverServices) ResolveMath(_ context.Context, batch MathBatch) (MathBatchResult, error) {
	services.mathBatch = batch
	units := make([]MathResolvedUnit, 0, len(batch.Items))
	for _, item := range batch.Items {
		unit := item.Unit
		units = append(units, MathResolvedUnit{
			ID: unit.ID, State: "succeeded", HTML: `<span class="rin-math"><mjx-container></mjx-container></span>`,
			Engine: "mathjax-chtml", EngineVersion: "4.1.3",
			Source:      MathSourceMetadata{TeX: unit.Source, Display: *unit.Display, MacroContextHash: unit.MacroContextHash, SourceLocation: unit.SourceLocation},
			Cache:       MathCacheResult{Status: "miss", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("1", 64)},
			Diagnostics: []contracts.Diagnostic{},
		})
	}
	return MathBatchResult{
		ContractVersion: MathResultContractVersion,
		Units:           units,
		CSS:             []string{"mjx-container{display:inline-block}"},
		Versions: map[string]string{
			"primary-engine": "mathjax-chtml", "primary-engine-version": "4.1.3", "font": "mathjax-newcm/v1",
			"plugin": "rin-math/v1", "sanitizer": "rin-math/v1", "compatibility": "reader/v1",
			"batch-contract": MathBatchContractVersion, "result-contract": MathResultContractVersion,
			"wrapper-contract": MathWrapperContractVersion, "cache-schema": MathCacheSchemaVersion,
		},
	}, nil
}

func TestWorkResolverCarriesSharedMathCSSOnce(t *testing.T) {
	services := &resolverServices{}
	draft, macroHash := resolverDraftBundle(t)
	display := true
	second := contracts.WorkUnit{
		Kind: contracts.WorkUnitMath, ID: "rw_00000000000000000000000000000004",
		Source: `x^2`, Display: &display, MacroContextHash: macroHash,
	}
	draft.WorkUnits = append(draft.WorkUnits, second)
	placeholder, err := contracts.WorkPlaceholder(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft.Pages[0].Fragment += placeholder
	resolved, err := (WorkResolver{Math: services, Diagrams: services, Code: services}).Resolve(
		context.Background(), draft, WorkResolutionOptions{
			MacroContexts: map[string]map[string]string{macroHash: {}},
			MathStrategy:  MathOutputCHTML, CodeTheme: "github-light",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	cssCarriers := 0
	for _, unit := range resolved.Units {
		if unit.Kind == contracts.WorkUnitMath && len(unit.CSS) > 0 {
			cssCarriers++
		}
	}
	if cssCarriers != 1 {
		t.Fatalf("shared math CSS was carried by %d units, want exactly one", cssCarriers)
	}
}

func (services *resolverServices) ResolveDiagrams(_ context.Context, batch DiagramBatch) (DiagramBatchResult, error) {
	services.diagramBatch = batch
	unit := batch.Items[0].Unit
	hash := strings.Repeat("2", 64)
	objectID := "diagrams/v1/svg-sha256/22/" + hash + ".svg"
	artifact := &contracts.ArtifactReference{
		ArtifactID: objectID, SHA256: hash, Bytes: 42,
		MediaType: "image/svg+xml; charset=utf-8", Visibility: "public",
	}
	return DiagramBatchResult{
		ContractVersion: DiagramResultContractVersion,
		Units: []DiagramResolvedUnit{{
			ID: unit.ID, State: "succeeded", DiagramID: "diagram-1", Engine: "rin-texsvg", EngineVersion: "texlive-2025",
			URL: "https://assets.example/" + objectID, SVGHash: hash, ObjectID: objectID, SVGBytes: 42, Artifact: artifact,
			Source:      DiagramSourceMetadata{DiagramType: unit.DiagramType, Source: unit.Source, Options: unit.Options, Layout: unit.Layout, SourceMode: DiagramSourceBody, SourceLocation: unit.SourceLocation},
			Cache:       DiagramCacheResult{Status: "miss", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("3", 64)},
			Diagnostics: []contracts.Diagnostic{},
		}},
		Versions: map[string]string{
			"engine": "rin-texsvg", "engine-version": "texlive-2025", "plugin": "rin-diagram/v1", "font": "newcm/v1",
			"sanitizer": "rin-svg/v1", "compatibility": "reader/v1", "batch-contract": DiagramBatchContractVersion,
			"result-contract": DiagramResultContractVersion, "artifact-contract": DiagramArtifactContract, "cache-schema": DiagramCacheSchemaVersion,
		},
	}, nil
}

func (services *resolverServices) ResolveCode(_ context.Context, batch CodeBatch) (CodeBatchResult, error) {
	services.codeBatch = batch
	unit := batch.Items[0]
	return CodeBatchResult{
		ContractVersion: CodeResultContractVersion,
		Units: []CodeResolvedUnit{{
			ID: unit.ID, State: "succeeded", HTML: `<pre class="shiki"><code>code</code></pre>`,
			Engine: "shiki", EngineVersion: "4.4.2", Theme: batch.Theme,
			CanonicalLanguage: "javascript", Highlighted: true,
			Source:      CodeSourceMetadata{Source: unit.Source, Language: unit.Language, Meta: unit.Meta, SourceLocation: unit.SourceLocation},
			Cache:       CodeCacheResult{Status: "miss", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("4", 64)},
			Diagnostics: []contracts.Diagnostic{},
		}},
		CSS: []string{}, Versions: validCodeVersions(),
	}, nil
}

func TestWorkResolverRoutesDraftUnitsThroughExactSharedServices(t *testing.T) {
	services := &resolverServices{}
	draft, macroHash := resolverDraftBundle(t)
	resolved, err := (WorkResolver{Math: services, Diagrams: services, Code: services}).Resolve(
		context.Background(), draft, WorkResolutionOptions{
			MacroContexts: map[string]map[string]string{macroHash: {}},
			MathStrategy:  MathOutputCHTML, CodeTheme: "github-light",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Units) != 3 || len(services.mathBatch.Items) != 1 || len(services.diagramBatch.Items) != 1 || len(services.codeBatch.Items) != 1 {
		t.Fatalf("work was not partitioned exactly: %#v", resolved)
	}
	if services.mathBatch.Items[0].Unit.Source != `\not\exists k\in\mathbb{R}` || services.mathBatch.Items[0].WrapperID != "" {
		t.Fatal("math source was rewritten or a second wrapper identity was invented")
	}
	if services.diagramBatch.Items[0].SourceMode != DiagramSourceBody || services.diagramBatch.Items[0].Body != services.diagramBatch.Items[0].Unit.Source {
		t.Fatal("diagram was not routed through exact body-mode service input")
	}
	if services.codeBatch.Theme != "github-light" || services.codeBatch.Items[0].Meta != "title=sample.js" {
		t.Fatal("code theme or fence metadata was lost")
	}
	if !strings.Contains(resolved.Units["rw_00000000000000000000000000000001"].HTML, "mjx-container") ||
		!strings.Contains(resolved.Units["rw_00000000000000000000000000000002"].HTML, "data-rin-diagram-object-id") ||
		!strings.Contains(resolved.Units["rw_00000000000000000000000000000003"].HTML, "shiki") {
		t.Fatal("resolved output did not remain typed")
	}
	if draft.State != contracts.DocumentBundleStateDraft || !strings.Contains(draft.Pages[0].Fragment, "<rin-work") {
		t.Fatal("resolution mutated or finalized the draft")
	}
}

func TestWorkResolverRejectsMissingServicesAndFinalBundles(t *testing.T) {
	draft, macroHash := resolverDraftBundle(t)
	_, err := (WorkResolver{}).Resolve(context.Background(), draft, WorkResolutionOptions{
		MacroContexts: map[string]map[string]string{macroHash: {}}, CodeTheme: "github-light",
	})
	if err == nil || !strings.Contains(err.Error(), "math service") {
		t.Fatalf("missing service error = %v", err)
	}
	draft.State = contracts.DocumentBundleStateFinal
	draft.Pages[0].FragmentFormat = contracts.FragmentFormatHTML
	draft.Pages[0].Fragment = "<p>final</p>"
	draft.WorkUnits = nil
	_, err = (WorkResolver{}).Resolve(context.Background(), draft, WorkResolutionOptions{CodeTheme: "github-light"})
	if err == nil || !strings.Contains(err.Error(), "requires a draft") {
		t.Fatalf("final bundle error = %v", err)
	}
}

func resolverDraftBundle(t *testing.T) (contracts.DocumentBundle, string) {
	t.Helper()
	macroHash := ComputeMathMacroContextHash(map[string]string{})
	display := false
	units := []contracts.WorkUnit{
		{Kind: contracts.WorkUnitMath, ID: "rw_00000000000000000000000000000001", Source: `\not\exists k\in\mathbb{R}`, Display: &display, MacroContextHash: macroHash},
		{Kind: contracts.WorkUnitDiagram, ID: "rw_00000000000000000000000000000002", Source: `\draw (0,0)--(1,1);`, DiagramType: "tikzpicture", Layout: &contracts.DiagramLayout{Alignment: "center"}},
		{Kind: contracts.WorkUnitCode, ID: "rw_00000000000000000000000000000003", Source: "const x = 1;", Language: "js", Meta: "title=sample.js"},
	}
	fragment := ""
	for _, unit := range units {
		placeholder, err := contracts.WorkPlaceholder(unit.ID)
		if err != nil {
			t.Fatal(err)
		}
		fragment += placeholder
	}
	bundle := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersion, ProjectHash: strings.Repeat("a", 64),
		State: contracts.DocumentBundleStateDraft, ContentKind: contracts.ContentKindMarkdown,
		DocumentEngine: "unified-markdown", Title: "Test",
		Pages: []contracts.DocumentPage{{
			ID: "page-main", SourcePath: "article.md", Fragment: fragment,
			FragmentFormat: contracts.FragmentFormatPlaceholders, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{},
		}},
		WorkUnits: units, Assets: []contracts.AssetReference{}, Diagnostics: []contracts.Diagnostic{},
		Provenance: contracts.Provenance{
			Adapter: "unified-markdown", AdapterVersion: "0.3.0", EngineVersion: "pipeline/v1",
			ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion,
		},
	}
	if err := bundle.Validate(); err != nil {
		t.Fatal(err)
	}
	return bundle, macroHash
}
