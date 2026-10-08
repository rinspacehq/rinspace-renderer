package completiondispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
)

func TestPostgresCompletionDispatcherSurvivesControlPlaneAndRendererRestart(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("RIN_RENDERER_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("RIN_RENDERER_TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repository, err := jobpostgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	queuedAt := now
	artifactExpiresAt := now.Add(24 * time.Hour)
	sourceArtifact, err := repository.CreateArtifact(ctx, jobpostgres.Artifact{
		ID:     "render-jobs/v1/source/99999999-9999-4999-8999-999999999999/" + strings.Repeat("4", 64),
		SHA256: strings.Repeat("4", 64), Kind: "source", Visibility: "private",
		StorageKey: "render-jobs/v1/source/99999999-9999-4999-8999-999999999999/" + strings.Repeat("4", 64),
		ByteSize:   42, MediaType: "application/zip", SchemaVersion: "rin-project-archive/v1", ExpiresAt: &artifactExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]string{
		"requestId": "render-99999999999999999999999999999999", "controlProjectId": "article:99",
		"sourceCommit": strings.Repeat("9", 40), "controlProjectHash": strings.Repeat("8", 64),
	})
	job, err := repository.CreateJob(ctx, jobpostgres.Job{
		ID: "99999999-9999-4999-8999-999999999999", PrincipalID: "dispatcher-integration", OwnerScope: "article:99",
		ContentKind: "markdown", DocumentEngine: "unified", ResourceClass: "document-light",
		PriorityClass: "publish", State: "queued", SourceArtifactID: sourceArtifact.ID, ProjectHash: strings.Repeat("7", 64),
		OptionsHash: strings.Repeat("6", 64), RendererVersion: "dispatcher-integration",
		RequestMetadata: metadata, MaxAttempts: 1, QueuedAt: &queuedAt, AvailableAt: &queuedAt,
		ExpiresAt: now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	leadership, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repository.ClaimNext(ctx, leadership, jobpostgres.ClaimInput{
		AttemptID: "98989898-9898-4898-8989-989898989898", WorkerID: "dispatcher-integration",
		LeaseTokenHash: strings.Repeat("5", 64), Now: now.Add(time.Second),
		LeaseExpiresAt: now.Add(time.Minute), Policy: dispatcherSchedulingPolicy(),
	})
	if err != nil || lease.Job.ID != job.ID {
		t.Fatalf("claim completion integration job = %#v, %v", lease, err)
	}
	if _, err := repository.FinishAttempt(ctx, jobpostgres.FinishInput{
		AttemptID: lease.AttemptID, LeaseTokenHash: strings.Repeat("5", 64),
		Now: now.Add(2 * time.Second), ErrorCode: "document_render_failed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := leadership.Close(); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		if request.Header.Get("X-Rin-Key-ID") != "integration-current" {
			t.Errorf("completion key ID = %q", request.Header.Get("X-Rin-Key-ID"))
		}
		if requests.Add(1) == 1 {
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	sender, err := NewHTTPSender(HTTPSenderConfig{
		Endpoint: server.URL + "/internal/v1/events/renderer", KeyID: "integration-current",
		Key: []byte(strings.Repeat("k", 32)), Client: server.Client(), Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatchClock := now.Add(3 * time.Second)
	newDispatcher := func() *Dispatcher {
		dispatcher, newErr := New(repository, sender, Config{
			PollInterval: time.Millisecond, LeaseDuration: time.Minute, RequestTimeout: 5 * time.Second,
			MaxAttempts: 3, Clock: func() time.Time { return dispatchClock }, Operations: operational.New(io.Discard),
		})
		if newErr != nil {
			t.Fatal(newErr)
		}
		return dispatcher
	}
	if delivered, err := newDispatcher().dispatchOne(ctx); err != nil || delivered {
		t.Fatalf("delivery during Control Plane restart = %v, %v", delivered, err)
	}
	if health, err := repository.CompletionHealth(ctx, dispatchClock); err != nil || health.State != "degraded" || health.Failed != 1 {
		t.Fatalf("restart backlog health = %#v, %v", health, err)
	}
	dispatchClock = dispatchClock.Add(10 * time.Second)
	if delivered, err := newDispatcher().dispatchOne(ctx); err != nil || !delivered {
		t.Fatalf("delivery after Renderer restart = %v, requests=%d, %v", delivered, requests.Load(), err)
	}
	if health, err := repository.CompletionHealth(ctx, dispatchClock); err != nil || health.State != "healthy" ||
		health.Failed != 0 || health.LastAcceptedAt == nil {
		t.Fatalf("recovered completion health = %#v, %v", health, err)
	}
}

func dispatcherSchedulingPolicy() jobpostgres.SchedulingPolicy {
	return jobpostgres.SchedulingPolicy{
		Resources: jobpostgres.ResourceCapacities{DocumentLight: 1, DocumentLaTeXML: 1, MathNode: 1, TeXSVG: 1, BatchMigration: 1, LatexPDF: 1,
			DocumentTypst: 1, TypstPDF: 1, Heavy: 4},
		Weights:       jobpostgres.PriorityWeights{Publish: 4, Preview: 3, Rebuild: 2, Migration: 1},
		AgingInterval: time.Minute, MaxAgingSteps: 1, PrincipalRunning: 1,
	}
}
