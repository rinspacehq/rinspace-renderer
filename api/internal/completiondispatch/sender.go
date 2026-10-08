package completiondispatch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
)

var keyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type Sender interface {
	Send(context.Context, jobpostgres.CompletionDelivery) error
}

type HTTPSenderConfig struct {
	Endpoint string
	KeyID    string
	Key      []byte
	Client   *http.Client
	Clock    func() time.Time
	Random   io.Reader
}

type HTTPSender struct {
	endpoint string
	keyID    string
	key      []byte
	client   *http.Client
	clock    func() time.Time
	random   io.Reader
}

func NewHTTPSender(config HTTPSenderConfig) (*HTTPSender, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, errors.New("Renderer completion endpoint is invalid")
	}
	if !keyIDPattern.MatchString(config.KeyID) || len(config.Key) < 32 {
		return nil, errors.New("Renderer completion signing identity is invalid")
	}
	if config.Client == nil {
		config.Client = http.DefaultClient
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &HTTPSender{
		endpoint: endpoint.String(), keyID: config.KeyID, key: append([]byte(nil), config.Key...),
		client: config.Client, clock: config.Clock, random: config.Random,
	}, nil
}

func (sender *HTTPSender) Send(ctx context.Context, delivery jobpostgres.CompletionDelivery) error {
	eventID, err := contracts.ControlPlaneCompletionEventID(delivery.JobID, int(delivery.TerminalVersion))
	if err != nil || eventID != delivery.EventID {
		return errors.New("Renderer completion delivery identity is invalid")
	}
	event := contracts.ControlPlaneCompletionEvent{
		Schema: contracts.ControlPlaneEventSchemaV1, EventID: delivery.EventID,
		Type: contracts.ControlPlaneCompletionEventType, Producer: "rin-renderer",
		OccurredAt: delivery.OccurredAt.UTC().Format(time.RFC3339Nano), CorrelationID: delivery.RequestID,
		Subject: contracts.ControlPlaneCompletionSubject{RendererJobID: delivery.JobID},
		Data: contracts.ControlPlaneCompletionData{
			SchemaVersion: delivery.SchemaVersion, ProtocolVersion: delivery.ProtocolVersion,
			TerminalVersion: int(delivery.TerminalVersion), RendererJobID: delivery.JobID,
			RequestID: delivery.RequestID, ControlProjectID: delivery.ControlProjectID,
			SourceCommit: delivery.SourceCommit, ControlProjectHash: delivery.ControlProjectHash,
			RendererProjectHash: delivery.RendererProjectHash, TerminalState: delivery.TerminalState,
			ResultHash: delivery.ResultHash, FailureCode: delivery.FailureCode,
		},
	}
	if err := contracts.ValidateControlPlaneCompletionEvent(event); err != nil {
		return fmt.Errorf("validate Renderer completion delivery: %w", err)
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode Renderer completion delivery: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, sender.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("create Renderer completion request")
	}
	if err := sender.sign(request, body); err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := sender.client.Do(request)
	if err != nil {
		return errors.New("Renderer completion endpoint unavailable")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("Renderer completion endpoint returned status %d", response.StatusCode)
	}
	return nil
}

func (sender *HTTPSender) sign(request *http.Request, body []byte) error {
	nonceBytes := make([]byte, 18)
	if _, err := io.ReadFull(sender.random, nonceBytes); err != nil {
		return errors.New("generate Renderer completion nonce")
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	timestamp := strconv.FormatInt(sender.clock().UTC().Unix(), 10)
	bodyHash := sha256.Sum256(body)
	canonical := strings.Join([]string{
		request.Method, request.URL.RequestURI(), timestamp, nonce, hex.EncodeToString(bodyHash[:]), sender.keyID,
	}, "\n")
	mac := hmac.New(sha256.New, sender.key)
	_, _ = mac.Write([]byte(canonical))
	request.Header.Set("X-Rin-Service", "rin-renderer")
	request.Header.Set("X-Rin-Key-ID", sender.keyID)
	request.Header.Set("X-Rin-Timestamp", timestamp)
	request.Header.Set("X-Rin-Nonce", nonce)
	request.Header.Set("X-Rin-Signature", hex.EncodeToString(mac.Sum(nil)))
	return nil
}
