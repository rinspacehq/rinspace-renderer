package jobworker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestHTTPCompletionNotifierSendsExactIdentityEnvelope(t *testing.T) {
	var envelope map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		for _, header := range []string{"X-Rin-Service", "X-Rin-Timestamp", "X-Rin-Nonce", "X-Rin-Signature"} {
			if request.Header.Get(header) == "" {
				t.Fatalf("missing signed callback header %s", header)
			}
		}
		if request.Header.Get("X-Rin-Service") != "rin-renderer" {
			t.Fatalf("unexpected callback service %q", request.Header.Get("X-Rin-Service"))
		}
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		response.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	completion := Completion{
		RendererJobID: "renderer-job", SourceCommit: strings.Repeat("a", 40), ControlProjectHash: strings.Repeat("b", 64), ResultHash: strings.Repeat("c", 64),
		Result: map[string]any{"schemaVersion": contracts.RenderResultSchemaVersion},
	}
	notifier := HTTPCompletionNotifier{Endpoint: server.URL + "/internal/v1/events/renderer", Key: []byte(strings.Repeat("k", 32)), Client: server.Client()}
	if err := notifier.Notify(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	data, _ := envelope["data"].(map[string]any)
	if envelope["type"] != "renderer.job.succeeded" || data["sourceCommit"] != completion.SourceCommit || data["projectHash"] != completion.ControlProjectHash || data["resultHash"] != completion.ResultHash {
		t.Fatalf("callback lost exact identity: %#v", envelope)
	}
}
