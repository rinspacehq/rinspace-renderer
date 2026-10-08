package typstpdfexecutor

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

const testJobID = "018fcafe-1234-4abc-8def-1234567890ab"

type fakeRepository struct {
	mu        sync.Mutex
	artifacts map[string]jobpostgres.Artifact
}

func (repository *fakeRepository) Artifact(_ context.Context, id string) (jobpostgres.Artifact, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	artifact, ok := repository.artifacts[id]
	if !ok {
		return jobpostgres.Artifact{}, errors.New("artifact not found")
	}
	return artifact, nil
}

func (repository *fakeRepository) CreateArtifact(_ context.Context, artifact jobpostgres.Artifact) (jobpostgres.Artifact, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if _, exists := repository.artifacts[artifact.ID]; exists {
		return jobpostgres.Artifact{}, errors.New("artifact exists")
	}
	repository.artifacts[artifact.ID] = artifact
	return artifact, nil
}

type fakeStore struct {
	mu     sync.Mutex
	bodies map[string][]byte
}

func (store *fakeStore) GetPrivate(_ context.Context, reference contracts.ArtifactReference) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	body, ok := store.bodies[reference.ArtifactID]
	if !ok {
		return nil, errors.New("artifact body not found")
	}
	return append([]byte(nil), body...), nil
}

