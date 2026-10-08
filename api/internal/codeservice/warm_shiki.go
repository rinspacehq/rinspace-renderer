package codeservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/nodeworker"
)

const (
	shikiBatchContract  = "rin-shiki-batch/v1"
	shikiResultContract = "rin-shiki-result/v1"
)

type WarmShikiConfig struct {
	NodeBin          string
	ScriptPath       string
	Timeout          time.Duration
	Workers          int
	MaxRequestBytes  int64
	MaxResponseBytes int64
	MaxTasks         uint64
	MaxRSSBytes      uint64
	StartTimeout     time.Duration
	StopGrace        time.Duration
	Environment      map[string]string
}

type WarmShikiRenderer struct {
	timeout    time.Duration
	supervisor *nodeworker.Supervisor
}

type shikiBatchRequest struct {
	ContractVersion string          `json:"contractVersion"`
	Theme           string          `json:"theme"`
	Items           []RenderRequest `json:"items"`
}

type shikiBatchResponse struct {
	ContractVersion string         `json:"contractVersion"`
	Engine          string         `json:"engine"`
	EngineVersion   string         `json:"engineVersion"`
	Theme           string         `json:"theme"`
	Items           []RenderResult `json:"items"`
}

func NewWarmShikiRenderer(config WarmShikiConfig) (*WarmShikiRenderer, error) {
	supervisor, err := nodeworker.New(nodeworker.Config{
		Command: config.NodeBin, Args: []string{config.ScriptPath, "--ndjson-worker"},
		Dir: nodeworker.ScriptDir(config.ScriptPath), Environment: config.Environment,
		Workers: config.Workers, MaxRequestBytes: config.MaxRequestBytes, MaxResponseBytes: config.MaxResponseBytes,
		MaxTasks: config.MaxTasks, MaxRSSBytes: config.MaxRSSBytes, StartTimeout: config.StartTimeout, StopGrace: config.StopGrace,
	})
	if err != nil {
		return nil, err
	}
	return &WarmShikiRenderer{timeout: config.Timeout, supervisor: supervisor}, nil
}

func (renderer *WarmShikiRenderer) RenderBatch(ctx context.Context, theme string, requests []RenderRequest) ([]RenderResult, error) {
	if renderer == nil || renderer.supervisor == nil {
		return nil, errors.New("Shiki renderer is not configured")
	}
	if len(requests) == 0 {
		return nil, nil
	}
	if renderer.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, renderer.timeout)
		defer cancel()
	}
	var response shikiBatchResponse
	err := renderer.supervisor.Do(ctx, "shiki.render-batch", shikiBatchRequest{
		ContractVersion: shikiBatchContract, Theme: theme, Items: requests,
	}, &response)
	if err != nil {
		return nil, warmShikiError(ctx, err)
	}
	if response.ContractVersion != shikiResultContract || response.Engine != "shiki" ||
		strings.TrimSpace(response.EngineVersion) == "" || response.Theme != theme || len(response.Items) != len(requests) {
		return nil, errors.New("Shiki worker returned an invalid batch contract")
	}
	return response.Items, nil
}

func (renderer *WarmShikiRenderer) Ready(ctx context.Context) error {
	return renderer.supervisor.Ready(ctx)
}
func (renderer *WarmShikiRenderer) Snapshot() nodeworker.Snapshot {
	return renderer.supervisor.Snapshot()
}
func (renderer *WarmShikiRenderer) Close() error { return renderer.supervisor.Close() }

func warmShikiError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("shiki.timeout: %w", err)
	}
	var remote *nodeworker.RemoteError
	if errors.As(err, &remote) {
		return fmt.Errorf("%s: %s", firstNonEmpty(remote.Code, "shiki.worker.failed"), remote.Message)
	}
	return fmt.Errorf("shiki.worker.failed: %w", err)
}
