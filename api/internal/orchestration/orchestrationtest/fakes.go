package orchestrationtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

var (
	_ orchestration.DocumentAdapter       = (*FakeAdapter)(nil)
	_ orchestration.ProjectFileReader     = MemoryProjectFiles{}
	_ orchestration.MathService           = (*FakeWorkServices)(nil)
	_ orchestration.DiagramService        = (*FakeWorkServices)(nil)
	_ orchestration.CodeService           = (*FakeWorkServices)(nil)
	_ orchestration.ArtifactStore         = (*MemoryArtifactStore)(nil)
	_ orchestration.VerifiedArtifactStore = (*MemoryArtifactStore)(nil)
	_ orchestration.Cache                 = (*MemoryCache)(nil)
	_ orchestration.WorkerDiagnostics     = FakeWorkerDiagnostics{}
)

type FakeAdapter struct {
	CapabilitiesValue orchestration.DocumentCapabilities
	AnalyzeFunc       func(context.Context, orchestration.ProjectSnapshot, orchestration.RenderOptions) (orchestration.Analysis, error)
	CompileFunc       func(context.Context, orchestration.ProjectSnapshot, orchestration.Analysis) (contracts.DocumentBundle, error)
	FinalizeFunc      func(context.Context, contracts.DocumentBundle, orchestration.ResolvedWork) (contracts.DocumentBundle, error)
}

func (fake *FakeAdapter) Capabilities() orchestration.DocumentCapabilities {
	return fake.CapabilitiesValue
}

func (fake *FakeAdapter) Analyze(ctx context.Context, snapshot orchestration.ProjectSnapshot, options orchestration.RenderOptions) (orchestration.Analysis, error) {
	if fake.AnalyzeFunc == nil {
		return orchestration.Analysis{}, errors.New("unexpected fake adapter Analyze call")
	}
	return fake.AnalyzeFunc(ctx, snapshot, options)
}

func (fake *FakeAdapter) Compile(ctx context.Context, snapshot orchestration.ProjectSnapshot, analysis orchestration.Analysis) (contracts.DocumentBundle, error) {
	if fake.CompileFunc == nil {
		return contracts.DocumentBundle{}, errors.New("unexpected fake adapter Compile call")
	}
	return fake.CompileFunc(ctx, snapshot, analysis)
}

func (fake *FakeAdapter) Finalize(ctx context.Context, draft contracts.DocumentBundle, resolved orchestration.ResolvedWork) (contracts.DocumentBundle, error) {
	if fake.FinalizeFunc == nil {
		return contracts.DocumentBundle{}, errors.New("unexpected fake adapter Finalize call")
	}
	return fake.FinalizeFunc(ctx, draft, resolved)
}

type FakeWorkServices struct {
	MathFunc    func(context.Context, orchestration.MathBatch) (orchestration.MathBatchResult, error)
	DiagramFunc func(context.Context, orchestration.DiagramBatch) (orchestration.DiagramBatchResult, error)
	CodeFunc    func(context.Context, orchestration.CodeBatch) (orchestration.CodeBatchResult, error)
}

type MemoryProjectFiles struct {
	Files map[string][]byte
}

func (reader MemoryProjectFiles) ReadFile(_ context.Context, projectPath string, maxBytes int64) ([]byte, error) {
	body, ok := reader.Files[projectPath]
	if !ok {
		return nil, errors.New("fake project file not found")
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("fake project file exceeds read limit")
	}
	return append([]byte(nil), body...), nil
}

func (fake *FakeWorkServices) ResolveMath(ctx context.Context, batch orchestration.MathBatch) (orchestration.MathBatchResult, error) {
	if fake.MathFunc == nil {
		return orchestration.MathBatchResult{}, errors.New("unexpected fake math call")
	}
	return fake.MathFunc(ctx, batch)
}

func (fake *FakeWorkServices) ResolveDiagrams(ctx context.Context, batch orchestration.DiagramBatch) (orchestration.DiagramBatchResult, error) {
	if fake.DiagramFunc == nil {
		return orchestration.DiagramBatchResult{}, errors.New("unexpected fake diagram call")
	}
	return fake.DiagramFunc(ctx, batch)
}

func (fake *FakeWorkServices) ResolveCode(ctx context.Context, batch orchestration.CodeBatch) (orchestration.CodeBatchResult, error) {
	if fake.CodeFunc == nil {
		return orchestration.CodeBatchResult{}, errors.New("unexpected fake code call")
	}
	return fake.CodeFunc(ctx, batch)
}

