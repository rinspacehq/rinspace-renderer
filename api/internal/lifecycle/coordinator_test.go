package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestShutdownStopsAdmissionAndDrains(t *testing.T) {
	coordinator := NewCoordinator()
	_, done, err := coordinator.BeginWorker(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		done()
	}()
	drained, err := coordinator.Shutdown(context.Background(), time.Second)
	if err != nil || !drained {
		t.Fatalf("Shutdown() = %v, %v", drained, err)
	}
	if err := coordinator.Ready(context.Background()); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Ready() error = %v", err)
	}
	if _, _, err := coordinator.BeginWorker(context.Background()); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("BeginWorker() error = %v", err)
	}
}

func TestShutdownCancelsWorkersAfterBound(t *testing.T) {
	coordinator := NewCoordinator()
	worker, done, err := coordinator.BeginWorker(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	drained, err := coordinator.Shutdown(context.Background(), 20*time.Millisecond)
	if err != nil || drained {
		t.Fatalf("Shutdown() = %v, %v", drained, err)
	}
	select {
	case <-worker.Done():
	case <-time.After(time.Second):
		t.Fatal("worker context was not canceled")
	}
}
