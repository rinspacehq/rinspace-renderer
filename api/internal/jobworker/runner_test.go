package jobworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/scheduler"
)

func TestWorkerReadinessTracksLeadershipLifecycle(t *testing.T) {
	runner := &Runner{}
	if err := runner.Ready(context.Background()); err == nil {
		t.Fatal("worker should not be ready before leadership and recovery")
	}
	runner.ready.Store(true)
	if err := runner.Ready(context.Background()); err != nil {
		t.Fatalf("ready worker: %v", err)
	}
	runner.ready.Store(false)
	if err := runner.Ready(context.Background()); err == nil {
		t.Fatal("worker should not remain ready after its run loop exits")
	}
}

func TestExecuteStoresCanonicalAndCompatibilityResult(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	job := testJob(expiresAt)
	schedule := &fakeScheduler{}
	store := &fakeStore{source: []byte("archive")}
	runner := newTestRunner(t, schedule, store, &fakeExecutor{result: successfulExecution()})
	runner.execute(context.Background(), scheduler.Lease{Job: job, AttemptID: "attempt-a", Token: "token-a"})
	if schedule.succeededArtifact == nil || schedule.succeededArtifact.SchemaVersion != jobresult.StoredOutputSchemaVersion || schedule.failedCode != "" {
		t.Fatalf("worker completion = %#v, failure %q", schedule.succeededArtifact, schedule.failedCode)
	}
	if schedule.refinement == nil || schedule.refinement.DocumentEngine != "latexml" || schedule.refinement.DocumentClass != "article" || schedule.refinement.MathCount != 0 {
		t.Fatalf("workload refinement = %#v", schedule.refinement)
	}
	output, err := jobresult.Decode(store.result)
	if err != nil || output.Result.JobID != job.ID || output.Result.RequestID != "request-a" || output.Result.Inline == nil ||
		output.Result.Inline.SchemaVersion != contracts.DocumentBundleSchemaVersionV2 || len(output.Result.Inline.Pages) != 1 ||
		len(output.Result.Inline.Pages[0].Blocks) != 1 || output.Result.Inline.Pages[0].Blocks[0].Kind != contracts.DocumentBlockParagraph ||
		!strings.Contains(output.Result.Inline.Pages[0].Fragment, `data-rin-block-id="rb_`) ||
		!strings.Contains(output.Result.Inline.Pages[0].Fragment, `>ok</article>`) {
		t.Fatalf("stored output = %#v, %v", output, err)
	}
	if schedule.completionResult == nil || schedule.completionResult.RendererProjectHash != output.Result.ProjectHash ||
		schedule.completionResult.ResultHash != output.Result.ResultHash {
		t.Fatalf("worker completion evidence = %#v, stored result = %#v", schedule.completionResult, output.Result)
	}
	if !strings.Contains(string(output.Compatibility), `"source":"private-source"`) {
		t.Fatalf("legacy compatibility payload was not retained privately: %s", output.Compatibility)
	}
}

func TestExecuteBindsPublicationResultToControlProjectHash(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	job := testJob(expiresAt)
	controlProjectHash := strings.Repeat("c", 64)
	job.RequestMetadata, _ = json.Marshal(requestMetadata{
		RequestID: "request-a", SourceName: "main.tex", ControlProjectID: "book:289",
		SourceCommit: strings.Repeat("a", 40), ControlProjectHash: controlProjectHash,
	})
	executor := &fakeExecutor{result: successfulExecution()}
	schedule := &fakeScheduler{}
	store := &fakeStore{source: []byte("archive")}
	runner := newTestRunner(t, schedule, store, executor)
	runner.execute(context.Background(), scheduler.Lease{Job: job, AttemptID: "attempt-a", Token: "token-a"})
	output, err := jobresult.Decode(store.result)
	if err != nil || output.Result.ProjectHash != controlProjectHash || output.Result.Inline.ProjectHash != controlProjectHash ||
		executor.request.ControlProjectHash != controlProjectHash || schedule.completionResult == nil ||
		schedule.completionResult.RendererProjectHash != controlProjectHash || output.Result.ProjectHash == job.ProjectHash {
		t.Fatalf("publication hash binding = output %#v, request %#v, completion %#v, err %v", output.Result, executor.request, schedule.completionResult, err)
	}
}

