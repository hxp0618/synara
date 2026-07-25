package lifecyclepolicy

import (
	"context"
	"log/slog"
	"time"
)

type resourceLifecycleEnforcer interface {
	EnforceResourceLifecycle(context.Context, time.Time, int) (int, error)
}

type backgroundObserver interface {
	ObserveBackground(kind string, started time.Time, err error)
}

type Controller struct {
	enforcer resourceLifecycleEnforcer
	interval time.Duration
	limit    int
	logger   *slog.Logger
	now      func() time.Time
	observer backgroundObserver
}

func NewController(
	enforcer resourceLifecycleEnforcer,
	interval time.Duration,
	logger *slog.Logger,
	observers ...backgroundObserver,
) *Controller {
	controller := &Controller{
		enforcer: enforcer,
		interval: interval,
		limit:    200,
		logger:   logger,
		now:      func() time.Time { return time.Now().UTC() },
	}
	if len(observers) > 0 {
		controller.observer = observers[0]
	}
	return controller
}

func (c *Controller) Run(ctx context.Context) {
	if c == nil || c.enforcer == nil {
		return
	}
	interval := c.interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		started := c.now()
		err := c.RunOnce(ctx, c.limit)
		if c.observer != nil {
			c.observer.ObserveBackground("resource-lifecycle", started, err)
		}
		if err != nil && ctx.Err() == nil && c.logger != nil {
			c.logger.Error("resource lifecycle sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Controller) RunOnce(ctx context.Context, limit int) error {
	if c == nil || c.enforcer == nil {
		return nil
	}
	if limit <= 0 {
		limit = c.limit
	}
	_, err := c.enforcer.EnforceResourceLifecycle(ctx, c.now(), limit)
	return err
}
