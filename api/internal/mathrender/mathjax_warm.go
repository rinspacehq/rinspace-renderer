package mathrender

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/nodeworker"
)

type WarmMathJaxConfig struct {
	NodeBin          string
	ScriptPath       string
	Timeout          time.Duration
	MaxSourceBytes   int64
	Workers          int
	MaxRequestBytes  int64
	MaxResponseBytes int64
	MaxTasks         uint64
	MaxRSSBytes      uint64
	StartTimeout     time.Duration
	StopGrace        time.Duration
	Environment      map[string]string
}

type WarmMathJaxRenderer struct {
	timeout        time.Duration
	maxSourceBytes int64
	supervisor     *nodeworker.Supervisor
}

func NewWarmMathJaxRenderer(config WarmMathJaxConfig) (*WarmMathJaxRenderer, error) {
	supervisor, err := nodeworker.New(nodeworker.Config{
		Command: config.NodeBin, Args: []string{config.ScriptPath, "--ndjson-worker"},
		Dir: nodeworker.ScriptDir(config.ScriptPath), Environment: config.Environment,
		Workers: config.Workers, MaxRequestBytes: config.MaxRequestBytes, MaxResponseBytes: config.MaxResponseBytes,
		MaxTasks: config.MaxTasks, MaxRSSBytes: config.MaxRSSBytes, StartTimeout: config.StartTimeout, StopGrace: config.StopGrace,
	})
	if err != nil {
		return nil, err
	}
	return &WarmMathJaxRenderer{timeout: config.Timeout, maxSourceBytes: config.MaxSourceBytes, supervisor: supervisor}, nil
}

func (renderer *WarmMathJaxRenderer) Render(ctx context.Context, request Request) (Result, error) {
	request.Source = strings.TrimSpace(request.Source)
	if request.Source == "" {
		return Result{}, &RenderError{Code: "math.source.empty", Message: "math source is empty"}
	}
	if renderer.maxSourceBytes > 0 && int64(len([]byte(request.Source))) > renderer.maxSourceBytes {
		return Result{}, &RenderError{Code: "math.source.too_large", Message: "math source exceeds max bytes"}
	}
	if renderer.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, renderer.timeout)
		defer cancel()
	}
	var response mathJaxWorkerResponse
	if err := renderer.supervisor.Do(ctx, "mathjax.render", request, &response); err != nil {
		return Result{}, warmMathJaxError(ctx, err)
	}
	if !response.OK {
		return Result{}, &RenderError{Code: firstNonEmpty(response.Code, "mathjax.render.failed"), Message: response.Message}
	}
	if strings.TrimSpace(response.HTML) == "" {
		return Result{}, &RenderError{Code: "mathjax.output.empty", Message: "MathJax renderer returned empty HTML"}
	}
	return Result{HTML: response.HTML, Engine: firstNonEmpty(response.Engine, "mathjax-chtml"), Version: response.Version, CSS: response.CSS}, nil
}

func (renderer *WarmMathJaxRenderer) RenderBatch(ctx context.Context, requests []Request) ([]BatchResult, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	results := make([]BatchResult, len(requests))
	filtered := make([]Request, 0, len(requests))
	indexes := make([]int, 0, len(requests))
	for index, request := range requests {
		source := strings.TrimSpace(request.Source)
		if source == "" {
			results[index].Err = &RenderError{Code: "math.source.empty", Message: "math source is empty"}
			continue
		}
		if renderer.maxSourceBytes > 0 && int64(len([]byte(source))) > renderer.maxSourceBytes {
			results[index].Err = &RenderError{Code: "math.source.too_large", Message: "math source exceeds max bytes"}
			continue
		}
		request.Source = source
		filtered = append(filtered, request)
		indexes = append(indexes, index)
	}
	if len(filtered) == 0 {
		return results, nil
	}
	if renderer.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, renderer.timeout)
		defer cancel()
	}
	var response mathJaxBatchWorkerResponse
	err := renderer.supervisor.Do(ctx, "mathjax.render_batch", batchWorkerRequest{Requests: filtered}, &response)
	if err != nil {
		return nil, warmMathJaxError(ctx, err)
	}
	if !response.OK {
		return nil, &RenderError{Code: firstNonEmpty(response.Code, "mathjax.render.failed"), Message: response.Message}
	}
	if len(response.Results) != len(filtered) {
		return nil, &RenderError{Code: "mathjax.output.invalid", Message: "MathJax renderer returned wrong batch result count"}
	}
	engine := firstNonEmpty(response.Engine, "mathjax-chtml")
	for index, item := range response.Results {
		target := indexes[index]
		if !item.OK {
			results[target].Err = &RenderError{Code: firstNonEmpty(item.Code, "mathjax.render.failed"), Message: item.Message}
			continue
		}
		if strings.TrimSpace(item.HTML) == "" {
			results[target].Err = &RenderError{Code: "mathjax.output.empty", Message: "MathJax renderer returned empty HTML"}
			continue
		}
		results[target].Result = Result{HTML: item.HTML, Engine: engine, Version: response.Version, CSS: response.CSS}
	}
	return results, nil
}

func (renderer *WarmMathJaxRenderer) Ready(ctx context.Context) error {
	return renderer.supervisor.Ready(ctx)
}

func (renderer *WarmMathJaxRenderer) Snapshot() nodeworker.Snapshot {
	return renderer.supervisor.Snapshot()
}

func (renderer *WarmMathJaxRenderer) Close() error {
	return renderer.supervisor.Close()
}

func warmMathJaxError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &RenderError{Code: "mathjax.timeout", Message: err.Error()}
	}
	if errors.Is(err, nodeworker.ErrRequestTooLarge) {
		return &RenderError{Code: "mathjax.request.too_large", Message: err.Error()}
	}
	if errors.Is(err, nodeworker.ErrResponseTooLarge) {
		return &RenderError{Code: "mathjax.output.too_large", Message: err.Error()}
	}
	var remote *nodeworker.RemoteError
	if errors.As(err, &remote) {
		return &RenderError{Code: firstNonEmpty(remote.Code, "mathjax.worker.failed"), Message: remote.Message}
	}
	return &RenderError{Code: "mathjax.worker.failed", Message: err.Error()}
}
