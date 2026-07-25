package warmcapacity

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	poolModeWarm             = "warm"
	poolStatusDisabled       = "disabled"
	targetKindKubernetes     = "kubernetes"
	targetStatusActive       = "active"
	capacityClassStandard    = "standard"
	capacityClassInteractive = "interactive"
	releaseChannelPromoted   = "promoted"
	releaseChannelCanary     = "canary"
	minObservationTTL        = 10 * time.Second
	maxObservationTTL        = time.Hour
	maxReasonLength          = 2000
)

type Observation struct {
	TenantID                uuid.UUID
	ExecutionTargetID       uuid.UUID
	WorkerPoolID            uuid.UUID
	WorkerPoolVersion       int64
	WarmSupported           bool
	WorkerReleaseRevisionID *uuid.UUID
	WorkerReleaseChannel    *string
	DesiredTotalUnits       int
	ClaimedUnits            int
	ReadyIdleUnits          int
	Source                  string
	Reason                  *string
	ObservedAt              time.Time
	TTL                     time.Duration
}

type FreshLookup struct {
	TenantID                uuid.UUID
	ExecutionTargetID       uuid.UUID
	WorkerPoolID            uuid.UUID
	WorkerPoolVersion       int64
	WorkerReleaseRevisionID *uuid.UUID
	WorkerReleaseChannel    *string
	Now                     time.Time
}

