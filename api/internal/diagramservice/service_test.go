package diagramservice

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/diagramengine"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderstorage"
)

const testWorkID = "rw_0123456789abcdef0123456789abcdef"

type fakeRenderer struct {
	mu       sync.Mutex
	calls    []diagramengine.Request
	response diagramengine.Response
	err      error
	wait     time.Duration
}

type rendererFunc func(context.Context, diagramengine.Request) (diagramengine.Response, error)

func (render rendererFunc) Render(ctx context.Context, request diagramengine.Request) (diagramengine.Response, error) {
	return render(ctx, request)
}

func (renderer *fakeRenderer) Render(ctx context.Context, request diagramengine.Request) (diagramengine.Response, error) {
	if renderer.wait > 0 {
		select {
		case <-time.After(renderer.wait):
		case <-ctx.Done():
			return diagramengine.Response{}, ctx.Err()
		}
	}
	renderer.mu.Lock()
	renderer.calls = append(renderer.calls, request)
	renderer.mu.Unlock()
	return renderer.response, renderer.err
}

func (renderer *fakeRenderer) callCount() int {
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	return len(renderer.calls)
}

type fakePublicStore struct {
	mu             sync.Mutex
	bodies         map[string][]byte
	exists         bool
	returnedObject string
	putCalls       int
	headCalls      int
}

func newFakePublicStore() *fakePublicStore {
	return &fakePublicStore{bodies: map[string][]byte{}, exists: true}
}

func (store *fakePublicStore) PutPublicObjectIfMissing(_ context.Context, objectID, _ string, body []byte) (renderstorage.PutResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.putCalls++
	store.bodies[objectID] = append([]byte(nil), body...)
	store.exists = true
	returned := objectID
	if store.returnedObject != "" {
		returned = store.returnedObject
	}
	return renderstorage.PutResult{ObjectID: returned, Uploaded: true}, nil
}

func (store *fakePublicStore) ReadPublicObject(_ context.Context, objectID string, maxBytes int64) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.headCalls++
	body, stored := store.bodies[objectID]
	if !store.exists || !stored {
		return nil, errors.New("not found")
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("too large")
	}
	return append([]byte(nil), body...), nil
}

func (store *fakePublicStore) PublicObjectURL(objectID string) string {
	return "https://storage.example/public/" + objectID
}