func TestStoreResultPreservesAdapterCanonicalResult(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	job := testJob(expiresAt)
	job.ContentKind = "markdown"
	job.DocumentEngine = "unified"
	response := successfulExecution().Response
	response.Engine = "rin-markdown"
	response.MainFile = "article.md"
	canonical, err := renderapi.CanonicalResult(job.ID, job.ProjectHash, job.ContentKind, response)
	if err != nil {
		t.Fatal(err)
	}
	canonical.Versions["markdownPipeline"] = "rin-markdown-pipeline/test"
	schedule := &fakeScheduler{}
	store := &fakeStore{source: []byte("archive")}
	runner := newTestRunner(t, schedule, store, &fakeExecutor{})
	storedJob, failureCode, _, err := runner.storeResult(context.Background(), scheduler.Lease{
		Job: job, AttemptID: "attempt-a", Token: "token-a",
	}, renderapi.ProjectExecutionResult{Status: 200, Response: response, Canonical: &canonical})
	if err != nil || failureCode != "" || storedJob.State != "succeeded" {
		t.Fatalf("store canonical Markdown result = state %q, failure %q, err %v", storedJob.State, failureCode, err)
	}
	output, err := jobresult.Decode(store.result)
	if err != nil || output.Result.Versions["markdownPipeline"] != "rin-markdown-pipeline/test" {
		t.Fatalf("adapter canonical result was remapped: %#v, %v", output.Result, err)
	}
}

func TestStoreResultRejectsCompatibilityResponseWithLeasedJobAsRequestIdentity(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	job := testJob(expiresAt)
	execution := successfulExecution()
	execution.Canonical = nil
	execution.Response.RequestID = job.ID
	schedule := &fakeScheduler{}
	runner := newTestRunner(t, schedule, &fakeStore{source: []byte("archive")}, &fakeExecutor{})
	storedJob, failureCode, result, err := runner.storeResult(context.Background(), scheduler.Lease{
		Job: job, AttemptID: "attempt-a", Token: "token-a",
	}, execution)
	if err != nil || failureCode != "invalid_render_result" || result != nil ||
		storedJob.State == "succeeded" || schedule.failedCode != "invalid_render_result" {
		t.Fatalf("mismatched compatibility identity = job %#v, failure %q, result %#v, scheduler failure %q, err %v",
			storedJob, failureCode, result, schedule.failedCode, err)
	}
}

