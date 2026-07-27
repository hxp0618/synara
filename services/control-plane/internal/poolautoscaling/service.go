package poolautoscaling

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	GateUnknown  = "unknown"
	GateHealthy  = "healthy"
	GateViolated = "violated"
)

type Policy struct {
	WorkerPoolID                   uuid.UUID `json:"workerPoolId"`
	WorkerPoolVersion              int64     `json:"workerPoolVersion"`
	TenantID                       uuid.UUID `json:"tenantId"`
	ExecutionTargetID              uuid.UUID `json:"executionTargetId"`
	Enabled                        bool      `json:"enabled"`
	MinIdleUnits                   int       `json:"minIdleUnits"`
	MaxIdleUnits                   int       `json:"maxIdleUnits"`
	TargetQueueDelaySeconds        int       `json:"targetQueueDelaySeconds"`
	InteractiveColdStartMaxSeconds *int      `json:"interactiveColdStartMaxSeconds,omitempty"`
	ScaleUpStep                    int       `json:"scaleUpStep"`
	ScaleDownStep                  int       `json:"scaleDownStep"`
	CooldownSeconds                int       `json:"cooldownSeconds"`
	ScaleDownStabilizationSeconds  int       `json:"scaleDownStabilizationSeconds"`
	Version                        int64     `json:"version"`
	UpdatedAt                      time.Time `json:"updatedAt"`
}

type State struct {
	PolicyVersion       int64      `json:"policyVersion"`
	DesiredIdleUnits    int        `json:"desiredIdleUnits"`
	QueueDepth          int64      `json:"queueDepth"`
	OldestQueuedAt      *time.Time `json:"oldestQueuedAt,omitempty"`
	ReadyIdleUnits      int        `json:"readyIdleUnits"`
	DecisionReason      string     `json:"decisionReason"`
	ColdStartGateStatus string     `json:"coldStartGateStatus"`
	LastQueueActiveAt   *time.Time `json:"lastQueueActiveAt,omitempty"`
	LastScaledAt        *time.Time `json:"lastScaledAt,omitempty"`
	DecisionVersion     int64      `json:"decisionVersion"`
	ObservedAt          time.Time  `json:"observedAt"`
}

type View struct {
	Policy Policy `json:"policy"`
	State  *State `json:"state,omitempty"`
}

type PutInput struct {
	ExpectedVersion                *int64 `json:"expectedVersion,omitempty"`
	Enabled                        bool   `json:"enabled"`
	MinIdleUnits                   int    `json:"minIdleUnits"`
	MaxIdleUnits                   int    `json:"maxIdleUnits"`
	TargetQueueDelaySeconds        int    `json:"targetQueueDelaySeconds"`
	InteractiveColdStartMaxSeconds *int   `json:"interactiveColdStartMaxSeconds,omitempty"`
	ScaleUpStep                    int    `json:"scaleUpStep"`
	ScaleDownStep                  int    `json:"scaleDownStep"`
	CooldownSeconds                int    `json:"cooldownSeconds"`
	ScaleDownStabilizationSeconds  int    `json:"scaleDownStabilizationSeconds"`
}

type ColdStartExpiry struct {
	TenantID          uuid.UUID
	ExecutionTargetID uuid.UUID
	WorkerPoolID      uuid.UUID
	WorkerPoolVersion int64
	Cutoff            time.Time
	Limit             int
}

type ColdStartExpirer func(context.Context, ColdStartExpiry) (int, error)

type Option func(*Service)

func WithColdStartExpirer(expirer ColdStartExpirer) Option {
	return func(service *Service) { service.expireColdStarts = expirer }
}

type Service struct {
	db               *gorm.DB
	authorizer       *authorization.Authorizer
	now              func() time.Time
	expireColdStarts ColdStartExpirer
}

func NewService(db *gorm.DB, options ...Option) *Service {
	service := &Service{
		db: db, authorizer: authorization.NewAuthorizer(db),
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service
}

func (s *Service) Get(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID, poolID uuid.UUID,
) (View, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return View{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return View{}, err
	}
	pool, err := loadPool(ctx, s.db, tenantID, targetID, poolID, false)
	if err != nil {
		return View{}, err
	}
	model, err := loadPolicy(ctx, s.db, pool)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		model = defaultPolicy(pool, principal.UserID)
	} else if err != nil {
		return View{}, problem.Wrap(500, "worker_pool_autoscaling_policy_load_failed", "Worker Pool autoscaling policy could not be loaded.", err)
	}
	return loadView(ctx, s.db, model)
}

