package cacheindex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

var _ Repository = (*jobpostgres.Repository)(nil)

func TestIndexValidatesArtifactBeforeHitAndTouch(t *testing.T) {
	now := time.Date(2026, 8, 9, 13, 0, 0, 0, time.UTC)
	key := testKey(t)
	reference := contracts.ArtifactReference{
		ArtifactID: "render-cache/v1/math/aa/" + strings.Repeat("a", 64), SHA256: strings.Repeat("b", 64),
		Bytes: 4, MediaType: "application/json", Visibility: "private", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}
	repository := &fakeRepository{entry: Entry{Key: key, Artifact: reference, SchemaVersion: "rin-math-cache/v1", ArtifactSchemaVersion: "rin-math-cache/v1", ExpiresAt: now.Add(time.Hour)}}
	store := &fakeStore{body: []byte("body")}
	index := New(repository, store)
	expected := Expectation{SchemaVersion: "rin-math-cache/v1", MediaType: "application/json", MaxBytes: 16}
	result := index.Lookup(context.Background(), key, expected, now)
	if !result.Hit || result.Record.Key != key || string(result.Body) != "body" || repository.touches != 1 || repository.deletes != 0 {
		t.Fatalf("Lookup() = %#v, repository = %#v", result, repository)
	}
	store.err = errors.New("hash mismatch at artifact store")
	result = index.Lookup(context.Background(), key, expected, now)
	if result.Hit || result.Reason != "artifact_invalid" || repository.deletes != 1 {
		t.Fatalf("invalid artifact Lookup() = %#v, deletes=%d", result, repository.deletes)
	}
}

func TestIndexFailuresBecomeMissesAndNeverBlockCorrectRendering(t *testing.T) {
	now := time.Date(2026, 8, 9, 13, 0, 0, 0, time.UTC)
	key := testKey(t)
	expected := Expectation{SchemaVersion: "rin-code-cache/v1", MediaType: "text/html", MaxBytes: 32}
	repository := &fakeRepository{err: errors.New("database unavailable")}
	index := New(repository, &fakeStore{})
	if result := index.Lookup(context.Background(), key, expected, now); result.Hit || result.Reason != "index_unavailable" {
		t.Fatalf("database failure Lookup() = %#v", result)
	}
	// The stage result is authoritative; a cache outage only selects this uncached path.
	uncached := []byte("correct uncached render")
	output := uncached
	if result := index.Lookup(context.Background(), key, expected, now); result.Hit {
		output = result.Body
	}
	if string(output) != string(uncached) {
		t.Fatalf("cache outage changed render output to %q", output)
	}
	reference := contracts.ArtifactReference{
		ArtifactID: "render-cache/v1/code/aa/" + strings.Repeat("a", 64), SHA256: strings.Repeat("b", 64),
		Bytes: 4, MediaType: "text/html", Visibility: "private", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}
	entry := Entry{Key: key, Artifact: reference, SchemaVersion: expected.SchemaVersion, ExpiresAt: now.Add(time.Hour)}
	if result := index.Store(context.Background(), entry, expected, now); result.Stored || result.Reason != "index_unavailable" {
		t.Fatalf("database failure Store() = %#v", result)
	}
	repository.err = nil
	repository.putErr = errors.New("write unavailable")
	if result := index.Store(context.Background(), entry, expected, now); result.Stored || result.Reason != "index_unavailable" {
		t.Fatalf("write failure Store() = %#v", result)
	}
}

func TestIndexRejectsSchemaMediaSizeAndExpiryBeforeTrust(t *testing.T) {
	now := time.Date(2026, 8, 9, 13, 0, 0, 0, time.UTC)
	key := testKey(t)
	expected := Expectation{SchemaVersion: "rin-math-cache/v1", MediaType: "application/json", MaxBytes: 4}
	base := Entry{
		Key: key, SchemaVersion: expected.SchemaVersion, ArtifactSchemaVersion: expected.SchemaVersion, ExpiresAt: now.Add(time.Hour),
		Artifact: contracts.ArtifactReference{ArtifactID: "cache", SHA256: strings.Repeat("a", 64), Bytes: 4, MediaType: expected.MediaType, Visibility: "private"},
	}
	for name, mutate := range map[string]func(*Entry){
		"schema":          func(entry *Entry) { entry.SchemaVersion = "wrong" },
		"artifact-schema": func(entry *Entry) { entry.ArtifactSchemaVersion = "wrong" },
		"media":           func(entry *Entry) { entry.Artifact.MediaType = "text/plain" },
		"size":            func(entry *Entry) { entry.Artifact.Bytes = 5 },
		"expiry":          func(entry *Entry) { entry.ExpiresAt = now },
	} {
		t.Run(name, func(t *testing.T) {
			entry := base
			mutate(&entry)
			repository := &fakeRepository{entry: entry}
			result := New(repository, &fakeStore{body: []byte("body")}).Lookup(context.Background(), key, expected, now)
			if result.Hit || result.Reason != "metadata_invalid" || repository.deletes != 1 {
				t.Fatalf("Lookup() = %#v, deletes=%d", result, repository.deletes)
			}
		})
	}
}