func TestStoreResultAtomicallyPreservesCompleteMarkdownBook(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	job := testJob(expiresAt)
	job.ContentKind = "markdown"
	job.DocumentEngine = "unified"
	bundle := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersion, ProjectHash: job.ProjectHash,
		BundleHash: strings.Repeat("c", 64), State: contracts.DocumentBundleStateFinal,
		ContentKind: contracts.ContentKindMarkdown, DocumentEngine: "rin-markdown", Title: "Book",
		Pages: []contracts.DocumentPage{
			{ID: "intro", SourcePath: "intro.md", Title: "Intro", Fragment: "<article>intro</article>", FragmentFormat: contracts.FragmentFormatHTML, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{strings.Repeat("d", 64)}},
			{ID: "chapter", SourcePath: "chapter.md", Title: "Chapter", Fragment: "<article>chapter</article>", FragmentFormat: contracts.FragmentFormatHTML, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{strings.Repeat("e", 64)}},
		},
		WorkUnits:   []contracts.WorkUnit{},
		Assets:      []contracts.AssetReference{{ID: "asset-1", Kind: "project-file", SHA256: strings.Repeat("f", 64), Bytes: 4, MediaType: "image/png", ProjectPath: "image.png"}},
		Diagnostics: []contracts.Diagnostic{{Code: "markdown.book.note", Severity: "info", Message: "book", Stage: "finalize"}},
		Provenance:  contracts.Provenance{Adapter: "rin-markdown", AdapterVersion: "test", EngineVersion: "test", ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion},
	}
	canonical := contracts.RenderResult{
		SchemaVersion: contracts.RenderResultSchemaVersion, JobID: job.ID, RequestID: "request-a",
		ProjectHash: job.ProjectHash, ResultHash: bundle.BundleHash, ContentKind: contracts.ContentKindMarkdown,
		Engine: "rin-markdown", Inline: &bundle, Assets: []contracts.ArtifactReference{},
		Diagnostics: append([]contracts.Diagnostic(nil), bundle.Diagnostics...), Versions: map[string]string{"rinRenderer": "test"},
		Cache: contracts.RenderCacheSummary{Hit: true, ReusedStages: []string{"page-finalizer"}},
	}
	schedule := &fakeScheduler{}
	store := &fakeStore{source: []byte("original-book-archive")}
	runner := newTestRunner(t, schedule, store, &fakeExecutor{})
	storedJob, failureCode, _, err := runner.storeResult(context.Background(), scheduler.Lease{Job: job, AttemptID: "attempt-a", Token: "token-a"},
		renderapi.ProjectExecutionResult{Status: 200, Response: successfulExecution().Response, Canonical: &canonical})
	if err != nil || failureCode != "" || storedJob.State != "succeeded" || schedule.succeededArtifact == nil || store.deleted != 0 {
		t.Fatalf("store Markdown Book = state %q, failure %q, artifact %#v, deleted %d, err %v", storedJob.State, failureCode, schedule.succeededArtifact, store.deleted, err)
	}
	output, err := jobresult.Decode(store.result)
	if err != nil || output.Result.Inline == nil || len(output.Result.Inline.Pages) != 2 ||
		output.Result.Inline.Pages[1].SourcePath != "chapter.md" || len(output.Result.Inline.Assets) != 1 ||
		len(output.Result.Diagnostics) != 1 || output.Result.Inline.Provenance.Adapter != "rin-markdown" ||
		!output.Result.Cache.Hit || output.Result.Inline.Pages[0].DependencyHashes[0] != strings.Repeat("d", 64) {
		t.Fatalf("stored Markdown Book lost canonical fields: %#v, %v", output.Result, err)
	}
}

func TestRenderForwardsMarkdownBookRequestMetadataAsOneProject(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	job := testJob(expiresAt)
	job.ContentKind = "markdown"
	job.DocumentEngine = "unified"
	job.RequestMetadata, _ = json.Marshal(requestMetadata{
		DocumentMode: "book", SourceName: "book.zip", Title: "Book", Options: `{"incrementalReuse":false}`,
		MarkdownBookPages: []projectidentity.MarkdownBookPage{{Path: "intro.md", ID: "intro"}, {Path: "chapter.md", ID: "chapter"}},
	})
	executor := &fakeExecutor{result: successfulExecution()}
	runner := newTestRunner(t, &fakeScheduler{}, &fakeStore{source: []byte("archive")}, executor)
	_, failure := runner.render(context.Background(), job)
	if failure != nil || executor.request.DocumentMode != "book" || !executor.request.DisableMarkdownBookReuse || len(executor.request.MarkdownBookPages) != 2 ||
		executor.request.MarkdownBookPages[1].ID != "chapter" {
		t.Fatalf("forwarded Markdown Book request = %#v, failure %#v", executor.request, failure)
	}
}

func TestStoreResultRejectsErrorsInAlreadyCanonicalBundle(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	job := testJob(expiresAt)
	response := successfulExecution().Response
	canonical, err := renderapi.CanonicalResult(job.ID, job.ProjectHash, job.ContentKind, response)
	if err != nil {
		t.Fatal(err)
	}
	canonical.Inline.Pages[0].Fragment += `<span class="ltx_ERROR undefined">\Needspace</span>`
	canonical.Inline.BundleHash = ""
	encoded, err := json.Marshal(canonical.Inline)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	canonical.Inline.BundleHash = hex.EncodeToString(digest[:])
	canonical.ResultHash = canonical.Inline.BundleHash
	schedule := &fakeScheduler{}
	store := &fakeStore{source: []byte("archive")}
	runner := newTestRunner(t, schedule, store, &fakeExecutor{})
	_, failureCode, result, err := runner.storeResult(context.Background(), scheduler.Lease{Job: job, AttemptID: "attempt-a", Token: "token-a"},
		renderapi.ProjectExecutionResult{Status: 200, Response: response, Canonical: &canonical})
	if err != nil || failureCode != "document_render_incomplete" || result != nil || schedule.failedRetryable || schedule.completionResult != nil {
		t.Fatalf("incomplete canonical bundle was accepted: code %q, result %#v, err %v", failureCode, result, err)
	}
}