func testService(t *testing.T, renderer *fakeRenderer, store *fakePublicStore, cache Cache, mutate ...func(*Config)) *Service {
	t.Helper()
	config := Config{
		Renderer: renderer, Store: store, Cache: cache,
		EngineVersion: "texsvg-test", FontVersion: "texlive-test+dvisvgm-test",
		Parallelism: 2, Timeout: time.Second,
	}
	for _, update := range mutate {
		update(&config)
	}
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func diagramItem(id, source string) orchestration.DiagramBatchItem {
	return orchestration.DiagramBatchItem{
		Unit: contracts.WorkUnit{
			Kind: contracts.WorkUnitDiagram, ID: id, DiagramType: "tikzpicture", Source: source,
			Options: "scale=1", Layout: &contracts.DiagramLayout{Alignment: "center"},
			SourceLocation: &contracts.SourceLocation{Path: "chapter/main.tex", Start: contracts.SourcePosition{Line: 7, Column: 3}},
		},
		SourceMode: orchestration.DiagramSourceBody, Body: source, WrapperID: "diagram-000001",
	}
}

func diagramBatch(items ...orchestration.DiagramBatchItem) orchestration.DiagramBatch {
	return orchestration.DiagramBatch{ContractVersion: orchestration.DiagramBatchContractVersion, Items: items, OutputStrategy: "svg"}
}

func TestResolveDiagramsSanitizesStoresAndPreservesMetadata(t *testing.T) {
	renderer := &fakeRenderer{response: diagramengine.Response{
		ID: "worker-diagram", Type: "tikzpicture", URL: "https://worker/internal",
		SVG:         `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10" onclick="bad()"><script>bad()</script><path d="M0 0L1 1"/></svg>`,
		Diagnostics: []diagramengine.Diagnostic{{Severity: "warning", Code: "diagram.worker.warning", Message: "worker warning"}},
	}}
	store := newFakePublicStore()
	item := diagramItem(testWorkID, `\draw (0,0)--(1,1);`)
	service := testService(t, renderer, store, NewMemoryCache(8))
	result, err := service.ResolveDiagrams(context.Background(), diagramBatch(item))
	if err != nil {
		t.Fatal(err)
	}
	unit := result.Units[0]
	if unit.State != "succeeded" || unit.Cache.Status != "miss" || !unit.Uploaded || unit.Source.Source != item.Unit.Source ||
		unit.Source.Layout.Alignment != "center" || unit.Source.SourceLocation.Start.Line != 7 {
		t.Fatalf("resolved unit = %#v", unit)
	}
	expectedObject := "diagrams/v1/svg-sha256/" + unit.SVGHash[:2] + "/" + unit.SVGHash + ".svg"
	if unit.ObjectID != expectedObject || unit.Artifact == nil || unit.Artifact.ArtifactID != expectedObject || unit.URL != store.PublicObjectURL(expectedObject) {
		t.Fatalf("artifact identity = %#v", unit)
	}
	stored := string(store.bodies[expectedObject])
	if strings.Contains(strings.ToLower(stored), "script") || strings.Contains(strings.ToLower(stored), "onclick") {
		t.Fatalf("unsafe SVG reached public store: %s", stored)
	}
	if renderer.callCount() != 1 || len(unit.Diagnostics) != 1 || unit.Diagnostics[0].SourceLocation == nil {
		t.Fatalf("calls=%d diagnostics=%#v", renderer.callCount(), unit.Diagnostics)
	}
}

func TestResolveDiagramsDeduplicatesBatchAndReusesVerifiedArtifact(t *testing.T) {
	renderer := &fakeRenderer{response: diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><path d="M0 0L2 2"/></svg>`}}
	store := newFakePublicStore()
	cache := NewMemoryCache(8)
	service := testService(t, renderer, store, cache)
	first := diagramItem(testWorkID, `\draw (0,0)--(2,2);`)
	second := diagramItem("rw_1123456789abcdef0123456789abcdef", first.Unit.Source)
	second.WrapperID = "diagram-000002"
	result, err := service.ResolveDiagrams(context.Background(), diagramBatch(first, second))
	if err != nil {
		t.Fatal(err)
	}
	if renderer.callCount() != 1 || result.Units[0].Cache.Status != "miss" || result.Units[1].Cache.Status != "hit" || result.Units[0].ObjectID != result.Units[1].ObjectID {
		t.Fatalf("dedupe result=%#v calls=%d", result.Units, renderer.callCount())
	}
	third := diagramItem("rw_2123456789abcdef0123456789abcdef", first.Unit.Source)
	result, err = service.ResolveDiagrams(context.Background(), diagramBatch(third))
	if err != nil {
		t.Fatal(err)
	}
	if renderer.callCount() != 1 || result.Units[0].Cache.Status != "hit" || store.headCalls == 0 {
		t.Fatalf("verified cache result=%#v calls=%d heads=%d", result.Units[0], renderer.callCount(), store.headCalls)
	}
}

func TestResolveDiagramsRendersAgainWhenCachedPublicObjectIsMissing(t *testing.T) {
	renderer := &fakeRenderer{response: diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><circle r="2"/></svg>`}}
	store := newFakePublicStore()
	service := testService(t, renderer, store, NewMemoryCache(8))
	item := diagramItem(testWorkID, `\draw (0,0) circle (2);`)
	if _, err := service.ResolveDiagrams(context.Background(), diagramBatch(item)); err != nil {
		t.Fatal(err)
	}
	store.exists = false
	item.Unit.ID = "rw_3123456789abcdef0123456789abcdef"
	result, err := service.ResolveDiagrams(context.Background(), diagramBatch(item))
	if err != nil {
		t.Fatal(err)
	}
	if renderer.callCount() != 2 || result.Units[0].Cache.Status != "miss" {
		t.Fatalf("missing artifact was trusted: calls=%d result=%#v", renderer.callCount(), result.Units[0])
	}
}

