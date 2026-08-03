package entitlements

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/productprofile"
)

type AssignPlanInput struct {
	PlanCode           string     `json:"planCode"`
	Status             string     `json:"status"`
	ExpectedVersion    int64      `json:"expectedVersion"`
	TrialEndsAt        *time.Time `json:"trialEndsAt"`
	CurrentPeriodStart time.Time  `json:"currentPeriodStart"`
	CurrentPeriodEnd   time.Time  `json:"currentPeriodEnd"`
	AssignmentSource   string     `json:"-"`
	Reason             string     `json:"reason"`
}

type SetFeatureOverrideInput struct {
	FlagKey         string
	Enabled         bool
	ExpectedVersion int64
	Reason          string
	ExpiresAt       *time.Time
}

func (s *Service) AssignPlanByPlatform(
	ctx context.Context,
	actorID, tenantID uuid.UUID,
	input AssignPlanInput,
	requestID, ipAddress string,
) (Snapshot, error) {
	input.PlanCode = strings.ToLower(strings.TrimSpace(input.PlanCode))
	input.Status = strings.ToLower(strings.TrimSpace(input.Status))
	input.AssignmentSource = strings.ToLower(strings.TrimSpace(input.AssignmentSource))
	input.Reason = strings.TrimSpace(input.Reason)
	if input.PlanCode == "" || (input.Status != "trialing" && input.Status != "active" && input.Status != "suspended" && input.Status != "cancelled") {
		return Snapshot{}, problem.New(400, "invalid_entitlement_profile_assignment", "Entitlement profile code and status are invalid.")
	}
	if input.AssignmentSource != "platform_admin" {
		return Snapshot{}, problem.New(400, "invalid_entitlement_profile_source", "Platform entitlement profile assignment source is invalid.")
	}
	if input.ExpectedVersion < 1 || !input.CurrentPeriodEnd.After(input.CurrentPeriodStart) || len(input.Reason) < 3 || len(input.Reason) > 500 {
		return Snapshot{}, problem.New(400, "invalid_entitlement_profile_assignment", "Entitlement profile version, reporting period, or reason is invalid.")
	}
	if input.Status == "trialing" && input.TrialEndsAt == nil {
		return Snapshot{}, problem.New(400, "entitlement_profile_evaluation_expiry_required", "An evaluation entitlement profile requires evaluationEndsAt.")
	}

	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var plan persistence.SaaSPlan
		if err := tx.Where("code = ? AND status = ?", input.PlanCode, "active").Take(&plan).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(409, "tenant_entitlement_profile_unavailable", "The requested Tenant entitlement profile is unavailable.")
		} else if err != nil {
			return err
		}
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "tenant_not_found", "Tenant not found.")
		} else if err != nil {
			return err
		}
		var current persistence.TenantSubscription
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ?", tenantID).Take(&current).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(503, "tenant_entitlement_profile_assignment_unavailable", "Tenant entitlement profile assignment is not configured.")
		} else if err != nil {
			return err
		}
		if current.Version != input.ExpectedVersion {
			return problem.New(409, "tenant_entitlement_profile_version_conflict", "Tenant entitlement profile changed; reload it before retrying.")
		}
		if err := tx.Model(&persistence.Tenant{}).Where("id = ?", tenantID).Update("plan_code", input.PlanCode).Error; err != nil {
			return problem.Wrap(409, "tenant_entitlement_profile_projection_update_failed", "Tenant entitlement profile projection could not be updated.", err)
		}
		result := tx.Model(&persistence.TenantSubscription{}).
			Where("tenant_id = ? AND version = ?", tenantID, current.Version).
			Updates(map[string]any{
				"plan_code": input.PlanCode, "status": input.Status, "version": current.Version + 1,
				"trial_ends_at": input.TrialEndsAt, "current_period_start": input.CurrentPeriodStart,
				"current_period_end": input.CurrentPeriodEnd, "assignment_source": input.AssignmentSource,
				"external_customer_id": nil, "external_subscription_id": nil,
				"billing_provider": nil, "external_price_id": nil, "provider_observed_at": nil,
			})
		if result.Error != nil {
			return problem.Wrap(409, "tenant_entitlement_profile_update_failed", "Tenant entitlement profile could not be updated.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "tenant_entitlement_profile_version_conflict", "Tenant entitlement profile changed; reload it before retrying.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &actorID,
			Action: "tenant_entitlement_profile.updated", ResourceType: "tenant_entitlement_profile", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"fromProfileCode": productprofile.PublicProfileCode(current.PlanCode),
				"toProfileCode":   productprofile.PublicProfileCode(input.PlanCode),
				"fromStatus":      productprofile.PublicLifecycleStatus(current.Status),
				"toStatus":        productprofile.PublicLifecycleStatus(input.Status),
				"fromVersion":     current.Version, "toVersion": current.Version + 1,
				"assignmentSource": input.AssignmentSource, "reason": input.Reason,
			},
		})
	})
	if err != nil {
		return Snapshot{}, err
	}
	return s.Resolve(ctx, tenantID)
}

