package placement

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/warmcapacity"
)

const (
	PoolModeResident     = "resident"
	PoolModePerExecution = "per-execution"
	PoolModeWarm         = "warm"

	CapacityClassStandard    = "standard"
	CapacityClassInteractive = "interactive"

	PoolStatusActive   = "active"
	PoolStatusDraining = "draining"
	PoolStatusDisabled = "disabled"

	WarmPoolModeDefault    = "default"
	WarmPoolModeDisabled   = "disabled"
	WarmPoolModeBalanced   = "balanced"
	WarmPoolModeLowLatency = "low-latency"
)

type Pool struct {
	ID                 uuid.UUID      `json:"id"`
	TenantID           *uuid.UUID     `json:"tenantId"`
	ExecutionTargetID  uuid.UUID      `json:"executionTargetId"`
	Name               string         `json:"name"`
	Mode               string         `json:"mode"`
	CapacityClass      string         `json:"capacityClass"`
	ClusterID          string         `json:"clusterId"`
	Region             string         `json:"region"`
	Namespace          string         `json:"namespace"`
	DesiredIdleUnits   int            `json:"desiredIdleUnits"`
	MaxActiveUnits     int            `json:"maxActiveUnits"`
	SchedulingTemplate map[string]any `json:"schedulingTemplate"`
	Status             string         `json:"status"`
	Version            int64          `json:"version"`
	CreatedAt          time.Time      `json:"createdAt"`
	UpdatedAt          time.Time      `json:"updatedAt"`
}

