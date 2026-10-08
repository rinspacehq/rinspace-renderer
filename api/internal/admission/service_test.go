package admission

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/lifecycle"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

func TestAdmitStoresSourceBeforeQueuedCommit(t *testing.T) {
	source := []byte("source archive")
	repository := &fakeRepository{}
	artifacts := &fakeArtifactStore{}
	var order []string
	repository.reserve = func(input jobpostgres.AdmissionReservationInput, _ jobpostgres.AdmissionLimits) (jobpostgres.AdmissionReservation, error) {
		order = append(order, "reserve")
		if input.Workload.FeatureStage != "admission" || !strings.Contains(input.Workload.ProfileKey, "/latex/latexml/unknown/") {
			t.Fatalf("reserved admission workload = %#v", input.Workload)
		}
		job := input.Job
		job.State = "uploading"
		job.DeclaredSourceBytes = input.DeclaredSourceBytes
		job.ReservationExpiresAt = &input.ReservationExpiresAt
		return jobpostgres.AdmissionReservation{Job: job, Uploading: true}, nil
	}
	artifacts.putPrivate = func(write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
		order = append(order, "source")
		if string(write.Body) != string(source) || !strings.HasPrefix(write.ArtifactID, "render-jobs/v1/source/") {
			t.Fatalf("artifact write = %#v", write)
		}
		return artifactReference(write, "private"), nil
	}
	repository.commit = func(input jobpostgres.CommitAdmissionInput) (jobpostgres.Job, error) {
		order = append(order, "queued")
		if len(order) != 3 || order[1] != "source" {
			t.Fatalf("queued before source storage: %v", order)
		}
		if input.Workload.FeatureStage != "admission" || !strings.Contains(string(input.Workload.Features), `"fileCount":1`) {
			t.Fatalf("committed admission workload = %#v", input.Workload)
		}
		return jobpostgres.Job{ID: input.JobID, State: "queued", SourceArtifactID: input.Artifact.ID}, nil
	}
	service := newTestService(t, repository, artifacts, nil)
	request := testRequest(source)
	result, err := service.Admit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Job.State != "queued" || result.Reused {
		t.Fatalf("admission result = %#v", result)
	}
	if strings.Join(order, ",") != "reserve,source,queued" {
		t.Fatalf("admission order = %v", order)
	}
	if repository.abortCalls != 0 || artifacts.deleteCalls != 0 {
		t.Fatalf("success cleanup calls = abort %d delete %d", repository.abortCalls, artifacts.deleteCalls)
	}
}

func TestAdmitRejectsBeforeReadingSource(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*fakeRepository, *Config) Readiness
		wantCode   string
		wantStatus int
	}{
		{
			name: "not ready",
			configure: func(_ *fakeRepository, _ *Config) Readiness {
				return ReadinessFunc(func(context.Context) error { return errors.New("worker unavailable") })
			},
			wantCode: CodeNotReady, wantStatus: 503,
		},
		{
			name: "principal quota",
			configure: func(repository *fakeRepository, _ *Config) Readiness {
				repository.reserveErr = jobpostgres.ErrPrincipalQuota
				return readyReadiness()
			},
			wantCode: CodePrincipalQuota, wantStatus: 429,
		},
		{
			name: "queue capacity",
			configure: func(repository *fakeRepository, _ *Config) Readiness {
				repository.reserveErr = jobpostgres.ErrGlobalCapacity
				return readyReadiness()
			},
			wantCode: CodeQueueCapacity, wantStatus: 503,
		},
		{
			name: "storage capacity",
			configure: func(repository *fakeRepository, _ *Config) Readiness {
				repository.reserveErr = jobpostgres.ErrStorageCapacity
				return readyReadiness()
			},
			wantCode: CodeStorageCapacity, wantStatus: 503,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeRepository{}
			artifacts := &fakeArtifactStore{}
			config := testConfig()
			readiness := test.configure(repository, &config)
			service, err := New(repository, artifacts, readiness, testPriorities(), config)
			if err != nil {
				t.Fatal(err)
			}
			request := testRequest([]byte("unused"))
			request.Source = panicReader{}
			_, err = service.Admit(context.Background(), request)
			assertAdmissionError(t, err, test.wantStatus, test.wantCode, true)
			if artifacts.putCalls != 0 {
				t.Fatalf("artifact writes = %d", artifacts.putCalls)
			}
		})
	}
}

