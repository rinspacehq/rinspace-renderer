package cacheindex

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

type Entry = orchestration.CacheRecord

type Repository interface {
	CacheEntry(context.Context, orchestration.CacheKey, time.Time) (Entry, bool, error)
	PutCacheEntry(context.Context, Entry) error
	TouchCacheEntry(context.Context, orchestration.CacheKey, time.Time) error
	DeleteCacheEntry(context.Context, orchestration.CacheKey) error
	RegisterCacheArtifact(context.Context, contracts.ArtifactReference, string) error
}

type Expectation = orchestration.CacheExpectation
type LookupResult = orchestration.CacheLookupResult
type StoreResult = orchestration.CacheStoreResult

type Index struct {
	repository Repository
	store      orchestration.VerifiedArtifactStore
}

const maintenanceTimeout = time.Second

func New(repository Repository, store orchestration.VerifiedArtifactStore) *Index {
	return &Index{repository: repository, store: store}
}

// PutPrivate makes cache artifacts visible to the durable SQL index before a cache entry can
// reference them. Other private artifact kinds continue to use their owning admission/result
// transactions and are deliberately rejected here.
func (index *Index) PutPrivate(ctx context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	if index == nil || index.repository == nil || index.store == nil || !strings.HasPrefix(write.ArtifactID, "render-cache/v1/") {
		return contracts.ArtifactReference{}, errors.New("cache artifact store request is invalid")
	}
	reference, err := index.store.PutPrivate(ctx, write)
	if err != nil {
		return contracts.ArtifactReference{}, err
	}
	if err := index.repository.RegisterCacheArtifact(ctx, reference, write.SchemaVersion); err != nil {
		return contracts.ArtifactReference{}, err
	}
	return reference, nil
}

func (index *Index) GetPrivate(ctx context.Context, reference contracts.ArtifactReference) ([]byte, error) {
	return index.store.GetPrivate(ctx, reference)
}

func (index *Index) DeletePrivate(ctx context.Context, reference contracts.ArtifactReference) error {
	return index.store.DeletePrivate(ctx, reference)
}

func (index *Index) PutPublicIfMissing(ctx context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	return index.store.PutPublicIfMissing(ctx, write)
}

func (index *Index) GetVerified(ctx context.Context, expected orchestration.ArtifactExpectation) ([]byte, error) {
	return index.store.GetVerified(ctx, expected)
}

// Lookup deliberately converts index and artifact failures to bounded misses. Callers can always
// run the correct uncached stage without interpreting storage-specific errors.
func (index *Index) Lookup(ctx context.Context, key orchestration.CacheKey, expected Expectation, now time.Time) LookupResult {
	if index == nil || index.repository == nil || index.store == nil || key.Validate() != nil ||
		!validExpectation(expected) || now.IsZero() {
		return LookupResult{Reason: "invalid_request"}
	}
	entry, found, err := index.repository.CacheEntry(ctx, key, now)
	if err != nil {
		return LookupResult{Reason: "index_unavailable"}
	}
	if !found {
		return LookupResult{Reason: "not_found"}
	}
	if entry.Key != key || entry.SchemaVersion != expected.SchemaVersion || entry.ArtifactSchemaVersion != expected.SchemaVersion ||
		entry.Artifact.MediaType != expected.MediaType || entry.Artifact.Bytes > expected.MaxBytes ||
		!entry.ExpiresAt.After(now) {
		maintenance, cancel := maintenanceContext(ctx)
		_ = index.repository.DeleteCacheEntry(maintenance, key)
		cancel()
		return LookupResult{Reason: "metadata_invalid"}
	}
	body, err := index.store.GetVerified(ctx, orchestration.ArtifactExpectation{
		Reference: entry.Artifact, MediaType: expected.MediaType,
		SchemaVersion: expected.SchemaVersion, MaxBytes: expected.MaxBytes,
	})
	if err != nil {
		maintenance, cancel := maintenanceContext(ctx)
		_ = index.repository.DeleteCacheEntry(maintenance, key)
		cancel()
		return LookupResult{Reason: "artifact_invalid"}
	}
	result := LookupResult{Record: entry, Body: body, Hit: true, Reason: "hit"}
	maintenance, cancel := maintenanceContext(ctx)
	defer cancel()
	if err := index.repository.TouchCacheEntry(maintenance, key, now); err != nil {
		result.Maintenance = "touch_failed"
	}
	return result
}

// Store first verifies the immutable artifact and then records its index entry. Any failure leaves
// the render result usable and is reported as a bounded non-stored outcome.
func (index *Index) Store(ctx context.Context, entry Entry, expected Expectation, now time.Time) StoreResult {
	if index == nil || index.repository == nil || index.store == nil || entry.Key.Validate() != nil ||
		!validExpectation(expected) || now.IsZero() || entry.SchemaVersion != expected.SchemaVersion ||
		entry.Artifact.MediaType != expected.MediaType || entry.Artifact.Bytes > expected.MaxBytes ||
		!entry.ExpiresAt.After(now) {
		return StoreResult{Reason: "invalid_request"}
	}
	if _, err := index.store.GetVerified(ctx, orchestration.ArtifactExpectation{
		Reference: entry.Artifact, MediaType: expected.MediaType,
		SchemaVersion: expected.SchemaVersion, MaxBytes: expected.MaxBytes,
	}); err != nil {
		return StoreResult{Reason: "artifact_invalid"}
	}
	if err := index.repository.PutCacheEntry(ctx, entry); err != nil {
		return StoreResult{Reason: "index_unavailable"}
	}
	return StoreResult{Stored: true, Reason: "stored"}
}

func validExpectation(expected Expectation) bool {
	return strings.TrimSpace(expected.SchemaVersion) != "" && strings.TrimSpace(expected.MediaType) != "" && expected.MaxBytes > 0
}

func maintenanceContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), maintenanceTimeout)
}

var _ orchestration.Cache = (*Index)(nil)
var _ orchestration.VerifiedArtifactStore = (*Index)(nil)
