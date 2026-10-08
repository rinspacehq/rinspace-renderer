package codeservice

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWarmShikiRendererUsesPinnedWorkerBatchContract(t *testing.T) {
	renderer := newTestWarmShikiRenderer(t)
	defer renderer.Close()
	requests := []RenderRequest{
		{ID: "rw_00000000000000000000000000000001", Source: `const value = "<tag>"`, Language: "js"},
		{ID: "rw_00000000000000000000000000000002", Source: `<script>x</script>`, Language: "not-real"},
	}
	results, err := renderer.RenderBatch(context.Background(), DefaultTheme, requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || !results[0].Highlighted || results[0].CanonicalLanguage != "javascript" ||
		results[1].Highlighted || len(results[1].Diagnostics) != 1 || strings.Contains(results[1].HTML, "<script>") {
		t.Fatalf("unexpected Shiki worker results: %#v", results)
	}
	second, err := renderer.RenderBatch(context.Background(), DefaultTheme, requests[:1])
	if err != nil || len(second) != 1 {
		t.Fatalf("warm worker reuse: %#v %v", second, err)
	}
	if snapshot := renderer.Snapshot(); snapshot.Starts != 1 || snapshot.Tasks != 2 {
		t.Fatalf("worker was not reused: %#v", snapshot)
	}
}

func TestWarmShikiRendererHealth(t *testing.T) {
	renderer := newTestWarmShikiRenderer(t)
	defer renderer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := renderer.Ready(ctx); err != nil {
		t.Fatal(err)
	}
}

func newTestWarmShikiRenderer(t *testing.T) *WarmShikiRenderer {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	script := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../engines/markdown/worker.mjs"))
	renderer, err := NewWarmShikiRenderer(WarmShikiConfig{
		NodeBin: "node", ScriptPath: script, Timeout: 15 * time.Second, Workers: 1,
		MaxRequestBytes: 4 << 20, MaxResponseBytes: 8 << 20, MaxTasks: 100,
		MaxRSSBytes: 2 << 30, StartTimeout: 15 * time.Second, StopGrace: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return renderer
}
