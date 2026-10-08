package artifactstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderstorage"
)

const testJobID = "11111111-1111-4111-8111-111111111111"

func TestFileStorePrivateLifecycleAndMetadata(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Ready(ctx); err != nil {
		t.Fatalf("artifact readiness: %v", err)
	}
	body := []byte("private source")
	digest := testSHA256(body)
	expiresAt := store.now().Add(time.Hour)
	write := orchestration.ArtifactWrite{
		ArtifactID:    "render-jobs/v1/source/" + testJobID + "/" + digest,
		Body:          body,
		MediaType:     "application/zip",
		SchemaVersion: "rin-project-archive/v1",
		ExpiresAt:     &expiresAt,
	}
	reference, err := store.PutPrivate(ctx, write)
	if err != nil {
		t.Fatal(err)
	}
	if reference.Visibility != "private" || reference.SHA256 != digest || reference.Bytes != int64(len(body)) {
		t.Fatalf("private reference = %#v", reference)
	}
	got, err := store.GetPrivate(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("private body = %q", got)
	}

	// Identical retries are safe; changed immutable metadata is rejected.
	if _, err := store.PutPrivate(ctx, write); err != nil {
		t.Fatalf("idempotent PutPrivate() error = %v", err)
	}
	changedExpiry := expiresAt.Add(time.Minute)
	write.ExpiresAt = &changedExpiry
	if _, err := store.PutPrivate(ctx, write); !errors.Is(err, ErrContentChanged) {
		t.Fatalf("changed private metadata error = %v, want ErrContentChanged", err)
	}

	wrongReference := reference
	wrongReference.MediaType = "text/plain"
	if _, err := store.GetPrivate(ctx, wrongReference); !errors.Is(err, ErrMetadata) {
		t.Fatalf("GetPrivate() metadata error = %v", err)
	}
	publicReference := reference
	publicReference.Visibility = "public"
	if _, err := store.GetPrivate(ctx, publicReference); !errors.Is(err, ErrPrivateOnly) {
		t.Fatalf("GetPrivate() visibility error = %v", err)
	}

	if err := store.DeletePrivate(ctx, reference); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePrivate(ctx, reference); err != nil {
		t.Fatalf("idempotent DeletePrivate() error = %v", err)
	}
	if _, err := store.GetPrivate(ctx, reference); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted GetPrivate() error = %v", err)
	}
}

func TestFileStoreCacheNamespaceAndVerifiedRead(t *testing.T) {
	store := newTestStore(t)
	body := []byte(`{"schemaVersion":"rin-math-cache/v1","html":"<mjx-container></mjx-container>"}`)
	artifactID, err := orchestration.CacheArtifactID(orchestration.CacheStageMath, body)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := store.now().Add(time.Hour)
	write := orchestration.ArtifactWrite{
		ArtifactID:    artifactID,
		Body:          body,
		MediaType:     "application/json",
		SchemaVersion: "rin-math-cache/v1",
		ExpiresAt:     &expiresAt,
	}
	reference, err := store.PutPrivate(context.Background(), write)
	if err != nil {
		t.Fatal(err)
	}
	expected := orchestration.ArtifactExpectation{
		Reference: reference, MediaType: write.MediaType, SchemaVersion: write.SchemaVersion, MaxBytes: int64(len(body)),
	}
	got, err := store.GetVerified(context.Background(), expected)
	if err != nil || string(got) != string(body) {
		t.Fatalf("GetVerified() = %q, %v", got, err)
	}
	wrongSchema := expected
	wrongSchema.SchemaVersion = "rin-math-cache/v2"
	if _, err := store.GetVerified(context.Background(), wrongSchema); !errors.Is(err, ErrSchema) {
		t.Fatalf("wrong schema error = %v", err)
	}
	wrongMedia := expected
	wrongMedia.MediaType = "text/html"
	if _, err := store.GetVerified(context.Background(), wrongMedia); !errors.Is(err, ErrMetadata) {
		t.Fatalf("wrong media error = %v", err)
	}
	tooSmall := expected
	tooSmall.MaxBytes--
	if _, err := store.GetVerified(context.Background(), tooSmall); !errors.Is(err, ErrMetadata) {
		t.Fatalf("oversize error = %v", err)
	}
	invalidID := write
	digest := testSHA256(body)
	invalidID.ArtifactID = "render-cache/v1/unknown/" + digest[:2] + "/" + digest
	if _, err := store.PutPrivate(context.Background(), invalidID); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("invalid cache stage error = %v", err)
	}
}

