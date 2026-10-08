package mathrender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/processcontrol"
)

type Request struct {
	Source      string            `json:"source"`
	DisplayMode bool              `json:"displayMode"`
	InputFormat string            `json:"inputFormat,omitempty"`
	Macros      map[string]string `json:"macros,omitempty"`
}

type Result struct {
	HTML    string
	Engine  string
	Version string
	CSS     string
}

type BatchResult struct {
	Result Result
	Err    error
}

type Renderer interface {
	Render(ctx context.Context, request Request) (Result, error)
	RenderBatch(ctx context.Context, requests []Request) ([]BatchResult, error)
}

type KaTeXRenderer struct {
	NodeBin        string
	ScriptPath     string
	Timeout        time.Duration
	MaxSourceBytes int64
}

type RenderError struct {
	Code    string
	Message string
}

func (e *RenderError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func NewKaTeXRenderer(scriptPath string, timeout time.Duration, maxSourceBytes int64) KaTeXRenderer {
	return KaTeXRenderer{
		NodeBin:        "node",
		ScriptPath:     scriptPath,
		Timeout:        timeout,
		MaxSourceBytes: maxSourceBytes,
	}
}

func (r KaTeXRenderer) Render(ctx context.Context, request Request) (Result, error) {
	source := strings.TrimSpace(request.Source)
	if source == "" {
		return Result{}, &RenderError{Code: "math.source.empty", Message: "math source is empty"}
	}
	if r.MaxSourceBytes > 0 && int64(len([]byte(source))) > r.MaxSourceBytes {
		return Result{}, &RenderError{Code: "math.source.too_large", Message: "math source exceeds max bytes"}
	}
	scriptPath := strings.TrimSpace(r.ScriptPath)
	if scriptPath == "" {
		return Result{}, &RenderError{Code: "katex.script.missing", Message: "KaTeX renderer script is not configured"}
	}
	nodeBin := strings.TrimSpace(r.NodeBin)
	if nodeBin == "" {
		nodeBin = "node"
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	payload, err := json.Marshal(request)
	if err != nil {
		return Result{}, fmt.Errorf("marshal katex render request: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.Command(nodeBin, scriptPath)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = processcontrol.Run(ctx, cmd, processcontrol.DefaultGrace)
	if ctx.Err() != nil {
		return Result{}, &RenderError{Code: "katex.timeout", Message: ctx.Err().Error()}
	}

	response, parseErr := parseKaTeXWorkerResponse(stdout.Bytes())
	if err != nil {
		if parseErr == nil && !response.OK {
			return Result{}, &RenderError{Code: firstNonEmpty(response.Code, "katex.render.failed"), Message: response.Message}
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return Result{}, &RenderError{Code: "katex.worker.failed", Message: message}
	}
	if parseErr != nil {
		return Result{}, parseErr
	}
	if !response.OK {
		return Result{}, &RenderError{Code: firstNonEmpty(response.Code, "katex.render.failed"), Message: response.Message}
	}
	if strings.TrimSpace(response.HTML) == "" {
		return Result{}, &RenderError{Code: "katex.output.empty", Message: "KaTeX renderer returned empty HTML"}
	}
	return Result{
		HTML:    response.HTML,
		Engine:  firstNonEmpty(response.Engine, "katex"),
		Version: response.Version,
	}, nil
}

func (r KaTeXRenderer) RenderBatch(ctx context.Context, requests []Request) ([]BatchResult, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	results := make([]BatchResult, len(requests))
	filtered := make([]Request, 0, len(requests))
	filteredIndexes := make([]int, 0, len(requests))
	for index, request := range requests {
		source := strings.TrimSpace(request.Source)
		if source == "" {
			results[index].Err = &RenderError{Code: "math.source.empty", Message: "math source is empty"}
			continue
		}
		if r.MaxSourceBytes > 0 && int64(len([]byte(source))) > r.MaxSourceBytes {
			results[index].Err = &RenderError{Code: "math.source.too_large", Message: "math source exceeds max bytes"}
			continue
		}
		request.Source = source
		filtered = append(filtered, request)
		filteredIndexes = append(filteredIndexes, index)
	}
	if len(filtered) == 0 {
		return results, nil
	}
	scriptPath := strings.TrimSpace(r.ScriptPath)
	if scriptPath == "" {
		return nil, &RenderError{Code: "katex.script.missing", Message: "KaTeX renderer script is not configured"}
	}
	nodeBin := strings.TrimSpace(r.NodeBin)
	if nodeBin == "" {
		nodeBin = "node"
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	payload, err := json.Marshal(batchWorkerRequest{Requests: filtered})
	if err != nil {
		return nil, fmt.Errorf("marshal katex batch render request: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.Command(nodeBin, scriptPath)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = processcontrol.Run(ctx, cmd, processcontrol.DefaultGrace)
	if ctx.Err() != nil {
		return nil, &RenderError{Code: "katex.timeout", Message: ctx.Err().Error()}
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, &RenderError{Code: "katex.worker.failed", Message: message}
	}

	response, parseErr := parseKaTeXBatchWorkerResponse(stdout.Bytes())
	if parseErr != nil {
		return nil, parseErr
	}
	if !response.OK {
		return nil, &RenderError{Code: firstNonEmpty(response.Code, "katex.render.failed"), Message: response.Message}
	}
	if len(response.Results) != len(filtered) {
		return nil, &RenderError{Code: "katex.output.invalid", Message: "KaTeX renderer returned wrong batch result count"}
	}
	engine := firstNonEmpty(response.Engine, "katex")
	for index, item := range response.Results {
		target := filteredIndexes[index]
		if !item.OK {
			results[target].Err = &RenderError{Code: firstNonEmpty(item.Code, "katex.render.failed"), Message: item.Message}
			continue
		}
		if strings.TrimSpace(item.HTML) == "" {
			results[target].Err = &RenderError{Code: "katex.output.empty", Message: "KaTeX renderer returned empty HTML"}
			continue
		}
		results[target].Result = Result{
			HTML:    item.HTML,
			Engine:  engine,
			Version: response.Version,
		}
	}
	return results, nil
}

func DefaultKaTeXScriptPath() string {
	if configured := strings.TrimSpace(os.Getenv("RIN_RENDERER_KATEX_SCRIPT")); configured != "" {
		return configured
	}
	for _, candidate := range []string{
		"../engines/katex/render-katex.mjs",
		"../../engines/katex/render-katex.mjs",
		"../../../engines/katex/render-katex.mjs",
		"rin-renderer/engines/katex/render-katex.mjs",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Clean("../engines/katex/render-katex.mjs")
}

type katexWorkerResponse struct {
	OK      bool   `json:"ok"`
	Engine  string `json:"engine"`
	Version string `json:"version"`
	HTML    string `json:"html"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type batchWorkerRequest struct {
	Requests []Request `json:"requests"`
}

type katexBatchWorkerResponse struct {
	OK      bool                   `json:"ok"`
	Engine  string                 `json:"engine"`
	Version string                 `json:"version"`
	Results []katexBatchWorkerItem `json:"results"`
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
}

type katexBatchWorkerItem struct {
	OK      bool   `json:"ok"`
	HTML    string `json:"html"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func parseKaTeXWorkerResponse(body []byte) (katexWorkerResponse, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return katexWorkerResponse{}, &RenderError{Code: "katex.output.empty", Message: "KaTeX renderer returned no output"}
	}
	var response katexWorkerResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return katexWorkerResponse{}, fmt.Errorf("parse katex renderer output: %w", err)
	}
	return response, nil
}

func parseKaTeXBatchWorkerResponse(body []byte) (katexBatchWorkerResponse, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return katexBatchWorkerResponse{}, &RenderError{Code: "katex.output.empty", Message: "KaTeX renderer returned no output"}
	}
	var response katexBatchWorkerResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return katexBatchWorkerResponse{}, fmt.Errorf("parse katex renderer batch output: %w", err)
	}
	return response, nil
}

func IsRenderError(err error) bool {
	var renderErr *RenderError
	return errors.As(err, &renderErr)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
