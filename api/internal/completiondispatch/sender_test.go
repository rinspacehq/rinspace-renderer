package completiondispatch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPSenderSignsKeyedEnvelopeAndRequires202(t *testing.T) {
	now := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	key := []byte(strings.Repeat("k", 32))
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if request.Header.Get("X-Rin-Key-ID") != "renderer-2026-08" || request.Header.Get("X-Rin-Service") != "rin-renderer" {
			t.Fatalf("signing identity headers = %#v", request.Header)
		}
		bodyHash := sha256.Sum256(body)
		canonical := strings.Join([]string{
			request.Method, request.URL.RequestURI(), request.Header.Get("X-Rin-Timestamp"),
			request.Header.Get("X-Rin-Nonce"), hex.EncodeToString(bodyHash[:]), request.Header.Get("X-Rin-Key-ID"),
		}, "\n")
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(canonical))
		if request.Header.Get("X-Rin-Signature") != hex.EncodeToString(mac.Sum(nil)) ||
			!strings.Contains(string(body), `"event_id":"renderer:11111111-1111-4111-8111-111111111111:completed:1"`) ||
			strings.Contains(string(body), "result_reference") {
			t.Fatalf("invalid signed completion request headers=%#v body=%s", request.Header, body)
		}
		response.WriteHeader(status)
	}))
	defer server.Close()
	sender, err := NewHTTPSender(HTTPSenderConfig{
		Endpoint: server.URL + "/internal/v1/events/renderer", KeyID: "renderer-2026-08", Key: key,
		Client: server.Client(), Clock: func() time.Time { return now }, Random: zeroReader{},
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery := testCompletionDelivery(now)
	delivery.SourceCommit = strings.Repeat("a", 40)
	delivery.ControlProjectHash = strings.Repeat("b", 64)
	delivery.RendererProjectHash = strings.Repeat("c", 64)
	if err := sender.Send(context.Background(), delivery); err == nil {
		t.Fatal("HTTP 200 was acknowledged as durable acceptance")
	}
	status = http.StatusAccepted
	if err := sender.Send(context.Background(), delivery); err != nil {
		t.Fatalf("HTTP 202 delivery: %v", err)
	}
}

func TestHTTPSenderHonorsRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)
	now := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	sender, err := NewHTTPSender(HTTPSenderConfig{
		Endpoint: server.URL, KeyID: "current", Key: []byte(strings.Repeat("k", 32)), Client: server.Client(),
		Clock: func() time.Time { return now }, Random: zeroReader{},
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery := testCompletionDelivery(now)
	delivery.SourceCommit = strings.Repeat("a", 40)
	delivery.ControlProjectHash = strings.Repeat("b", 64)
	delivery.RendererProjectHash = strings.Repeat("c", 64)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := sender.Send(ctx, delivery); err == nil {
		t.Fatal("timed-out completion delivery succeeded")
	}
}