type Policy struct {
	TenantID          *uuid.UUID `json:"tenantId"`
	ExecutionTargetID uuid.UUID  `json:"executionTargetId"`
	Version           int64      `json:"version"`
	DefaultPoolID     uuid.UUID  `json:"defaultPoolId"`
	BalancedPoolID    *uuid.UUID `json:"balancedPoolId"`
	LowLatencyPoolID  *uuid.UUID `json:"lowLatencyPoolId"`
	UpdatedBy         *uuid.UUID `json:"updatedBy"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type State struct {
	Pools  []Pool `json:"pools"`
	Policy Policy `json:"policy"`
}

type UpdatePolicyInput struct {
	ExpectedVersion  int64      `json:"expectedVersion"`
	DefaultPoolID    uuid.UUID  `json:"defaultPoolId"`
	BalancedPoolID   *uuid.UUID `json:"balancedPoolId"`
	LowLatencyPoolID *uuid.UUID `json:"lowLatencyPoolId"`
}

type CreatePoolInput struct {
	Name               string         `json:"name"`
	Mode               string         `json:"mode"`
	CapacityClass      string         `json:"capacityClass"`
	ClusterID          string         `json:"clusterId"`
	Region             string         `json:"region"`
	Namespace          string         `json:"namespace"`
	DesiredIdleUnits   int            `json:"desiredIdleUnits"`
	MaxActiveUnits     int            `json:"maxActiveUnits"`
	SchedulingTemplate map[string]any `json:"schedulingTemplate"`
	Status             string         `json:"status"`
}

type UpdatePoolInput struct {
	ExpectedVersion int64 `json:"expectedVersion"`
	CreatePoolInput
}

type Selection struct {
	Pool          Pool   `json:"pool"`
	CapacityClass string `json:"capacityClass"`
	PolicyVersion int64  `json:"policyVersion"`
}

func ApplySelection(execution *persistence.AgentExecution, selection Selection) {
	if execution == nil {
		return
	}
	poolID := selection.Pool.ID
	poolVersion := selection.Pool.Version
	capacityClass := selection.CapacityClass
	policyVersion := selection.PolicyVersion
	execution.WorkerPoolID = &poolID
	execution.WorkerPoolVersion = &poolVersion
	execution.CapacityClass = &capacityClass
	execution.PlacementPolicyVersion = &policyVersion
	execution.PlacementRegion = selection.Pool.Region
	execution.PlacementClusterID = selection.Pool.ClusterID
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	now        func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{
		db:         db,
		authorizer: authorization.NewAuthorizer(db),
		now:        func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) List(ctx context.Context, principal identity.Principal, tenantID, targetID uuid.UUID) (State, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return State{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return State{}, err
	}
	var state State
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		target, err := s.loadAccessibleTarget(ctx, tx, tenantID, targetID, false)
		if err != nil {
			return err
		}
		state, err = s.EnsureDefault(ctx, tx, target)
		return err
	})
	return state, err
}

func (s *Service) UpdatePolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	input UpdatePolicyInput,
) (State, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return State{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return State{}, err
	}
	if input.ExpectedVersion <= 0 {
		return State{}, problem.New(400, "invalid_execution_placement_policy_version", "expectedVersion must be positive.")
	}
	if input.DefaultPoolID == uuid.Nil {
		return State{}, problem.New(400, "invalid_execution_placement_policy_default_pool", "defaultPoolId is required.")
	}
	var state State
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		target, err := s.loadAccessibleTarget(ctx, tx, tenantID, targetID, true)
		if err != nil {
			return err
		}
		if target.TenantID == nil {
			return problem.New(403, "shared_execution_target_placement_policy_immutable", "Platform-shared execution target placement policy cannot be changed by a tenant.")
		}
		current, models, err := s.ensureDefaultLocked(ctx, tx, target)
		if err != nil {
			return err
		}
		if input.ExpectedVersion != current.Version {
			return problem.New(409, "execution_placement_policy_version_conflict", "Execution placement policy changed; reload it before saving.")
		}
		nextBalanced, err := normalizeOptionalPoolID(input.BalancedPoolID)
		if err != nil {
			return err
		}
		nextLowLatency, err := normalizeOptionalPoolID(input.LowLatencyPoolID)
		if err != nil {
			return err
		}
		resolved, err := validatePolicyInput(target, models, input.DefaultPoolID, nextBalanced, nextLowLatency)
		if err != nil {
			return err
		}
		if current.DefaultPoolID == resolved.defaultPool.ID &&
			sameUUIDPointer(current.BalancedPoolID, resolved.balancedPoolID) &&
			sameUUIDPointer(current.LowLatencyPoolID, resolved.lowLatencyPoolID) {
			state = toState(current, models)
			return nil
		}
		now := s.now()
		updates := map[string]any{
			"version":             current.Version + 1,
			"default_pool_id":     resolved.defaultPool.ID,
			"balanced_pool_id":    resolved.balancedPoolID,
			"low_latency_pool_id": resolved.lowLatencyPoolID,
			"updated_by":          principal.UserID,
			"updated_at":          now,
		}
		result := tx.WithContext(ctx).Model(&persistence.ExecutionPlacementPolicy{}).
			Where("execution_target_id = ? AND version = ?", target.ID, current.Version).
			Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "execution_placement_policy_update_failed", "Execution placement policy could not be updated.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "execution_placement_policy_version_conflict", "Execution placement policy changed; reload it before saving.")
		}
		current.Version++
		current.DefaultPoolID = resolved.defaultPool.ID
		current.BalancedPoolID = resolved.balancedPoolID
		current.LowLatencyPoolID = resolved.lowLatencyPoolID
		current.UpdatedBy = &principal.UserID
		current.UpdatedAt = now
		state = toState(current, models)
		return nil
	})
	return state, err
}

func (s *Service) CreatePool(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	input CreatePoolInput,
) (Pool, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Pool{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return Pool{}, err
	}
	normalized, err := normalizePoolInput(input)
	if err != nil {
		return Pool{}, err
	}
	var created Pool
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		target, err := s.loadAccessibleTarget(ctx, tx, tenantID, targetID, true)
		if err != nil {
			return err
		}
		if target.TenantID == nil {
			return problem.New(403, "shared_execution_target_worker_pool_immutable", "Platform-shared execution target worker pools cannot be changed by a tenant.")
		}
		if err := validatePoolTargetMode(target, normalized.Mode); err != nil {
			return err
		}
		now := s.now()
		model := persistence.WorkerPool{
			ID:                 uuid.New(),
			TenantID:           target.TenantID,
			ExecutionTargetID:  target.ID,
			Name:               normalized.Name,
			Mode:               normalized.Mode,
			CapacityClass:      normalized.CapacityClass,
			ClusterID:          normalized.ClusterID,
			Region:             normalized.Region,
			Namespace:          normalized.Namespace,
			DesiredIdleUnits:   normalized.DesiredIdleUnits,
			MaxActiveUnits:     normalized.MaxActiveUnits,
			SchedulingTemplate: normalized.SchedulingTemplate,
			Status:             normalized.Status,
			Version:            1,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return problem.Wrap(409, "worker_pool_create_rejected", "Worker pool could not be created.", err)
		}
		created = toPool(model)
		return nil
	})
	return created, err
}

func (s *Service) UpdatePool(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID, poolID uuid.UUID,
	input UpdatePoolInput,
) (Pool, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Pool{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return Pool{}, err
	}
	if input.ExpectedVersion <= 0 {
		return Pool{}, problem.New(400, "invalid_worker_pool_version", "expectedVersion must be positive.")
	}
	normalized, err := normalizePoolInput(input.CreatePoolInput)
	if err != nil {
		return Pool{}, err
	}
	var updated Pool
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		target, err := s.loadAccessibleTarget(ctx, tx, tenantID, targetID, true)
		if err != nil {
			return err
		}
		if target.TenantID == nil {
			return problem.New(403, "shared_execution_target_worker_pool_immutable", "Platform-shared execution target worker pools cannot be changed by a tenant.")
		}
		if err := validatePoolTargetMode(target, normalized.Mode); err != nil {
			return err
		}
		var current persistence.WorkerPool
		err = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND execution_target_id = ? AND tenant_id = ?", poolID, targetID, tenantID).
			Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "worker_pool_not_found", "Worker pool not found.")
		}
		if err != nil {
			return problem.Wrap(500, "worker_pool_load_failed", "Worker pool could not be loaded.", err)
		}
		if current.Version != input.ExpectedVersion {
			return problem.New(409, "worker_pool_version_conflict", "Worker pool changed; reload it before saving.")
		}
		if current.Mode != normalized.Mode || current.CapacityClass != normalized.CapacityClass {
			return problem.New(409, "worker_pool_identity_immutable", "Worker pool mode and capacityClass cannot change after creation.")
		}
		var blocked int64
		if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where(
				"execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ? AND status IN ?",
				target.ID,
				current.ID,
				current.Version,
				[]string{"queued", "recovering", "leased", "running", "waiting-for-approval", "suspended"},
			).
			Count(&blocked).Error; err != nil {
			return problem.Wrap(500, "worker_pool_update_probe_failed", "Worker pool update safety could not be evaluated.", err)
		}
		if blocked > 0 {
			return problem.New(
				409,
				"worker_pool_version_update_blocked",
				"Worker pool version cannot change while a nonterminal Execution still references the current pool snapshot.",
			)
		}
		now := s.now()
		expectedVersion := current.Version
		current.Name = normalized.Name
		current.Mode = normalized.Mode
		current.CapacityClass = normalized.CapacityClass
		current.ClusterID = normalized.ClusterID
		current.Region = normalized.Region
		current.Namespace = normalized.Namespace
		current.DesiredIdleUnits = normalized.DesiredIdleUnits
		current.MaxActiveUnits = normalized.MaxActiveUnits
		current.SchedulingTemplate = normalized.SchedulingTemplate
		current.Status = normalized.Status
		current.Version++
		current.UpdatedAt = now
		result := tx.WithContext(ctx).Model(&persistence.WorkerPool{}).
			Where("id = ? AND version = ?", current.ID, expectedVersion).
			Select(
				"name", "mode", "capacity_class", "cluster_id", "region", "namespace",
				"desired_idle_units", "max_active_units", "scheduling_template", "status", "version", "updated_at",
			).
			Updates(&current)
		if result.Error != nil {
			return problem.Wrap(500, "worker_pool_update_failed", "Worker pool could not be updated.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "worker_pool_version_conflict", "Worker pool changed; reload it before saving.")
		}
		updated = toPool(current)
		return nil
	})
	return updated, err
}

func (s *Service) EnsureDefault(ctx context.Context, tx *gorm.DB, target persistence.ExecutionTarget) (State, error) {
	policy, pools, err := s.ensureDefaultLocked(ctx, nonNilDB(tx, s.db), target)
	if err != nil {
		return State{}, err
	}
	return toState(policy, pools), nil
}

func (s *Service) SelectExecution(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	warmPoolMode string,
) (Selection, error) {
	policy, models, err := s.ensureDefaultLocked(ctx, nonNilDB(tx, s.db), target)
	if err != nil {
		return Selection{}, err
	}
	return s.selectExecutionFromPolicy(ctx, nonNilDB(tx, s.db), target, policy, models, warmPoolMode)
}

func (s *Service) PreviewExecution(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	warmPoolMode string,
) (Selection, error) {
	mode, err := normalizeWarmPoolMode(warmPoolMode)
	if err != nil {
		return Selection{}, err
	}
	models, err := s.loadPools(ctx, nonNilDB(tx, s.db), target.ID)
	if err != nil {
		return Selection{}, err
	}
	var policy persistence.ExecutionPlacementPolicy
	err = nonNilDB(tx, s.db).WithContext(ctx).
		Where("execution_target_id = ?", target.ID).
		Take(&policy).Error
	if err == nil {
		return s.selectExecutionFromPolicy(ctx, nonNilDB(tx, s.db), target, policy, models, mode)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Selection{}, problem.Wrap(500, "execution_placement_policy_load_failed", "Execution placement policy could not be loaded.", err)
	}

	defaultPool := previewDefaultPool(target, models)
	if defaultPool.Status != PoolStatusActive {
		return Selection{}, problem.New(409, "execution_placement_policy_invalid", "Execution placement default pool must be active.")
	}
	selected := toPool(defaultPool)
	return Selection{Pool: selected, CapacityClass: selected.CapacityClass, PolicyVersion: 1}, nil
}

func (s *Service) selectExecutionFromPolicy(
	ctx context.Context,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	policy persistence.ExecutionPlacementPolicy,
	models []persistence.WorkerPool,
	warmPoolMode string,
) (Selection, error) {
	mode, err := normalizeWarmPoolMode(warmPoolMode)
	if err != nil {
		return Selection{}, err
	}
	byID := make(map[uuid.UUID]persistence.WorkerPool, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	candidates := make([]uuid.UUID, 0, 3)
	switch mode {
	case WarmPoolModeDisabled, WarmPoolModeDefault:
		candidates = append(candidates, policy.DefaultPoolID)
	case WarmPoolModeBalanced:
		if policy.BalancedPoolID != nil {
			candidates = append(candidates, *policy.BalancedPoolID)
		}
		candidates = append(candidates, policy.DefaultPoolID)
	case WarmPoolModeLowLatency:
		if policy.LowLatencyPoolID != nil {
			candidates = append(candidates, *policy.LowLatencyPoolID)
		}
		if policy.BalancedPoolID != nil {
			candidates = append(candidates, *policy.BalancedPoolID)
		}
		candidates = append(candidates, policy.DefaultPoolID)
	}
	seen := make(map[uuid.UUID]struct{}, len(candidates))
	var coldFallback *persistence.WorkerPool
	var capacity *warmcapacity.Service
	if target.TenantID != nil {
		capacity = warmcapacity.NewService(db)
	}
	now := s.now()
	for _, candidateID := range candidates {
		if _, exists := seen[candidateID]; exists {
			continue
		}
		seen[candidateID] = struct{}{}
		pool, ok := byID[candidateID]
		if !ok || pool.Status != PoolStatusActive {
			continue
		}
		if coldFallback == nil {
			candidate := pool
			coldFallback = &candidate
		}
		if capacity == nil || pool.Mode != PoolModeWarm {
			continue
		}
		observation, err := capacity.GetFresh(ctx, warmcapacity.FreshLookup{
			TenantID:          *target.TenantID,
			ExecutionTargetID: target.ID,
			WorkerPoolID:      pool.ID,
			WorkerPoolVersion: pool.Version,
			Now:               now,
		})
		if err != nil {
			return Selection{}, err
		}
		if observation != nil && observation.WarmSupported && observation.ReadyIdleUnits > 0 {
			return selectionForPool(pool, policy.Version), nil
		}
	}
	if coldFallback != nil {
		return selectionForPool(*coldFallback, policy.Version), nil
	}
	return Selection{}, problem.New(409, "execution_placement_policy_invalid", "Execution placement policy does not resolve to an active Worker pool.")
}

func selectionForPool(pool persistence.WorkerPool, policyVersion int64) Selection {
	selected := toPool(pool)
	return Selection{Pool: selected, CapacityClass: selected.CapacityClass, PolicyVersion: policyVersion}
}

func previewDefaultPool(
	target persistence.ExecutionTarget,
	models []persistence.WorkerPool,
) persistence.WorkerPool {
	defaultID := deterministicUUID(target.ID.String() + ":worker-pool:default:000058")
	for _, model := range models {
		if model.ID == defaultID || strings.EqualFold(model.Name, "default") {
			return model
		}
	}
	return persistence.WorkerPool{
		ID:                 defaultID,
		TenantID:           target.TenantID,
		ExecutionTargetID:  target.ID,
		Name:               "default",
		Mode:               defaultPoolMode(target.Kind),
		CapacityClass:      CapacityClassStandard,
		ClusterID:          "",
		Region:             "",
		Namespace:          "",
		DesiredIdleUnits:   0,
		MaxActiveUnits:     1,
		SchedulingTemplate: map[string]any{},
		Status:             PoolStatusActive,
		Version:            1,
	}
}

type resolvedPolicyInput struct {
	defaultPool      persistence.WorkerPool
	balancedPoolID   *uuid.UUID
	lowLatencyPoolID *uuid.UUID
}

func validatePolicyInput(
	target persistence.ExecutionTarget,
	models []persistence.WorkerPool,
	defaultPoolID uuid.UUID,
	balancedPoolID, lowLatencyPoolID *uuid.UUID,
) (resolvedPolicyInput, error) {
	byID := make(map[uuid.UUID]persistence.WorkerPool, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	defaultPool, ok := byID[defaultPoolID]
	if !ok || !sameUUIDPointer(defaultPool.TenantID, target.TenantID) || defaultPool.ExecutionTargetID != target.ID {
		return resolvedPolicyInput{}, problem.New(409, "execution_placement_pool_not_found", "Execution placement default pool is unavailable for this target.")
	}
	if defaultPool.Status != PoolStatusActive {
		return resolvedPolicyInput{}, problem.New(409, "execution_placement_policy_invalid", "Execution placement default pool must be active.")
	}
	if balancedPoolID != nil {
		if balanced, ok := byID[*balancedPoolID]; !ok || !sameUUIDPointer(balanced.TenantID, target.TenantID) || balanced.ExecutionTargetID != target.ID {
			return resolvedPolicyInput{}, problem.New(409, "execution_placement_pool_not_found", "Execution placement balanced pool is unavailable for this target.")
		}
	}
	if lowLatencyPoolID != nil {
		if lowLatency, ok := byID[*lowLatencyPoolID]; !ok || !sameUUIDPointer(lowLatency.TenantID, target.TenantID) || lowLatency.ExecutionTargetID != target.ID {
			return resolvedPolicyInput{}, problem.New(409, "execution_placement_pool_not_found", "Execution placement low-latency pool is unavailable for this target.")
		}
	}
	return resolvedPolicyInput{
		defaultPool:      defaultPool,
		balancedPoolID:   balancedPoolID,
		lowLatencyPoolID: lowLatencyPoolID,
	}, nil
}

func (s *Service) ensureDefaultLocked(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
) (persistence.ExecutionPlacementPolicy, []persistence.WorkerPool, error) {
	lockedTarget, err := s.lockTarget(ctx, tx, target.ID)
	if err != nil {
		return persistence.ExecutionPlacementPolicy{}, nil, err
	}
	models, err := s.loadPools(ctx, tx, lockedTarget.ID)
	if err != nil {
		return persistence.ExecutionPlacementPolicy{}, nil, err
	}
	var policy persistence.ExecutionPlacementPolicy
	err = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("execution_target_id = ?", lockedTarget.ID).
		Take(&policy).Error
	if err == nil {
		if !sameUUIDPointer(policy.TenantID, lockedTarget.TenantID) {
			return persistence.ExecutionPlacementPolicy{}, nil, problem.New(409, "execution_placement_policy_invalid", "Execution placement policy ownership does not match the execution target.")
		}
		return policy, models, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionPlacementPolicy{}, nil, problem.Wrap(500, "execution_placement_policy_load_failed", "Execution placement policy could not be loaded.", err)
	}
	defaultPool, models, err := s.ensureDefaultPool(ctx, tx, lockedTarget, models)
	if err != nil {
		return persistence.ExecutionPlacementPolicy{}, nil, err
	}
	now := s.now()
	policy = persistence.ExecutionPlacementPolicy{
		TenantID:          lockedTarget.TenantID,
		ExecutionTargetID: lockedTarget.ID,
		Version:           1,
		DefaultPoolID:     defaultPool.ID,
		UpdatedAt:         now,
	}
	if err := tx.WithContext(ctx).Create(&policy).Error; err != nil {
		return persistence.ExecutionPlacementPolicy{}, nil, problem.Wrap(409, "execution_placement_policy_create_rejected", "Execution placement policy could not be created.", err)
	}
	return policy, models, nil
}

func (s *Service) ensureDefaultPool(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	models []persistence.WorkerPool,
) (persistence.WorkerPool, []persistence.WorkerPool, error) {
	defaultID := deterministicUUID(target.ID.String() + ":worker-pool:default:000058")
	for _, model := range models {
		if model.ID == defaultID || strings.EqualFold(model.Name, "default") {
			return model, models, nil
		}
	}
	now := s.now()
	model := persistence.WorkerPool{
		ID:                 defaultID,
		TenantID:           target.TenantID,
		ExecutionTargetID:  target.ID,
		Name:               "default",
		Mode:               defaultPoolMode(target.Kind),
		CapacityClass:      CapacityClassStandard,
		ClusterID:          "",
		Region:             "",
		Namespace:          "",
		DesiredIdleUnits:   0,
		MaxActiveUnits:     1,
		SchedulingTemplate: map[string]any{},
		Status:             PoolStatusActive,
		Version:            1,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
		return persistence.WorkerPool{}, nil, problem.Wrap(409, "worker_pool_create_rejected", "Worker pool could not be created.", err)
	}
	models = append(models, model)
	return model, models, nil
}

func (s *Service) loadAccessibleTarget(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, targetID uuid.UUID,
	forUpdate bool,
) (persistence.ExecutionTarget, error) {
	query := tx.WithContext(ctx)
	if forUpdate {
		query = persistence.WithLocking(query, "UPDATE", "")
	}
	var target persistence.ExecutionTarget
	err := query.Where("id = ? AND (tenant_id = ? OR tenant_id IS NULL)", targetID, tenantID).Take(&target).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	if err != nil {
		return persistence.ExecutionTarget{}, problem.Wrap(500, "execution_target_lookup_failed", "Execution target could not be loaded.", err)
	}
	return target, nil
}

func (s *Service) lockTarget(ctx context.Context, tx *gorm.DB, targetID uuid.UUID) (persistence.ExecutionTarget, error) {
	var target persistence.ExecutionTarget
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ?", targetID).
		Take(&target).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	if err != nil {
		return persistence.ExecutionTarget{}, problem.Wrap(500, "execution_target_lookup_failed", "Execution target could not be loaded.", err)
	}
	return target, nil
}

func (s *Service) loadPools(ctx context.Context, tx *gorm.DB, targetID uuid.UUID) ([]persistence.WorkerPool, error) {
	models := make([]persistence.WorkerPool, 0)
	if err := tx.WithContext(ctx).
		Where("execution_target_id = ?", targetID).
		Order("CASE WHEN status = 'active' THEN 0 ELSE 1 END, LOWER(name), id").
		Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "worker_pools_load_failed", "Worker pools could not be loaded.", err)
	}
	return models, nil
}

func toState(policy persistence.ExecutionPlacementPolicy, models []persistence.WorkerPool) State {
	pools := make([]Pool, 0, len(models))
	for _, model := range models {
		pools = append(pools, toPool(model))
	}
	return State{Pools: pools, Policy: toPolicy(policy)}
}

func toPool(model persistence.WorkerPool) Pool {
	template := model.SchedulingTemplate
	if template == nil {
		template = map[string]any{}
	}
	return Pool{
		ID:                 model.ID,
		TenantID:           model.TenantID,
		ExecutionTargetID:  model.ExecutionTargetID,
		Name:               model.Name,
		Mode:               model.Mode,
		CapacityClass:      model.CapacityClass,
		ClusterID:          model.ClusterID,
		Region:             model.Region,
		Namespace:          model.Namespace,
		DesiredIdleUnits:   model.DesiredIdleUnits,
		MaxActiveUnits:     model.MaxActiveUnits,
		SchedulingTemplate: template,
		Status:             model.Status,
		Version:            model.Version,
		CreatedAt:          model.CreatedAt,
		UpdatedAt:          model.UpdatedAt,
	}
}

func toPolicy(model persistence.ExecutionPlacementPolicy) Policy {
	return Policy{
		TenantID:          model.TenantID,
		ExecutionTargetID: model.ExecutionTargetID,
		Version:           model.Version,
		DefaultPoolID:     model.DefaultPoolID,
		BalancedPoolID:    model.BalancedPoolID,
		LowLatencyPoolID:  model.LowLatencyPoolID,
		UpdatedBy:         model.UpdatedBy,
		UpdatedAt:         model.UpdatedAt,
	}
}

func defaultPoolMode(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "kubernetes":
		return PoolModePerExecution
	case "docker":
		return PoolModeResident
	default:
		return PoolModeResident
	}
}

func normalizeWarmPoolMode(mode string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	if normalized == "" {
		normalized = WarmPoolModeDefault
	}
	switch normalized {
	case WarmPoolModeDefault, WarmPoolModeDisabled, WarmPoolModeBalanced, WarmPoolModeLowLatency:
		return normalized, nil
	default:
		return "", problem.New(400, "invalid_warm_pool_mode", "warmPoolMode must be disabled, default, balanced, or low-latency.")
	}
}

func normalizeOptionalPoolID(value *uuid.UUID) (*uuid.UUID, error) {
	if value == nil {
		return nil, nil
	}
	if *value == uuid.Nil {
		return nil, problem.New(400, "invalid_execution_placement_pool_id", "Pool references must not be empty UUIDs.")
	}
	return value, nil
}

func normalizePoolInput(input CreatePoolInput) (CreatePoolInput, error) {
	name := strings.TrimSpace(input.Name)
	if len(name) == 0 || len(name) > 160 {
		return CreatePoolInput{}, problem.New(400, "invalid_worker_pool_name", "Worker pool name must be between 1 and 160 characters.")
	}
	mode := strings.ToLower(strings.TrimSpace(input.Mode))
	switch mode {
	case PoolModeResident, PoolModePerExecution, PoolModeWarm:
	default:
		return CreatePoolInput{}, problem.New(400, "invalid_worker_pool_mode", "Worker pool mode must be resident, per-execution, or warm.")
	}
	capacityClass := strings.ToLower(strings.TrimSpace(input.CapacityClass))
	switch capacityClass {
	case CapacityClassStandard, CapacityClassInteractive:
	default:
		return CreatePoolInput{}, problem.New(400, "invalid_worker_pool_capacity_class", "Worker pool capacityClass must be standard or interactive.")
	}
	status := strings.ToLower(strings.TrimSpace(input.Status))
	switch status {
	case PoolStatusActive, PoolStatusDraining, PoolStatusDisabled:
	default:
		return CreatePoolInput{}, problem.New(400, "invalid_worker_pool_status", "Worker pool status must be active, draining, or disabled.")
	}
	if input.DesiredIdleUnits < 0 || input.MaxActiveUnits < input.DesiredIdleUnits {
		return CreatePoolInput{}, problem.New(400, "invalid_worker_pool_capacity_bounds", "Worker pool desiredIdleUnits and maxActiveUnits are invalid.")
	}
	template := input.SchedulingTemplate
	if template == nil {
		template = map[string]any{}
	}
	return CreatePoolInput{
		Name:               name,
		Mode:               mode,
		CapacityClass:      capacityClass,
		ClusterID:          strings.TrimSpace(input.ClusterID),
		Region:             strings.TrimSpace(input.Region),
		Namespace:          strings.TrimSpace(input.Namespace),
		DesiredIdleUnits:   input.DesiredIdleUnits,
		MaxActiveUnits:     input.MaxActiveUnits,
		SchedulingTemplate: template,
		Status:             status,
	}, nil
}

func validatePoolTargetMode(target persistence.ExecutionTarget, mode string) error {
	if mode == PoolModeWarm && !strings.EqualFold(strings.TrimSpace(target.Kind), "kubernetes") {
		return problem.New(
			409,
			"worker_pool_mode_target_unsupported",
			"Warm Worker pools are only supported on Kubernetes execution targets.",
		)
	}
	return nil
}

func deterministicUUID(seed string) uuid.UUID {
	sum := md5.Sum([]byte(seed))
	raw := hex.EncodeToString(sum[:])
	id, err := uuid.Parse(raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32])
	if err != nil {
		panic(err)
	}
	return id
}

func sameUUIDPointer(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func nonNilDB(tx, fallback *gorm.DB) *gorm.DB {
	if tx != nil {
		return tx
	}
	return fallback
}

func requireActiveTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}
