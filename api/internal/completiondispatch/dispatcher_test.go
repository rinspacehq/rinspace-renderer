package completiondispatch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
)

type memoryCompletionRepository struct {
	mutex        sync.Mutex
	delivery     jobpostgres.CompletionDelivery
	available    bool
	leaseToken   string
	acknowledged int
	failed       int
	dead         int
}

func (repository *memoryCompletionRepository) ClaimCompletion(_ context.Context, input jobpostgres.ClaimCompletionInput) (jobpostgres.CompletionDelivery, error) {
	repository.mutex.Lock()
	defer repository.mutex.Unlock()
	if !repository.available {
		return jobpostgres.CompletionDelivery{}, jobpostgres.ErrNoCompletionDelivery
	}
	repository.available = false
	repository.leaseToken = input.LeaseTokenHash
	repository.delivery.AttemptCount++
	return repository.delivery, nil
}

func (repository *memoryCompletionRepository) AckCompletion(_ context.Context, _ string, token string, _ time.Time) error {
	repository.mutex.Lock()
	defer repository.mutex.Unlock()
	if token != repository.leaseToken {
		return jobpostgres.ErrCompletionLeaseLost
	}
	repository.acknowledged++
	repository.leaseToken = ""
	return nil
}

func (repository *memoryCompletionRepository) FailCompletion(_ context.Context, _ string, token string, _ time.Time, code string, maxAttempts int) (string, error) {
	repository.mutex.Lock()
	defer repository.mutex.Unlock()
	if token != repository.leaseToken || code != "control_plane_unavailable" {
		return "", jobpostgres.ErrCompletionLeaseLost
	}
	repository.failed++
	repository.leaseToken = ""
	if repository.delivery.AttemptCount >= maxAttempts {
		repository.dead++
		return "dead_letter", nil
	}
	repository.available = true
	return "failed", nil
}

type sequenceSender struct {
	mutex  sync.Mutex
	errors []error
	calls  int
}

func (sender *sequenceSender) Send(context.Context, jobpostgres.CompletionDelivery) error {
	sender.mutex.Lock()
	defer sender.mutex.Unlock()
	index := sender.calls
	sender.calls++
	if index < len(sender.errors) {
		return sender.errors[index]
	}
	return nil
}

func TestDispatcherRetriesAcrossRestartAndAcknowledgesOnlyAcceptance(t *testing.T) {
	now := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	repository := &memoryCompletionRepository{delivery: testCompletionDelivery(now), available: true}
	sender := &sequenceSender{errors: []error{errors.New("Control Plane restarting"), nil}}
	newDispatcher := func() *Dispatcher {
		dispatcher, err := New(repository, sender, Config{
			PollInterval: time.Millisecond, LeaseDuration: time.Minute, RequestTimeout: 5 * time.Second,
			MaxAttempts: 3, Clock: func() time.Time { return now }, Random: zeroReader{},
			Operations: operational.New(io.Discard),
		})
		if err != nil {
			t.Fatal(err)
		}
		return dispatcher
	}
	first := newDispatcher()
	if delivered, err := first.dispatchOne(context.Background()); err != nil || delivered || repository.failed != 1 || repository.acknowledged != 0 {
		t.Fatalf("first delivery = delivered %v, failed %d, ack %d, %v", delivered, repository.failed, repository.acknowledged, err)
	}
	second := newDispatcher()
	if delivered, err := second.dispatchOne(context.Background()); err != nil || !delivered || repository.failed != 1 || repository.acknowledged != 1 {
		t.Fatalf("restarted delivery = delivered %v, failed %d, ack %d, %v", delivered, repository.failed, repository.acknowledged, err)
	}
}

func TestDispatcherDeadLettersAtConfiguredBound(t *testing.T) {
	now := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	repository := &memoryCompletionRepository{delivery: testCompletionDelivery(now), available: true}
	sender := &sequenceSender{errors: []error{errors.New("unavailable")}}
	var logs bytes.Buffer
	dispatcher, err := New(repository, sender, Config{
		PollInterval: time.Millisecond, LeaseDuration: time.Minute, RequestTimeout: time.Second,
		MaxAttempts: 1, Clock: func() time.Time { return now }, Random: zeroReader{},
		Operations: operational.New(&logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	if delivered, err := dispatcher.dispatchOne(context.Background()); err != nil || delivered || repository.dead != 1 || repository.acknowledged != 0 {
		t.Fatalf("dead-letter delivery = delivered %v, dead %d, ack %d, %v", delivered, repository.dead, repository.acknowledged, err)
	}
	for _, forbidden := range []string{repository.delivery.JobID, repository.delivery.ControlProjectID, repository.delivery.SourceCommit, repository.delivery.ResultReference} {
		if forbidden != "" && strings.Contains(logs.String(), forbidden) {
			t.Fatalf("completion dispatcher log leaked delivery identity %q: %s", forbidden, logs.String())
		}
	}
}

func TestDispatcherLifecycleReadiness(t *testing.T) {
	repository := &memoryCompletionRepository{}
	dispatcher, err := New(repository, &sequenceSender{}, Config{
		PollInterval: time.Millisecond, LeaseDuration: time.Minute, RequestTimeout: time.Second,
		MaxAttempts: 3, Operations: operational.New(io.Discard),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Ready(context.Background()); err == nil {
		t.Fatal("dispatcher was ready before Run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for dispatcher.Ready(context.Background()) != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := dispatcher.Ready(context.Background()); err != nil {
		t.Fatalf("running dispatcher readiness: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stop dispatcher: %v", err)
	}
	if err := dispatcher.Ready(context.Background()); err == nil {
		t.Fatal("dispatcher stayed ready after shutdown")
	}
}

type zeroReader struct{}

func (zeroReader) Read(body []byte) (int, error) {
	for index := range body {
		body[index] = byte(index + 1)
	}
	return len(body), nil
}

func testCompletionDelivery(now time.Time) jobpostgres.CompletionDelivery {
	return jobpostgres.CompletionDelivery{
		EventID: "renderer:11111111-1111-4111-8111-111111111111:completed:1",
		JobID:   "11111111-1111-4111-8111-111111111111", SchemaVersion: "rin-renderer-completion/v2",
		ProtocolVersion: "v2", TerminalVersion: 1,
		RequestID: "render-11111111111111111111111111111111", ControlProjectID: "article:42",
		SourceCommit: "", ControlProjectHash: "", RendererProjectHash: "",
		TerminalState: "failed", FailureCode: "document_render_failed", OccurredAt: now,
	}
}
