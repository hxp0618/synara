package reconcilerleadership

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/leadership"
)

var ErrLeadershipLost = errors.New("reconciler leadership lost")

type RunnerConfig struct {
	LeaseName         string
	CycleInterval     time.Duration
	AcquireRetryDelay time.Duration
	RenewInterval     time.Duration
	AssertInterval    time.Duration
	Logger            *slog.Logger
}

type RunContext struct {
	Context      context.Context
	Lease        leadership.Lease
	AssertActive func(context.Context) error
}

type Runner struct {
	service *leadership.Service
	config  RunnerConfig
	logger  *slog.Logger
}

func NewRunner(service *leadership.Service, cfg RunnerConfig) (*Runner, error) {
	if service == nil {
		return nil, errors.New("reconciler leadership service is required")
	}
	leaseName, err := leadership.ValidateLeaseName(cfg.LeaseName)
	if err != nil {
		return nil, err
	}
	if cfg.CycleInterval <= 0 {
		return nil, errors.New("reconciler leadership cycle interval must be positive")
	}
	if cfg.AcquireRetryDelay <= 0 {
		return nil, errors.New("reconciler leadership acquire retry delay must be positive")
	}
	if cfg.RenewInterval <= 0 {
		return nil, errors.New("reconciler leadership renew interval must be positive")
	}
	if cfg.AssertInterval <= 0 {
		return nil, errors.New("reconciler leadership assert interval must be positive")
	}
	if ttl := service.LeaseTTL(); ttl > 0 {
		if cfg.RenewInterval >= ttl {
			return nil, errors.New("reconciler leadership renew interval must be less than the lease TTL")
		}
		if cfg.AssertInterval >= ttl {
			return nil, errors.New("reconciler leadership assert interval must be less than the lease TTL")
		}
	}
	return &Runner{
		service: service,
		config: RunnerConfig{
			LeaseName:         leaseName,
			CycleInterval:     cfg.CycleInterval,
			AcquireRetryDelay: cfg.AcquireRetryDelay,
			RenewInterval:     cfg.RenewInterval,
			AssertInterval:    cfg.AssertInterval,
			Logger:            cfg.Logger,
		},
		logger: cfg.Logger,
	}, nil
}

func (r *Runner) Run(ctx context.Context, work func(RunContext) error) {
	if r == nil || r.service == nil || work == nil {
		return
	}
	for ctx.Err() == nil {
		lease, held, err := r.service.Acquire(ctx, r.config.LeaseName)
		if err != nil {
			r.logError("failed to acquire reconciler leadership", err)
			if !sleepContext(ctx, r.config.AcquireRetryDelay) {
				return
			}
			continue
		}
		if !held {
			if !sleepContext(ctx, r.config.AcquireRetryDelay) {
				return
			}
			continue
		}
		r.runWhileLeader(ctx, lease, work)
	}
}

func (r *Runner) runWhileLeader(ctx context.Context, lease leadership.Lease, work func(RunContext) error) {
	leaderCtx, cancelLeader := context.WithCancel(ctx)
	defer cancelLeader()
	workCtx := r.service.WithFence(leaderCtx, lease)

	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		r.monitorLeadership(ctx, leaderCtx, cancelLeader, lease.FencingToken)
	}()

	timer := time.NewTimer(0)
	defer stopTimer(timer)
	assert := func(assertCtx context.Context) error {
		return r.assertActive(assertCtx, lease.FencingToken)
	}

	for {
		select {
		case <-ctx.Done():
			cancelLeader()
		case <-leaderCtx.Done():
		case <-timer.C:
			if err := assert(ctx); err != nil {
				cancelLeader()
				if !errors.Is(err, ErrLeadershipLost) && !errors.Is(err, context.Canceled) {
					r.logError("failed to assert reconciler leadership", err)
				}
				break
			}
			err := work(RunContext{Context: workCtx, Lease: lease, AssertActive: assert})
			if errors.Is(err, ErrLeadershipLost) || errors.Is(err, leadership.ErrFenceInactive) || errors.Is(err, context.Canceled) {
				cancelLeader()
				break
			}
			if err != nil && leaderCtx.Err() == nil && ctx.Err() == nil {
				r.logError("reconciler leadership work failed", err)
			}
			timer.Reset(r.config.CycleInterval)
			continue
		}
		break
	}

	cancelLeader()
	<-monitorDone
	released, err := r.service.Release(context.WithoutCancel(ctx), r.config.LeaseName, lease.FencingToken)
	if err != nil {
		r.logError("failed to release reconciler leadership", err)
		return
	}
	if !released {
		r.logDebug(
			"reconciler leadership already transferred before release",
			"lease", r.config.LeaseName,
			"holder", r.service.HolderID(),
			"token", lease.FencingToken,
		)
	}
}

func (r *Runner) monitorLeadership(
	parent context.Context,
	leader context.Context,
	cancelLeader context.CancelFunc,
	fencingToken int64,
) {
	renewTicker := time.NewTicker(r.config.RenewInterval)
	defer renewTicker.Stop()
	assertTicker := time.NewTicker(r.config.AssertInterval)
	defer assertTicker.Stop()

	for {
		select {
		case <-parent.Done():
			return
		case <-leader.Done():
			return
		case <-renewTicker.C:
			_, held, err := r.service.Renew(parent, r.config.LeaseName, fencingToken)
			if err != nil {
				r.logError("failed to renew reconciler leadership", err)
				cancelLeader()
				return
			}
			if !held {
				cancelLeader()
				return
			}
		case <-assertTicker.C:
			if err := r.assertActive(parent, fencingToken); err != nil {
				if !errors.Is(err, ErrLeadershipLost) && !errors.Is(err, context.Canceled) {
					r.logError("failed to verify reconciler leadership", err)
				}
				cancelLeader()
				return
			}
		}
	}
}

func (r *Runner) assertActive(ctx context.Context, fencingToken int64) error {
	_, held, err := r.service.AssertActive(ctx, r.config.LeaseName, fencingToken)
	if err != nil {
		return err
	}
	if !held {
		return ErrLeadershipLost
	}
	return nil
}

func (r *Runner) logError(message string, err error) {
	if r.logger == nil || err == nil {
		return
	}
	r.logger.Error(message, "lease", r.config.LeaseName, "holder", r.service.HolderID(), "error", err)
}

func (r *Runner) logDebug(message string, args ...any) {
	if r.logger == nil {
		return
	}
	r.logger.Debug(message, args...)
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		delay = time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer stopTimer(timer)
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
