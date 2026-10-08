package markdownadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration/orchestrationtest"
)

type adapterWorkerFunc func(context.Context, string, any, any) error

func (worker adapterWorkerFunc) Do(ctx context.Context, operation string, payload any, result any) error {
	return worker(ctx, operation, payload, result)
}

type bookMemoryCache struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	records map[string]orchestration.CacheRecord
}

func newBookMemoryCache() *bookMemoryCache {
	return &bookMemoryCache{bodies: map[string][]byte{}, records: map[string]orchestration.CacheRecord{}}
}

func (cache *bookMemoryCache) PutPrivate(_ context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	digest := sha256.Sum256(write.Body)
	reference := contracts.ArtifactReference{ArtifactID: write.ArtifactID, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(write.Body)), MediaType: write.MediaType, Visibility: "private"}
	cache.mu.Lock()
	cache.bodies[write.ArtifactID] = append([]byte(nil), write.Body...)
	cache.mu.Unlock()
	return reference, nil
}

func (cache *bookMemoryCache) GetPrivate(_ context.Context, reference contracts.ArtifactReference) ([]byte, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	body, ok := cache.bodies[reference.ArtifactID]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), body...), nil
}

func (cache *bookMemoryCache) DeletePrivate(_ context.Context, reference contracts.ArtifactReference) error {
	cache.mu.Lock()
	delete(cache.bodies, reference.ArtifactID)
	cache.mu.Unlock()
	return nil
}

func (cache *bookMemoryCache) PutPublicIfMissing(context.Context, orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	return contracts.ArtifactReference{}, errors.New("unexpected public write")
}

func (cache *bookMemoryCache) Lookup(_ context.Context, key orchestration.CacheKey, expected orchestration.CacheExpectation, now time.Time) orchestration.CacheLookupResult {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	record, ok := cache.records[key.String()]
	if !ok || !record.ExpiresAt.After(now) || record.SchemaVersion != expected.SchemaVersion {
		return orchestration.CacheLookupResult{Reason: "not_found"}
	}
	return orchestration.CacheLookupResult{Record: record, Body: append([]byte(nil), cache.bodies[record.Artifact.ArtifactID]...), Hit: true, Reason: "hit"}
}

func (cache *bookMemoryCache) Store(_ context.Context, record orchestration.CacheRecord, _ orchestration.CacheExpectation, _ time.Time) orchestration.CacheStoreResult {
	cache.mu.Lock()
	cache.records[record.Key.String()] = record
	cache.mu.Unlock()
	return orchestration.CacheStoreResult{Stored: true, Reason: "stored"}
}

func TestFinalizeSendsExactResolvedWorkContract(t *testing.T) {
	workID := "rw_11111111111111111111111111111111"
	projectHash := strings.Repeat("a", 64)
	draft := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersion, ProjectHash: projectHash,
		State: contracts.DocumentBundleStateDraft, ContentKind: contracts.ContentKindMarkdown,
		DocumentEngine: Engine, Title: "Article",
		Pages: []contracts.DocumentPage{{
			ID: "page-article", SourcePath: "article.md", Fragment: `<article><rin-work data-id="` + workID + `"></rin-work></article>`,
			FragmentFormat: contracts.FragmentFormatPlaceholders, TOC: []contracts.TOCEntry{},
			DependencyHashes: []string{strings.Repeat("b", 64)},
		}},
		WorkUnits: []contracts.WorkUnit{{Kind: contracts.WorkUnitCode, ID: workID, Source: "x", Language: "text"}},
		Assets:    []contracts.AssetReference{}, Diagnostics: []contracts.Diagnostic{},
		Provenance: contracts.Provenance{Adapter: Engine, AdapterVersion: AdapterVersion, EngineVersion: "pipeline", ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion},
	}
	worker := adapterWorkerFunc(func(_ context.Context, operation string, payload any, result any) error {
		if operation != finalizeOperation {
			t.Fatalf("operation = %q", operation)
		}
		body, _ := json.Marshal(payload)
		var values map[string]any
		_ = json.Unmarshal(body, &values)
		resolved := values["resolvedWork"].(map[string]any)["units"].(map[string]any)[workID].(map[string]any)
		if resolved["id"] != workID || resolved["kind"] != "code" || resolved["html"] != "<pre><code>x</code></pre>" {
			t.Fatalf("resolved worker payload = %#v", resolved)
		}
		final := draft
		final.State = contracts.DocumentBundleStateFinal
		final.BundleHash = strings.Repeat("c", 64)
		final.Pages[0].Fragment = "<article><pre><code>x</code></pre></article>"
		final.Pages[0].FragmentFormat = contracts.FragmentFormatHTML
		final.WorkUnits = []contracts.WorkUnit{}
		encoded, _ := json.Marshal(workerBundleResponse{Publishable: true, Bundle: final})
		return json.Unmarshal(encoded, result)
	})
	adapter := Adapter{Worker: worker, MaxFileBytes: 1024}
	final, err := adapter.Finalize(context.Background(), draft, orchestration.ResolvedWork{Units: map[string]orchestration.ResolvedWorkUnit{
		workID: {ID: workID, Kind: contracts.WorkUnitCode, HTML: "<pre><code>x</code></pre>", CSS: []string{}, Diagnostics: []contracts.Diagnostic{}},
	}})
	if err != nil || final.State != contracts.DocumentBundleStateFinal {
		t.Fatalf("Finalize() = %#v, %v", final, err)
	}
}