func TestResolveDiagramsRejectsCorruptCachedArtifactContent(t *testing.T) {
	renderer := &fakeRenderer{response: diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><path d="M0 0L3 3"/></svg>`}}
	store := newFakePublicStore()
	service := testService(t, renderer, store, NewMemoryCache(8))
	item := diagramItem(testWorkID, `\draw (0,0)--(3,3);`)
	first, err := service.ResolveDiagrams(context.Background(), diagramBatch(item))
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.bodies[first.Units[0].ObjectID] = []byte("corrupt")
	store.mu.Unlock()
	item.Unit.ID = "rw_8123456789abcdef0123456789abcdef"
	second, err := service.ResolveDiagrams(context.Background(), diagramBatch(item))
	if err != nil {
		t.Fatal(err)
	}
	if renderer.callCount() != 2 || second.Units[0].State != "succeeded" || second.Units[0].Cache.Status != "miss" {
		t.Fatalf("corrupt cache was trusted: calls=%d result=%#v", renderer.callCount(), second.Units[0])
	}
}

func TestResolveDiagramsVersionChangeMakesCacheEntryUnreachable(t *testing.T) {
	renderer := &fakeRenderer{response: diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="2" height="2"/></svg>`}}
	store := newFakePublicStore()
	cache := NewMemoryCache(8)
	item := diagramItem(testWorkID, `\draw (0,0) rectangle (2,2);`)
	if _, err := testService(t, renderer, store, cache).ResolveDiagrams(context.Background(), diagramBatch(item)); err != nil {
		t.Fatal(err)
	}
	item.Unit.ID = "rw_4123456789abcdef0123456789abcdef"
	changed := testService(t, renderer, store, cache, func(config *Config) { config.SanitizerVersion = "rin-svg-sanitizer/v2" })
	result, err := changed.ResolveDiagrams(context.Background(), diagramBatch(item))
	if err != nil {
		t.Fatal(err)
	}
	if renderer.callCount() != 2 || result.Units[0].Cache.Status != "miss" {
		t.Fatalf("versioned cache boundary failed: calls=%d result=%#v", renderer.callCount(), result.Units[0])
	}
}

func TestResolveDiagramsReturnsStructuredFailureAndHonorsLimits(t *testing.T) {
	renderer := &fakeRenderer{err: errors.New("worker unavailable")}
	service := testService(t, renderer, newFakePublicStore(), nil, func(config *Config) { config.MaxSourceBytes = 8 })
	tooLarge := diagramItem(testWorkID, "123456789")
	failed, err := service.ResolveDiagrams(context.Background(), diagramBatch(tooLarge))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Units[0].State != "failed" || failed.Units[0].Diagnostics[0].Code != "diagram.source.too_large" || renderer.callCount() != 0 {
		t.Fatalf("source bound = %#v calls=%d", failed.Units[0], renderer.callCount())
	}
	item := diagramItem("rw_5123456789abcdef0123456789abcdef", "short")
	failed, err = service.ResolveDiagrams(context.Background(), diagramBatch(item))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Units[0].State != "failed" || failed.Units[0].Diagnostics[0].Code != "diagram.render.failed" {
		t.Fatalf("worker failure = %#v", failed.Units[0])
	}
}