func (s *Service) Put(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID, poolID uuid.UUID,
	input PutInput,
	requestID, ipAddress string,
) (View, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return View{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return View{}, err
	}
	var result persistence.WorkerPoolAutoscalingPolicy
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		pool, err := loadPool(ctx, tx, tenantID, targetID, poolID, true)
		if err != nil {
			return err
		}
		if err := validateInput(pool, input); err != nil {
			return err
		}
		current, loadErr := loadPolicy(ctx, tx, pool)
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			if input.ExpectedVersion != nil && *input.ExpectedVersion != 0 {
				return problem.New(409, "worker_pool_autoscaling_policy_version_conflict", "Worker Pool autoscaling policy changed; reload it before saving.")
			}
			result = modelFromInput(pool, input, principal.UserID, 1)
			if err := tx.WithContext(ctx).Create(&result).Error; err != nil {
				return problem.Wrap(409, "worker_pool_autoscaling_policy_create_rejected", "Worker Pool autoscaling policy could not be created.", err)
			}
		} else if loadErr != nil {
			return problem.Wrap(500, "worker_pool_autoscaling_policy_load_failed", "Worker Pool autoscaling policy could not be loaded.", loadErr)
		} else {
			if input.ExpectedVersion == nil || *input.ExpectedVersion != current.Version {
				return problem.New(409, "worker_pool_autoscaling_policy_version_conflict", "Worker Pool autoscaling policy changed; reload it before saving.")
			}
			result = modelFromInput(pool, input, principal.UserID, current.Version+1)
			updates := map[string]any{
				"enabled": result.Enabled, "min_idle_units": result.MinIdleUnits,
				"max_idle_units":                     result.MaxIdleUnits,
				"target_queue_delay_seconds":         result.TargetQueueDelaySeconds,
				"interactive_cold_start_max_seconds": result.InteractiveColdStartMaxSeconds,
				"scale_up_step":                      result.ScaleUpStep, "scale_down_step": result.ScaleDownStep,
				"cooldown_seconds":                 result.CooldownSeconds,
				"scale_down_stabilization_seconds": result.ScaleDownStabilizationSeconds,
				"version":                          result.Version, "updated_by": result.UpdatedBy,
			}
			updated := tx.WithContext(ctx).Model(&persistence.WorkerPoolAutoscalingPolicy{}).
				Where("worker_pool_id = ? AND worker_pool_version = ? AND version = ?", pool.ID, pool.Version, current.Version).
				Updates(updates)
			if updated.Error != nil || updated.RowsAffected != 1 {
				return problem.Wrap(409, "worker_pool_autoscaling_policy_version_conflict", "Worker Pool autoscaling policy changed; reload it before saving.", updated.Error)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "worker_pool.autoscaling_policy_updated", ResourceType: "worker_pool", ResourceID: &pool.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"executionTargetId": targetID, "workerPoolVersion": pool.Version,
				"enabled": input.Enabled, "minIdleUnits": input.MinIdleUnits,
				"maxIdleUnits": input.MaxIdleUnits, "targetQueueDelaySeconds": input.TargetQueueDelaySeconds,
				"interactiveColdStartMaxSeconds": input.InteractiveColdStartMaxSeconds,
			},
		})
	})
	if err != nil {
		return View{}, err
	}
	return loadView(ctx, s.db, result)
}

type Summary struct {
	Evaluated         int `json:"evaluated"`
	ScaledUp          int `json:"scaledUp"`
	ScaledDown        int `json:"scaledDown"`
	ColdStartViolated int `json:"coldStartViolated"`
	ExpiredExecutions int `json:"expiredExecutions"`
}

