package outbox

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"
)

type Publisher interface {
	Publish(context.Context, Message) error
}

type PublisherFunc func(context.Context, Message) error

func (f PublisherFunc) Publish(ctx context.Context, message Message) error { return f(ctx, message) }

// DatabasePublisher completes the postgres-outbox delivery boundary. Business state is already
// committed atomically before a message becomes claimable; database-backed Workers consume that
// authoritative state through their idempotent Claim API. External queue builds can replace this
// publisher without changing the dispatcher or message contract.
type DatabasePublisher struct{}

func (DatabasePublisher) Publish(ctx context.Context, _ Message) error { return ctx.Err() }

type BackgroundObserver interface {
	ObserveBackground(kind string, started time.Time, err error)
}

type Dispatcher struct {
	service      *Service
	publisher    Publisher
	pollInterval time.Duration
	observer     BackgroundObserver
	logger       *slog.Logger
}

func NewDispatcher(
	service *Service,
	publisher Publisher,
	pollInterval time.Duration,
	observer BackgroundObserver,
	logger *slog.Logger,
) (*Dispatcher, error) {
	if service == nil || publisher == nil {
		return nil, errors.New("outbox service and publisher are required")
	}
	if pollInterval <= 0 {
		return nil, errors.New("outbox poll interval must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{service: service, publisher: publisher, pollInterval: pollInterval, observer: observer, logger: logger}, nil
}

func (d *Dispatcher) Run(ctx context.Context) {
	for {
		started := time.Now()
		count, err := d.DispatchOnce(ctx)
		if d.observer != nil {
			d.observer.ObserveBackground("outbox", started, err)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			d.logger.Error("outbox dispatch failed", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		if count > 0 {
			continue
		}
		timer := time.NewTimer(d.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	decision, pressureErr := d.service.RefreshPressure(ctx)
	if decision.BatchSize == 0 {
		return 0, pressureErr
	}
	messages, err := d.service.ClaimLimit(ctx, decision.BatchSize)
	if err != nil {
		return 0, errors.Join(pressureErr, err)
	}
	if len(messages) == 0 {
		return 0, pressureErr
	}
	concurrency := min(max(decision.Concurrency, 1), len(messages))
	lanes := make([][]Message, concurrency)
	for _, message := range messages {
		lane := outboxDispatchLane(message.MessageKey, concurrency)
		lanes[lane] = append(lanes[lane], message)
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	failures := make([]error, 0)
	if pressureErr != nil {
		failures = append(failures, pressureErr)
	}
	for _, lane := range lanes {
		if len(lane) == 0 {
			continue
		}
		lane := lane
		wait.Add(1)
		go func() {
			defer wait.Done()
			for _, message := range lane {
				if err := d.dispatchMessage(ctx, message); err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
				}
			}
		}()
	}
	wait.Wait()
	return len(messages), errors.Join(failures...)
}

func (d *Dispatcher) dispatchMessage(ctx context.Context, message Message) error {
	if ctx.Err() != nil {
		_ = d.service.Release(context.WithoutCancel(ctx), message.ID)
		return ctx.Err()
	}
	if err := d.publisher.Publish(ctx, message); err != nil {
		if ctx.Err() != nil {
			_ = d.service.Release(context.WithoutCancel(ctx), message.ID)
			return ctx.Err()
		}
		if failErr := d.service.Fail(ctx, message, err); failErr != nil {
			return failErr
		}
		return fmt.Errorf("publish outbox message %s topic %s: %s", message.ID, message.Topic, errorSummary(err))
	}
	if err := d.service.Acknowledge(ctx, message.ID); err != nil {
		return err
	}
	return nil
}

func outboxDispatchLane(messageKey string, lanes int) int {
	if lanes <= 1 {
		return 0
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(messageKey))
	return int(hash.Sum32() % uint32(lanes))
}
