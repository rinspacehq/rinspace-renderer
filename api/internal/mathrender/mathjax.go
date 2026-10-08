package mathrender

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/processcontrol"
)

type MathJaxRenderer struct {
	NodeBin        string
	ScriptPath     string
	Timeout        time.Duration
	MaxSourceBytes int64
}

func NewMathJaxRenderer(scriptPath string, timeout time.Duration, maxSourceBytes int64) MathJaxRenderer {
	return MathJaxRenderer{
		NodeBin:        "node",
		ScriptPath:     scriptPath,
		Timeout:        timeout,
		MaxSourceBytes: maxSourceBytes,
	}
}

func (r MathJaxRenderer) Render(ctx context.Context, request Request) (Result, error) {
	source := strings.TrimSpace(request.Source)
	if source == "" {
		return Result{}, &RenderError{Code: "math.source.empty", Message: "math source is empty"}
	}
	if r.MaxSourceBytes > 0 && int64(len([]byte(source))) > r.MaxSourceBytes {
		return Result{}, &RenderError{Code: "math.source.too_large", Message: "math source exceeds max bytes"}
	}
	scriptPath := strings.TrimSpace(r.ScriptPath)
	if scriptPath == "" {
		return Result{}, &RenderError{Code: "mathjax.script.missing", Message: "MathJax renderer script is not configured"}
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
		return Result{}, fmt.Errorf("marshal mathjax render request: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.Command(nodeBin, scriptPath)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = processcontrol.Run(ctx, cmd, processcontrol.DefaultGrace)
	if ctx.Err() != nil {
		return Result{}, &RenderError{Code: "mathjax.timeout", Message: ctx.Err().Error()}
	}

	response, parseErr := parseMathJaxWorkerResponse(stdout.Bytes())
	if err != nil {
		if parseErr == nil && !response.OK {
			return Result{}, &RenderError{Code: firstNonEmpty(response.Code, "mathjax.render.failed"), Message: response.Message}
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return Result{}, &RenderError{Code: "mathjax.worker.failed", Message: message}
	}
	if parseErr != nil {
		return Result{}, parseErr
	}
	if !response.OK {
		return Result{}, &RenderError{Code: firstNonEmpty(response.Code, "mathjax.render.failed"), Message: response.Message}
	}
	if strings.TrimSpace(response.HTML) == "" {
		return Result{}, &RenderError{Code: "mathjax.output.empty", Message: "MathJax renderer returned empty HTML"}
	}
	return Result{
		HTML:    response.HTML,
		Engine:  firstNonEmpty(response.Engine, "mathjax-chtml"),
		Version: response.Version,
		CSS:     response.CSS,
	}, nil
}

func (r MathJaxRenderer) RenderBatch(ctx context.Context, requests []Request) ([]BatchResult, error) {
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
		return nil, &RenderError{Code: "mathjax.script.missing", Message: "MathJax renderer script is not configured"}
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
		return nil, fmt.Errorf("marshal mathjax batch render request: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.Command(nodeBin, scriptPath)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = processcontrol.Run(ctx, cmd, processcontrol.DefaultGrace)
	if ctx.Err() != nil {
		return nil, &RenderError{Code: "mathjax.timeout", Message: ctx.Err().Error()}
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, &RenderError{Code: "mathjax.worker.failed", Message: message}
	}

	response, parseErr := parseMathJaxBatchWorkerResponse(stdout.Bytes())
	if parseErr != nil {
		return nil, parseErr
	}
	if !response.OK {
		return nil, &RenderError{Code: firstNonEmpty(response.Code, "mathjax.render.failed"), Message: response.Message}
	}
	if len(response.Results) != len(filtered) {
		return nil, &RenderError{Code: "mathjax.output.invalid", Message: "MathJax renderer returned wrong batch result count"}
	}
	engine := firstNonEmpty(response.Engine, "mathjax-chtml")
	for index, item := range response.Results {
		target := filteredIndexes[index]
		if !item.OK {
			results[target].Err = &RenderError{Code: firstNonEmpty(item.Code, "mathjax.render.failed"), Message: item.Message}
			continue
		}
		if strings.TrimSpace(item.HTML) == "" {
			results[target].Err = &RenderError{Code: "mathjax.output.empty", Message: "MathJax renderer returned empty HTML"}
			continue
		}
		results[target].Result = Result{
			HTML:    item.HTML,
			Engine:  engine,
			Version: response.Version,
			CSS:     response.CSS,
		}
	}
	return results, nil
}

func DefaultMathJaxScriptPath() string {
	if configured := strings.TrimSpace(os.Getenv("RIN_RENDERER_MATHJAX_SCRIPT")); configured != "" {
		return configured
	}
	for _, candidate := range []string{
		"../engines/mathjax/render-mathjax.mjs",
		"../../engines/mathjax/render-mathjax.mjs",
		"../../../engines/mathjax/render-mathjax.mjs",
		"rin-renderer/engines/mathjax/render-mathjax.mjs",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Clean("../engines/mathjax/render-mathjax.mjs")
}

type mathJaxWorkerResponse struct {
	OK      bool   `json:"ok"`
	Engine  string `json:"engine"`
	Version string `json:"version"`
	HTML    string `json:"html"`
	CSS     string `json:"css"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type mathJaxBatchWorkerResponse struct {
	OK      bool                     `json:"ok"`
	Engine  string                   `json:"engine"`
	Version string                   `json:"version"`
	CSS     string                   `json:"css"`
	Results []mathJaxBatchWorkerItem `json:"results"`
	Code    string                   `json:"code"`
	Message string                   `json:"message"`
}

type mathJaxBatchWorkerItem struct {
	OK      bool   `json:"ok"`
	HTML    string `json:"html"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func parseMathJaxWorkerResponse(body []byte) (mathJaxWorkerResponse, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return mathJaxWorkerResponse{}, &RenderError{Code: "mathjax.output.empty", Message: "MathJax renderer returned no output"}
	}
	var response mathJaxWorkerResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return mathJaxWorkerResponse{}, fmt.Errorf("parse mathjax renderer output: %w", err)
	}
	return response, nil
}

func parseMathJaxBatchWorkerResponse(body []byte) (mathJaxBatchWorkerResponse, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return mathJaxBatchWorkerResponse{}, &RenderError{Code: "mathjax.output.empty", Message: "MathJax renderer returned no output"}
	}
	var response mathJaxBatchWorkerResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return mathJaxBatchWorkerResponse{}, fmt.Errorf("parse mathjax renderer batch output: %w", err)
	}
	return response, nil
}