func (s *Service) SetFeatureOverrideByPlatform(
	ctx context.Context,
	actorID, tenantID uuid.UUID,
	input SetFeatureOverrideInput,
	requestID, ipAddress string,
) (Snapshot, error) {
	input.FlagKey = strings.ToLower(strings.TrimSpace(input.FlagKey))
	input.Reason = strings.TrimSpace(input.Reason)
	if input.FlagKey == "" || input.ExpectedVersion < 0 || len(input.Reason) < 3 || len(input.Reason) > 500 {
		return Snapshot{}, problem.New(400, "invalid_feature_flag_override", "Feature Flag key, version, or reason is invalid.")
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(s.now()) {
		return Snapshot{}, problem.New(400, "invalid_feature_flag_expiry", "Feature Flag override expiry must be in the future.")
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var flag persistence.FeatureFlag
		if err := tx.Where("key = ? AND status = ?", input.FlagKey, "active").Take(&flag).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "feature_flag_not_found", "Active Feature Flag not found.")
		} else if err != nil {
			return err
		}
		var current persistence.TenantFeatureFlagOverride
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ? AND flag_key = ?", tenantID, input.FlagKey).Take(&current).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			if input.ExpectedVersion != 0 {
				return problem.New(409, "feature_flag_override_version_conflict", "Feature Flag override changed; reload it before retrying.")
			}
			current = persistence.TenantFeatureFlagOverride{
				TenantID: tenantID, FlagKey: input.FlagKey, Enabled: input.Enabled, Version: 1,
				Reason: input.Reason, ExpiresAt: input.ExpiresAt, UpdatedBy: actorID,
			}
			if err := tx.Create(&current).Error; err != nil {
				return problem.Wrap(409, "feature_flag_override_version_conflict", "Feature Flag override changed; reload it before retrying.", err)
			}
		} else if loadErr != nil {
			return loadErr
		} else {
			if current.Version != input.ExpectedVersion {
				return problem.New(409, "feature_flag_override_version_conflict", "Feature Flag override changed; reload it before retrying.")
			}
			result := tx.Model(&persistence.TenantFeatureFlagOverride{}).
				Where("tenant_id = ? AND flag_key = ? AND version = ?", tenantID, input.FlagKey, current.Version).
				Updates(map[string]any{
					"enabled": input.Enabled, "version": current.Version + 1, "reason": input.Reason,
					"expires_at": input.ExpiresAt, "updated_by": actorID,
				})
			if result.Error != nil || result.RowsAffected != 1 {
				return problem.Wrap(409, "feature_flag_override_version_conflict", "Feature Flag override changed; reload it before retrying.", result.Error)
			}
			current.Version++
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &actorID,
			Action: "tenant_feature_flag.updated", ResourceType: "feature_flag", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"flagKey": input.FlagKey, "enabled": input.Enabled,
				"version": current.Version, "reason": input.Reason,
			},
		})
	})
	if err != nil {
		return Snapshot{}, err
	}
	return s.Resolve(ctx, tenantID)
}
