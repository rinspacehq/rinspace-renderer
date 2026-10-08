package codeservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

type fakeRenderer struct {
	mu       sync.Mutex
	requests [][]RenderRequest
	err      error
}

func (renderer *fakeRenderer) RenderBatch(_ context.Context, _ string, requests []RenderRequest) ([]RenderResult, error) {
	renderer.mu.Lock()
	renderer.requests = append(renderer.requests, append([]RenderRequest(nil), requests...))
	renderer.mu.Unlock()
	if renderer.err != nil {
		return nil, renderer.err
	}
	results := make([]RenderResult, len(requests))
	for index, request := range requests {
		if strings.HasPrefix(request.Language, "unknown") {
			results[index] = RenderResult{
				ID:          request.ID,
				HTML:        `<pre class="rin-code-plain"><code>` + request.Source + `</code></pre>`,
				Diagnostics: []RenderDiagnostic{{Code: "code.language.unknown", Severity: "warning", Message: "unknown language"}},
			}
			continue
		}
		results[index] = RenderResult{
			ID: request.ID, HTML: `<pre class="shiki"><code>` + request.Source + `</code></pre>`,
			CanonicalLanguage: "javascript", Highlighted: true, Diagnostics: []RenderDiagnostic{},
		}
	}
	return results, nil
}

type memoryCacheStore struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	records map[string]orchestration.CacheRecord
	corrupt bool
}

func newMemoryCacheStore() *memoryCacheStore {
	return &memoryCacheStore{bodies: map[string][]byte{}, records: map[string]orchestration.CacheRecord{}}
}

func (store *memoryCacheStore) PutPrivate(_ context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	digest := sha256.Sum256(write.Body)
	reference := contracts.ArtifactReference{
		ArtifactID: write.ArtifactID, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(write.Body)),
		MediaType: write.MediaType, Visibility: "private",
	}
	store.mu.Lock()
	store.bodies[write.ArtifactID] = append([]byte(nil), write.Body...)
	store.mu.Unlock()
	return reference, nil
}

func (store *memoryCacheStore) GetPrivate(_ context.Context, reference contracts.ArtifactReference) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	body, ok := store.bodies[reference.ArtifactID]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), body...), nil
}

func (store *memoryCacheStore) DeletePrivate(_ context.Context, reference contracts.ArtifactReference) error {
	store.mu.Lock()
	delete(store.bodies, reference.ArtifactID)
	store.mu.Unlock()
	return nil
}

func (store *memoryCacheStore) PutPublicIfMissing(context.Context, orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	return contracts.ArtifactReference{}, errors.New("unexpected public write")
}

func (store *memoryCacheStore) Lookup(_ context.Context, key orchestration.CacheKey, expected orchestration.CacheExpectation, now time.Time) orchestration.CacheLookupResult {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, ok := store.records[key.String()]
	if !ok || !record.ExpiresAt.After(now) || record.SchemaVersion != expected.SchemaVersion || record.Artifact.MediaType != expected.MediaType {
		return orchestration.CacheLookupResult{Reason: "not_found"}
	}
	body := append([]byte(nil), store.bodies[record.Artifact.ArtifactID]...)
	if store.corrupt {
		body = []byte("not-json")
	}
	return orchestration.CacheLookupResult{Record: record, Body: body, Hit: true, Reason: "hit"}
}

func (store *memoryCacheStore) Store(_ context.Context, record orchestration.CacheRecord, _ orchestration.CacheExpectation, _ time.Time) orchestration.CacheStoreResult {
	store.mu.Lock()
	store.records[record.Key.String()] = record
	store.mu.Unlock()
	return orchestration.CacheStoreResult{Stored: true, Reason: "stored"}
}

