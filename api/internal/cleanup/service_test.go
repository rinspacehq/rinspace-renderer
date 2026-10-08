package cleanup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
)

func TestRunOnceStagesDeletesAndAcknowledgesPrivateOnly(t *testing.T) {
	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	repository := &fakeRepository{artifacts: []jobpostgres.Artifact{{
		ID: "private-id", StorageKey: "render-jobs/v1/debug/11111111-1111-4111-8111-111111111111/worker.log",
		SHA256:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ByteSize: 3, MediaType: "text/plain", Visibility: "private", ExpiresAt: &now,
	}}}
	store := &fakeStore{}
	service, err := New(repository, store, Config{BatchLimit: 10, EventTTL: 7 * 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.ExpiredJobs != 1 || report.StagedArtifacts != 1 || report.DeletedArtifacts != 1 || repository.acked != "private-id" {
		t.Fatalf("cleanup report/repository = %#v / %#v", report, repository)
	}
	if store.reference.Visibility != "private" || store.reference.ArtifactID != repository.artifacts[0].StorageKey {
		t.Fatalf("deleted reference = %#v", store.reference)
	}
}

func TestRunOnceKeepsTombstoneWhenObjectDeleteFails(t *testing.T) {
	now := time.Now().UTC()
	repository := &fakeRepository{artifacts: []jobpostgres.Artifact{{ID: "a", StorageKey: "a"}}}
	service, err := New(repository, &fakeStore{err: errors.New("storage unavailable")}, Config{
		BatchLimit: 1, EventTTL: time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() ignored private deletion failure")
	}
	if repository.acked != "" {
		t.Fatal("failed object deletion acknowledged tombstone")
	}
}

type fakeRepository struct {
	artifacts []jobpostgres.Artifact
	acked     string
}

func (*fakeRepository) ExpireTerminalJobs(context.Context, time.Time, int) (int, error) {
	return 1, nil
}
func (*fakeRepository) CleanupExpiredMetadata(context.Context, time.Time, time.Time, int) (jobpostgres.MetadataCleanup, error) {
	return jobpostgres.MetadataCleanup{Idempotency: 1}, nil
}
func (*fakeRepository) StageExpiredPrivateArtifacts(context.Context, time.Time, int) (int, error) {
	return 1, nil
}
func (repository *fakeRepository) PendingArtifactDeletions(context.Context, int) ([]jobpostgres.Artifact, error) {
	return repository.artifacts, nil
}
func (repository *fakeRepository) AckArtifactDeletion(_ context.Context, id string) error {
	repository.acked = id
	return nil
}

type fakeStore struct {
	reference contracts.ArtifactReference
	err       error
}

func (store *fakeStore) DeletePrivate(_ context.Context, reference contracts.ArtifactReference) error {
	store.reference = reference
	return store.err
}