func TestRenderUsesLeasedJobIDForCanonicalIdentity(t *testing.T) {
	job := testJob(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	executor := &fakeExecutor{result: successfulExecution()}
	runner := newTestRunner(t, &fakeScheduler{}, &fakeStore{source: []byte("archive")}, executor)
	_, failure := runner.render(context.Background(), job)
	if failure != nil || executor.request.JobID != job.ID || executor.request.RequestID != "request-a" {
		t.Fatalf("canonical identities = job %q request %q; want job %q request %q; failure %#v",
			executor.request.JobID, executor.request.RequestID, job.ID, "request-a", failure)
	}
}

func TestRenderFallsBackToLeasedJobIDWithoutControlRequestIdentity(t *testing.T) {
	job := testJob(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	job.RequestMetadata, _ = json.Marshal(requestMetadata{SourceName: "main.tex"})
	executor := &fakeExecutor{result: successfulExecution()}
	runner := newTestRunner(t, &fakeScheduler{}, &fakeStore{source: []byte("archive")}, executor)
	_, failure := runner.render(context.Background(), job)
	if failure != nil || executor.request.JobID != job.ID || executor.request.RequestID != job.ID {
		t.Fatalf("fallback identities = job %q request %q; want %q; failure %#v",
			executor.request.JobID, executor.request.RequestID, job.ID, failure)
	}
}

func TestStoreResultRejectsAdapterCanonicalIdentityMismatch(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	job := testJob(expiresAt)
	response := successfulExecution().Response
	canonical, err := renderapi.CanonicalResult(job.ID, job.ProjectHash, job.ContentKind, response)
	if err != nil {
		t.Fatal(err)
	}
	canonical.RequestID = "another-job"
	schedule := &fakeScheduler{}
	store := &fakeStore{source: []byte("archive")}
	runner := newTestRunner(t, schedule, store, &fakeExecutor{})
	_, failureCode, _, err := runner.storeResult(context.Background(), scheduler.Lease{
		Job: job, AttemptID: "attempt-a", Token: "token-a",
	}, renderapi.ProjectExecutionResult{Status: 200, Response: response, Canonical: &canonical})
	if err != nil || failureCode != "invalid_render_result" || schedule.failedCode != "invalid_render_result" || len(store.result) != 0 {
		t.Fatalf("canonical identity mismatch = failure %q, scheduler %q, stored %d, err %v", failureCode, schedule.failedCode, len(store.result), err)
	}
}

func TestExecuteHonorsCancellationBeforePublishingResult(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	schedule := &fakeScheduler{cancelRequested: true}
	store := &fakeStore{source: []byte("archive")}
	runner := newTestRunner(t, schedule, store, &fakeExecutor{result: successfulExecution()})
	runner.execute(context.Background(), scheduler.Lease{Job: testJob(expiresAt), AttemptID: "attempt-a", Token: "token-a"})
	if schedule.confirmed != 1 || schedule.succeededArtifact != nil || len(store.result) != 0 {
		t.Fatalf("cancellation completion = confirmed %d, artifact %#v, stored %d", schedule.confirmed, schedule.succeededArtifact, len(store.result))
	}
}

func TestExecuteClassifiesRendererFailure(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	schedule := &fakeScheduler{}
	store := &fakeStore{source: []byte("archive")}
	runner := newTestRunner(t, schedule, store, &fakeExecutor{result: renderapi.ProjectExecutionResult{Status: 502, Err: errors.New("worker failed")}})
	runner.execute(context.Background(), scheduler.Lease{Job: testJob(expiresAt), AttemptID: "attempt-a", Token: "token-a"})
	if schedule.failedCode != "document_render_failed" || !schedule.failedRetryable || store.deleted != 1 {
		t.Fatalf("failure classification = %q, retryable %v, deleted %d", schedule.failedCode, schedule.failedRetryable, store.deleted)
	}
}

func TestExecuteStoresTerminalFailureForCompatibility(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	schedule := &fakeScheduler{}
	store := &fakeStore{source: []byte("archive")}
	runner := newTestRunner(t, schedule, store, &fakeExecutor{result: renderapi.ProjectExecutionResult{
		Status: 422, Message: "invalid project archive", Err: errors.New("zip: not a valid zip file"),
		Diagnostics: []renderapi.Diagnostic{{Severity: "error", Code: "latex.invalid", Message: "bad source"}},
	}})
	runner.execute(context.Background(), scheduler.Lease{Job: testJob(expiresAt), AttemptID: "attempt-a", Token: "token-a"})
	var failure jobresult.StoredFailure
	if err := json.Unmarshal(store.result, &failure); err != nil {
		t.Fatal(err)
	}
	if schedule.failedCode != "invalid_project_source" || schedule.failedRetryable || store.deleted != 0 || failure.Status != 422 || failure.Error != "invalid project archive" || len(failure.Diagnostics) != 1 {
		t.Fatalf("stored terminal failure = %#v, scheduler %#v, deleted %d", failure, schedule, store.deleted)
	}
}

func TestExecuteClassifiesTimeoutSeparatelyFromDurationSamples(t *testing.T) {
	expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	schedule := &fakeScheduler{}
	store := &fakeStore{source: []byte("archive")}
	runner := newTestRunner(t, schedule, store, &fakeExecutor{result: renderapi.ProjectExecutionResult{
		Status: 502, Message: "document render timed out", Err: errors.New("worker stopped"),
		Diagnostics: []renderapi.Diagnostic{{Severity: "error", Code: "latexml.timeout", Message: "timeout"}},
	}})
	runner.execute(context.Background(), scheduler.Lease{Job: testJob(expiresAt), AttemptID: "attempt-a", Token: "token-a"})
	if schedule.failedCode != "document_render_timeout" || schedule.failedRetryable || schedule.refinement != nil {
		t.Fatalf("timeout classification = %q, retryable %v, refinement %#v", schedule.failedCode, schedule.failedRetryable, schedule.refinement)
	}
}

func TestIncompleteOutputFailsTerminallyWithoutStoringSuccess(t *testing.T) {
	for _, status := range []int{422, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			expiresAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
			schedule := &fakeScheduler{}
			store := &fakeStore{source: []byte("archive")}
			runner := newTestRunner(t, schedule, store, &fakeExecutor{result: renderapi.ProjectExecutionResult{
				Status: status, Err: errors.New("incomplete renderer output"),
				Diagnostics: []renderapi.Diagnostic{{Severity: "error", Code: "final_output.render_error", Message: "native error node"}},
			}})
			runner.execute(context.Background(), scheduler.Lease{Job: testJob(expiresAt), AttemptID: "attempt-a", Token: "token-a"})
			if schedule.failedCode != "document_render_incomplete" || schedule.failedRetryable || schedule.completionResult != nil {
				t.Fatalf("incomplete output classification: %#v", schedule)
			}
			var failure jobresult.StoredFailure
			if err := json.Unmarshal(store.result, &failure); err != nil || len(failure.Diagnostics) != 1 {
				t.Fatalf("terminal diagnostics not retained: %#v, %v", failure, err)
			}
		})
	}
}