type Service struct {
	db  *gorm.DB
	now func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{
		db:  db,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) Observe(ctx context.Context, input Observation) (persistence.WorkerPoolWarmCapacity, error) {
	if input.TenantID == uuid.Nil || input.ExecutionTargetID == uuid.Nil || input.WorkerPoolID == uuid.Nil || input.WorkerPoolVersion <= 0 {
		return persistence.WorkerPoolWarmCapacity{}, problem.New(400, "invalid_warm_capacity_scope", "Tenant, Execution Target, Worker Pool, and positive Worker Pool version are required.")
	}
	if input.TTL < minObservationTTL || input.TTL > maxObservationTTL {
		return persistence.WorkerPoolWarmCapacity{}, problem.New(400, "invalid_warm_capacity_ttl", "Warm capacity TTL must be between 10 seconds and 1 hour.")
	}
	source := strings.TrimSpace(input.Source)
	if source == "" || len(source) > 160 {
		return persistence.WorkerPoolWarmCapacity{}, problem.New(400, "invalid_warm_capacity_source", "Warm capacity source must be between 1 and 160 characters.")
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return persistence.WorkerPoolWarmCapacity{}, err
	}
	releaseRevisionID, releaseChannel, err := normalizeReleasePair(input.WorkerReleaseRevisionID, input.WorkerReleaseChannel)
	if err != nil {
		return persistence.WorkerPoolWarmCapacity{}, err
	}
	if err := validateCounters(input.WarmSupported, input.DesiredTotalUnits, input.ClaimedUnits, input.ReadyIdleUnits); err != nil {
		return persistence.WorkerPoolWarmCapacity{}, err
	}
	observedAt := input.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = s.now()
	}

	var result persistence.WorkerPoolWarmCapacity
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		target, pool, err := loadScopedWarmPool(ctx, tx, input.TenantID, input.ExecutionTargetID, input.WorkerPoolID)
		if err != nil {
			return err
		}
		if target.Kind != targetKindKubernetes || target.Status != targetStatusActive || target.TenantID == nil {
			return problem.New(409, "warm_capacity_target_unsupported", "Warm capacity authority is only available for active tenant-owned Kubernetes Execution Targets.")
		}
		if pool.Mode != poolModeWarm {
			return problem.New(409, "warm_capacity_pool_mode_invalid", "Warm capacity authority only applies to Worker Pools in warm mode.")
		}
		if pool.Status == poolStatusDisabled {
			return problem.New(409, "warm_capacity_pool_disabled", "Warm capacity authority cannot be written for a disabled Worker Pool.")
		}
		if pool.Version != input.WorkerPoolVersion {
			return problem.New(409, "warm_capacity_pool_version_stale", "Warm capacity authority must reference the current Worker Pool version.")
		}
		if pool.DesiredIdleUnits > pool.MaxActiveUnits {
			return problem.New(409, "warm_capacity_pool_shape_invalid", "Worker Pool desired idle units exceed max active units.")
		}
		if input.DesiredTotalUnits > pool.MaxActiveUnits {
			return problem.New(400, "invalid_warm_capacity_counters", "Warm desired total units must not exceed the Worker Pool max active units.")
		}
		if releaseRevisionID != nil {
			var revision persistence.WorkerReleaseRevision
			if err := tx.WithContext(ctx).
				Where("id = ? AND tenant_id = ? AND execution_target_id = ?", *releaseRevisionID, input.TenantID, input.ExecutionTargetID).
				Take(&revision).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return problem.New(409, "warm_capacity_release_pair_invalid", "Warm capacity authority must reference a Worker release revision owned by this Execution Target.")
				}
				return problem.Wrap(500, "warm_capacity_release_lookup_failed", "Warm capacity release metadata could not be loaded.", err)
			}
		}

		var current persistence.WorkerPoolWarmCapacity
		err = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ?",
				input.TenantID, input.ExecutionTargetID, input.WorkerPoolID, input.WorkerPoolVersion).
			Take(&current).Error
		now := s.now()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = persistence.WorkerPoolWarmCapacity{
				WorkerPoolID:            input.WorkerPoolID,
				WorkerPoolVersion:       input.WorkerPoolVersion,
				TenantID:                input.TenantID,
				ExecutionTargetID:       input.ExecutionTargetID,
				CapacityClass:           pool.CapacityClass,
				WarmSupported:           input.WarmSupported,
				WorkerReleaseRevisionID: releaseRevisionID,
				WorkerReleaseChannel:    releaseChannel,
				DesiredIdleUnits:        pool.DesiredIdleUnits,
				MaxActiveUnits:          pool.MaxActiveUnits,
				DesiredTotalUnits:       input.DesiredTotalUnits,
				ClaimedUnits:            input.ClaimedUnits,
				ReadyIdleUnits:          input.ReadyIdleUnits,
				Source:                  source,
				Reason:                  reason,
				ObservedAt:              observedAt,
				ExpiresAt:               observedAt.Add(input.TTL),
				Version:                 1,
				UpdatedAt:               now,
			}
			if err := tx.WithContext(ctx).Create(&result).Error; err != nil {
				return problem.Wrap(409, "warm_capacity_create_rejected", "Warm capacity authority could not be created.", err)
			}
			return nil
		}
		if err != nil {
			return problem.Wrap(500, "warm_capacity_load_failed", "Warm capacity authority could not be loaded.", err)
		}
		if !observedAt.After(current.ObservedAt) {
			return problem.New(409, "warm_capacity_observation_stale", "Warm capacity authority must advance observedAt monotonically.")
		}
		result = current
		result.CapacityClass = pool.CapacityClass
		result.WarmSupported = input.WarmSupported
		result.WorkerReleaseRevisionID = releaseRevisionID
		result.WorkerReleaseChannel = releaseChannel
		result.DesiredIdleUnits = pool.DesiredIdleUnits
		result.MaxActiveUnits = pool.MaxActiveUnits
		result.DesiredTotalUnits = input.DesiredTotalUnits
		result.ClaimedUnits = input.ClaimedUnits
		result.ReadyIdleUnits = input.ReadyIdleUnits
		result.Source = source
		result.Reason = reason
		result.ObservedAt = observedAt
		result.ExpiresAt = observedAt.Add(input.TTL)
		result.Version = current.Version + 1
		result.UpdatedAt = now

		updates := map[string]any{
			"capacity_class":             result.CapacityClass,
			"warm_supported":             result.WarmSupported,
			"worker_release_revision_id": result.WorkerReleaseRevisionID,
			"worker_release_channel":     result.WorkerReleaseChannel,
			"desired_idle_units":         result.DesiredIdleUnits,
			"max_active_units":           result.MaxActiveUnits,
			"desired_total_units":        result.DesiredTotalUnits,
			"claimed_units":              result.ClaimedUnits,
			"ready_idle_units":           result.ReadyIdleUnits,
			"source":                     result.Source,
			"reason":                     result.Reason,
			"observed_at":                result.ObservedAt,
			"expires_at":                 result.ExpiresAt,
			"version":                    result.Version,
			"updated_at":                 result.UpdatedAt,
		}
		update := tx.WithContext(ctx).Model(&persistence.WorkerPoolWarmCapacity{}).
			Where("tenant_id = ? AND execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ? AND version = ?",
				input.TenantID, input.ExecutionTargetID, input.WorkerPoolID, input.WorkerPoolVersion, current.Version).
			Updates(updates)
		if update.Error != nil {
			return problem.Wrap(409, "warm_capacity_update_rejected", "Warm capacity authority could not be advanced.", update.Error)
		}
		if update.RowsAffected != 1 {
			return problem.New(409, "warm_capacity_version_conflict", "Warm capacity authority changed concurrently; reload and retry.")
		}
		return nil
	})
	return result, err
}

