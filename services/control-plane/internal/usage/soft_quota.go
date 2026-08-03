package usage

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	executionSecondsEntitlement = "usage.execution_seconds_per_period"
	softWarningEntitlement      = "usage.soft_warning_percent"
)

type usagePeriodPolicy struct {
	Subscription persistence.TenantSubscription
	LimitSeconds *int64
	WarningPct   int
}

type periodTotals struct {
	InputTokens         int64 `gorm:"column:input_tokens"`
	CachedInputTokens   int64 `gorm:"column:cached_input_tokens"`
	OutputTokens        int64 `gorm:"column:output_tokens"`
	ReasoningTokens     int64 `gorm:"column:reasoning_tokens"`
	TotalTokens         int64 `gorm:"column:total_tokens"`
	NetworkIngressBytes int64 `gorm:"column:network_ingress_bytes"`
	NetworkEgressBytes  int64 `gorm:"column:network_egress_bytes"`
	DurationMillis      int64 `gorm:"column:duration_millis"`
}

func loadUsagePeriodPolicy(ctx context.Context, db *gorm.DB, tenantID uuid.UUID) (usagePeriodPolicy, error) {
	var subscription persistence.TenantSubscription
	if err := db.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&subscription).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return usagePeriodPolicy{}, problem.New(503, "tenant_entitlement_profile_unavailable", "Tenant entitlement profile is not configured.")
	} else if err != nil {
		return usagePeriodPolicy{}, problem.Wrap(500, "tenant_entitlement_profile_load_failed", "Tenant entitlement profile could not be loaded.", err)
	}
	var entitlements []persistence.PlanEntitlement
	if err := db.WithContext(ctx).
		Where("plan_code = ? AND entitlement_key IN ?", subscription.PlanCode, []string{executionSecondsEntitlement, softWarningEntitlement}).
		Find(&entitlements).Error; err != nil {
		return usagePeriodPolicy{}, problem.Wrap(500, "usage_entitlements_load_failed", "Usage Entitlements could not be loaded.", err)
	}
	policy := usagePeriodPolicy{Subscription: subscription, WarningPct: 80}
	for _, entitlement := range entitlements {
		if entitlement.ValueKind != "integer" || entitlement.IntegerValue == nil {
			return usagePeriodPolicy{}, problem.New(503, "usage_entitlement_invalid", "Usage Entitlements are not configured with integer values.")
		}
		switch entitlement.Key {
		case executionSecondsEntitlement:
			if *entitlement.IntegerValue <= 0 {
				return usagePeriodPolicy{}, problem.New(503, "usage_entitlement_invalid", "Execution seconds Entitlement must be positive.")
			}
			value := *entitlement.IntegerValue
			policy.LimitSeconds = &value
		case softWarningEntitlement:
			if *entitlement.IntegerValue < 1 || *entitlement.IntegerValue >= 100 {
				return usagePeriodPolicy{}, problem.New(503, "usage_entitlement_invalid", "Soft warning Entitlement must be between 1 and 99 percent.")
			}
			policy.WarningPct = int(*entitlement.IntegerValue)
		}
	}
	return policy, nil
}

func loadPeriodTotals(ctx context.Context, db *gorm.DB, tenantID uuid.UUID, start, end time.Time) (periodTotals, error) {
	var totals periodTotals
	err := db.WithContext(ctx).Table("execution_usage_summaries AS usage").
		Select(`COALESCE(SUM(usage.input_tokens), 0) AS input_tokens,
			COALESCE(SUM(usage.cached_input_tokens), 0) AS cached_input_tokens,
			COALESCE(SUM(usage.output_tokens), 0) AS output_tokens,
			COALESCE(SUM(usage.reasoning_tokens), 0) AS reasoning_tokens,
			COALESCE(SUM(usage.total_tokens), 0) AS total_tokens,
			COALESCE(SUM(usage.network_ingress_bytes), 0) AS network_ingress_bytes,
			COALESCE(SUM(usage.network_egress_bytes), 0) AS network_egress_bytes,
			COALESCE(SUM(usage.duration_millis), 0) AS duration_millis`).
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = usage.tenant_id AND execution.id = usage.execution_id").
		Where("usage.tenant_id = ? AND execution.queued_at >= ? AND execution.queued_at < ?", tenantID, start, end).
		Scan(&totals).Error
	if err != nil {
		return periodTotals{}, problem.Wrap(500, "tenant_usage_load_failed", "Tenant Usage could not be loaded.", err)
	}
	return totals, nil
}

func executionSeconds(durationMillis int64) int64 {
	if durationMillis <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(durationMillis) / 1000))
}

func thresholdReached(observed, limit int64, percentage int) bool {
	if observed < 0 || limit <= 0 || percentage < 1 || percentage > 100 {
		return false
	}
	whole := (limit / 100) * int64(percentage)
	remainder := ((limit % 100) * int64(percentage))
	required := whole + (remainder+99)/100
	return observed >= required
}

// ProjectSoftQuotaAlerts reconciles durable threshold crossings from the
// canonical Usage projection. It is intentionally absent from admission: a
// projection failure or limit crossing must never become an Execution stop.
func ProjectSoftQuotaAlerts(ctx context.Context, db *gorm.DB, tenantID uuid.UUID, observedAt time.Time) error {
	policy, err := loadUsagePeriodPolicy(ctx, db, tenantID)
	if err != nil {
		return err
	}
	if policy.LimitSeconds == nil {
		return nil
	}
	totals, err := loadPeriodTotals(ctx, db, tenantID, policy.Subscription.CurrentPeriodStart, policy.Subscription.CurrentPeriodEnd)
	if err != nil {
		return err
	}
	observed := executionSeconds(totals.DurationMillis)
	thresholds := []int{policy.WarningPct, 100}
	for _, threshold := range thresholds {
		if !thresholdReached(observed, *policy.LimitSeconds, threshold) {
			continue
		}
		severity := "warning"
		if threshold == 100 {
			severity = "limit_reached"
		}
		alert := persistence.TenantUsageQuotaAlert{
			ID: uuid.New(), TenantID: tenantID, SubscriptionVersion: policy.Subscription.Version,
			MetricKey: "execution_seconds", PeriodStart: policy.Subscription.CurrentPeriodStart,
			PeriodEnd: policy.Subscription.CurrentPeriodEnd, ThresholdPercent: threshold,
			Severity: severity, LimitUnits: *policy.LimitSeconds, ObservedUnits: observed,
			FirstObservedAt: observedAt, LastObservedAt: observedAt,
		}
		if err := db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "tenant_id"}, {Name: "subscription_version"}, {Name: "metric_key"},
				{Name: "period_start"}, {Name: "threshold_percent"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"period_end", "severity", "limit_units", "observed_units", "last_observed_at",
			}),
		}).Create(&alert).Error; err != nil {
			return problem.Wrap(500, "usage_quota_alert_projection_failed", "Usage quota alert could not be projected.", err)
		}
	}
	return nil
}
