package persistence

import (
	"time"

	"github.com/google/uuid"
)

type SaaSPlan struct {
	Code        string     `gorm:"column:code;primaryKey"`
	DisplayName string     `gorm:"column:display_name"`
	Status      string     `gorm:"column:status"`
	Version     int64      `gorm:"column:version;not null;default:1"`
	TrialDays   int        `gorm:"column:trial_days"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
	RetiredAt   *time.Time `gorm:"column:retired_at"`
}

func (SaaSPlan) TableName() string { return "saas_plans" }

type PlanEntitlement struct {
	PlanCode     string    `gorm:"column:plan_code;primaryKey"`
	Key          string    `gorm:"column:entitlement_key;primaryKey"`
	ValueKind    string    `gorm:"column:value_kind"`
	BooleanValue *bool     `gorm:"column:boolean_value"`
	IntegerValue *int64    `gorm:"column:integer_value"`
	StringValue  *string   `gorm:"column:string_value"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

func (PlanEntitlement) TableName() string { return "plan_entitlements" }

type TenantSubscription struct {
	TenantID               uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	PlanCode               string     `gorm:"column:plan_code"`
	Status                 string     `gorm:"column:status"`
	Version                int64      `gorm:"column:version;not null;default:1"`
	TrialEndsAt            *time.Time `gorm:"column:trial_ends_at"`
	CurrentPeriodStart     time.Time  `gorm:"column:current_period_start"`
	CurrentPeriodEnd       time.Time  `gorm:"column:current_period_end"`
	AssignmentSource       string     `gorm:"column:assignment_source"`
	ExternalCustomerID     *string    `gorm:"column:external_customer_id"`
	ExternalSubscriptionID *string    `gorm:"column:external_subscription_id"`
	BillingProvider        *string    `gorm:"column:billing_provider"`
	ExternalPriceID        *string    `gorm:"column:external_price_id"`
	ProviderObservedAt     *time.Time `gorm:"column:provider_observed_at"`
	CreatedAt              time.Time  `gorm:"column:created_at"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
}

func (TenantSubscription) TableName() string { return "tenant_subscriptions" }

type FeatureFlag struct {
	Key            string     `gorm:"column:key;primaryKey"`
	Description    string     `gorm:"column:description"`
	Status         string     `gorm:"column:status"`
	DefaultEnabled bool       `gorm:"column:default_enabled"`
	Version        int64      `gorm:"column:version;not null;default:1"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
	RetiredAt      *time.Time `gorm:"column:retired_at"`
}

func (FeatureFlag) TableName() string { return "feature_flags" }

type TenantFeatureFlagOverride struct {
	TenantID  uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	FlagKey   string     `gorm:"column:flag_key;primaryKey"`
	Enabled   bool       `gorm:"column:enabled"`
	Version   int64      `gorm:"column:version;not null;default:1"`
	Reason    string     `gorm:"column:reason"`
	ExpiresAt *time.Time `gorm:"column:expires_at"`
	UpdatedBy uuid.UUID  `gorm:"column:updated_by;type:uuid"`
	CreatedAt time.Time  `gorm:"column:created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

func (TenantFeatureFlagOverride) TableName() string { return "tenant_feature_flag_overrides" }