func TestAdmitRejectsUntrustedPriorityBeforeReadingSource(t *testing.T) {
	repository := &fakeRepository{}
	artifacts := &fakeArtifactStore{}
	priorities, err := NewTrustedPriorityPolicy("rebuild", nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(repository, artifacts, readyReadiness(), priorities, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest([]byte("unused"))
	request.Source = panicReader{}
	_, err = service.Admit(context.Background(), request)
	assertAdmissionError(t, err, 403, CodePriorityForbidden, false)
	if repository.reserveCalls != 0 || artifacts.putCalls != 0 {
		t.Fatalf("untrusted priority caused side effects: reserve %d put %d", repository.reserveCalls, artifacts.putCalls)
	}
}

func TestPreviewRequestHashIgnoresOnlyVolatileDraftMetadata(t *testing.T) {
	request := testRequest([]byte("preview"))
	request.ResourceClass = "latex-pdf"
	request.RequestMetadata = json.RawMessage(`{"outputKind":"latex-pdf-preview","draftRevision":41}`)
	first := requestHash(request, "preview")
	request.RequestMetadata = json.RawMessage(`{"outputKind":"latex-pdf-preview","draftRevision":42}`)
	if second := requestHash(request, "preview"); second != first {
		t.Fatalf("unchanged preview content changed request hash: %s != %s", first, second)
	}
	request.ProjectHash = strings.Repeat("f", 64)
	if changed := requestHash(request, "preview"); changed == first {
		t.Fatal("changed preview snapshot reused request hash")
	}
}

func TestAdmitStopsBeforeReadingSourceDuringShutdown(t *testing.T) {
	coordinator := lifecycle.NewCoordinator()
	if drained, err := coordinator.Shutdown(context.Background(), time.Second); err != nil || !drained {
		t.Fatalf("Shutdown() = %v, %v", drained, err)
	}
	service, err := New(&fakeRepository{}, &fakeArtifactStore{}, coordinator, testPriorities(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest([]byte("unused"))
	request.Source = panicReader{}
	_, err = service.Admit(context.Background(), request)
	assertAdmissionError(t, err, 503, CodeNotReady, true)
}

func TestAdmitEnforcesDeclaredAndStreamedBounds(t *testing.T) {
	t.Run("declared too large", func(t *testing.T) {
		repository := &fakeRepository{}
		artifacts := &fakeArtifactStore{}
		service := newTestService(t, repository, artifacts, nil)
		request := testRequest([]byte("unused"))
		tooLarge := int64(65)
		request.DeclaredSourceBytes = &tooLarge
		request.Source = panicReader{}
		_, err := service.Admit(context.Background(), request)
		assertAdmissionError(t, err, 413, CodeSourceTooLarge, false)
		if repository.reserveCalls != 0 {
			t.Fatal("oversized declaration reserved capacity")
		}
	})

	tests := []struct {
		name     string
		request  func() Request
		wantCode string
		status   int
	}{
		{
			name: "stream exceeds hard limit",
			request: func() Request {
				request := testRequest(bytes.Repeat([]byte("x"), 65))
				request.DeclaredSourceBytes = nil
				return request
			},
			wantCode: CodeSourceTooLarge, status: 413,
		},
		{
			name: "declared length mismatch",
			request: func() Request {
				request := testRequest([]byte("abc"))
				declared := int64(4)
				request.DeclaredSourceBytes = &declared
				return request
			},
			wantCode: CodeInvalidSource, status: 400,
		},
		{
			name: "source hash mismatch",
			request: func() Request {
				request := testRequest([]byte("abc"))
				request.ExpectedSourceSHA256 = strings.Repeat("f", 64)
				return request
			},
			wantCode: CodeInvalidSource, status: 400,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := acceptingRepository()
			artifacts := &fakeArtifactStore{}
			service := newTestService(t, repository, artifacts, nil)
			_, err := service.Admit(context.Background(), test.request())
			assertAdmissionError(t, err, test.status, test.wantCode, false)
			if repository.abortCalls != 1 {
				t.Fatalf("abort calls = %d, want 1", repository.abortCalls)
			}
			if artifacts.putCalls != 0 {
				t.Fatalf("artifact writes = %d", artifacts.putCalls)
			}
		})
	}
}

func TestAdmitIdempotencyDoesNotDuplicateSource(t *testing.T) {
	source := []byte("same source")
	tests := []struct {
		name      string
		uploading bool
		wantError bool
	}{
		{name: "accepted result", uploading: false},
		{name: "upload in progress", uploading: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeRepository{}
			repository.reserve = func(input jobpostgres.AdmissionReservationInput, _ jobpostgres.AdmissionLimits) (jobpostgres.AdmissionReservation, error) {
				state := "queued"
				if test.uploading {
					state = "uploading"
				}
				return jobpostgres.AdmissionReservation{
					Job:    jobpostgres.Job{ID: input.Job.ID, State: state},
					Reused: true, Uploading: test.uploading,
				}, nil
			}
			artifacts := &fakeArtifactStore{}
			service := newTestService(t, repository, artifacts, nil)
			request := testRequest(source)
			request.Source = panicReader{}
			result, err := service.Admit(context.Background(), request)
			if test.wantError {
				assertAdmissionError(t, err, 503, CodeAdmissionPending, true)
			} else if err != nil || !result.Reused || result.Job.State != "queued" {
				t.Fatalf("reused admission = %#v, %v", result, err)
			}
			if artifacts.putCalls != 0 || repository.commitCalls != 0 {
				t.Fatal("idempotent retry duplicated source or queue commit")
			}
		})
	}
}

func TestAdmitCleansUpAfterStoreOrCommitFailure(t *testing.T) {
	source := []byte("source")
	t.Run("store", func(t *testing.T) {
		repository := acceptingRepository()
		artifacts := &fakeArtifactStore{putErr: errors.New("disk full")}
		service := newTestService(t, repository, artifacts, nil)
		_, err := service.Admit(context.Background(), testRequest(source))
		assertAdmissionError(t, err, 503, CodeAdmissionFailed, true)
		if repository.abortCalls != 1 || artifacts.deleteCalls != 0 {
			t.Fatalf("store failure cleanup = abort %d delete %d", repository.abortCalls, artifacts.deleteCalls)
		}
	})
	t.Run("commit", func(t *testing.T) {
		repository := acceptingRepository()
		repository.commitErr = errors.New("database unavailable")
		artifacts := &fakeArtifactStore{}
		service := newTestService(t, repository, artifacts, nil)
		_, err := service.Admit(context.Background(), testRequest(source))
		assertAdmissionError(t, err, 503, CodeAdmissionFailed, true)
		if repository.abortCalls != 1 || artifacts.deleteCalls != 1 {
			t.Fatalf("commit failure cleanup = abort %d delete %d", repository.abortCalls, artifacts.deleteCalls)
		}
	})
}

type fakeRepository struct {
	reserve      func(jobpostgres.AdmissionReservationInput, jobpostgres.AdmissionLimits) (jobpostgres.AdmissionReservation, error)
	commit       func(jobpostgres.CommitAdmissionInput) (jobpostgres.Job, error)
	reserveErr   error
	commitErr    error
	reserveCalls int
	commitCalls  int
	abortCalls   int
}

func (repository *fakeRepository) ReserveAdmission(_ context.Context, input jobpostgres.AdmissionReservationInput, limits jobpostgres.AdmissionLimits) (jobpostgres.AdmissionReservation, error) {
	repository.reserveCalls++
	if repository.reserveErr != nil {
		return jobpostgres.AdmissionReservation{}, repository.reserveErr
	}
	if repository.reserve != nil {
		return repository.reserve(input, limits)
	}
	return jobpostgres.AdmissionReservation{}, errors.New("unexpected reserve")
}

func (repository *fakeRepository) CommitAdmission(_ context.Context, input jobpostgres.CommitAdmissionInput) (jobpostgres.Job, error) {
	repository.commitCalls++
	if repository.commitErr != nil {
		return jobpostgres.Job{}, repository.commitErr
	}
	if repository.commit != nil {
		return repository.commit(input)
	}
	return jobpostgres.Job{}, errors.New("unexpected commit")
}

func (repository *fakeRepository) AbortAdmission(context.Context, string, string) error {
	repository.abortCalls++
	return nil
}

func acceptingRepository() *fakeRepository {
	repository := &fakeRepository{}
	repository.reserve = func(input jobpostgres.AdmissionReservationInput, _ jobpostgres.AdmissionLimits) (jobpostgres.AdmissionReservation, error) {
		job := input.Job
		job.State = "uploading"
		job.DeclaredSourceBytes = input.DeclaredSourceBytes
		job.ReservationExpiresAt = &input.ReservationExpiresAt
		return jobpostgres.AdmissionReservation{Job: job, Uploading: true}, nil
	}
	repository.commit = func(input jobpostgres.CommitAdmissionInput) (jobpostgres.Job, error) {
		return jobpostgres.Job{ID: input.JobID, State: "queued", SourceArtifactID: input.Artifact.ID}, nil
	}
	return repository
}

type fakeArtifactStore struct {
	putPrivate  func(orchestration.ArtifactWrite) (contracts.ArtifactReference, error)
	putErr      error
	putCalls    int
	deleteCalls int
}

func (store *fakeArtifactStore) PutPrivate(_ context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	store.putCalls++
	if store.putErr != nil {
		return contracts.ArtifactReference{}, store.putErr
	}
	if store.putPrivate != nil {
		return store.putPrivate(write)
	}
	return artifactReference(write, "private"), nil
}

func (store *fakeArtifactStore) GetPrivate(context.Context, contracts.ArtifactReference) ([]byte, error) {
	return nil, errors.New("unexpected private get")
}

func (store *fakeArtifactStore) DeletePrivate(context.Context, contracts.ArtifactReference) error {
	store.deleteCalls++
	return nil
}

func (store *fakeArtifactStore) PutPublicIfMissing(context.Context, orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	return contracts.ArtifactReference{}, errors.New("unexpected public put")
}

func artifactReference(write orchestration.ArtifactWrite, visibility string) contracts.ArtifactReference {
	digest := sha256.Sum256(write.Body)
	reference := contracts.ArtifactReference{
		ArtifactID: write.ArtifactID,
		SHA256:     hex.EncodeToString(digest[:]),
		Bytes:      int64(len(write.Body)),
		MediaType:  write.MediaType,
		Visibility: visibility,
	}
	if write.ExpiresAt != nil {
		reference.ExpiresAt = write.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return reference
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("source body was read before admission") }

func newTestService(t *testing.T, repository Repository, artifacts orchestration.ArtifactStore, readiness Readiness) *Service {
	t.Helper()
	if readiness == nil {
		readiness = readyReadiness()
	}
	service, err := New(repository, artifacts, readiness, testPriorities(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testConfig() Config {
	return Config{
		Limits: jobpostgres.AdmissionLimits{
			GlobalNonTerminal:  32,
			PrincipalQueued:    8,
			PreviewQueued:      20,
			PrivateSourceBytes: 2 << 30,
		},
		MaxSourceBytes: 64,
		ReservationTTL: 5 * time.Minute,
		SourceTTL:      24 * time.Hour,
		JobTTL:         8 * 24 * time.Hour,
		IdempotencyTTL: 24 * time.Hour,
		RetryAfter:     15 * time.Second,
		Now: func() time.Time {
			return time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
		},
		Random: bytes.NewReader(make([]byte, 64)),
	}
}

func testRequest(source []byte) Request {
	digest := sha256.Sum256(source)
	declared := int64(len(source))
	return Request{
		PrincipalID:          "principal-a",
		OwnerScope:           "book-260",
		ContentKind:          "latex",
		DocumentEngine:       "latexml",
		ResourceClass:        "document-latexml",
		PriorityIntent:       "publish",
		ProjectHash:          strings.Repeat("a", 64),
		OptionsHash:          strings.Repeat("b", 64),
		RendererVersion:      "test-version",
		MaxAttempts:          2,
		IdempotencyKey:       "request-1",
		ExpectedSourceSHA256: hex.EncodeToString(digest[:]),
		DeclaredSourceBytes:  &declared,
		Source:               bytes.NewReader(source),
		SourceMediaType:      "application/zip",
		SourceSchemaVersion:  "rin-project-archive/v1",
	}
}

func readyReadiness() Readiness {
	return ReadinessFunc(func(context.Context) error { return nil })
}

func testPriorities() PriorityAssigner {
	return PriorityAssignerFunc(func(_ context.Context, _ string, intent string) (string, error) {
		if intent == "" {
			return "rebuild", nil
		}
		return intent, nil
	})
}

func assertAdmissionError(t *testing.T, err error, status int, code string, retry bool) {
	t.Helper()
	var admissionError *Error
	if !errors.As(err, &admissionError) {
		t.Fatalf("error = %v, want admission Error", err)
	}
	if admissionError.StatusCode != status || admissionError.Code != code {
		t.Fatalf("admission error = %#v, want status %d code %q", admissionError, status, code)
	}
	if retry != (admissionError.RetryAfter > 0) {
		t.Fatalf("RetryAfter = %v, retry expectation %v", admissionError.RetryAfter, retry)
	}
}

var _ io.Reader = panicReader{}