func (s *Service) RunOnce(ctx context.Context, limit int) (Summary, error) {
	if limit < 1 || limit > 10_000 {
		return Summary{}, problem.New(400, "invalid_worker_pool_autoscaling_batch_size", "Worker Pool autoscaling batch size must be between 1 and 10000.")
	}
	var policies []persistence.WorkerPoolAutoscalingPolicy
	if err := s.db.WithContext(ctx).
		Order("execution_target_id, worker_pool_id, worker_pool_version").Limit(limit).
		Find(&policies).Error; err != nil {
		return Summary{}, problem.Wrap(500, "worker_pool_autoscaling_policies_load_failed", "Worker Pool autoscaling policies could not be loaded.", err)
	}
	summary := Summary{}
	var failures []error
	for _, policy := range policies {
		decision, previousDesired, err := s.evaluatePolicy(ctx, policy)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		summary.Evaluated++
		if decision.DesiredIdleUnits > previousDesired {
			summary.ScaledUp++
		} else if decision.DesiredIdleUnits < previousDesired {
			summary.ScaledDown++
		}
		if decision.ColdStartGateStatus == GateViolated {
			summary.ColdStartViolated++
			if policy.InteractiveColdStartMaxSeconds != nil && s.expireColdStarts != nil {
				expired, expireErr := s.expireColdStarts(ctx, ColdStartExpiry{
					TenantID: policy.TenantID, ExecutionTargetID: policy.ExecutionTargetID,
					WorkerPoolID: policy.WorkerPoolID, WorkerPoolVersion: policy.WorkerPoolVersion,
					Cutoff: decision.ObservedAt.Add(-time.Duration(*policy.InteractiveColdStartMaxSeconds) * time.Second),
					Limit:  200,
				})
				summary.ExpiredExecutions += expired
				if expireErr != nil {
					failures = append(failures, expireErr)
				}
			}
		}
	}
	return summary, errors.Join(failures...)
}

func (s *Service) evaluatePolicy(
	ctx context.Context,
	policy persistence.WorkerPoolAutoscalingPolicy,
) (persistence.WorkerPoolAutoscalingState, int, error) {
	var decision persistence.WorkerPoolAutoscalingState
	previousDesired := 0
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		pool, err := loadPool(ctx, tx, policy.TenantID, policy.ExecutionTargetID, policy.WorkerPoolID, true)
		if err != nil {
			return err
		}
		currentPolicy, err := loadPolicy(ctx, tx, pool)
		if err != nil {
			return problem.Wrap(500, "worker_pool_autoscaling_policy_load_failed", "Worker Pool autoscaling policy could not be loaded for evaluation.", err)
		}
		policy = currentPolicy
		now := s.now()
		queue, err := loadQueuePressure(ctx, tx, policy)
		if err != nil {
			return err
		}
		readyIdle, err := loadReadyIdle(ctx, tx, policy, now)
		if err != nil {
			return err
		}
		var current persistence.WorkerPoolAutoscalingState
		stateErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("worker_pool_id = ? AND worker_pool_version = ?", policy.WorkerPoolID, policy.WorkerPoolVersion).
			Take(&current).Error
		if stateErr != nil && !errors.Is(stateErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "worker_pool_autoscaling_state_load_failed", "Worker Pool autoscaling state could not be loaded.", stateErr)
		}
		decision, previousDesired = decide(policy, pool, current, stateErr == nil, queue, readyIdle, now)
		if errors.Is(stateErr, gorm.ErrRecordNotFound) {
			if err := tx.WithContext(ctx).Create(&decision).Error; err != nil {
				return problem.Wrap(409, "worker_pool_autoscaling_state_create_rejected", "Worker Pool autoscaling state could not be created.", err)
			}
			return nil
		}
		updates := map[string]any{
			"policy_version": decision.PolicyVersion, "desired_idle_units": decision.DesiredIdleUnits,
			"queue_depth": decision.QueueDepth, "oldest_queued_at": decision.OldestQueuedAt,
			"ready_idle_units": decision.ReadyIdleUnits, "decision_reason": decision.DecisionReason,
			"cold_start_gate_status": decision.ColdStartGateStatus,
			"last_queue_active_at":   decision.LastQueueActiveAt, "last_scaled_at": decision.LastScaledAt,
			"decision_version": decision.DecisionVersion, "observed_at": decision.ObservedAt,
			"updated_at": decision.UpdatedAt,
		}
		updated := tx.WithContext(ctx).Model(&persistence.WorkerPoolAutoscalingState{}).
			Where(
				"worker_pool_id = ? AND worker_pool_version = ? AND decision_version = ?",
				policy.WorkerPoolID, policy.WorkerPoolVersion, current.DecisionVersion,
			).
			Updates(updates)
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "worker_pool_autoscaling_state_conflict", "Worker Pool autoscaling state changed during evaluation.", updated.Error)
		}
		return nil
	})
	return decision, previousDesired, err
}

type queuePressure struct {
	Depth          int64
	OldestQueuedAt *time.Time
}