func TestResolveCodeDeduplicatesBatchAndReusesLayeredCache(t *testing.T) {
	renderer := &fakeRenderer{}
	cache := newMemoryCacheStore()
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	service := testService(t, renderer, cache, func() time.Time { return now })
	first := codeUnit("rw_00000000000000000000000000000001", "const x = 1;", "js")
	second := codeUnit("rw_00000000000000000000000000000002", first.Source, first.Language)
	second.Meta = "title=second.js"
	batch := codeBatch(first, second)
	result, err := service.ResolveCode(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(renderer.requests) != 1 || len(renderer.requests[0]) != 1 {
		t.Fatalf("duplicate source was rendered more than once: %#v", renderer.requests)
	}
	if result.Units[0].Cache.Status != "miss" || result.Units[1].Cache.Status != "hit" || result.Units[1].Source.Meta != second.Meta {
		t.Fatalf("unexpected in-batch reuse metadata: %#v", result.Units)
	}
	third := codeUnit("rw_00000000000000000000000000000003", first.Source, first.Language)
	result, err = service.ResolveCode(context.Background(), codeBatch(third))
	if err != nil {
		t.Fatal(err)
	}
	if len(renderer.requests) != 1 || result.Units[0].Cache.Status != "hit" {
		t.Fatalf("layered cache was not reused: calls=%d result=%#v", len(renderer.requests), result.Units[0])
	}
}

func TestResolveCodePreservesUnknownLanguageAndFallsBackAfterWorkerFailure(t *testing.T) {
	renderer := &fakeRenderer{}
	service := testService(t, renderer, nil, nil)
	unknown := codeUnit("rw_00000000000000000000000000000004", `<script>alert(1)</script>`, `unknown" onclick="x`)
	result, err := service.ResolveCode(context.Background(), codeBatch(unknown))
	if err != nil {
		t.Fatal(err)
	}
	if result.Units[0].Highlighted || result.Units[0].Source.Language != unknown.Language || len(result.Units[0].Diagnostics) != 1 {
		t.Fatalf("unknown language fallback lost metadata: %#v", result.Units[0])
	}

	renderer.err = errors.New("worker crashed")
	fallback := codeUnit("rw_00000000000000000000000000000005", `<script>alert(2)</script>`, "js")
	result, err = service.ResolveCode(context.Background(), codeBatch(fallback))
	if err != nil {
		t.Fatal(err)
	}
	if result.Units[0].Highlighted || strings.Contains(result.Units[0].HTML, "<script>") || result.Units[0].Diagnostics[0].Code != "code.worker.failed" {
		t.Fatalf("worker failure was not escaped safely: %#v", result.Units[0])
	}
}

func TestResolveCodeRejectsThemeDriftAndCorruptCache(t *testing.T) {
	renderer := &fakeRenderer{}
	cache := newMemoryCacheStore()
	service := testService(t, renderer, cache, time.Now)
	unit := codeUnit("rw_00000000000000000000000000000006", "let x = 1", "js")
	batch := codeBatch(unit)
	if _, err := service.ResolveCode(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	cache.corrupt = true
	unit.ID = "rw_00000000000000000000000000000007"
	if _, err := service.ResolveCode(context.Background(), codeBatch(unit)); err != nil {
		t.Fatal(err)
	}
	if len(renderer.requests) != 2 {
		t.Fatalf("corrupt cache was trusted: calls=%d", len(renderer.requests))
	}
	batch = codeBatch(unit)
	batch.Theme = "dark-plus"
	if _, err := service.ResolveCode(context.Background(), batch); err == nil {
		t.Fatal("theme drift was accepted")
	}
}

func testService(t *testing.T, renderer Renderer, cache *memoryCacheStore, now func() time.Time) *Service {
	t.Helper()
	config := Config{Renderer: renderer, EngineVersion: ShikiVersion, Theme: DefaultTheme, Now: now}
	if cache != nil {
		config.Cache = cache
		config.Store = cache
	}
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func codeUnit(id, source, language string) contracts.WorkUnit {
	return contracts.WorkUnit{Kind: contracts.WorkUnitCode, ID: id, Source: source, Language: language}
}

func codeBatch(units ...contracts.WorkUnit) orchestration.CodeBatch {
	return orchestration.CodeBatch{
		ContractVersion: orchestration.CodeBatchContractVersion, Items: units,
		Theme: DefaultTheme, OutputStrategy: "html",
	}
}