func TestResolveDiagramsRejectsUnexpectedStoreObjectID(t *testing.T) {
	renderer := &fakeRenderer{response: diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><path d="M0 0"/></svg>`}}
	store := newFakePublicStore()
	store.returnedObject = "wrong/object.svg"
	result, err := testService(t, renderer, store, nil).ResolveDiagrams(context.Background(), diagramBatch(diagramItem(testWorkID, "short")))
	if err != nil {
		t.Fatal(err)
	}
	if result.Units[0].State != "failed" || !strings.Contains(result.Units[0].Diagnostics[0].Message, "unexpected object ID") {
		t.Fatalf("store identity failure = %#v", result.Units[0])
	}
}

func TestResolveDiagramsOwnsTimeoutAndBoundedParallelism(t *testing.T) {
	timed := &fakeRenderer{wait: 50 * time.Millisecond, response: diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><path d="M0 0L1 1"/></svg>`}}
	timeoutService := testService(t, timed, newFakePublicStore(), nil, func(config *Config) { config.Timeout = 5 * time.Millisecond })
	result, err := timeoutService.ResolveDiagrams(context.Background(), diagramBatch(diagramItem(testWorkID, "short")))
	if err != nil {
		t.Fatal(err)
	}
	if result.Units[0].State != "failed" || !strings.Contains(result.Units[0].Diagnostics[0].Message, "deadline exceeded") {
		t.Fatalf("timeout result = %#v", result.Units[0])
	}

	var mu sync.Mutex
	active, maximum := 0, 0
	bounded := rendererFunc(func(ctx context.Context, _ diagramengine.Request) (diagramengine.Response, error) {
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		mu.Unlock()
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			return diagramengine.Response{}, ctx.Err()
		}
		mu.Lock()
		active--
		mu.Unlock()
		return diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><path d="M0 0L1 1"/></svg>`}, nil
	})
	service, err := New(Config{
		Renderer: bounded, Store: newFakePublicStore(), EngineVersion: "v1", FontVersion: "font-v1",
		Parallelism: 2, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	items := make([]orchestration.DiagramBatchItem, 5)
	for index := range items {
		items[index] = diagramItem("rw_"+strings.Repeat(string(rune('1'+index)), 32), "source-"+string(rune('a'+index)))
	}
	if _, err := service.ResolveDiagrams(context.Background(), diagramBatch(items...)); err != nil {
		t.Fatal(err)
	}
	if maximum != 2 {
		t.Fatalf("maximum worker concurrency = %d, want 2", maximum)
	}
	mu.Lock()
	maximum = 0
	mu.Unlock()
	start := make(chan struct{})
	errorsByRequest := make(chan error, 2)
	for batchIndex := 0; batchIndex < 2; batchIndex++ {
		batchIndex := batchIndex
		go func() {
			<-start
			requestItems := make([]orchestration.DiagramBatchItem, 2)
			for index := range requestItems {
				digit := rune('6' + batchIndex*2 + index)
				requestItems[index] = diagramItem("rw_"+strings.Repeat(string(digit), 32), "request-source-"+string(digit))
			}
			_, err := service.ResolveDiagrams(context.Background(), diagramBatch(requestItems...))
			errorsByRequest <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsByRequest; err != nil {
			t.Fatal(err)
		}
	}
	if maximum != 2 {
		t.Fatalf("cross-request worker concurrency = %d, want global limit 2", maximum)
	}
	secondService, err := New(Config{
		Renderer: bounded, Store: newFakePublicStore(), EngineVersion: "v1", FontVersion: "font-v1",
		Parallelism: 2, Timeout: time.Second, Pool: service.pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	maximum = 0
	mu.Unlock()
	start = make(chan struct{})
	for index, currentService := range []*Service{service, secondService} {
		index, currentService := index, currentService
		go func() {
			<-start
			items := []orchestration.DiagramBatchItem{
				diagramItem("rw_"+strings.Repeat(string(rune('a'+index*2)), 32), "shared-pool-a-"+string(rune('a'+index))),
				diagramItem("rw_"+strings.Repeat(string(rune('b'+index*2)), 32), "shared-pool-b-"+string(rune('a'+index))),
			}
			_, err := currentService.ResolveDiagrams(context.Background(), diagramBatch(items...))
			errorsByRequest <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsByRequest; err != nil {
			t.Fatal(err)
		}
	}
	if maximum != 2 {
		t.Fatalf("shared-pool worker concurrency = %d, want global limit 2", maximum)
	}
}

func TestResolveDiagramsCoalescesIdenticalConcurrentRequests(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	renderer := rendererFunc(func(ctx context.Context, _ diagramengine.Request) (diagramengine.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return diagramengine.Response{}, ctx.Err()
		}
		return diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><path d="M0 0L1 1"/></svg>`}, nil
	})
	service, err := New(Config{Renderer: renderer, Store: newFakePublicStore(), EngineVersion: "v1", FontVersion: "font-v1", Parallelism: 2, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan orchestration.DiagramBatchResult, 2)
	errorsByRequest := make(chan error, 2)
	for index := 0; index < 2; index++ {
		index := index
		go func() {
			<-start
			item := diagramItem("rw_"+strings.Repeat(string(rune('a'+index)), 32), "identical")
			result, err := service.ResolveDiagrams(context.Background(), diagramBatch(item))
			results <- result
			errorsByRequest <- err
		}()
	}
	close(start)
	statuses := map[string]int{}
	uploaded := []bool{}
	for range 2 {
		if err := <-errorsByRequest; err != nil {
			t.Fatal(err)
		}
		result := <-results
		statuses[result.Units[0].Cache.Status]++
		uploaded = append(uploaded, result.Units[0].Uploaded)
	}
	if calls != 1 || statuses["miss"] != 1 || statuses["hit"] != 1 {
		t.Fatalf("singleflight calls=%d statuses=%v uploaded=%v", calls, statuses, uploaded)
	}
}