func (store *fakeStore) PutPrivate(_ context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	if write.ExpiresAt == nil || len(write.Body) == 0 {
		return contracts.ArtifactReference{}, errors.New("invalid write")
	}
	digest := sha256.Sum256(write.Body)
	reference := contracts.ArtifactReference{
		ArtifactID: write.ArtifactID, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(write.Body)),
		MediaType: write.MediaType, Visibility: "private", ExpiresAt: write.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if err := reference.Validate(); err != nil {
		return contracts.ArtifactReference{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if current, exists := store.bodies[write.ArtifactID]; exists && !bytes.Equal(current, write.Body) {
		return contracts.ArtifactReference{}, errors.New("immutable content changed")
	}
	store.bodies[write.ArtifactID] = append([]byte(nil), write.Body...)
	return reference, nil
}

type fakeBroker struct {
	mu           sync.Mutex
	image        string
	onStart      func(string) error
	inspect      func(int) BrokerInspection
	inspectCalls int
	startCalls   int
	stopCalls    int
	removeCalls  int
	startError   error
	removeError  error
}

func (broker *fakeBroker) Start(_ context.Context, id string) (BrokerInspection, error) {
	broker.mu.Lock()
	broker.startCalls++
	callback := broker.onStart
	err := broker.startError
	broker.mu.Unlock()
	if err != nil {
		return BrokerInspection{}, err
	}
	if callback != nil {
		if err := callback(id); err != nil {
			return BrokerInspection{}, err
		}
	}
	return BrokerInspection{ID: id, Profile: brokerProfile, State: "running", Running: true, Image: broker.image}, nil
}

func (broker *fakeBroker) Inspect(_ context.Context, id string) (BrokerInspection, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.inspectCalls++
	if broker.inspect != nil {
		return broker.inspect(broker.inspectCalls), nil
	}
	return BrokerInspection{ID: id, Profile: brokerProfile, State: "exited", Image: broker.image}, nil
}

func (broker *fakeBroker) Stop(_ context.Context, id string) (BrokerInspection, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.stopCalls++
	return BrokerInspection{ID: id, Profile: brokerProfile, State: "exited", Image: broker.image}, nil
}

func (broker *fakeBroker) Remove(_ context.Context, id string) (BrokerInspection, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.removeCalls++
	if broker.removeError != nil {
		return BrokerInspection{}, broker.removeError
	}
	return BrokerInspection{ID: id, Profile: brokerProfile, State: "missing"}, nil
}

type executorFixture struct {
	executor   *Executor
	repository *fakeRepository
	store      *fakeStore
	broker     *fakeBroker
	job        jobpostgres.Job
	root       string
	image      string
	digest     string
	now        time.Time
}

func newExecutorFixture(t *testing.T, wallTimeout time.Duration) executorFixture {
	t.Helper()
	return newExecutorFixtureWith(t, wallTimeout,
		map[string][]byte{"main.typ": []byte("#set text(size: 12pt)\n= Typst Title\n")}, "main.typ")
}

func newExecutorFixtureWith(t *testing.T, wallTimeout time.Duration, files map[string][]byte, entrypoint string) executorFixture {
	t.Helper()
	root := t.TempDir()
	now := time.Date(2026, 9, 16, 5, 6, 7, 0, time.UTC)
	archive, snapshotHash := snapshotArchive(t, files)
	sourceHash := sha256.Sum256(archive)
	imageDigest := "sha256:" + strings.Repeat("b", 64)
	metadata, err := json.Marshal(requestMetadata{
		RequestID: "render-0123456789abcdef0123456789abcdef", Entrypoint: entrypoint,
		OutputKind: PreviewOutputKind, SnapshotHash: snapshotHash, SessionID: "session-018fcafe",
		DraftRevision: 42, EnginePolicyID: PolicyID, ImageDigest: imageDigest,
		PreviewContractVersion: PreviewContractVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(24 * time.Hour)
	sourceID := "render-jobs/v1/source/" + testJobID + "/" + hex.EncodeToString(sourceHash[:])
	repository := &fakeRepository{artifacts: map[string]jobpostgres.Artifact{
		sourceID: {
			ID: sourceID, SHA256: hex.EncodeToString(sourceHash[:]), Kind: "source", Visibility: "private",
			StorageKey: sourceID, ByteSize: int64(len(archive)), MediaType: "application/x-tar",
			SchemaVersion: SnapshotSchemaVersion, ExpiresAt: &expiresAt,
		},
	}}
	store := &fakeStore{bodies: map[string][]byte{sourceID: archive}}
	image := "ghcr.io/rinspace/typst-pdf@" + imageDigest
	broker := &fakeBroker{image: image}
	executor, err := New(repository, store, broker, Config{
		SpoolRoot: root, RendererVersion: "renderer-test", PollInterval: time.Millisecond,
		WallTimeout: wallTimeout, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	job := jobpostgres.Job{
		ID: testJobID, ContentKind: "typst", DocumentEngine: "typst", ResourceClass: ResourceClass,
		PriorityClass: "preview", SourceArtifactID: sourceID, ProjectHash: snapshotHash,
		OptionsHash: strings.Repeat("c", 64), RendererVersion: "renderer-test", RequestMetadata: metadata,
		ExpiresAt: now.Add(8 * 24 * time.Hour),
	}
	return executorFixture{executor: executor, repository: repository, store: store, broker: broker, job: job, root: root, image: image, digest: imageDigest, now: now}
}

func zipProjectArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// newExportFixture builds a committed Typst PDF export job whose source is a
// published rin-project-archive/v1 zip rather than a draft session snapshot.
func newExportFixture(t *testing.T, wallTimeout time.Duration, files map[string][]byte, entrypoint string) executorFixture {
	t.Helper()
	root := t.TempDir()
	now := time.Date(2026, 9, 16, 5, 6, 7, 0, time.UTC)
	archive := zipProjectArchive(t, files)
	sourceHash := sha256.Sum256(archive)
	projectHash := hex.EncodeToString(sourceHash[:])
	imageDigest := "sha256:" + strings.Repeat("b", 64)
	metadata, err := json.Marshal(requestMetadata{
		RequestID: "render-0123456789abcdef0123456789abcdef", Entrypoint: entrypoint,
		OutputKind: ExportOutputKind, EnginePolicyID: PolicyID, ImageDigest: imageDigest,
		ControlProjectID: "book:77", SourceCommit: strings.Repeat("a", 40),
		ControlProjectHash: strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(24 * time.Hour)
	sourceID := "render-jobs/v1/source/" + testJobID + "/" + projectHash
	repository := &fakeRepository{artifacts: map[string]jobpostgres.Artifact{
		sourceID: {
			ID: sourceID, SHA256: projectHash, Kind: "source", Visibility: "private",
			StorageKey: sourceID, ByteSize: int64(len(archive)), MediaType: ExportArchiveMediaType,
			SchemaVersion: ExportArchiveSchemaVersion, ExpiresAt: &expiresAt,
		},
	}}
	store := &fakeStore{bodies: map[string][]byte{sourceID: archive}}
	image := "ghcr.io/rinspace/typst-pdf@" + imageDigest
	broker := &fakeBroker{image: image}
	executor, err := New(repository, store, broker, Config{
		SpoolRoot: root, RendererVersion: "renderer-test", PollInterval: time.Millisecond,
		WallTimeout: wallTimeout, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	job := jobpostgres.Job{
		ID: testJobID, ContentKind: "typst", DocumentEngine: "typst", ResourceClass: ResourceClass,
		PriorityClass: "publish", SourceArtifactID: sourceID, ProjectHash: projectHash,
		OptionsHash: strings.Repeat("c", 64), RendererVersion: "renderer-test", RequestMetadata: metadata,
		ExpiresAt: now.Add(8 * 24 * time.Hour),
	}
	return executorFixture{executor: executor, repository: repository, store: store, broker: broker, job: job, root: root, image: image, digest: imageDigest, now: now}
}

func TestExecutorStoresCommittedExportWithProjectIdentity(t *testing.T) {
	binaryAsset := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0xfe}
	files := map[string][]byte{
		"main.typ":         []byte("#set text(size: 12pt)\n= Book\n#include \"chapters/one.typ\"\n"),
		"chapters/one.typ": []byte("== One\n"),
		"assets/logo.png":  binaryAsset,
	}
	fixture := newExportFixture(t, 100*time.Millisecond, files, "main.typ")
	fixture.broker.onStart = func(id string) error {
		workspace := filepath.Join(fixture.root, id, "workspace")
		extracted, err := os.ReadFile(filepath.Join(workspace, "assets", "logo.png"))
		if err != nil || !bytes.Equal(extracted, binaryAsset) {
			return fmt.Errorf("binary project asset was not extracted: %v", err)
		}
		if body, err := os.ReadFile(filepath.Join(workspace, "chapters", "one.typ")); err != nil || !bytes.Equal(body, files["chapters/one.typ"]) {
			return fmt.Errorf("nested Typst chapter was not extracted: %v", err)
		}
		return writeSuccessfulOutput(filepath.Join(fixture.root, id, "output"), false)
	}
	outcome := fixture.executor.Execute(context.Background(), fixture.job)
	if outcome.Failure != nil {
		t.Fatalf("Execute() failure = %#v", outcome.Failure)
	}
	if outcome.Evidence.RendererProjectHash != fixture.job.ProjectHash {
		t.Fatalf("export evidence project hash = %q", outcome.Evidence.RendererProjectHash)
	}
	fixture.store.mu.Lock()
	resultBody := append([]byte(nil), fixture.store.bodies[outcome.ResultReference.ArtifactID]...)
	fixture.store.mu.Unlock()
	var result Result
	if err := json.Unmarshal(resultBody, &result); err != nil || result.Validate() != nil {
		t.Fatalf("stored export result is invalid: %v, %#v", err, result)
	}
	if result.OutputKind != ExportOutputKind || result.PriorityClass != "publish" ||
		result.ControlProjectID != "book:77" || result.SourceCommit != strings.Repeat("a", 40) ||
		result.ControlProjectHash != strings.Repeat("d", 64) || result.EnginePolicyID != PolicyID ||
		result.ImageDigest != fixture.digest {
		t.Fatalf("export result identity = %#v", result)
	}
	if result.SessionID != "" || result.SnapshotHash != "" || result.DraftRevision != nil {
		t.Fatalf("export result carried a session identity = %#v", result)
	}
	if result.IdempotencyKey != ExportIdempotencyKey("book:77", strings.Repeat("a", 40), "main.typ", PolicyID, fixture.digest) {
		t.Fatalf("export idempotency key = %q", result.IdempotencyKey)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, fixture.job.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("export spool was not cleaned: %v", err)
	}
}

func TestExecutorRejectsMixedOrUnmatchedExportJobs(t *testing.T) {
	t.Run("preview job with export metadata", func(t *testing.T) {
		fixture := newExecutorFixture(t, 100*time.Millisecond)
		fixture.job.RequestMetadata = fixture.metadata(t, func(metadata *requestMetadata) {
			metadata.OutputKind = ExportOutputKind
			metadata.ControlProjectID = "book:77"
			metadata.SourceCommit = strings.Repeat("a", 40)
			metadata.ControlProjectHash = strings.Repeat("d", 64)
		})
		outcome := fixture.executor.Execute(context.Background(), fixture.job)
		if outcome.Failure == nil || outcome.Failure.Code != "invalid_typst_job" || fixture.broker.startCalls != 0 {
			t.Fatalf("mixed preview/export job = %#v, starts=%d", outcome.Failure, fixture.broker.startCalls)
		}
	})

	t.Run("export archive hash does not match the job", func(t *testing.T) {
		fixture := newExportFixture(t, 100*time.Millisecond, map[string][]byte{"main.typ": []byte("= Book\n")}, "main.typ")
		fixture.job.ProjectHash = strings.Repeat("f", 64)
		outcome := fixture.executor.Execute(context.Background(), fixture.job)
		if outcome.Failure == nil || outcome.Failure.Code != "invalid_typst_project_archive" || fixture.broker.startCalls != 0 {
			t.Fatalf("unmatched export archive = %#v, starts=%d", outcome.Failure, fixture.broker.startCalls)
		}
	})

	t.Run("preview snapshot cannot satisfy an export", func(t *testing.T) {
		fixture := newExecutorFixture(t, 100*time.Millisecond)
		fixture.job.PriorityClass = "publish"
		fixture.job.RequestMetadata = fixture.metadata(t, func(metadata *requestMetadata) {
			metadata.OutputKind = ExportOutputKind
			metadata.SnapshotHash = ""
			metadata.SessionID = ""
			metadata.DraftRevision = 0
			metadata.PreviewContractVersion = ""
			metadata.ControlProjectID = "book:77"
			metadata.SourceCommit = strings.Repeat("a", 40)
			metadata.ControlProjectHash = strings.Repeat("d", 64)
		})
		outcome := fixture.executor.Execute(context.Background(), fixture.job)
		if outcome.Failure == nil || outcome.Failure.Code != "invalid_typst_project_archive" || fixture.broker.startCalls != 0 {
			t.Fatalf("snapshot used as export source = %#v, starts=%d", outcome.Failure, fixture.broker.startCalls)
		}
	})

	t.Run("export entrypoint missing from archive", func(t *testing.T) {
		fixture := newExportFixture(t, 100*time.Millisecond, map[string][]byte{"main.typ": []byte("= Book\n")}, "book.typ")
		outcome := fixture.executor.Execute(context.Background(), fixture.job)
		if outcome.Failure == nil || outcome.Failure.Code != "invalid_typst_project_archive" || fixture.broker.startCalls != 0 {
			t.Fatalf("export without entrypoint = %#v, starts=%d", outcome.Failure, fixture.broker.startCalls)
		}
	})

	t.Run("export archive escapes the workspace", func(t *testing.T) {
		fixture := newExportFixture(t, 100*time.Millisecond, map[string][]byte{
			"main.typ": []byte("= Book\n"), "../escape.typ": []byte("= Escape\n"),
		}, "main.typ")
		outcome := fixture.executor.Execute(context.Background(), fixture.job)
		if outcome.Failure == nil || outcome.Failure.Code != "invalid_typst_project_archive" || fixture.broker.startCalls != 0 {
			t.Fatalf("escaping export archive = %#v, starts=%d", outcome.Failure, fixture.broker.startCalls)
		}
	})
}

func (fixture executorFixture) metadata(t *testing.T, mutate func(*requestMetadata)) []byte {
	t.Helper()
	var metadata requestMetadata
	if err := json.Unmarshal(fixture.job.RequestMetadata, &metadata); err != nil {
		t.Fatal(err)
	}
	mutate(&metadata)
	body, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestExecutorStoresSuccessfulPreviewAndCleansRuntime(t *testing.T) {
	fixture := newExecutorFixture(t, 100*time.Millisecond)
	fixture.broker.onStart = func(id string) error { return writeSuccessfulOutput(filepath.Join(fixture.root, id, "output"), false) }
	outcome := fixture.executor.Execute(context.Background(), fixture.job)
	if outcome.Failure != nil {
		t.Fatalf("Execute() failure = %#v", outcome.Failure)
	}
	if outcome.ResultArtifact.Kind != "result" || outcome.Evidence.RendererProjectHash != fixture.job.ProjectHash || len(outcome.Evidence.ResultHash) != 64 {
		t.Fatalf("Execute() outcome = %#v", outcome)
	}
	fixture.store.mu.Lock()
	resultBody := append([]byte(nil), fixture.store.bodies[outcome.ResultReference.ArtifactID]...)
	fixture.store.mu.Unlock()
	var result Result
	if err := json.Unmarshal(resultBody, &result); err != nil || result.Validate() != nil {
		t.Fatalf("stored result is invalid: %v, %#v", err, result)
	}
	if len(result.Artifacts) != 2 || result.Artifacts[0].Path != ".rinspace/typst/main.pdf" ||
		result.Artifacts[1].Path != ".rinspace/typst/main.log" {
		t.Fatalf("stored Typst result = %#v", result)
	}
	if result.Versions.Typst != TypstVersion || result.RenderProfileID != RenderProfileID(fixture.digest, "renderer-test") {
		t.Fatalf("Typst result versions = %#v", result.Versions)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Severity != "warning" ||
		result.Diagnostics[0].Source == nil || result.Diagnostics[0].Source["path"] != "main.typ" ||
		result.Diagnostics[0].Source["line"] != "3" || result.Diagnostics[0].Source["column"] != "5" {
		t.Fatalf("Typst diagnostics = %#v", result.Diagnostics)
	}
	for _, artifact := range result.Artifacts {
		expiresAt, _ := time.Parse(time.RFC3339, artifact.ExpiresAt)
		if expiresAt.Sub(fixture.now) != 24*time.Hour || !strings.HasPrefix(artifact.ArtifactID, "render-jobs/v1/typst/"+testJobID+"/") {
			t.Fatalf("artifact identity or TTL = %#v", artifact)
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.root, fixture.job.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Typst spool was not cleaned: %v", err)
	}
	if fixture.broker.stopCalls == 0 || fixture.broker.removeCalls == 0 {
		t.Fatalf("broker cleanup calls: stop=%d remove=%d", fixture.broker.stopCalls, fixture.broker.removeCalls)
	}
}

// T26 recovery coverage: a source snapshot whose TTL elapsed, whose visibility
// was narrowed away, or that disappeared entirely must fail retryably before any
// container is created, so a stale or re-privileged artifact can never be
// compiled into a fresh result.
func TestExecutorRejectsExpiredOrDeprivilegedSourceSnapshot(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(fixture *executorFixture)
	}{
		{
			name: "expired snapshot",
			mutate: func(fixture *executorFixture) {
				artifact := fixture.repository.artifacts[fixture.job.SourceArtifactID]
				expired := fixture.now.Add(-time.Minute)
				artifact.ExpiresAt = &expired
				fixture.repository.artifacts[fixture.job.SourceArtifactID] = artifact
			},
		},
		{
			name: "visibility revoked",
			mutate: func(fixture *executorFixture) {
				artifact := fixture.repository.artifacts[fixture.job.SourceArtifactID]
				artifact.Visibility = "public"
				fixture.repository.artifacts[fixture.job.SourceArtifactID] = artifact
			},
		},
		{
			name: "missing snapshot",
			mutate: func(fixture *executorFixture) {
				delete(fixture.repository.artifacts, fixture.job.SourceArtifactID)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newExecutorFixture(t, 100*time.Millisecond)
			testCase.mutate(&fixture)
			outcome := fixture.executor.Execute(context.Background(), fixture.job)
			if outcome.Failure == nil || outcome.Failure.Code != "typst_source_unavailable" ||
				outcome.Failure.Status != 503 || !outcome.Failure.Retryable {
				t.Fatalf("Execute() failure = %#v", outcome.Failure)
			}
			if fixture.broker.startCalls != 0 || fixture.broker.inspectCalls != 0 {
				t.Fatalf("broker was used for an unusable snapshot: start=%d inspect=%d",
					fixture.broker.startCalls, fixture.broker.inspectCalls)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, fixture.job.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected snapshot created a spool: %v", err)
			}
		})
	}
}

func TestExecutorClassifiesCompileErrorAndOOMWithArtifacts(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		exitCode  int
		oom       bool
		wantCode  string
		retryable bool
	}{
		{name: "compile-error", exitCode: 12, wantCode: "typst_compile_failed"},
		{name: "oom", exitCode: 137, oom: true, wantCode: "typst_compile_oom", retryable: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newExecutorFixture(t, 100*time.Millisecond)
			fixture.broker.onStart = func(id string) error {
				return os.WriteFile(filepath.Join(fixture.root, id, "output", "compile.log"),
					[]byte("error: unknown variable: foo\n┌─ /workspace/main.typ:7:1\n"), 0o600)
			}
			fixture.broker.inspect = func(int) BrokerInspection {
				return BrokerInspection{ID: testJobID, Profile: brokerProfile, State: "exited", ExitCode: testCase.exitCode, OOMKilled: testCase.oom, Image: fixture.broker.image}
			}
			outcome := fixture.executor.Execute(context.Background(), fixture.job)
			if outcome.Failure == nil || outcome.Failure.Code != testCase.wantCode || outcome.Failure.Retryable != testCase.retryable ||
				outcome.Failure.ExitCode == nil || *outcome.Failure.ExitCode != testCase.exitCode ||
				len(outcome.Failure.Diagnostics) != 1 || len(outcome.Failure.Artifacts) != 1 ||
				outcome.Failure.Artifacts[0].ArtifactID[:len("render-jobs/v1/typst/")] != "render-jobs/v1/typst/" {
				t.Fatalf("Execute() failure = %#v", outcome.Failure)
			}
		})
	}
}

func TestExecutorStopsTimeoutAndCancellation(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		cancel   bool
		wantCode string
	}{
		{name: "timeout", wantCode: "typst_compile_timeout"},
		{name: "cancel", cancel: true, wantCode: "typst_worker_interrupted"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newExecutorFixture(t, 15*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fixture.broker.onStart = func(id string) error {
				if err := os.WriteFile(filepath.Join(fixture.root, id, "output", "compile.log"), []byte("warning: still compiling\n"), 0o600); err != nil {
					return err
				}
				if testCase.cancel {
					cancel()
				}
				return nil
			}
			fixture.broker.inspect = func(int) BrokerInspection {
				return BrokerInspection{ID: testJobID, Profile: brokerProfile, State: "running", Running: true, Image: fixture.broker.image}
			}
			outcome := fixture.executor.Execute(ctx, fixture.job)
			if outcome.Failure == nil || outcome.Failure.Code != testCase.wantCode || !outcome.Failure.Retryable ||
				len(outcome.Failure.Artifacts) != 1 || fixture.broker.stopCalls == 0 {
				t.Fatalf("Execute() timeout/cancel = %#v, stops=%d", outcome.Failure, fixture.broker.stopCalls)
			}
			if outcome.Failure.Status != 504 && outcome.Failure.Status != 503 {
				t.Fatalf("Execute() timeout/cancel status = %d", outcome.Failure.Status)
			}
		})
	}
}

func TestExecutorRecoversStaleSpoolBeforeStarting(t *testing.T) {
	fixture := newExecutorFixture(t, 100*time.Millisecond)
	stale := filepath.Join(fixture.root, fixture.job.ID)
	if err := os.MkdirAll(filepath.Join(stale, "output"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "stale-marker"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.broker.onStart = func(id string) error {
		if _, err := os.Stat(filepath.Join(fixture.root, id, "stale-marker")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("stale spool survived recovery")
		}
		return writeSuccessfulOutput(filepath.Join(fixture.root, id, "output"), false)
	}
	outcome := fixture.executor.Execute(context.Background(), fixture.job)
	if outcome.Failure != nil || fixture.broker.removeCalls < 2 {
		t.Fatalf("Execute() recovery = %#v, removes=%d", outcome.Failure, fixture.broker.removeCalls)
	}
}

func TestExecutorRejectsUnsafePDFAndArtifactQuota(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		prepare  func(string) error
		wantCode string
	}{
		{name: "unsafe-pdf", prepare: func(directory string) error { return writeSuccessfulOutput(directory, true) }, wantCode: "unsafe_typst_pdf_quarantined"},
		{name: "quota", prepare: func(directory string) error {
			if err := writeSuccessfulOutput(directory, false); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(directory, "unexpected.bin"), nil, 0o600); err != nil {
				return err
			}
			return os.Truncate(filepath.Join(directory, "unexpected.bin"), (48<<20)+1)
		}, wantCode: "typst_artifact_quota_exceeded"},
		{name: "missing-log", prepare: func(directory string) error {
			return os.WriteFile(filepath.Join(directory, "main.pdf"), validOnePagePDF(false), 0o600)
		}, wantCode: "invalid_typst_artifact"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newExecutorFixture(t, 100*time.Millisecond)
			fixture.broker.onStart = func(id string) error { return testCase.prepare(filepath.Join(fixture.root, id, "output")) }
			outcome := fixture.executor.Execute(context.Background(), fixture.job)
			if outcome.Failure == nil || outcome.Failure.Code != testCase.wantCode {
				t.Fatalf("Execute() failure = %#v", outcome.Failure)
			}
		})
	}
}

func TestExecutorRejectsMismatchedCompilerImage(t *testing.T) {
	fixture := newExecutorFixture(t, 100*time.Millisecond)
	fixture.broker.image = "ghcr.io/rinspace/typst-pdf@sha256:" + strings.Repeat("d", 64)
	fixture.broker.onStart = func(id string) error { return writeSuccessfulOutput(filepath.Join(fixture.root, id, "output"), false) }
	outcome := fixture.executor.Execute(context.Background(), fixture.job)
	if outcome.Failure == nil || outcome.Failure.Code != "typst_image_mismatch" || outcome.Failure.Retryable {
		t.Fatalf("Execute() image mismatch = %#v", outcome.Failure)
	}
}

func TestExecutorRejectsInvalidTypstJobContract(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*executorFixture)
	}{
		{name: "content-kind", mutate: func(fixture *executorFixture) { fixture.job.ContentKind = "latex" }},
		{name: "resource-class", mutate: func(fixture *executorFixture) { fixture.job.ResourceClass = "latex-pdf" }},
		{name: "renderer-version", mutate: func(fixture *executorFixture) { fixture.job.RendererVersion = "other" }},
		{name: "project-hash", mutate: func(fixture *executorFixture) { fixture.job.ProjectHash = strings.Repeat("a", 64) }},
		{name: "options-hash", mutate: func(fixture *executorFixture) { fixture.job.OptionsHash = "" }},
		{name: "latex-contract-version", mutate: func(fixture *executorFixture) {
			fixture.job.RequestMetadata = fixture.metadata(t, func(metadata *requestMetadata) {
				metadata.PreviewContractVersion = "rin-latex-pdf-preview/v1"
			})
		}},
		{name: "export-output-kind", mutate: func(fixture *executorFixture) {
			fixture.job.RequestMetadata = fixture.metadata(t, func(metadata *requestMetadata) {
				metadata.OutputKind = ExportOutputKind
			})
		}},
		{name: "entrypoint-extension", mutate: func(fixture *executorFixture) {
			fixture.job.RequestMetadata = fixture.metadata(t, func(metadata *requestMetadata) { metadata.Entrypoint = "main.tex" })
		}},
		{name: "policy", mutate: func(fixture *executorFixture) {
			fixture.job.RequestMetadata = fixture.metadata(t, func(metadata *requestMetadata) { metadata.EnginePolicyID = "rinspace-latex-pdf-safe-v1" })
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newExecutorFixture(t, 100*time.Millisecond)
			testCase.mutate(&fixture)
			fixture.broker.onStart = func(id string) error { return writeSuccessfulOutput(filepath.Join(fixture.root, id, "output"), false) }
			outcome := fixture.executor.Execute(context.Background(), fixture.job)
			if outcome.Failure == nil || outcome.Failure.Code != "invalid_typst_job" || outcome.Failure.Retryable || fixture.broker.startCalls != 0 {
				t.Fatalf("Execute() invalid contract = %#v, starts=%d", outcome.Failure, fixture.broker.startCalls)
			}
		})
	}
}

func TestExecutorRejectsSnapshotMismatchAndMissingEntrypoint(t *testing.T) {
	t.Run("hash-mismatch", func(t *testing.T) {
		fixture := newExecutorFixture(t, 100*time.Millisecond)
		fixture.store.mu.Lock()
		body := append([]byte(nil), fixture.store.bodies[fixture.job.SourceArtifactID]...)
		fixture.store.mu.Unlock()
		body[len(body)/2] ^= 0xff
		fixture.store.mu.Lock()
		fixture.store.bodies[fixture.job.SourceArtifactID] = body
		fixture.store.mu.Unlock()
		outcome := fixture.executor.Execute(context.Background(), fixture.job)
		if outcome.Failure == nil || outcome.Failure.Code != "invalid_typst_snapshot" || outcome.Failure.Retryable {
			t.Fatalf("Execute() hash mismatch = %#v", outcome.Failure)
		}
	})
	t.Run("absent-entrypoint", func(t *testing.T) {
		fixture := newExecutorFixtureWith(t, 100*time.Millisecond,
			map[string][]byte{"docs/main.typ": []byte("= Documented\n")}, "main.typ")
		outcome := fixture.executor.Execute(context.Background(), fixture.job)
		if outcome.Failure == nil || outcome.Failure.Code != "invalid_typst_snapshot" || fixture.broker.startCalls != 0 {
			t.Fatalf("Execute() absent entrypoint = %#v, starts=%d", outcome.Failure, fixture.broker.startCalls)
		}
	})
}

func TestParseTypstDiagnosticsDedupesAndBounds(t *testing.T) {
	if diagnostics := parseTypstDiagnostics([]byte("error: 重複\n┌─ /workspace/main.typ:1:1\nerror: 重複\n")); len(diagnostics) != 1 {
		t.Fatalf("duplicate Typst diagnostics were kept: %#v", diagnostics)
	}
	var body strings.Builder
	for index := 0; index < 200; index++ {
		fmt.Fprintf(&body, "warning: diagnostic %d\n", index)
	}
	diagnostics := parseTypstDiagnostics([]byte(body.String()))
	if len(diagnostics) != 80 || diagnostics[79].Message != "diagnostic 79" {
		t.Fatalf("Typst diagnostics were not bounded: %d", len(diagnostics))
	}
	if plain := parseTypstDiagnostics([]byte("= Typst Title\n")); len(plain) != 0 {
		t.Fatalf("non-diagnostic lines were parsed: %#v", plain)
	}
	if escape := parseTypstDiagnostics([]byte("error: \x1b[31mred\x07\n")); len(escape) != 1 || strings.ContainsAny(escape[0].Message, "\x1b\x07") {
		t.Fatalf("control characters survived diagnostic parsing: %#v", escape)
	}
}

func TestSnapshotArchiveRejectsTraversalAndTrailingData(t *testing.T) {
	unsafeArchive, _ := snapshotArchive(t, map[string][]byte{"../main.typ": []byte("bad")})
	if _, _, err := decodeSnapshotArchive(unsafeArchive); err == nil {
		t.Fatal("snapshot traversal was accepted")
	}
	absoluteArchive, _ := snapshotArchive(t, map[string][]byte{"/etc/passwd": []byte("bad")})
	if _, _, err := decodeSnapshotArchive(absoluteArchive); err == nil {
		t.Fatal("absolute snapshot path was accepted")
	}
	valid, _ := snapshotArchive(t, map[string][]byte{"main.typ": []byte("ok")})
	valid = append(valid, append([]byte{1}, make([]byte, 511)...)...)
	valid = append(valid, make([]byte, 1024)...)
	if _, _, err := decodeSnapshotArchive(valid); err == nil {
		t.Fatal("snapshot trailing data was accepted")
	}
}

func TestPrepareSpoolSealsWorkspaceAndRejectsUnsafeInput(t *testing.T) {
	root := t.TempDir()
	archive, hash := snapshotArchive(t, map[string][]byte{"main.typ": []byte("= Title\n"), "chapters/one.typ": []byte("= One\n")})
	spool, err := prepareSpool(root, testJobID, "main.typ", archive, hash)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(spool.workspace)
	if err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("Typst workspace was not sealed read-only: %v, %v", info, err)
	}
	body, err := os.ReadFile(filepath.Join(spool.workspace, "chapters", "one.typ"))
	if err != nil || string(body) != "= One\n" {
		t.Fatalf("Typst snapshot content = %q, %v", body, err)
	}
	if _, err := prepareSpool(root, "not a job id", "main.typ", archive, hash); err == nil {
		t.Fatal("invalid job id was accepted")
	}
	if _, err := prepareSpool(root, testJobID, "missing.typ", archive, hash); err == nil {
		t.Fatal("absent Typst entrypoint was accepted")
	}
	if _, err := prepareSpool(root, testJobID, "main.typ", archive, strings.Repeat("a", 64)); err == nil {
		t.Fatal("mismatched Typst snapshot hash was accepted")
	}
	if err := cleanupSpool(root, testJobID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spool.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Typst spool survived cleanup: %v", err)
	}
	if err := cleanupSpool(root, "../escape"); err == nil {
		t.Fatal("unsafe Typst cleanup path was accepted")
	}
}

func snapshotArchive(t *testing.T, files map[string][]byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	for name, value := range files {
		header := &tar.Header{Name: name, Mode: 0o444, Size: int64(len(value)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	// Tests use one or two files except for archive-specific rejection cases, so use the
	// decoder's contract hash when possible and a placeholder for deliberately invalid paths.
	if decoded, hash, err := decodeSnapshotArchive(body.Bytes()); err == nil && len(decoded) == len(files) {
		return body.Bytes(), hash
	}
	return body.Bytes(), strings.Repeat("0", 64)
}

func writeSuccessfulOutput(directory string, unsafe bool) error {
	files := map[string][]byte{
		"main.pdf":    validOnePagePDF(unsafe),
		"compile.log": []byte("warning: unknown font family: Noto\n┌─ /workspace/main.typ:3:5\n"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(directory, name), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func validOnePagePDF(unsafe bool) []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	}
	if unsafe {
		objects[0] = "<< /Type /Catalog /Pages 2 0 R /OpenAction 5 0 R >>"
		objects = append(objects, "<< /S /JavaScript /JS (app.alert\\(1\\)) >>")
	}
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		offsets[index+1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := body.Len()
	fmt.Fprintf(&body, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for index := 1; index <= len(objects); index++ {
		fmt.Fprintf(&body, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&body, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return body.Bytes()
}

func TestRenderProfileIDVariesWithProfileInputs(t *testing.T) {
	imageDigest := "sha256:" + strings.Repeat("a", 64)
	base := RenderProfileID(imageDigest, "renderer-test")
	if base != RenderProfileID(imageDigest, "renderer-test") {
		t.Fatal("render profile id is not stable for equal inputs")
	}
	if !strings.HasPrefix(base, renderProfilePrefix+".") {
		t.Fatalf("render profile id %q lost its prefix", base)
	}
	if RenderProfileID("sha256:"+strings.Repeat("b", 64), "renderer-test") == base {
		t.Fatal("a compiler image change (fonts/packages) did not change the render profile id")
	}
	if RenderProfileID(imageDigest, "renderer-next") == base {
		t.Fatal("a Renderer/adapter version change did not change the render profile id")
	}
}