func TestWorkloadRefinementUsesAnalyzedBoundedCounts(t *testing.T) {
	response := successfulExecution().Response
	response.AnalysisSource = `\documentclass [11pt] {book}`
	response.Project = map[string]any{"files": []map[string]any{{"path": "main.tex"}, {"path": "chapter.tex"}}}
	response.Reader = map[string]any{"pages": []map[string]any{{"id": "one"}, {"id": "two"}}}
	response.Math.Count = 12
	response.Diagrams = []renderapi.DiagramRef{{Type: "tikz"}}
	response.HTML = "<pre><code>a</code></pre><pre><code>b</code></pre>"
	refinement := workloadRefinement(jobpostgres.Job{ContentKind: "latex"}, response)
	if refinement.DocumentClass != "book" || refinement.FileCount != 2 || refinement.PageCount != 2 || refinement.MathCount != 12 || refinement.DiagramCount != 1 || refinement.CodeBlockCount != 2 {
		t.Fatalf("workload refinement = %#v", refinement)
	}
}

func TestWaitForLeaseMaintenanceIsBounded(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if !waitForLeaseMaintenance(done, time.Second) {
		t.Fatal("closed lease maintenance did not drain")
	}
	if waitForLeaseMaintenance(make(chan struct{}), time.Millisecond) {
		t.Fatal("blocked lease maintenance reported drained")
	}
}

