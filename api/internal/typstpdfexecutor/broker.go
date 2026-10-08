package typstpdfexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	brokerResponseLimit = 64 << 10
	brokerProfile       = "typst-pdf-v1"
	brokerRoutePrefix   = "/v1/workloads/typst-pdf"
)

// BrokerInspection is the fixed subset of a workload broker response the
// Typst PDF worker trusts. The broker owns the container lifecycle; the
// executor only verifies identity, image digest and terminal state.
type BrokerInspection struct {
	ID        string `json:"id"`
	Profile   string `json:"profile"`
	State     string `json:"state"`
	Running   bool   `json:"running"`
	ExitCode  int    `json:"exitCode"`
	OOMKilled bool   `json:"oomKilled"`
	Image     string `json:"image"`
}

type Broker interface {
	Start(context.Context, string) (BrokerInspection, error)
	Inspect(context.Context, string) (BrokerInspection, error)
	Stop(context.Context, string) (BrokerInspection, error)
	Remove(context.Context, string) (BrokerInspection, error)
}

// BrokerClient talks to the workload broker over its fixed, authenticated
// typst-pdf routes. It never accepts a broker URL, profile or command from a
// job or from source.
type BrokerClient struct {
	baseURL    *url.URL
	credential string
	client     *http.Client
}

func NewBrokerClient(rawURL, credential string, client *http.Client) (*BrokerClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("Typst PDF workload broker URL is invalid")
	}
	host := parsed.Hostname()
	address := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && !strings.EqualFold(host, "workload-broker") && (address == nil || !address.IsLoopback()) {
		return nil, errors.New("Typst PDF workload broker host is not allowed")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("Typst PDF workload broker URL cannot contain a path")
	}
	if len([]byte(credential)) < 32 || strings.ContainsAny(credential, "\r\n") {
		return nil, errors.New("Typst PDF workload broker credential is invalid")
	}
	if client == nil {
		client = &http.Client{Timeout: 40 * time.Second}
	}
	if client.Timeout <= 0 || client.Timeout > 45*time.Second {
		return nil, errors.New("Typst PDF workload broker timeout is invalid")
	}
	parsed.Path = ""
	return &BrokerClient{baseURL: parsed, credential: credential, client: client}, nil
}

func (client *BrokerClient) Start(ctx context.Context, jobID string) (BrokerInspection, error) {
	return client.request(ctx, http.MethodPost, jobID, "start")
}

func (client *BrokerClient) Inspect(ctx context.Context, jobID string) (BrokerInspection, error) {
	return client.request(ctx, http.MethodGet, jobID, "")
}

func (client *BrokerClient) Stop(ctx context.Context, jobID string) (BrokerInspection, error) {
	return client.request(ctx, http.MethodPost, jobID, "stop")
}

func (client *BrokerClient) Remove(ctx context.Context, jobID string) (BrokerInspection, error) {
	return client.request(ctx, http.MethodDelete, jobID, "")
}

func (client *BrokerClient) request(ctx context.Context, method, jobID, action string) (BrokerInspection, error) {
	if !jobIDPattern.MatchString(jobID) {
		return BrokerInspection{}, errors.New("Typst PDF workload broker job ID is invalid")
	}
	endpoint := *client.baseURL
	endpoint.Path = path.Join(brokerRoutePrefix, jobID, action)
	var body io.Reader
	if method != http.MethodGet {
		body = bytes.NewReader([]byte("{}"))
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return BrokerInspection{}, fmt.Errorf("create Typst PDF workload broker request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.credential)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.client.Do(request)
	if err != nil {
		return BrokerInspection{}, fmt.Errorf("call Typst PDF workload broker: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, brokerResponseLimit+1))
	if err != nil || len(responseBody) > brokerResponseLimit {
		return BrokerInspection{}, errors.New("Typst PDF workload broker response is invalid")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var envelope struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(responseBody, &envelope)
		code := strings.TrimSpace(envelope.Error)
		if code == "" {
			code = "broker_request_failed"
		}
		return BrokerInspection{}, fmt.Errorf("Typst PDF workload broker returned %d: %s", response.StatusCode, code)
	}
	var inspection BrokerInspection
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	if err := decoder.Decode(&inspection); err != nil || inspection.ID != jobID ||
		inspection.Profile != brokerProfile || strings.TrimSpace(inspection.State) == "" {
		return BrokerInspection{}, errors.New("Typst PDF workload broker inspection is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return BrokerInspection{}, errors.New("Typst PDF workload broker response contains trailing data")
	}
	return inspection, nil
}