func TestSingleflightLeaderTimeoutDoesNotFailUnrelatedFollower(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	renderer := rendererFunc(func(ctx context.Context, _ diagramengine.Request) (diagramengine.Response, error) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()
		if current == 1 {
			<-ctx.Done()
			return diagramengine.Response{}, ctx.Err()
		}
		return diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><path d="M0 0L1 1"/></svg>`}, nil
	})
	service, err := New(Config{
		Renderer: renderer, Store: newFakePublicStore(), EngineVersion: "v1", FontVersion: "font-v1",
		Parallelism: 1, Timeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan orchestration.DiagramBatchResult, 2)
	for index := 0; index < 2; index++ {
		index := index
		go func() {
			<-start
			item := diagramItem("rw_"+strings.Repeat(string(rune('c'+index)), 32), "identical-timeout")
			result, resolveErr := service.ResolveDiagrams(context.Background(), diagramBatch(item))
			if resolveErr != nil {
				results <- orchestration.DiagramBatchResult{}
				return
			}
			results <- result
		}()
	}
	close(start)
	states := map[string]int{}
	for range 2 {
		result := <-results
		if len(result.Units) != 1 {
			t.Fatal("ResolveDiagrams returned a contract error")
		}
		states[result.Units[0].State]++
	}
	if calls != 2 || states["failed"] != 1 || states["succeeded"] != 1 {
		t.Fatalf("leader timeout isolation calls=%d states=%v", calls, states)
	}
}

func TestResolveDiagramsBoundsBatchOptionsAndSVGOutput(t *testing.T) {
	renderer := &fakeRenderer{response: diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><path d="M0 0"/></svg>`}}
	service := testService(t, renderer, newFakePublicStore(), nil, func(config *Config) {
		config.MaxBatchSize = 1
		config.MaxOptionsBytes = 4
		config.MaxSVGBytes = 32
	})
	item := diagramItem(testWorkID, "short")
	duplicate := item
	duplicate.Unit.ID = "rw_6123456789abcdef0123456789abcdef"
	if _, err := service.ResolveDiagrams(context.Background(), diagramBatch(item, duplicate)); err == nil {
		t.Fatal("oversized batch was accepted")
	}
	item.Unit.Options = "12345"
	result, err := service.ResolveDiagrams(context.Background(), diagramBatch(item))
	if err != nil {
		t.Fatal(err)
	}
	if result.Units[0].Diagnostics[0].Code != "diagram.options.too_large" || renderer.callCount() != 0 {
		t.Fatalf("options limit = %#v calls=%d", result.Units[0], renderer.callCount())
	}
	item.Unit.Options = ""
	result, err = service.ResolveDiagrams(context.Background(), diagramBatch(item))
	if err != nil {
		t.Fatal(err)
	}
	if result.Units[0].State != "failed" || !strings.Contains(result.Units[0].Diagnostics[0].Message, "SVG exceeds") {
		t.Fatalf("SVG limit = %#v", result.Units[0])
	}
}

func TestDiagramCacheKeyDistinguishesEmptySentinelValues(t *testing.T) {
	renderer := &fakeRenderer{response: diagramengine.Response{SVG: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><path d="M0 0L1 1"/></svg>`}}
	service := testService(t, renderer, newFakePublicStore(), NewMemoryCache(8))
	first := diagramItem(testWorkID, "short")
	first.Unit.Options = ""
	if _, err := service.ResolveDiagrams(context.Background(), diagramBatch(first)); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Unit.ID = "rw_7123456789abcdef0123456789abcdef"
	second.Unit.Options = "<rin-empty>"
	if _, err := service.ResolveDiagrams(context.Background(), diagramBatch(second)); err != nil {
		t.Fatal(err)
	}
	if renderer.callCount() != 2 {
		t.Fatalf("empty and literal sentinel collided in cache: calls=%d", renderer.callCount())
	}
}
