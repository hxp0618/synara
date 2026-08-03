package persistence

import (
	"time"

	"github.com/google/uuid"
)

type TenantUsageQuotaAlert struct {
	ID                  uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	TenantID            uuid.UUID `gorm:"column:tenant_id;type:uuid;uniqueIndex:uq_tenant_usage_quota_alerts_threshold,priority:1"`
	SubscriptionVersion int64     `gorm:"column:subscription_version;uniqueIndex:uq_tenant_usage_quota_alerts_threshold,priority:2"`
	MetricKey           string    `gorm:"column:metric_key;uniqueIndex:uq_tenant_usage_quota_alerts_threshold,priority:3"`
	PeriodStart         time.Time `gorm:"column:period_start;uniqueIndex:uq_tenant_usage_quota_alerts_threshold,priority:4"`
	PeriodEnd           time.Time `gorm:"column:period_end"`
	ThresholdPercent    int       `gorm:"column:threshold_percent;uniqueIndex:uq_tenant_usage_quota_alerts_threshold,priority:5"`
	Severity            string    `gorm:"column:severity"`
	LimitUnits          int64     `gorm:"column:limit_units"`
	ObservedUnits       int64     `gorm:"column:observed_units"`
	FirstObservedAt     time.Time `gorm:"column:first_observed_at"`
	LastObservedAt      time.Time `gorm:"column:last_observed_at"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	UpdatedAt           time.Time `gorm:"column:updated_at"`
}

func (TenantUsageQuotaAlert) TableName() string { return "tenant_usage_quota_alerts" }