func loadQueuePressure(
	ctx context.Context,
	tx *gorm.DB,
	policy persistence.WorkerPoolAutoscalingPolicy,
) (queuePressure, error) {
	query := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where(
			"tenant_id = ? AND execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ? AND status IN ?",
			policy.TenantID, policy.ExecutionTargetID, policy.WorkerPoolID, policy.WorkerPoolVersion,
			[]string{"queued", "recovering"},
		)
	var depth int64
	if err := query.Count(&depth).Error; err != nil {
		return queuePressure{}, problem.Wrap(500, "worker_pool_autoscaling_queue_load_failed", "Worker Pool queue pressure could not be loaded.", err)
	}
	if depth == 0 {
		return queuePressure{}, nil
	}
	var oldest persistence.AgentExecution
	if err := query.Select("queued_at").Order("queued_at ASC, id ASC").Take(&oldest).Error; err != nil {
		return queuePressure{}, problem.Wrap(500, "worker_pool_autoscaling_queue_load_failed", "Worker Pool queue pressure could not be loaded.", err)
	}
	oldestQueuedAt := oldest.QueuedAt.UTC()
	return queuePressure{Depth: depth, OldestQueuedAt: &oldestQueuedAt}, nil
}

func loadReadyIdle(
	ctx context.Context,
	tx *gorm.DB,
	policy persistence.WorkerPoolAutoscalingPolicy,
	now time.Time,
) (int, error) {
	var row persistence.WorkerPoolWarmCapacity
	err := tx.WithContext(ctx).
		Where(
			"tenant_id = ? AND execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ? AND expires_at > ?",
			policy.TenantID, policy.ExecutionTargetID, policy.WorkerPoolID, policy.WorkerPoolVersion, now,
		).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, problem.Wrap(500, "worker_pool_autoscaling_capacity_load_failed", "Worker Pool warm capacity could not be loaded.", err)
	}
	return row.ReadyIdleUnits, nil
}

func decide(
	policy persistence.WorkerPoolAutoscalingPolicy,
	pool persistence.WorkerPool,
	current persistence.WorkerPoolAutoscalingState,
	hasCurrent bool,
	queue queuePressure,
	readyIdle int,
	now time.Time,
) (persistence.WorkerPoolAutoscalingState, int) {
	configured := clamp(pool.DesiredIdleUnits, policy.MinIdleUnits, policy.MaxIdleUnits)
	desired := configured
	reason := "initialized"
	decisionVersion := int64(1)
	var lastQueueActiveAt, lastScaledAt *time.Time
	if hasCurrent && current.PolicyVersion == policy.Version {
		desired = clamp(current.DesiredIdleUnits, policy.MinIdleUnits, policy.MaxIdleUnits)
		decisionVersion = current.DecisionVersion + 1
		lastQueueActiveAt = current.LastQueueActiveAt
		lastScaledAt = current.LastScaledAt
	}
	previousDesired := desired
	if !policy.Enabled {
		desired = configured
		reason = "disabled"
	} else if queue.Depth > 0 {
		activeAt := now
		lastQueueActiveAt = &activeAt
		target := clamp(int(queue.Depth), policy.MinIdleUnits, policy.MaxIdleUnits)
		if target < desired && readyIdle < int(queue.Depth) {
			target = desired
		}
		age := queueAge(queue.OldestQueuedAt, now)
		if age >= time.Duration(policy.TargetQueueDelaySeconds)*time.Second {
			reason = "target-delay"
			if target <= desired {
				target = min(policy.MaxIdleUnits, desired+policy.ScaleUpStep)
			}
		} else {
			reason = "queue-pressure"
		}
		if target > desired && cooldownElapsed(lastScaledAt, policy.CooldownSeconds, now) {
			desired = min(target, desired+policy.ScaleUpStep)
		}
	} else {
		reason = "stable"
		if lastQueueActiveAt == nil {
			activeAt := now
			lastQueueActiveAt = &activeAt
		}
		if desired > policy.MinIdleUnits {
			if now.Sub(*lastQueueActiveAt) < time.Duration(policy.ScaleDownStabilizationSeconds)*time.Second ||
				!cooldownElapsed(lastScaledAt, policy.CooldownSeconds, now) {
				reason = "scale-down-stabilizing"
			} else {
				desired = max(policy.MinIdleUnits, desired-policy.ScaleDownStep)
				reason = "scale-down"
			}
		}
	}
	if desired != previousDesired {
		scaledAt := now
		lastScaledAt = &scaledAt
	}
	gate := GateUnknown
	if policy.InteractiveColdStartMaxSeconds != nil {
		gate = GateHealthy
		if queue.Depth > 0 && queueAge(queue.OldestQueuedAt, now) >=
			time.Duration(*policy.InteractiveColdStartMaxSeconds)*time.Second {
			gate = GateViolated
		}
	}
	return persistence.WorkerPoolAutoscalingState{
		WorkerPoolID: policy.WorkerPoolID, WorkerPoolVersion: policy.WorkerPoolVersion,
		TenantID: policy.TenantID, ExecutionTargetID: policy.ExecutionTargetID,
		PolicyVersion: policy.Version, DesiredIdleUnits: desired,
		QueueDepth: queue.Depth, OldestQueuedAt: queue.OldestQueuedAt, ReadyIdleUnits: readyIdle,
		DecisionReason: reason, ColdStartGateStatus: gate,
		LastQueueActiveAt: lastQueueActiveAt, LastScaledAt: lastScaledAt,
		DecisionVersion: decisionVersion, ObservedAt: now, UpdatedAt: now,
	}, previousDesired
}