func (s *Service) GetFresh(ctx context.Context, lookup FreshLookup) (*persistence.WorkerPoolWarmCapacity, error) {
	if lookup.TenantID == uuid.Nil || lookup.ExecutionTargetID == uuid.Nil || lookup.WorkerPoolID == uuid.Nil || lookup.WorkerPoolVersion <= 0 {
		return nil, problem.New(400, "invalid_warm_capacity_lookup", "Tenant, Execution Target, Worker Pool, and positive Worker Pool version are required.")
	}
	releaseRevisionID, releaseChannel, err := normalizeReleasePair(lookup.WorkerReleaseRevisionID, lookup.WorkerReleaseChannel)
	if err != nil {
		return nil, err
	}
	at := lookup.Now.UTC()
	if at.IsZero() {
		at = s.now()
	}

	var row persistence.WorkerPoolWarmCapacity
	err = s.db.WithContext(ctx).
		Where("tenant_id = ? AND execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ?",
			lookup.TenantID, lookup.ExecutionTargetID, lookup.WorkerPoolID, lookup.WorkerPoolVersion).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, problem.Wrap(500, "warm_capacity_lookup_failed", "Warm capacity authority could not be loaded.", err)
	}
	if !row.ExpiresAt.After(at) {
		return nil, nil
	}
	if !releasePairMatches(row, releaseRevisionID, releaseChannel) {
		return nil, nil
	}
	return &row, nil
}

func loadScopedWarmPool(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, targetID, poolID uuid.UUID,
) (persistence.ExecutionTarget, persistence.WorkerPool, error) {
	var target persistence.ExecutionTarget
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND tenant_id = ?", targetID, tenantID).
		Take(&target).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return persistence.ExecutionTarget{}, persistence.WorkerPool{}, problem.New(404, "warm_capacity_target_not_found", "Execution Target not found for warm capacity authority.")
		}
		return persistence.ExecutionTarget{}, persistence.WorkerPool{}, problem.Wrap(500, "warm_capacity_target_load_failed", "Execution Target could not be loaded for warm capacity authority.", err)
	}
	var pool persistence.WorkerPool
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND tenant_id = ? AND execution_target_id = ?", poolID, tenantID, targetID).
		Take(&pool).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return persistence.ExecutionTarget{}, persistence.WorkerPool{}, problem.New(404, "warm_capacity_pool_not_found", "Worker Pool not found for warm capacity authority.")
		}
		return persistence.ExecutionTarget{}, persistence.WorkerPool{}, problem.Wrap(500, "warm_capacity_pool_load_failed", "Worker Pool could not be loaded for warm capacity authority.", err)
	}
	return target, pool, nil
}

func validateCounters(warmSupported bool, desiredTotal, claimed, readyIdle int) error {
	if desiredTotal < 0 || claimed < 0 || readyIdle < 0 {
		return problem.New(400, "invalid_warm_capacity_counters", "Warm capacity counters must be non-negative.")
	}
	if readyIdle > desiredTotal {
		return problem.New(400, "invalid_warm_capacity_counters", "Warm ready idle units must not exceed desiredTotalUnits.")
	}
	if !warmSupported && (desiredTotal != 0 || readyIdle != 0) {
		return problem.New(400, "invalid_warm_capacity_counters", "Unsupported warm capacity must report zero desired and ready idle units.")
	}
	return nil
}

func normalizeReason(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil, nil
	}
	if len(trimmed) > maxReasonLength {
		return nil, problem.New(400, "invalid_warm_capacity_reason", "Warm capacity reason is too long.")
	}
	return &trimmed, nil
}

func normalizeReleasePair(revisionID *uuid.UUID, channel *string) (*uuid.UUID, *string, error) {
	if revisionID == nil && channel == nil {
		return nil, nil, nil
	}
	if revisionID == nil || channel == nil {
		return nil, nil, problem.New(400, "invalid_warm_capacity_release_pair", "Warm capacity release revision and channel must be provided together.")
	}
	if *revisionID == uuid.Nil {
		return nil, nil, problem.New(400, "invalid_warm_capacity_release_pair", "Warm capacity release revision must be non-zero.")
	}
	normalized := strings.ToLower(strings.TrimSpace(*channel))
	switch normalized {
	case releaseChannelPromoted, releaseChannelCanary:
		return revisionID, &normalized, nil
	default:
		return nil, nil, problem.New(400, "invalid_warm_capacity_release_pair", "Warm capacity release channel must be promoted or canary.")
	}
}

func releasePairMatches(
	row persistence.WorkerPoolWarmCapacity,
	revisionID *uuid.UUID,
	channel *string,
) bool {
	if revisionID == nil && channel == nil {
		return true
	}
	if row.WorkerReleaseRevisionID == nil || row.WorkerReleaseChannel == nil {
		return false
	}
	return *row.WorkerReleaseRevisionID == *revisionID && *row.WorkerReleaseChannel == *channel
}
