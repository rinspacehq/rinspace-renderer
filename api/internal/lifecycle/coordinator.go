package lifecycle

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrShuttingDown = errors.New("renderer is shutting down")

type Coordinator struct {
	mu        sync.Mutex
	accepting bool
	active    int
	idle      chan struct{}
	workers   context.Context
	cancel    context.CancelFunc
}

func NewCoordinator() *Coordinator {
	workers, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &Coordinator{accepting: true, idle: idle, workers: workers, cancel: cancel}
}

// Ready implements admission.Readiness without importing the admission package.
func (coordinator *Coordinator) Ready(context.Context) error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if !coordinator.accepting {
		return ErrShuttingDown
	}
	return nil
}

// BeginWorker registers accepted work and returns a context canceled when bounded drain expires.
func (coordinator *Coordinator) BeginWorker(parent context.Context) (context.Context, func(), error) {
	coordinator.mu.Lock()
	if !coordinator.accepting {
		coordinator.mu.Unlock()
		return nil, nil, ErrShuttingDown
	}
	if coordinator.active == 0 {
		coordinator.idle = make(chan struct{})
	}
	coordinator.active++
	workerRoot := coordinator.workers
	coordinator.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	stopRoot := context.AfterFunc(workerRoot, cancel)
	var once sync.Once
	done := func() {
		once.Do(func() {
			stopRoot()
			cancel()
			coordinator.mu.Lock()
			coordinator.active--
			if coordinator.active == 0 {
				close(coordinator.idle)
			}
			coordinator.mu.Unlock()
		})
	}
	return ctx, done, nil
}

// Shutdown rejects new work immediately. It returns true if existing work drains within grace;
// otherwise it cancels all registered worker contexts and returns false.
func (coordinator *Coordinator) Shutdown(ctx context.Context, grace time.Duration) (bool, error) {
	if grace <= 0 {
		return false, errors.New("renderer shutdown grace must be positive")
	}
	coordinator.mu.Lock()
	coordinator.accepting = false
	idle := coordinator.idle
	coordinator.mu.Unlock()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-idle:
		coordinator.cancel()
		return true, nil
	case <-timer.C:
		coordinator.cancel()
		return false, nil
	case <-ctx.Done():
		coordinator.cancel()
		return false, ctx.Err()
	}
}
