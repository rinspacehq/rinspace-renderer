package mathrender

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWarmMathJaxRendererMatchesProcessRendererBytes(t *testing.T) {
	script := testMathJaxScriptPath(t)
	processRenderer := NewMathJaxRenderer(script, 10*time.Second, 32<<10)
	warm := newTestWarmMathJaxRenderer(t, WarmMathJaxConfig{ScriptPath: script, Workers: 1, MaxTasks: 100})
	defer warm.Close()

	requests := []Request{
		{Source: `\not\exists k\in\mathbb{R}`, DisplayMode: false},
		{Source: `\begin{bmatrix}1&2\\3&4\end{bmatrix}`, DisplayMode: true},
		{Source: `\left\langle\frac{x}{y}\right\rangle`, DisplayMode: true},
		{Source: `\foo`, DisplayMode: false, Macros: map[string]string{"foo": `\mathbf{x}`}},
	}
	for index, request := range requests {
		want, err := processRenderer.Render(context.Background(), request)
		if err != nil {
			t.Fatalf("process render %d: %v", index, err)
		}
		got, err := warm.Render(context.Background(), request)
		if err != nil {
			t.Fatalf("warm render %d: %v", index, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("warm render %d changed bytes\nwant: %#v\n got: %#v", index, want, got)
		}
	}
	snapshot := warm.Snapshot()
	if snapshot.Starts != 1 || snapshot.Tasks != uint64(len(requests)) {
		t.Fatalf("expected one reused worker, got %#v", snapshot)
	}
}

func TestWarmMathJaxRendererBatchMatchesProcessRendererBytesAndErrors(t *testing.T) {
	script := testMathJaxScriptPath(t)
	processRenderer := NewMathJaxRenderer(script, 10*time.Second, 32<<10)
	warm := newTestWarmMathJaxRenderer(t, WarmMathJaxConfig{ScriptPath: script, Workers: 1, MaxTasks: 100})
	defer warm.Close()
	requests := []Request{
		{Source: `x^2`, DisplayMode: false},
		{Source: `\definitelyunsupportedcommand{x}`, DisplayMode: true},
		{Source: ` `, DisplayMode: false},
		{Source: strings.Repeat("x", 33<<10), DisplayMode: false},
		{Source: `\foo`, DisplayMode: false, Macros: map[string]string{"foo": `\mathbf{x}`}},
		{Source: `\foo`, DisplayMode: false, Macros: map[string]string{"foo": `\mathbb{R}`}},
	}
	want, err := processRenderer.RenderBatch(context.Background(), requests)
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	got, err := warm.RenderBatch(context.Background(), requests)
	if err != nil {
		t.Fatalf("warm batch: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("result count changed: want %d got %d", len(want), len(got))
	}
	for index := range want {
		if !reflect.DeepEqual(got[index].Result, want[index].Result) || errorText(got[index].Err) != errorText(want[index].Err) {
			t.Fatalf("batch result %d changed\nwant: %#v / %v\n got: %#v / %v", index, want[index].Result, want[index].Err, got[index].Result, got[index].Err)
		}
	}
	if got[4].Err != nil || got[5].Err != nil || got[4].Result.HTML == got[5].Result.HTML {
		t.Fatalf("per-item macro contexts were not rendered independently: %#v %#v", got[4], got[5])
	}
}

func TestWarmMathJaxRendererMapsBoundedResponseFailure(t *testing.T) {
	warm := newTestWarmMathJaxRenderer(t, WarmMathJaxConfig{
		ScriptPath: testMathJaxScriptPath(t), Workers: 1, MaxTasks: 100, MaxResponseBytes: 256,
	})
	defer warm.Close()
	_, err := warm.Render(context.Background(), Request{Source: `x`, DisplayMode: false})
	var renderErr *RenderError
	if !errors.As(err, &renderErr) || renderErr.Code != "mathjax.output.too_large" {
		t.Fatalf("expected bounded output error, got %#v", err)
	}
	if snapshot := warm.Snapshot(); snapshot.ProtocolErrors < 1 || snapshot.Restarts < 1 {
		t.Fatalf("expected protocol failure to recycle worker: %#v", snapshot)
	}
}

func TestWarmMathJaxRendererHealth(t *testing.T) {
	warm := newTestWarmMathJaxRenderer(t, WarmMathJaxConfig{ScriptPath: testMathJaxScriptPath(t), Workers: 2, MaxTasks: 100})
	defer warm.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := warm.Ready(ctx); err != nil {
		t.Fatalf("warm worker health: %v", err)
	}
	if snapshot := warm.Snapshot(); snapshot.Alive != 2 || snapshot.Capacity != 2 {
		t.Fatalf("unexpected health snapshot: %#v", snapshot)
	}
}

func newTestWarmMathJaxRenderer(t *testing.T, config WarmMathJaxConfig) *WarmMathJaxRenderer {
	t.Helper()
	config.NodeBin = "node"
	config.Timeout = 10 * time.Second
	config.MaxSourceBytes = 32 << 10
	if config.Workers == 0 {
		config.Workers = 1
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = 1 << 20
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = 16 << 20
	}
	if config.MaxTasks == 0 {
		config.MaxTasks = 100
	}
	config.MaxRSSBytes = 2 << 30
	config.StartTimeout = 10 * time.Second
	config.StopGrace = 100 * time.Millisecond
	renderer, err := NewWarmMathJaxRenderer(config)
	if err != nil {
		t.Fatalf("new warm MathJax renderer: %v", err)
	}
	return renderer
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