func TestMarkdownBookAnalysisAndCompilePreserveOrderedStablePages(t *testing.T) {
	one := []byte("# One")
	two := []byte("# Two")
	files := []contracts.ProjectGraphFile{
		{Path: "chapters/one.md", SHA256: contracts.SHA256Hex(one), Bytes: int64(len(one)), MediaType: "text/markdown", Role: contracts.ProjectFileRoleSource},
		{Path: "chapters/two.md", SHA256: contracts.SHA256Hex(two), Bytes: int64(len(two)), MediaType: "text/markdown", Role: contracts.ProjectFileRoleSource},
	}
	projectHash, err := contracts.ComputeProjectHash(contracts.ContentKindMarkdown, files)
	if err != nil {
		t.Fatal(err)
	}
	graph := contracts.ProjectGraph{
		SchemaVersion: contracts.ProjectGraphSchemaVersion, ProjectHash: projectHash, ContentKind: contracts.ContentKindMarkdown,
		Entrypoints: []contracts.ProjectEntrypoint{
			{Path: "chapters/one.md", Role: contracts.EntrypointRoleBookPage},
			{Path: "chapters/two.md", Role: contracts.EntrypointRoleBookPage},
		},
		Files: files, References: []contracts.ProjectReference{}, Options: map[string]any{
			"documentMode": "book", "title": "Book", "pageIds": map[string]any{
				"chapters/one.md": "legacy-one", "chapters/two.md": "legacy-two",
			},
		},
	}
	snapshot := orchestration.ProjectSnapshot{Graph: graph, Files: orchestrationtest.MemoryProjectFiles{Files: map[string][]byte{
		"chapters/one.md": one, "chapters/two.md": two,
	}}}
	worker := adapterWorkerFunc(func(_ context.Context, operation string, payload any, result any) error {
		if operation != compileBookOperation {
			t.Fatalf("operation = %q", operation)
		}
		request := payload.(compileBookPayload)
		if len(request.Pages) != 2 || request.Pages[0].PageID != "legacy-one" || request.Pages[1].PageID != "legacy-two" {
			t.Fatalf("book compile pages = %#v", request.Pages)
		}
		bundle := contracts.DocumentBundle{
			SchemaVersion: contracts.DocumentBundleSchemaVersion, ProjectHash: projectHash, State: contracts.DocumentBundleStateDraft,
			ContentKind: contracts.ContentKindMarkdown, DocumentEngine: Engine, Title: "Book",
			Pages: []contracts.DocumentPage{
				{ID: "legacy-one", SourcePath: "chapters/one.md", Fragment: "<article><h1>One</h1></article>", FragmentFormat: contracts.FragmentFormatPlaceholders, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{files[0].SHA256}},
				{ID: "legacy-two", SourcePath: "chapters/two.md", Fragment: "<article><h1>Two</h1></article>", FragmentFormat: contracts.FragmentFormatPlaceholders, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{files[1].SHA256}},
			},
			WorkUnits: []contracts.WorkUnit{}, Assets: []contracts.AssetReference{}, Diagnostics: []contracts.Diagnostic{},
			Provenance: contracts.Provenance{Adapter: Engine, AdapterVersion: AdapterVersion, EngineVersion: "pipeline", ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion},
		}
		*result.(*workerBundleResponse) = workerBundleResponse{Publishable: false, Bundle: bundle}
		return nil
	})
	adapter := Adapter{Worker: worker, MaxFileBytes: 1024}
	analysis, err := adapter.Analyze(context.Background(), snapshot, orchestration.RenderOptions{Mode: "book"})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.WorkloadFeatures["pages"] != 2 {
		t.Fatalf("analysis = %#v", analysis)
	}
	bundle, err := adapter.Compile(context.Background(), snapshot, analysis)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Pages) != 2 || bundle.Pages[0].ID != "legacy-one" || bundle.Pages[1].ID != "legacy-two" {
		t.Fatalf("compiled bundle pages = %#v", bundle.Pages)
	}
}

