package entitlements

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/subscriptionpolicy"
)

type Value struct {
	Kind    string  `json:"kind"`
	Boolean *bool   `json:"boolean"`
	Integer *int64  `json:"integer"`
	String  *string `json:"string"`
}

type Plan struct {
	Code        string `json:"code"`
	DisplayName string `json:"displayName"`
	Status      string `json:"status"`
	Version     int64  `json:"version"`
	TrialDays   int    `json:"trialDays"`
}

type Subscription struct {
	Status              string     `json:"status"`
	Version             int64      `json:"version"`
	TrialEndsAt         *time.Time `json:"trialEndsAt"`
	CurrentPeriodStart  time.Time  `json:"currentPeriodStart"`
	CurrentPeriodEnd    time.Time  `json:"currentPeriodEnd"`
	AssignmentSource    string     `json:"assignmentSource"`
	EntitlementsActive  bool       `json:"entitlementsActive"`
	EntitlementGrace    bool       `json:"entitlementGrace"`
	EntitlementPolicy   string     `json:"entitlementPolicy"`
	EntitlementDecision string     `json:"entitlementDecision"`
}

type Snapshot struct {
	TenantID     uuid.UUID        `json:"tenantId"`
	Plan         Plan             `json:"plan"`
	Subscription Subscription     `json:"planAssignment"`
	Entitlements map[string]Value `json:"entitlements"`
	Features     map[string]bool  `json:"features"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	now        func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, authorizer: authorization.NewAuthorizer(db), now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Get(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (Snapshot, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Snapshot{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.QuotaRead); err != nil {
		return Snapshot{}, err
	}
	return s.Resolve(ctx, tenantID)
}

func (s *Service) Resolve(ctx context.Context, tenantID uuid.UUID) (Snapshot, error) {
	type tenantSubscriptionState struct {
		TenantID               uuid.UUID  `gorm:"column:tenant_id"`
		TenantPlanCode         string     `gorm:"column:tenant_plan_code"`
		SubscriptionPlanCode   *string    `gorm:"column:subscription_plan_code"`
		SubscriptionStatus     *string    `gorm:"column:subscription_status"`
		SubscriptionVersion    *int64     `gorm:"column:subscription_version"`
		TrialEndsAt            *time.Time `gorm:"column:trial_ends_at"`
		CurrentPeriodStart     *time.Time `gorm:"column:current_period_start"`
		CurrentPeriodEnd       *time.Time `gorm:"column:current_period_end"`
		SubscriptionAssignment *string    `gorm:"column:subscription_assignment_source"`
	}
	var state tenantSubscriptionState
	if err := s.db.WithContext(ctx).
		Table("tenants AS tenant").
		Select(`tenant.id AS tenant_id, tenant.plan_code AS tenant_plan_code,
			subscription.plan_code AS subscription_plan_code,
			subscription.status AS subscription_status,
			subscription.version AS subscription_version,
			subscription.trial_ends_at AS trial_ends_at,
			subscription.current_period_start AS current_period_start,
			subscription.current_period_end AS current_period_end,
			subscription.assignment_source AS subscription_assignment_source`).
		Joins("LEFT JOIN tenant_subscriptions AS subscription ON subscription.tenant_id = tenant.id").
		Where("tenant.id = ? AND tenant.deleted_at IS NULL", tenantID).
		Take(&state).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Snapshot{}, problem.New(404, "tenant_not_found", "Tenant not found.")
	} else if err != nil {
		return Snapshot{}, problem.Wrap(500, "tenant_entitlements_load_failed", "Tenant entitlements could not be loaded.", err)
	}
	if state.SubscriptionPlanCode == nil || state.SubscriptionStatus == nil || state.SubscriptionVersion == nil ||
		state.CurrentPeriodStart == nil || state.CurrentPeriodEnd == nil || state.SubscriptionAssignment == nil {
		return Snapshot{}, problem.New(503, "tenant_entitlement_profile_assignment_unavailable", "Tenant entitlement profile assignment is not configured.")
	}
	if *state.SubscriptionPlanCode != state.TenantPlanCode {
		return Snapshot{}, problem.New(503, "tenant_entitlement_profile_projection_drift", "Tenant entitlement profile projection does not match its assignment.")
	}
	decision, err := subscriptionpolicy.Evaluate(*state.SubscriptionStatus)
	if err != nil {
		return Snapshot{}, problem.Wrap(503, "tenant_entitlement_policy_invalid", "Tenant entitlement profile status is not covered by the effective policy.", err)
	}
	var plan persistence.SaaSPlan
	if err := s.db.WithContext(ctx).Where("code = ?", state.TenantPlanCode).Take(&plan).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Snapshot{}, problem.New(503, "tenant_entitlement_profile_unavailable", "Tenant entitlement profile is not configured.")
	} else if err != nil {
		return Snapshot{}, problem.Wrap(500, "tenant_entitlement_profile_load_failed", "Tenant entitlement profile could not be loaded.", err)
	}

	var entitlementModels []persistence.PlanEntitlement
	if decision.EntitlementsActive {
		if err := s.db.WithContext(ctx).Where("plan_code = ?", plan.Code).Order("entitlement_key").Find(&entitlementModels).Error; err != nil {
			return Snapshot{}, problem.Wrap(500, "entitlement_profile_values_load_failed", "Entitlement profile values could not be loaded.", err)
		}
	}
	entitlementValues := make(map[string]Value, len(entitlementModels))
	for _, item := range entitlementModels {
		entitlementValues[item.Key] = Value{
			Kind: item.ValueKind, Boolean: item.BooleanValue, Integer: item.IntegerValue, String: item.StringValue,
		}
	}

	var flags []persistence.FeatureFlag
	if err := s.db.WithContext(ctx).Where("status = ?", "active").Order("key").Find(&flags).Error; err != nil {
		return Snapshot{}, problem.Wrap(500, "feature_flags_load_failed", "Feature Flags could not be loaded.", err)
	}
	var overrides []persistence.TenantFeatureFlagOverride
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND (expires_at IS NULL OR expires_at > ?)", tenantID, s.now()).Find(&overrides).Error; err != nil {
		return Snapshot{}, problem.Wrap(500, "feature_flag_overrides_load_failed", "Tenant Feature Flag overrides could not be loaded.", err)
	}
	overrideByKey := make(map[string]bool, len(overrides))
	for _, item := range overrides {
		overrideByKey[item.FlagKey] = item.Enabled
	}
	featureValues := make(map[string]bool, len(flags))
	for _, flag := range flags {
		enabled := flag.DefaultEnabled
		if override, ok := overrideByKey[flag.Key]; ok {
			enabled = override
		}
		featureValues[flag.Key] = enabled
	}
	return Snapshot{
		TenantID: tenantID,
		Plan:     Plan{Code: plan.Code, DisplayName: plan.DisplayName, Status: plan.Status, Version: plan.Version, TrialDays: plan.TrialDays},
		Subscription: Subscription{
			Status: *state.SubscriptionStatus, Version: *state.SubscriptionVersion, TrialEndsAt: state.TrialEndsAt,
			CurrentPeriodStart: *state.CurrentPeriodStart, CurrentPeriodEnd: *state.CurrentPeriodEnd,
			AssignmentSource: *state.SubscriptionAssignment, EntitlementsActive: decision.EntitlementsActive,
			EntitlementGrace: decision.Grace, EntitlementPolicy: subscriptionpolicy.Version,
			EntitlementDecision: decision.Reason,
		},
		Entitlements: entitlementValues,
		Features:     featureValues,
	}, nil
}
