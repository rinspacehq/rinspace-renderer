package pdfexecutor

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

func TestBrokerClientUsesFixedAuthenticatedPDFRoutes(t *testing.T) {
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
		fmt.Fprintf(response, `{"id":%q,"profile":"pdf-v1","state":"exited","running":false,"exitCode":137,"oomKilled":true,"image":"registry.invalid/pdf@sha256:%s","containerId":"ignored"}`,
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
		"POST /v1/workloads/pdf/" + testJobID + "/start",
		"GET /v1/workloads/pdf/" + testJobID,
		"POST /v1/workloads/pdf/" + testJobID + "/stop",
		"DELETE /v1/workloads/pdf/" + testJobID,
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
		url        string
		credential string
	}{
		{url: "http://example.com:8091", credential: strings.Repeat("k", 32)},
		{url: "http://127.0.0.1:8091/private", credential: strings.Repeat("k", 32)},
		{url: "http://127.0.0.1:8091", credential: "short"},
	} {
		if _, err := NewBrokerClient(testCase.url, testCase.credential, &http.Client{Timeout: time.Second}); err == nil {
			t.Fatalf("NewBrokerClient(%q) accepted invalid configuration", testCase.url)
		}
	}
}
