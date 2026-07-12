package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
		if count >= d.service.BatchSize() {
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
	messages, err := d.service.Claim(ctx)
	if err != nil {
		return 0, err
	}
	var failures []error
	for _, message := range messages {
		if err := d.publisher.Publish(ctx, message); err != nil {
			if ctx.Err() != nil {
				_ = d.service.Release(context.WithoutCancel(ctx), message.ID)
				return len(messages), ctx.Err()
			}
			if failErr := d.service.Fail(ctx, message, err); failErr != nil {
				failures = append(failures, failErr)
			} else {
				failures = append(failures, fmt.Errorf("publish outbox message %s topic %s: %s", message.ID, message.Topic, errorSummary(err)))
			}
			continue
		}
		if err := d.service.Acknowledge(ctx, message.ID); err != nil {
			failures = append(failures, err)
		}
	}
	return len(messages), errors.Join(failures...)
}
