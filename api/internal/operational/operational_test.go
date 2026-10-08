package operational

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSnapshotSeparatesReadinessAndNormalizesPrivateErrors(t *testing.T) {
	registry := New(nil)
	registry.now = func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) }
	if err := registry.AddProbe("database", func(context.Context) error { return errors.New("dial password=secret source=/private/book") }); err != nil {
		t.Fatal(err)
	}
	if err := registry.AddProbe("artifact_store", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot(context.Background())
	if snapshot.Ready || snapshot.Status != "degraded" || len(snapshot.Degraded) != 1 || snapshot.Degraded[0] != "database" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Components["database"].Reason != "unavailable" {
		t.Fatalf("private readiness reason escaped normalization: %#v", snapshot.Components["database"])
	}
	if err := registry.Ready(context.Background()); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Ready() error = %v", err)
	}
}

func TestMetricsAreBoundedAndStructuredLogsRejectSourceFields(t *testing.T) {
	var logBody bytes.Buffer
	registry := New(&logBody)
	registry.Count("admissions_total", "latex", "document-latexml")
	registry.Count("admissions_total", "../../private-source")
	registry.Gauge("resource_capacity", 2, "document-light")
	registry.Observe("stage_duration_seconds", 1500*time.Millisecond, "math")
	collected := false
	if err := registry.AddCollector("queue", func(context.Context) error {
		collected = true
		registry.Gauge("queue_projects", 3, "queued")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	registry.Metrics(response, httptest.NewRequest("GET", "/metrics", nil))
	body := response.Body.String()
	for _, expected := range []string{
		"rin_renderer_admissions_total_latex_document_latexml 1",
		"rin_renderer_resource_capacity_document_light 2",
		"rin_renderer_stage_duration_seconds_math_count 1",
		"rin_renderer_queue_projects_queued 3",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	if !collected {
		t.Fatal("metrics collector did not refresh durable gauges")
	}
	if strings.Contains(body, "private-source") {
		t.Fatalf("unsafe metric label was exported: %s", body)
	}
	registry.Event("job_finished", slog.String("job_id", "11111111-1111-4111-8111-111111111111"),
		slog.String("state", "succeeded"), slog.String("source", "private source body"),
		slog.String("request_id", "private source body"))
	if strings.Contains(logBody.String(), "private source") || !strings.Contains(logBody.String(), `"event":"job_finished"`) {
		t.Fatalf("structured log safety = %s", logBody.String())
	}
}

func TestWarnEventsAreEmittedAtWarningLevelAndStayBounded(t *testing.T) {
	var logBody bytes.Buffer
	registry := New(&logBody)
	registry.Warn("job_degraded",
		slog.String("job_id", "job-1"), slog.String("codes", "latexml.error,math.primary_fallback,math.texsvg.failed"),
		slog.Int("diagnostic_count", 3), slog.String("source", "/private/book.tex"))
	line := strings.TrimSpace(logBody.String())
	if !strings.Contains(line, `"level":"WARN"`) || !strings.Contains(line, `"event":"job_degraded"`) ||
		!strings.Contains(line, `"codes":"latexml.error,math.primary_fallback,math.texsvg.failed"`) ||
		!strings.Contains(line, `"diagnostic_count":3`) {
		t.Fatalf("warn event = %s", line)
	}
	if strings.Contains(line, "/private/book.tex") {
		t.Fatalf("warn event leaked a rejected attribute: %s", line)
	}
}
