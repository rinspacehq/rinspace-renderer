package jobworker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Completion struct {
	RendererJobID      string
	SourceCommit       string
	ControlProjectHash string
	ResultHash         string
	Result             map[string]any
}

type CompletionNotifier interface {
	Notify(context.Context, Completion) error
}

type HTTPCompletionNotifier struct {
	Endpoint string
	Key      []byte
	Client   *http.Client
}

func (notifier HTTPCompletionNotifier) Notify(ctx context.Context, completion Completion) error {
	if strings.TrimSpace(notifier.Endpoint) == "" || len(notifier.Key) < 32 {
		return errors.New("Control Plane callback configuration is invalid")
	}
	now := time.Now().UTC()
	payload := map[string]any{
		"schema": "rin-control-event/v1", "event_id": "renderer:" + completion.RendererJobID + ":succeeded",
		"type": "renderer.job.succeeded", "occurred_at": now.Format(time.RFC3339Nano), "producer": "rin-renderer",
		"subject": map[string]any{"rendererJobId": completion.RendererJobID},
		"data": map[string]any{
			"rendererJobId": completion.RendererJobID, "sourceCommit": completion.SourceCommit,
			"projectHash": completion.ControlProjectHash, "resultHash": completion.ResultHash, "result": completion.Result,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, notifier.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}
	nonce := hex.EncodeToString(nonceBytes)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	bodyHash := sha256.Sum256(body)
	canonical := strings.Join([]string{http.MethodPost, request.URL.RequestURI(), timestamp, nonce, hex.EncodeToString(bodyHash[:])}, "\n")
	mac := hmac.New(sha256.New, notifier.Key)
	_, _ = mac.Write([]byte(canonical))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Rin-Service", "rin-renderer")
	request.Header.Set("X-Rin-Timestamp", timestamp)
	request.Header.Set("X-Rin-Nonce", nonce)
	request.Header.Set("X-Rin-Signature", hex.EncodeToString(mac.Sum(nil)))
	client := notifier.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return errors.New("Control Plane rejected Renderer completion callback")
	}
	return nil
}