func TestIndexRegistersPrivateCacheArtifactBeforeIndexing(t *testing.T) {
	now := time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC)
	reference := contracts.ArtifactReference{
		ArtifactID: "render-cache/v1/page-finalizer/aa/" + strings.Repeat("a", 64), SHA256: strings.Repeat("b", 64),
		Bytes: 4, MediaType: "application/vnd.rinspace.markdown-page-cache+json", Visibility: "private",
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}
	repository := &fakeRepository{}
	index := New(repository, &fakeStore{reference: reference})
	got, err := index.PutPrivate(context.Background(), orchestration.ArtifactWrite{
		ArtifactID: reference.ArtifactID, Body: []byte("body"), MediaType: reference.MediaType,
		SchemaVersion: "rin-markdown-book-page-cache/v1", ExpiresAt: ptrTime(now.Add(time.Hour)),
	})
	if err != nil || got != reference || repository.registered != 1 {
		t.Fatalf("PutPrivate() = %#v, %v; registered=%d", got, err, repository.registered)
	}
	if _, err := index.PutPrivate(context.Background(), orchestration.ArtifactWrite{ArtifactID: "render-jobs/v1/debug/x/y"}); err == nil {
		t.Fatal("PutPrivate() accepted a non-cache artifact")
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func testKey(t *testing.T) orchestration.CacheKey {
	t.Helper()
	versions := map[string]string{}
	for _, name := range orchestration.RequiredCacheVersions(orchestration.CacheStageMath) {
		versions[name] = "v1"
	}
	key, err := orchestration.BuildCacheKey(orchestration.CacheKeyInput{
		Stage: orchestration.CacheStageMath, NormalizedInputs: map[string]string{"source": "x"}, Versions: versions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

type fakeRepository struct {
	entry      Entry
	found      bool
	err        error
	putErr     error
	touches    int
	deletes    int
	registered int
}

func (repository *fakeRepository) CacheEntry(context.Context, orchestration.CacheKey, time.Time) (Entry, bool, error) {
	if repository.err != nil {
		return Entry{}, false, repository.err
	}
	if !repository.found && repository.entry.Key.Version == "" {
		return Entry{}, false, nil
	}
	return repository.entry, true, nil
}
func (repository *fakeRepository) PutCacheEntry(context.Context, Entry) error {
	if repository.err != nil {
		return repository.err
	}
	return repository.putErr
}
func (repository *fakeRepository) TouchCacheEntry(context.Context, orchestration.CacheKey, time.Time) error {
	repository.touches++
	return nil
}
func (repository *fakeRepository) DeleteCacheEntry(context.Context, orchestration.CacheKey) error {
	repository.deletes++
	return nil
}
func (repository *fakeRepository) RegisterCacheArtifact(context.Context, contracts.ArtifactReference, string) error {
	repository.registered++
	return repository.putErr
}

type fakeStore struct {
	body      []byte
	err       error
	reference contracts.ArtifactReference
}

func (store *fakeStore) PutPrivate(context.Context, orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	return store.reference, store.err
}
func (*fakeStore) GetPrivate(context.Context, contracts.ArtifactReference) ([]byte, error) {
	return nil, errors.New("not used")
}
func (*fakeStore) DeletePrivate(context.Context, contracts.ArtifactReference) error {
	return errors.New("not used")
}
func (*fakeStore) PutPublicIfMissing(context.Context, orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	return contracts.ArtifactReference{}, errors.New("not used")
}
func (store *fakeStore) GetVerified(context.Context, orchestration.ArtifactExpectation) ([]byte, error) {
	return append([]byte(nil), store.body...), store.err
}