func TestLeaseRecoveryIsBounded(t *testing.T) {
	schedule := &fakeScheduler{blockRecovery: true}
	runner := &Runner{scheduler: schedule, config: Config{RecoveryTimeout: time.Millisecond}}
	if _, err := runner.recover(context.Background(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked recovery error = %v", err)
	}
}

func newTestRunner(t *testing.T, schedule *fakeScheduler, store *fakeStore, executor *fakeExecutor) *Runner {
	t.Helper()
	runner, err := New(schedule, &fakeRepository{artifact: jobpostgres.Artifact{
		ID: "source-id", StorageKey: "render-jobs/v1/source/11111111-1111-4111-8111-111111111111/" + strings.Repeat("a", 64),
		SHA256: strings.Repeat("a", 64), Kind: "source", Visibility: "private", ByteSize: 7,
		MediaType: "application/zip", ExpiresAt: timePointer(time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)),
	}}, store, executor, fakeLifecycle{}, Config{
		WorkerID: "worker-a", PollInterval: time.Millisecond, HeartbeatInterval: time.Hour,
		RecoveryTimeout: time.Second, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func testJob(expiresAt time.Time) jobpostgres.Job {
	metadata, _ := json.Marshal(requestMetadata{RequestID: "request-a", SourceName: "main.tex"})
	return jobpostgres.Job{
		ID: "11111111-1111-4111-8111-111111111111", ContentKind: "latex", DocumentEngine: "latexml",
		SourceArtifactID: "source-id", ProjectHash: strings.Repeat("b", 64), RequestMetadata: metadata, ExpiresAt: expiresAt,
	}
}

func successfulExecution() renderapi.ProjectExecutionResult {
	return renderapi.ProjectExecutionResult{Status: 200, Response: renderapi.ProjectRenderResponse{
		RequestID: "request-a", Title: "Title", HTML: "<article>ok</article>", Engine: "latexml", MainFile: "main.tex",
		Versions: renderapi.Versions{RinRenderer: "test", LateXML: "test"}, Diagrams: []renderapi.DiagramRef{},
		Assets: []renderapi.AssetRef{}, AssetFiles: []renderapi.AssetFile{}, Diagnostics: []renderapi.Diagnostic{},
		Source: "private-source",
	}}
}

type fakeScheduler struct {
	cancelRequested   bool
	blockRecovery     bool
	confirmed         int
	failedCode        string
	failedRetryable   bool
	succeededArtifact *jobpostgres.Artifact
	completionResult  *jobpostgres.CompletionResultEvidence
	refinement        *jobpostgres.WorkloadRefinement
}

func (*fakeScheduler) AcquireLeadership(context.Context) (*jobpostgres.Leadership, error) {
	return nil, errors.New("unused")
}
func (*fakeScheduler) Claim(context.Context, *jobpostgres.Leadership, string) (scheduler.Lease, error) {
	return scheduler.Lease{}, jobpostgres.ErrNoEligibleJob
}
func (*fakeScheduler) Heartbeat(_ context.Context, lease scheduler.Lease) (scheduler.Lease, error) {
	return lease, nil
}
func (fake *fakeScheduler) RefineWorkload(_ context.Context, _ scheduler.Lease, refinement jobpostgres.WorkloadRefinement) (jobpostgres.WorkloadRecord, error) {
	fake.refinement = &refinement
	return jobpostgres.WorkloadRecord{ProfileKey: "test"}, nil
}
func (fake *fakeScheduler) Recover(ctx context.Context, _ *jobpostgres.Leadership) (int, error) {
	if fake.blockRecovery {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return 0, nil
}
func (fake *fakeScheduler) Fail(_ context.Context, lease scheduler.Lease, retryable bool, code string) (jobpostgres.Job, error) {
	fake.failedCode, fake.failedRetryable = code, retryable
	return lease.Job, nil
}
func (fake *fakeScheduler) FailWithResult(_ context.Context, lease scheduler.Lease, retryable bool, code string, artifact jobpostgres.Artifact) (jobpostgres.Job, error) {
	fake.failedCode, fake.failedRetryable, fake.succeededArtifact = code, retryable, &artifact
	if retryable {
		lease.Job.State = "queued"
	} else {
		lease.Job.State = "failed"
	}
	return lease.Job, nil
}
func (fake *fakeScheduler) SucceedWithResult(_ context.Context, lease scheduler.Lease, artifact jobpostgres.Artifact, evidence jobpostgres.CompletionResultEvidence) (jobpostgres.Job, error) {
	fake.succeededArtifact = &artifact
	fake.completionResult = &evidence
	lease.Job.State = "succeeded"
	return lease.Job, nil
}
func (fake *fakeScheduler) CancellationRequested(context.Context, scheduler.Lease) (bool, error) {
	return fake.cancelRequested, nil
}
func (fake *fakeScheduler) ConfirmCancellation(_ context.Context, lease scheduler.Lease) (jobpostgres.Job, error) {
	fake.confirmed++
	lease.Job.State = "canceled"
	return lease.Job, nil
}

type fakeRepository struct{ artifact jobpostgres.Artifact }

func (fake *fakeRepository) Artifact(context.Context, string) (jobpostgres.Artifact, error) {
	return fake.artifact, nil
}

type fakeStore struct {
	source, result []byte
	deleted        int
}

func (fake *fakeStore) GetPrivate(context.Context, contracts.ArtifactReference) ([]byte, error) {
	return fake.source, nil
}
func (fake *fakeStore) PutPrivate(_ context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	fake.result = append([]byte(nil), write.Body...)
	digest := sha256.Sum256(write.Body)
	return contracts.ArtifactReference{ArtifactID: write.ArtifactID, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(write.Body)), MediaType: write.MediaType, Visibility: "private", ExpiresAt: write.ExpiresAt.Format(time.RFC3339)}, nil
}
func (fake *fakeStore) DeletePrivate(context.Context, contracts.ArtifactReference) error {
	fake.deleted++
	return nil
}

type fakeExecutor struct {
	result  renderapi.ProjectExecutionResult
	request renderapi.ProjectExecutionRequest
}

func (fake *fakeExecutor) Execute(_ context.Context, request renderapi.ProjectExecutionRequest) renderapi.ProjectExecutionResult {
	fake.request = request
	return fake.result
}

type fakeLifecycle struct{}

func (fakeLifecycle) BeginWorker(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func timePointer(value time.Time) *time.Time { return &value }

func TestDegradedDiagnosticCodesSurfacesWarningsFromSuccessfulRenders(t *testing.T) {
	execution := renderapi.ProjectExecutionResult{
		Diagnostics: []renderapi.Diagnostic{{Severity: "warning", Code: "math.primary_fallback"}, {Severity: "info", Code: "latexml.info"}},
		Response: renderapi.ProjectRenderResponse{Diagnostics: []renderapi.Diagnostic{
			{Severity: "warning", Code: "math.primary_fallback"},
			{Severity: "error", Code: "final_output.invalid_document"},
			{Severity: "", Code: "renderer.project.not_implemented"},
		}},
	}
	got := degradedDiagnosticCodes(execution)
	want := []string{"final_output.invalid_document", "math.primary_fallback"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("degraded codes = %v want %v", got, want)
	}
	if clean := degradedDiagnosticCodes(renderapi.ProjectExecutionResult{}); len(clean) != 0 {
		t.Fatalf("clean render reported degraded codes: %v", clean)
	}
}

func TestDegradedCodeListStaysInsideTheOperationalLogValueLimit(t *testing.T) {
	codes := make([]string, 0, 40)
	for index := 0; index < 40; index++ {
		codes = append(codes, fmt.Sprintf("math.fallback.code.%02d", index))
	}
	value := degradedCodeList(codes)
	if len(value) > 128 || !strings.HasPrefix(value, "math.fallback.code.00") {
		t.Fatalf("degraded code list = %q (len %d)", value, len(value))
	}
	chunks := strings.Split(value, ",")
	for _, chunk := range chunks {
		if !strings.HasPrefix(chunk, "math.fallback.code.") {
			t.Fatalf("degraded code list truncated a code: %q", value)
		}
	}
}