func loadPool(
	ctx context.Context,
	db *gorm.DB,
	tenantID, targetID, poolID uuid.UUID,
	lock bool,
) (persistence.WorkerPool, error) {
	query := db.WithContext(ctx)
	if lock {
		query = persistence.WithLocking(query, "UPDATE", "")
	}
	var pool persistence.WorkerPool
	err := query.Where(
		"tenant_id = ? AND execution_target_id = ? AND id = ?", tenantID, targetID, poolID,
	).Take(&pool).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerPool{}, problem.New(404, "worker_pool_not_found", "Worker Pool not found.")
	}
	if err != nil {
		return persistence.WorkerPool{}, problem.Wrap(500, "worker_pool_load_failed", "Worker Pool could not be loaded.", err)
	}
	if strings.TrimSpace(pool.Mode) != "warm" || strings.TrimSpace(pool.Status) == "disabled" {
		return persistence.WorkerPool{}, problem.New(409, "worker_pool_autoscaling_unsupported", "Autoscaling requires an active or draining warm Worker Pool.")
	}
	return pool, nil
}

func loadPolicy(
	ctx context.Context,
	db *gorm.DB,
	pool persistence.WorkerPool,
) (persistence.WorkerPoolAutoscalingPolicy, error) {
	var policy persistence.WorkerPoolAutoscalingPolicy
	err := db.WithContext(ctx).Where(
		"worker_pool_id = ? AND worker_pool_version = ?", pool.ID, pool.Version,
	).Take(&policy).Error
	return policy, err
}

func defaultPolicy(pool persistence.WorkerPool, updatedBy uuid.UUID) persistence.WorkerPoolAutoscalingPolicy {
	return persistence.WorkerPoolAutoscalingPolicy{
		WorkerPoolID: pool.ID, WorkerPoolVersion: pool.Version,
		TenantID: *pool.TenantID, ExecutionTargetID: pool.ExecutionTargetID,
		Enabled: false, MinIdleUnits: pool.MinIdleUnits, MaxIdleUnits: pool.MaxActiveUnits,
		TargetQueueDelaySeconds: 30, ScaleUpStep: 1, ScaleDownStep: 1,
		CooldownSeconds: 30, ScaleDownStabilizationSeconds: 300,
		Version: 0, UpdatedBy: updatedBy,
	}
}

func modelFromInput(
	pool persistence.WorkerPool,
	input PutInput,
	updatedBy uuid.UUID,
	version int64,
) persistence.WorkerPoolAutoscalingPolicy {
	return persistence.WorkerPoolAutoscalingPolicy{
		WorkerPoolID: pool.ID, WorkerPoolVersion: pool.Version,
		TenantID: *pool.TenantID, ExecutionTargetID: pool.ExecutionTargetID,
		Enabled: input.Enabled, MinIdleUnits: input.MinIdleUnits, MaxIdleUnits: input.MaxIdleUnits,
		TargetQueueDelaySeconds:        input.TargetQueueDelaySeconds,
		InteractiveColdStartMaxSeconds: input.InteractiveColdStartMaxSeconds,
		ScaleUpStep:                    input.ScaleUpStep, ScaleDownStep: input.ScaleDownStep,
		CooldownSeconds:               input.CooldownSeconds,
		ScaleDownStabilizationSeconds: input.ScaleDownStabilizationSeconds,
		Version:                       version, UpdatedBy: updatedBy,
	}
}