func TestMarkdownBookPageFinalizerCacheReusesVerifiedPages(t *testing.T) {
	projectHash := strings.Repeat("a", 64)
	dependencyOne := strings.Repeat("b", 64)
	dependencyTwo := strings.Repeat("c", 64)
	draft := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersion, ProjectHash: projectHash, State: contracts.DocumentBundleStateDraft,
		ContentKind: contracts.ContentKindMarkdown, DocumentEngine: Engine, Title: "Book",
		Pages: []contracts.DocumentPage{
			{ID: "page-one", SourcePath: "one.md", Title: "One", Fragment: `<article><h1 id="one">One</h1></article>`, FragmentFormat: contracts.FragmentFormatPlaceholders, TOC: []contracts.TOCEntry{{ID: "one", Depth: 1, Text: "One"}}, DependencyHashes: []string{dependencyOne}},
			{ID: "page-two", SourcePath: "two.md", Title: "Two", Fragment: `<article><h1 id="two">Two</h1></article>`, FragmentFormat: contracts.FragmentFormatPlaceholders, TOC: []contracts.TOCEntry{{ID: "two", Depth: 1, Text: "Two"}}, DependencyHashes: []string{dependencyTwo}},
		},
		WorkUnits: []contracts.WorkUnit{}, Assets: []contracts.AssetReference{}, Diagnostics: []contracts.Diagnostic{},
		Provenance: contracts.Provenance{Adapter: Engine, AdapterVersion: AdapterVersion, EngineVersion: "pipeline-v1", ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion},
	}
	plan := bookCachePlan{SchemaVersion: bookPageCacheSchemaVersion, GlobalMetadataHash: strings.Repeat("d", 64), Pages: []bookCachePlanPage{
		{ID: "page-one", SourcePath: "one.md", PageSemanticHash: strings.Repeat("e", 64)},
		{ID: "page-two", SourcePath: "two.md", PageSemanticHash: strings.Repeat("f", 64)},
	}}
	finalBundle := draft
	finalBundle.State = contracts.DocumentBundleStateFinal
	finalBundle.BundleHash = strings.Repeat("1", 64)
	finalBundle.Pages = append([]contracts.DocumentPage(nil), draft.Pages...)
	for index := range finalBundle.Pages {
		finalBundle.Pages[index].FragmentFormat = contracts.FragmentFormatHTML
		finalBundle.Pages[index].TOC = []contracts.TOCEntry{{ID: "rin-md-" + []string{"one", "two"}[index], Depth: 1, Text: []string{"One", "Two"}[index]}}
	}
	records := []bookPageCacheRecord{
		{SchemaVersion: bookPageCacheSchemaVersion, PageSemanticHash: plan.Pages[0].PageSemanticHash, PageHash: strings.Repeat("2", 64), Page: finalBundle.Pages[0]},
		{SchemaVersion: bookPageCacheSchemaVersion, PageSemanticHash: plan.Pages[1].PageSemanticHash, PageHash: strings.Repeat("3", 64), Page: finalBundle.Pages[1]},
	}
	finalizeCalls := 0
	worker := adapterWorkerFunc(func(_ context.Context, operation string, payload any, result any) error {
		switch operation {
		case planBookCacheOperation:
			*result.(*bookCachePlan) = plan
		case finalizeOperation:
			request := payload.(finalizePayload)
			reused := make([]string, 0, len(request.PageCache))
			for _, page := range plan.Pages {
				if _, ok := request.PageCache[page.ID]; ok {
					reused = append(reused, page.ID)
				}
			}
			finalizeCalls++
			*result.(*workerBundleResponse) = workerBundleResponse{Publishable: true, Bundle: finalBundle, PageCache: &bookPageCacheResult{
				SchemaVersion: bookPageCacheSchemaVersion, GlobalMetadataHash: plan.GlobalMetadataHash, ProjectDependencyHash: strings.Repeat("4", 64),
				ReusedPageIDs: reused, Records: records,
			}}
		default:
			t.Fatalf("unexpected operation %q", operation)
		}
		return nil
	})
	cache := newBookMemoryCache()
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	adapter := Adapter{Worker: worker, MaxFileBytes: 1024, Cache: cache, Store: cache,
		MathFontVersion: "mathjax-newcm-1", BookCacheCompatibility: "book-v1", Now: func() time.Time { return now }}
	first, err := adapter.FinalizeDetailedWithBookReuse(context.Background(), draft, orchestration.ResolvedWork{Units: map[string]orchestration.ResolvedWorkUnit{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.CacheDecision != "full-render-reuse-disabled" || len(first.ReusedPageIDs) != 0 || len(cache.records) != 2 {
		t.Fatalf("first finalization = %#v, records=%d", first, len(cache.records))
	}
	if first.ProjectResultKey == nil || first.ProjectResultKey.Stage != orchestration.CacheStageProjectResult {
		t.Fatalf("project result key = %#v", first.ProjectResultKey)
	}
	second, err := adapter.FinalizeDetailed(context.Background(), draft, orchestration.ResolvedWork{Units: map[string]orchestration.ResolvedWorkUnit{}})
	if err != nil {
		t.Fatal(err)
	}
	if second.CacheDecision != "incremental-page-reuse" || len(second.ReusedPageIDs) != 2 || finalizeCalls != 2 || second.Bundle.BundleHash != finalBundle.BundleHash {
		t.Fatalf("second finalization = %#v, calls=%d", second, finalizeCalls)
	}
	withoutCache, err := (Adapter{Worker: worker, MaxFileBytes: 1024}).FinalizeDetailed(
		context.Background(), draft, orchestration.ResolvedWork{Units: map[string]orchestration.ResolvedWorkUnit{}},
	)
	if err != nil || withoutCache.CacheDecision != "full-render-cache-unconfigured" || len(withoutCache.ReusedPageIDs) != 0 {
		t.Fatalf("uncached fallback = %#v, %v", withoutCache, err)
	}
}
