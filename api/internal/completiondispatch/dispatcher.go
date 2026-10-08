package completiondispatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
)

type Repository interface {
	ClaimCompletion(context.Context, jobpostgres.ClaimCompletionInput) (jobpostgres.CompletionDelivery, error)
	AckCompletion(context.Context, string, string, time.Time) error
	FailCompletion(context.Context, string, string, time.Time, string, int) (string, error)
}

type Config struct {
	PollInterval   time.Duration
	LeaseDuration  time.Duration
	RequestTimeout time.Duration
	MaxAttempts    int
	Clock          func() time.Time
	Random         io.Reader
	Operations     *operational.Registry
}

type Dispatcher struct {
	repository Repository
	sender     Sender
	config     Config
	ready      atomic.Bool
	running    atomic.Bool
}

func New(repository Repository, sender Sender, config Config) (*Dispatcher, error) {
	if repository == nil || sender == nil || config.PollInterval <= 0 || config.RequestTimeout <= 0 ||
		config.LeaseDuration <= config.RequestTimeout || config.LeaseDuration > 5*time.Minute ||
		config.MaxAttempts < 1 || config.MaxAttempts > 100 {
		return nil, errors.New("Renderer completion dispatcher configuration is invalid")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Operations == nil {
		config.Operations = operational.Default()
	}
	return &Dispatcher{repository: repository, sender: sender, config: config}, nil
}

func (dispatcher *Dispatcher) Run(ctx context.Context) error {
	if !dispatcher.running.CompareAndSwap(false, true) {
		return errors.New("Renderer completion dispatcher is already running")
	}
	defer dispatcher.running.Store(false)
	dispatcher.ready.Store(true)
	defer dispatcher.ready.Store(false)
	for {
		delivered, err := dispatcher.dispatchOne(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			dispatcher.config.Operations.Count("completion_dispatch_errors_total")
			dispatcher.config.Operations.Event("completion_dispatch_error", slog.String("failure_code", "completion_delivery_failed"))
		}
		if delivered {
			continue
		}
		if !wait(ctx, dispatcher.config.PollInterval) {
			return nil
		}
	}
}

func (dispatcher *Dispatcher) Ready(context.Context) error {
	if !dispatcher.ready.Load() {
		return errors.New("completion_dispatcher_unavailable")
	}
	return nil
}

func (dispatcher *Dispatcher) dispatchOne(ctx context.Context) (bool, error) {
	tokenBytes := make([]byte, 32)
	if _, err := io.ReadFull(dispatcher.config.Random, tokenBytes); err != nil {
		return false, errors.New("generate Renderer completion delivery lease")
	}
	tokenDigest := sha256.Sum256(tokenBytes)
	tokenHash := hex.EncodeToString(tokenDigest[:])
	now := dispatcher.config.Clock().UTC()
	delivery, err := dispatcher.repository.ClaimCompletion(ctx, jobpostgres.ClaimCompletionInput{
		LeaseTokenHash: tokenHash, Now: now, LeaseExpiresAt: now.Add(dispatcher.config.LeaseDuration),
	})
	if errors.Is(err, jobpostgres.ErrNoCompletionDelivery) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	requestContext, cancel := context.WithTimeout(ctx, dispatcher.config.RequestTimeout)
	err = dispatcher.sender.Send(requestContext, delivery)
	cancel()
	now = dispatcher.config.Clock().UTC()
	if err == nil {
		if ackErr := dispatcher.repository.AckCompletion(ctx, delivery.EventID, tokenHash, now); ackErr != nil {
			return false, ackErr
		}
		dispatcher.config.Operations.Count("completion_deliveries_total", "accepted")
		dispatcher.config.Operations.Event("completion_delivered", slog.String("state", "accepted"))
		return true, nil
	}
	state, failErr := dispatcher.repository.FailCompletion(
		ctx, delivery.EventID, tokenHash, now, "control_plane_unavailable", dispatcher.config.MaxAttempts,
	)
	if failErr != nil {
		return false, failErr
	}
	dispatcher.config.Operations.Count("completion_deliveries_total", state)
	dispatcher.config.Operations.Event("completion_delivery_failed",
		slog.String("state", state), slog.String("failure_code", "control_plane_unavailable"))
	return false, nil
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
