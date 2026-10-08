package pdfexecutor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	return BrokerInspection{ID: id, Profile: "pdf-v1", State: "running", Running: true, Image: broker.image}, nil
}

func (broker *fakeBroker) Inspect(_ context.Context, id string) (BrokerInspection, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.inspectCalls++
	if broker.inspect != nil {
		return broker.inspect(broker.inspectCalls), nil
	}
	return BrokerInspection{ID: id, Profile: "pdf-v1", State: "exited", Image: broker.image}, nil
}

func (broker *fakeBroker) Stop(_ context.Context, id string) (BrokerInspection, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.stopCalls++
	return BrokerInspection{ID: id, Profile: "pdf-v1", State: "exited", Image: broker.image}, nil
}

func (broker *fakeBroker) Remove(_ context.Context, id string) (BrokerInspection, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.removeCalls++
	if broker.removeError != nil {
		return BrokerInspection{}, broker.removeError
	}
	return BrokerInspection{ID: id, Profile: "pdf-v1", State: "missing"}, nil
}

type executorFixture struct {
	executor   *Executor
	repository *fakeRepository
	store      *fakeStore
	broker     *fakeBroker
	job        jobpostgres.Job
	root       string
	now        time.Time
}

func newExecutorFixture(t *testing.T, wallTimeout time.Duration) executorFixture {
	t.Helper()
	root := t.TempDir()
	now := time.Date(2026, 8, 29, 3, 4, 5, 0, time.UTC)
	archive, snapshotHash := snapshotArchive(t, map[string][]byte{"main.tex": []byte("\\documentclass{article}\\begin{document}ok\\end{document}\n")})
	sourceHash := sha256.Sum256(archive)
	imageDigest := "sha256:" + strings.Repeat("b", 64)
	metadata, err := json.Marshal(requestMetadata{
		RequestID: "render-0123456789abcdef0123456789abcdef", Entrypoint: "main.tex",
		OutputKind: "latex-pdf-preview", SnapshotHash: snapshotHash, SessionID: "session-018fcafe",
		DraftRevision: 42, EnginePolicyID: PolicyID, ImageDigest: imageDigest, PreviewContractVersion: PreviewSchemaVersion,
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
	broker := &fakeBroker{image: "ghcr.io/rinspace/latex-pdf@" + imageDigest}
	executor, err := New(repository, store, broker, Config{
		SpoolRoot: root, RendererVersion: "renderer-test", PollInterval: time.Millisecond,
		WallTimeout: wallTimeout, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	job := jobpostgres.Job{
		ID: testJobID, ContentKind: "latex", DocumentEngine: "latexmk", ResourceClass: "latex-pdf",
		PriorityClass: "preview", SourceArtifactID: sourceID, ProjectHash: snapshotHash,
		OptionsHash: strings.Repeat("c", 64), RendererVersion: "renderer-test", RequestMetadata: metadata,
		ExpiresAt: now.Add(8 * 24 * time.Hour),
	}
	return executorFixture{executor: executor, repository: repository, store: store, broker: broker, job: job, root: root, now: now}
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
	var result PreviewResult
	if err := json.Unmarshal(resultBody, &result); err != nil || result.Validate() != nil {
		t.Fatalf("stored result is invalid: %v, %#v", err, result)
	}
	if len(result.Artifacts) != 5 || result.Artifacts[0].Path != ".rinspace/main.pdf" || len(result.Diagnostics) != 1 {
		t.Fatalf("stored PDF result = %#v", result)
	}
	for _, artifact := range result.Artifacts {
		expiresAt, _ := time.Parse(time.RFC3339, artifact.ExpiresAt)
		if expiresAt.Sub(fixture.now) != 24*time.Hour || !strings.HasPrefix(artifact.ArtifactID, "render-jobs/v1/debug/"+testJobID+"/") {
			t.Fatalf("artifact identity or TTL = %#v", artifact)
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.root, fixture.job.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PDF spool was not cleaned: %v", err)
	}
	if fixture.broker.stopCalls == 0 || fixture.broker.removeCalls == 0 {
		t.Fatalf("broker cleanup calls: stop=%d remove=%d", fixture.broker.stopCalls, fixture.broker.removeCalls)
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
		{name: "compile-error", exitCode: 12, wantCode: "pdf_compile_failed"},
		{name: "oom", exitCode: 137, oom: true, wantCode: "pdf_compile_oom", retryable: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newExecutorFixture(t, 100*time.Millisecond)
			fixture.broker.onStart = func(id string) error {
				return os.WriteFile(filepath.Join(fixture.root, id, "output", "compile.log"), []byte("main.tex:7: Undefined control sequence\n"), 0o600)
			}
			fixture.broker.inspect = func(int) BrokerInspection {
				return BrokerInspection{ID: testJobID, Profile: "pdf-v1", State: "exited", ExitCode: testCase.exitCode, OOMKilled: testCase.oom, Image: fixture.broker.image}
			}
			outcome := fixture.executor.Execute(context.Background(), fixture.job)
			if outcome.Failure == nil || outcome.Failure.Code != testCase.wantCode || outcome.Failure.Retryable != testCase.retryable ||
				outcome.Failure.ExitCode == nil || *outcome.Failure.ExitCode != testCase.exitCode ||
				len(outcome.Failure.Diagnostics) != 1 || len(outcome.Failure.Artifacts) != 1 {
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
		{name: "timeout", wantCode: "pdf_compile_timeout"},
		{name: "cancel", cancel: true, wantCode: "pdf_worker_interrupted"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newExecutorFixture(t, 15*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fixture.broker.onStart = func(id string) error {
				if err := os.WriteFile(filepath.Join(fixture.root, id, "output", "compile.log"), []byte("LaTeX Warning: still compiling\n"), 0o600); err != nil {
					return err
				}
				if testCase.cancel {
					cancel()
				}
				return nil
			}
			fixture.broker.inspect = func(int) BrokerInspection {
				return BrokerInspection{ID: testJobID, Profile: "pdf-v1", State: "running", Running: true, Image: fixture.broker.image}
			}
			outcome := fixture.executor.Execute(ctx, fixture.job)
			if outcome.Failure == nil || outcome.Failure.Code != testCase.wantCode || !outcome.Failure.Retryable ||
				len(outcome.Failure.Artifacts) != 1 || fixture.broker.stopCalls == 0 {
				t.Fatalf("Execute() timeout/cancel = %#v, stops=%d", outcome.Failure, fixture.broker.stopCalls)
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
		{name: "unsafe-pdf", prepare: func(directory string) error { return writeSuccessfulOutput(directory, true) }, wantCode: "unsafe_pdf_quarantined"},
		{name: "quota", prepare: func(directory string) error {
			if err := writeSuccessfulOutput(directory, false); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(directory, "unexpected.bin"), nil, 0o600); err != nil {
				return err
			}
			return os.Truncate(filepath.Join(directory, "unexpected.bin"), (48<<20)+1)
		}, wantCode: "pdf_artifact_quota_exceeded"},
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

func TestSnapshotArchiveRejectsTraversalAndTrailingData(t *testing.T) {
	unsafeArchive, _ := snapshotArchive(t, map[string][]byte{"../main.tex": []byte("bad")})
	if _, _, err := decodeSnapshotArchive(unsafeArchive); err == nil {
		t.Fatal("snapshot traversal was accepted")
	}
	valid, _ := snapshotArchive(t, map[string][]byte{"main.tex": []byte("ok")})
	valid = append(valid, append([]byte{1}, make([]byte, 511)...)...)
	valid = append(valid, make([]byte, 1024)...)
	if _, _, err := decodeSnapshotArchive(valid); err == nil {
		t.Fatal("snapshot trailing data was accepted")
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
	// Tests use one file except for archive-specific rejection cases, so use the decoder's
	// contract hash when possible and a placeholder for deliberately invalid paths.
	if decoded, hash, err := decodeSnapshotArchive(body.Bytes()); err == nil && len(decoded) == len(files) {
		return body.Bytes(), hash
	}
	return body.Bytes(), strings.Repeat("0", 64)
}

func writeSuccessfulOutput(directory string, unsafe bool) error {
	pdf := validOnePagePDF(unsafe)
	var synctex bytes.Buffer
	gzipWriter := gzip.NewWriter(&synctex)
	if _, err := gzipWriter.Write([]byte("SyncTeX Version:1\nInput:1:/workspace/main.tex\n")); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}
	files := map[string][]byte{
		"main.pdf": pdf, "main.synctex.gz": synctex.Bytes(),
		"compile.log": []byte("LaTeX Warning: test warning\n"), "main.aux": []byte("\\relax\n"),
		"main.fls": []byte("INPUT /workspace/main.tex\nOUTPUT /output/main.pdf\n"),
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