type MemoryArtifactStore struct {
	mu            sync.Mutex
	PrivateBodies map[string][]byte
	PublicBodies  map[string][]byte
	Writes        map[string]orchestration.ArtifactWrite
	References    map[string]contracts.ArtifactReference
	PutFunc       func(orchestration.ArtifactWrite) (contracts.ArtifactReference, error)
}

func NewMemoryArtifactStore() *MemoryArtifactStore {
	return &MemoryArtifactStore{
		PrivateBodies: map[string][]byte{},
		PublicBodies:  map[string][]byte{},
		Writes:        map[string]orchestration.ArtifactWrite{},
		References:    map[string]contracts.ArtifactReference{},
	}
}

func (store *MemoryArtifactStore) PutPrivate(_ context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	if store.PutFunc == nil {
		return contracts.ArtifactReference{}, errors.New("fake artifact PutFunc is not configured")
	}
	reference, err := store.PutFunc(write)
	if err != nil {
		return contracts.ArtifactReference{}, err
	}
	store.mu.Lock()
	store.PrivateBodies[reference.ArtifactID] = append([]byte(nil), write.Body...)
	store.Writes[reference.ArtifactID] = write
	store.References[reference.ArtifactID] = reference
	store.mu.Unlock()
	return reference, nil
}

func (store *MemoryArtifactStore) GetVerified(_ context.Context, expected orchestration.ArtifactExpectation) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	write, ok := store.Writes[expected.Reference.ArtifactID]
	if !ok || store.References[expected.Reference.ArtifactID] != expected.Reference ||
		write.MediaType != expected.MediaType || write.SchemaVersion != expected.SchemaVersion ||
		expected.MaxBytes <= 0 || int64(len(write.Body)) > expected.MaxBytes {
		return nil, errors.New("fake artifact metadata mismatch")
	}
	digest := sha256.Sum256(write.Body)
	if expected.Reference.SHA256 != hex.EncodeToString(digest[:]) || expected.Reference.Bytes != int64(len(write.Body)) {
		return nil, errors.New("fake artifact content mismatch")
	}
	return append([]byte(nil), write.Body...), nil
}

func (store *MemoryArtifactStore) GetPrivate(_ context.Context, reference contracts.ArtifactReference) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	body, ok := store.PrivateBodies[reference.ArtifactID]
	if !ok {
		return nil, errors.New("fake artifact not found")
	}
	return append([]byte(nil), body...), nil
}

func (store *MemoryArtifactStore) DeletePrivate(_ context.Context, reference contracts.ArtifactReference) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.PrivateBodies, reference.ArtifactID)
	delete(store.Writes, reference.ArtifactID)
	delete(store.References, reference.ArtifactID)
	return nil
}

func (store *MemoryArtifactStore) PutPublicIfMissing(_ context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	if store.PutFunc == nil {
		return contracts.ArtifactReference{}, errors.New("fake artifact PutFunc is not configured")
	}
	reference, err := store.PutFunc(write)
	if err != nil {
		return contracts.ArtifactReference{}, err
	}
	store.mu.Lock()
	if _, exists := store.PublicBodies[reference.ArtifactID]; !exists {
		store.PublicBodies[reference.ArtifactID] = append([]byte(nil), write.Body...)
		store.Writes[reference.ArtifactID] = write
		store.References[reference.ArtifactID] = reference
	}
	store.mu.Unlock()
	return reference, nil
}

type MemoryCache struct {
	mu      sync.Mutex
	Entries map[orchestration.CacheKey]orchestration.CacheRecord
}

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{Entries: map[orchestration.CacheKey]orchestration.CacheRecord{}}
}

func (cache *MemoryCache) Lookup(_ context.Context, key orchestration.CacheKey, _ orchestration.CacheExpectation, now time.Time) orchestration.CacheLookupResult {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.Entries[key]
	if !ok || !entry.ExpiresAt.After(now) {
		return orchestration.CacheLookupResult{Reason: "not_found"}
	}
	return orchestration.CacheLookupResult{Record: entry, Hit: true, Reason: "hit"}
}

func (cache *MemoryCache) Store(_ context.Context, entry orchestration.CacheRecord, _ orchestration.CacheExpectation, _ time.Time) orchestration.CacheStoreResult {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.Entries[entry.Key] = entry
	return orchestration.CacheStoreResult{Stored: true, Reason: "stored"}
}

type FakeWorkerDiagnostics struct {
	Health []orchestration.WorkerHealth
	Err    error
}

func (fake FakeWorkerDiagnostics) Snapshot(context.Context) ([]orchestration.WorkerHealth, error) {
	return append([]orchestration.WorkerHealth(nil), fake.Health...), fake.Err
}
