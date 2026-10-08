package diagramengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Request struct {
	Type    string `json:"type,omitempty"`
	Options string `json:"options,omitempty"`
	Body    string `json:"body,omitempty"`
	Source  string `json:"source,omitempty"`
}

type Diagnostic struct {
	Severity string            `json:"severity"`
	Code     string            `json:"code"`
	Message  string            `json:"message"`
	Source   map[string]string `json:"source,omitempty"`
}

type Response struct {
	ID          string       `json:"id"`
	URL         string       `json:"url"`
	SVG         string       `json:"svg"`
	Cached      bool         `json:"cached"`
	Type        string       `json:"type"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

type HTTPWorker struct {
	Endpoint   string
	Token      string
	HTTPClient *http.Client
}

func (w HTTPWorker) Render(ctx context.Context, req Request) (Response, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(w.Endpoint), "/")
	if endpoint == "" {
		return Response{}, errors.New("RIN_RENDERER_TEXSVG_ENDPOINT is required")
	}
	kind := strings.TrimSpace(req.Type)
	if kind == "" {
		return Response{}, errors.New("diagram type is required")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	upstreamURL := endpoint + "/" + url.PathEscape(kind)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if strings.TrimSpace(w.Token) != "" {
		httpReq.Header.Set("X-Rin-Renderer-Token", strings.TrimSpace(w.Token))
		httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(w.Token))
	}

	resp, err := w.httpClient().Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("diagram worker failed: %s %s", resp.Status, strings.TrimSpace(string(respBody)))
	}

	var rendered Response
	if err := json.Unmarshal(respBody, &rendered); err != nil {
		return Response{}, fmt.Errorf("diagram worker returned invalid JSON: %w", err)
	}
	rendered.SVG = strings.TrimSpace(rendered.SVG)
	if rendered.SVG == "" {
		return Response{}, errors.New("diagram worker returned empty SVG")
	}
	return rendered, nil
}

func (w HTTPWorker) httpClient() *http.Client {
	if w.HTTPClient != nil {
		return w.HTTPClient
	}
	return http.DefaultClient
}