func TestFileStorePublicDiagramContractAndIsolation(t *testing.T) {
	store := newTestStore(t)
	body := []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	artifactID := renderstorage.SVGObjectID(body)
	reference, err := store.PutPublicIfMissing(context.Background(), orchestration.ArtifactWrite{
		ArtifactID: artifactID, Body: body, MediaType: renderstorage.SVGContentType,
		SchemaVersion: "rin-diagram-svg/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reference.ArtifactID != artifactID || reference.Visibility != "public" || reference.ExpiresAt != "" {
		t.Fatalf("public reference = %#v", reference)
	}
	if _, err := store.PutPublicIfMissing(context.Background(), orchestration.ArtifactWrite{
		ArtifactID: artifactID, Body: body, MediaType: renderstorage.SVGContentType,
		SchemaVersion: "rin-diagram-svg/v1",
	}); err != nil {
		t.Fatalf("idempotent PutPublicIfMissing() error = %v", err)
	}
	publicPath, err := store.PublicObjectPath(artifactID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("public body = %q", got)
	}
	verified, err := store.GetVerified(context.Background(), orchestration.ArtifactExpectation{
		Reference: reference, MediaType: renderstorage.SVGContentType,
		SchemaVersion: "rin-diagram-svg/v1", MaxBytes: int64(len(body)),
	})
	if err != nil || string(verified) != string(body) {
		t.Fatalf("verified public diagram = %q, %v", verified, err)
	}

	privateID := "render-jobs/v1/debug/" + testJobID + "/worker.log"
	if _, err := store.PublicObjectPath(privateID); !errors.Is(err, ErrPublicOnly) {
		t.Fatalf("private PublicObjectPath() error = %v", err)
	}
	if _, err := store.PutPublicIfMissing(context.Background(), orchestration.ArtifactWrite{
		ArtifactID: privateID,
		Body:       []byte("secret"),
		MediaType:  "text/plain",
	}); !errors.Is(err, ErrPublicOnly) {
		t.Fatalf("private PutPublicIfMissing() error = %v", err)
	}
	if strings.Contains(publicPath, store.privateRoot) {
		t.Fatalf("public path overlaps private root: %q", publicPath)
	}
}

func TestFileStoreNamespacesLimitsExpiryAndTampering(t *testing.T) {
	limits := Limits{SourceBytes: 4, ResultBytes: 5, DebugBytes: 6, CacheBytes: 6, PublicBytes: 7}
	store, err := NewFileStore(t.TempDir(), limits)
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return fixedNow }
	expiresAt := fixedNow.Add(time.Hour)

	invalidIDs := []string{
		"../render-jobs/v1/debug/" + testJobID + "/worker.log",
		"render-jobs/v1/debug/" + testJobID + "/../worker.log",
		"render-jobs/v1/debug/AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA/worker.log",
		"diagrams/v1/svg-sha256/aa/" + strings.Repeat("a", 64) + ".svg",
	}
	for _, artifactID := range invalidIDs[:3] {
		_, err := store.PutPrivate(context.Background(), orchestration.ArtifactWrite{
			ArtifactID: artifactID,
			Body:       []byte("x"),
			MediaType:  "text/plain",
			ExpiresAt:  &expiresAt,
		})
		if err == nil {
			t.Fatalf("PutPrivate(%q) succeeded", artifactID)
		}
	}
	if _, err := store.PutPublicIfMissing(context.Background(), orchestration.ArtifactWrite{
		ArtifactID: invalidIDs[3],
		Body:       []byte("x"),
		MediaType:  "image/svg+xml",
	}); err == nil {
		t.Fatal("PutPublicIfMissing() accepted mismatched hash path")
	}

	debugID := "render-jobs/v1/debug/" + testJobID + "/worker.log"
	if _, err := store.PutPrivate(context.Background(), orchestration.ArtifactWrite{
		ArtifactID: debugID,
		Body:       []byte("1234567"),
		MediaType:  "text/plain",
		ExpiresAt:  &expiresAt,
	}); !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("debug size error = %v", err)
	}
	past := fixedNow.Add(-time.Second)
	if _, err := store.PutPrivate(context.Background(), orchestration.ArtifactWrite{
		ArtifactID: debugID,
		Body:       []byte("log"),
		MediaType:  "text/plain",
		ExpiresAt:  &past,
	}); !errors.Is(err, ErrExpired) {
		t.Fatalf("past expiry error = %v", err)
	}

	reference, err := store.PutPrivate(context.Background(), orchestration.ArtifactWrite{
		ArtifactID: debugID,
		Body:       []byte("log"),
		MediaType:  "text/plain",
		ExpiresAt:  &expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.privatePath(debugID), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPrivate(context.Background(), reference); !errors.Is(err, ErrMetadata) {
		t.Fatalf("tampered GetPrivate() error = %v", err)
	}
}

func TestFileStoreCleanupExpiredIsBounded(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := store.now()
	for _, name := range []string{"first.log", "second.log"} {
		expiresAt := now.Add(time.Minute)
		if _, err := store.PutPrivate(ctx, orchestration.ArtifactWrite{
			ArtifactID: "render-jobs/v1/debug/" + testJobID + "/" + name,
			Body:       []byte(name),
			MediaType:  "text/plain",
			ExpiresAt:  &expiresAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := store.CleanupExpired(ctx, now.Add(2*time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("first cleanup deleted = %d, want 1", deleted)
	}
	deleted, err = store.CleanupExpired(ctx, now.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("second cleanup deleted = %d, want 1", deleted)
	}
}

func TestArtifactReferenceValidateIsPublic(t *testing.T) {
	reference := contracts.ArtifactReference{
		ArtifactID: "render-jobs/v1/debug/" + testJobID + "/worker.log",
		SHA256:     strings.Repeat("a", 64),
		Bytes:      1,
		MediaType:  "text/plain",
		Visibility: "private",
		ExpiresAt:  "2026-08-10T00:00:00Z",
	}
	if err := reference.Validate(); err != nil {
		t.Fatal(err)
	}
}

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewFileStore(t.TempDir(), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return fixedNow }
	return store
}

func testSHA256(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
