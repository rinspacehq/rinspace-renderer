package typstpdfexecutor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBrokerClientUsesFixedAuthenticatedTypstRoutes(t *testing.T) {
	credential := strings.Repeat("k", 32)
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+credential {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		mu.Lock()
		requests = append(requests, request.Method+" "+request.URL.Path)
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(response, `{"id":%q,"profile":"typst-pdf-v1","state":"exited","running":false,"exitCode":137,"oomKilled":true,"image":"registry.invalid/typst-pdf@sha256:%s","containerId":"ignored"}`,
			testJobID, strings.Repeat("a", 64))
	}))
	defer server.Close()
	client, err := NewBrokerClient(server.URL, credential, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if inspection, err := client.Start(context.Background(), testJobID); err != nil || !inspection.OOMKilled || inspection.ExitCode != 137 {
		t.Fatalf("Start() = %#v, %v", inspection, err)
	}
	if _, err := client.Inspect(context.Background(), testJobID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stop(context.Background(), testJobID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Remove(context.Background(), testJobID); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /v1/workloads/typst-pdf/" + testJobID + "/start",
		"GET /v1/workloads/typst-pdf/" + testJobID,
		"POST /v1/workloads/typst-pdf/" + testJobID + "/stop",
		"DELETE /v1/workloads/typst-pdf/" + testJobID,
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("broker routes = %#v", requests)
	}
}

func TestBrokerClientRejectsRemoteAndInvalidConfiguration(t *testing.T) {
	if _, err := NewBrokerClient("http://workload-broker:8091", strings.Repeat("k", 32), &http.Client{Timeout: time.Second}); err != nil {
		t.Fatalf("compose broker host was rejected: %v", err)
	}
	for _, testCase := range []struct {
		name       string
		url        string
		credential string
		timeout    time.Duration
	}{
		{name: "remote-host", url: "http://example.com:8091", credential: strings.Repeat("k", 32), timeout: time.Second},
		{name: "private-path", url: "http://127.0.0.1:8091/private", credential: strings.Repeat("k", 32), timeout: time.Second},
		{name: "short-credential", url: "http://127.0.0.1:8091", credential: "short", timeout: time.Second},
		{name: "query", url: "http://127.0.0.1:8091?profile=typst-pdf-v1", credential: strings.Repeat("k", 32), timeout: time.Second},
		{name: "scheme", url: "file:///var/run/broker.sock", credential: strings.Repeat("k", 32), timeout: time.Second},
		{name: "long-timeout", url: "http://127.0.0.1:8091", credential: strings.Repeat("k", 32), timeout: time.Minute},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewBrokerClient(testCase.url, testCase.credential, &http.Client{Timeout: testCase.timeout}); err == nil {
				t.Fatalf("NewBrokerClient(%q) accepted invalid configuration", testCase.url)
			}
		})
	}
}

func TestBrokerClientRejectsInvalidTypstResponses(t *testing.T) {
	credential := strings.Repeat("k", 32)
	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "latex-profile", status: 200, body: `{"id":"` + testJobID + `","profile":"pdf-v1","state":"exited"}`},
		{name: "other-job", status: 200, body: `{"id":"018fcafe-1234-4abc-8def-1234567890ac","profile":"typst-pdf-v1","state":"exited"}`},
		{name: "missing-state", status: 200, body: `{"id":"` + testJobID + `","profile":"typst-pdf-v1","state":""}`},
		{name: "trailing-data", status: 200, body: `{"id":"` + testJobID + `","profile":"typst-pdf-v1","state":"exited"} {}`},
		{name: "broker-error", status: 503, body: `{"error":"broker_unavailable","reason":"no capacity"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.WriteHeader(testCase.status)
				fmt.Fprint(response, testCase.body)
			}))
			defer server.Close()
			client, err := NewBrokerClient(server.URL, credential, &http.Client{Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Inspect(context.Background(), testJobID); err == nil {
				t.Fatalf("Inspect() accepted %q", testCase.name)
			}
			if _, err := client.Inspect(context.Background(), "bad/id"); err == nil {
				t.Fatal("Inspect() accepted an invalid job id")
			}
		})
	}
}