func validateInput(pool persistence.WorkerPool, input PutInput) error {
	if pool.TenantID == nil || input.MinIdleUnits < pool.MinIdleUnits ||
		input.MaxIdleUnits < input.MinIdleUnits || input.MaxIdleUnits > pool.MaxActiveUnits {
		return problem.New(400, "invalid_worker_pool_autoscaling_bounds", "Autoscaling bounds must satisfy Worker Pool minIdleUnits <= minIdleUnits <= maxIdleUnits <= maxActiveUnits.")
	}
	if input.TargetQueueDelaySeconds < 1 || input.TargetQueueDelaySeconds > 3600 ||
		input.ScaleUpStep < 1 || input.ScaleUpStep > 10_000 ||
		input.ScaleDownStep < 1 || input.ScaleDownStep > 10_000 ||
		input.CooldownSeconds < 1 || input.CooldownSeconds > 3600 ||
		input.ScaleDownStabilizationSeconds < 1 || input.ScaleDownStabilizationSeconds > 86_400 {
		return problem.New(400, "invalid_worker_pool_autoscaling_policy", "Worker Pool autoscaling timing and step values are outside their supported bounds.")
	}
	if input.InteractiveColdStartMaxSeconds != nil &&
		(*input.InteractiveColdStartMaxSeconds < 5 || *input.InteractiveColdStartMaxSeconds > 3600 ||
			*input.InteractiveColdStartMaxSeconds < input.TargetQueueDelaySeconds) {
		return problem.New(400, "invalid_interactive_cold_start_limit", "interactiveColdStartMaxSeconds must be between 5 and 3600 and not less than targetQueueDelaySeconds.")
	}
	return nil
}

func loadView(
	ctx context.Context,
	db *gorm.DB,
	model persistence.WorkerPoolAutoscalingPolicy,
) (View, error) {
	view := View{Policy: toPolicy(model)}
	if model.Version <= 0 {
		return view, nil
	}
	var state persistence.WorkerPoolAutoscalingState
	err := db.WithContext(ctx).Where(
		"worker_pool_id = ? AND worker_pool_version = ?", model.WorkerPoolID, model.WorkerPoolVersion,
	).Take(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return view, nil
	}
	if err != nil {
		return View{}, problem.Wrap(500, "worker_pool_autoscaling_state_load_failed", "Worker Pool autoscaling state could not be loaded.", err)
	}
	converted := toState(state)
	view.State = &converted
	return view, nil
}

func toPolicy(model persistence.WorkerPoolAutoscalingPolicy) Policy {
	return Policy{
		WorkerPoolID: model.WorkerPoolID, WorkerPoolVersion: model.WorkerPoolVersion,
		TenantID: model.TenantID, ExecutionTargetID: model.ExecutionTargetID,
		Enabled: model.Enabled, MinIdleUnits: model.MinIdleUnits, MaxIdleUnits: model.MaxIdleUnits,
		TargetQueueDelaySeconds:        model.TargetQueueDelaySeconds,
		InteractiveColdStartMaxSeconds: model.InteractiveColdStartMaxSeconds,
		ScaleUpStep:                    model.ScaleUpStep, ScaleDownStep: model.ScaleDownStep,
		CooldownSeconds:               model.CooldownSeconds,
		ScaleDownStabilizationSeconds: model.ScaleDownStabilizationSeconds,
		Version:                       model.Version, UpdatedAt: model.UpdatedAt,
	}
}

func toState(model persistence.WorkerPoolAutoscalingState) State {
	return State{
		PolicyVersion: model.PolicyVersion, DesiredIdleUnits: model.DesiredIdleUnits,
		QueueDepth: model.QueueDepth, OldestQueuedAt: model.OldestQueuedAt,
		ReadyIdleUnits: model.ReadyIdleUnits, DecisionReason: model.DecisionReason,
		ColdStartGateStatus: model.ColdStartGateStatus,
		LastQueueActiveAt:   model.LastQueueActiveAt, LastScaledAt: model.LastScaledAt,
		DecisionVersion: model.DecisionVersion, ObservedAt: model.ObservedAt,
	}
}

func requireActiveTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}

func queueAge(oldest *time.Time, now time.Time) time.Duration {
	if oldest == nil || oldest.IsZero() || !oldest.Before(now) {
		return 0
	}
	return now.Sub(*oldest)
}

func cooldownElapsed(last *time.Time, seconds int, now time.Time) bool {
	return last == nil || now.Sub(*last) >= time.Duration(seconds)*time.Second
}

func clamp(value, minimum, maximum int) int {
	return min(max(value, minimum), maximum)
}
