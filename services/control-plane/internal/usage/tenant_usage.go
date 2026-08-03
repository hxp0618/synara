package usage

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type TenantPeriodUsage struct {
	InputTokens               int64            `json:"inputTokens"`
	CachedInputTokens         int64            `json:"cachedInputTokens"`
	OutputTokens              int64            `json:"outputTokens"`
	ReasoningTokens           int64            `json:"reasoningTokens"`
	TotalTokens               int64            `json:"totalTokens"`
	NetworkIngressBytes       int64            `json:"networkIngressBytes"`
	NetworkEgressBytes        int64            `json:"networkEgressBytes"`
	ExecutionSeconds          int64            `json:"executionSeconds"`
	ProviderCostByCurrency    map[string]int64 `json:"providerCostByCurrency"`
	ProviderCostReportedCount int64            `json:"providerCostReportedCount"`
	ProviderCostMissingCount  int64            `json:"providerCostMissingCount"`
	PlatformCharges           []PlatformCharge `json:"platformCharges"`
	PlatformCostByCurrency    map[string]int64 `json:"platformCostByCurrency"`
	KnownCostByCurrency       map[string]int64 `json:"knownCostByCurrency"`
}

type SoftQuota struct {
	Metric            string   `json:"metric"`
	Unit              string   `json:"unit"`
	Limit             *int64   `json:"limit"`
	Observed          int64    `json:"observed"`
	WarningPercent    int      `json:"warningPercent"`
	PercentageUsed    *float64 `json:"percentageUsed"`
	State             string   `json:"state"`
	Enforcement       string   `json:"enforcement"`
	HardStop          bool     `json:"hardStop"`
	RecommendedAction *string  `json:"recommendedAction"`
}

type SoftQuotaAlert struct {
	ID               uuid.UUID `json:"id"`
	Metric           string    `json:"metric"`
	ThresholdPercent int       `json:"thresholdPercent"`
	Severity         string    `json:"severity"`
	Limit            int64     `json:"limit"`
	Observed         int64     `json:"observed"`
	FirstObservedAt  string    `json:"firstObservedAt"`
	LastObservedAt   string    `json:"lastObservedAt"`
}

type TenantUsage struct {
	TenantID            uuid.UUID         `json:"tenantId"`
	SubscriptionVersion int64             `json:"entitlementProfileVersion"`
	PeriodStart         string            `json:"periodStart"`
	PeriodEnd           string            `json:"periodEnd"`
	Usage               TenantPeriodUsage `json:"usage"`
	SoftQuota           SoftQuota         `json:"softQuota"`
	Alerts              []SoftQuotaAlert  `json:"alerts"`
}

func (s *Service) GetTenantUsage(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (TenantUsage, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return TenantUsage{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.QuotaRead); err != nil {
		return TenantUsage{}, err
	}
	policy, err := loadUsagePeriodPolicy(ctx, s.db, tenantID)
	if err != nil {
		return TenantUsage{}, err
	}
	if err := ProjectSoftQuotaAlerts(ctx, s.db, tenantID, s.now()); err != nil {
		return TenantUsage{}, err
	}
	totals, err := loadPeriodTotals(ctx, s.db, tenantID, policy.Subscription.CurrentPeriodStart, policy.Subscription.CurrentPeriodEnd)
	if err != nil {
		return TenantUsage{}, err
	}
	type currencyTotal struct {
		CurrencyCode string `gorm:"column:currency_code"`
		AmountMicros int64  `gorm:"column:amount_micros"`
	}
	var currencyTotals []currencyTotal
	if err := s.db.WithContext(ctx).Table("execution_usage_summaries AS usage").
		Select("usage.currency_code, COALESCE(SUM(usage.provider_cost_micros), 0) AS amount_micros").
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = usage.tenant_id AND execution.id = usage.execution_id").
		Where("usage.tenant_id = ? AND execution.queued_at >= ? AND execution.queued_at < ? AND usage.provider_cost_reported = ?", tenantID, policy.Subscription.CurrentPeriodStart, policy.Subscription.CurrentPeriodEnd, true).
		Group("usage.currency_code").Order("usage.currency_code").Scan(&currencyTotals).Error; err != nil {
		return TenantUsage{}, problem.Wrap(500, "tenant_usage_cost_load_failed", "Tenant Provider cost could not be loaded.", err)
	}
	costs := make(map[string]int64, len(currencyTotals))
	for _, total := range currencyTotals {
		costs[total.CurrencyCode] = total.AmountMicros
	}
	platformCharges, err := s.loadTenantPlatformCharges(
		ctx,
		tenantID,
		policy.Subscription.CurrentPeriodStart,
		policy.Subscription.CurrentPeriodEnd,
	)
	if err != nil {
		return TenantUsage{}, err
	}
	platformCosts := make(map[string]int64)
	knownCosts := make(map[string]int64, len(costs))
	for currency, amount := range costs {
		knownCosts[currency] = amount
	}
	for _, charge := range platformCharges {
		platformCosts[charge.CurrencyCode] += charge.AmountMicros
		knownCosts[charge.CurrencyCode] += charge.AmountMicros
	}
	type providerCostReportingCounts struct {
		Total    int64 `gorm:"column:total_count"`
		Reported int64 `gorm:"column:reported_count"`
	}
	var reportingCounts providerCostReportingCounts
	if err := s.db.WithContext(ctx).Table("execution_usage_summaries AS usage").
		Select("COUNT(*) AS total_count, COALESCE(SUM(CASE WHEN usage.provider_cost_reported THEN 1 ELSE 0 END), 0) AS reported_count").
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = usage.tenant_id AND execution.id = usage.execution_id").
		Where("usage.tenant_id = ? AND execution.queued_at >= ? AND execution.queued_at < ?", tenantID, policy.Subscription.CurrentPeriodStart, policy.Subscription.CurrentPeriodEnd).
		Scan(&reportingCounts).Error; err != nil {
		return TenantUsage{}, problem.Wrap(500, "tenant_usage_cost_coverage_load_failed", "Tenant Provider cost coverage could not be loaded.", err)
	}
	observedSeconds := executionSeconds(totals.DurationMillis)
	softQuota := SoftQuota{
		Metric: "execution_seconds", Unit: "seconds", Limit: policy.LimitSeconds,
		Observed: observedSeconds, WarningPercent: policy.WarningPct,
		State: "unlimited", Enforcement: "soft", HardStop: false,
	}
	if policy.LimitSeconds != nil {
		percentage := float64(observedSeconds) * 100 / float64(*policy.LimitSeconds)
		softQuota.PercentageUsed = &percentage
		softQuota.State = "within_limit"
		if percentage >= 100 {
			softQuota.State = "limit_reached"
			recommendation := "Work continues because this is a soft limit. Ask an internal administrator to adjust the entitlement profile, or wait for the next reporting period before adding background workloads."
			softQuota.RecommendedAction = &recommendation
		} else if percentage >= float64(policy.WarningPct) {
			softQuota.State = "approaching_limit"
			recommendation := "Prefer batch work, reduce background runs, or ask an internal administrator to adjust the entitlement profile before this reporting period ends."
			softQuota.RecommendedAction = &recommendation
		}
	}
	var alertModels []persistence.TenantUsageQuotaAlert
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND subscription_version = ? AND period_start = ?", tenantID, policy.Subscription.Version, policy.Subscription.CurrentPeriodStart).
		Order("threshold_percent, first_observed_at").Find(&alertModels).Error; err != nil {
		return TenantUsage{}, problem.Wrap(500, "usage_quota_alerts_load_failed", "Usage quota alerts could not be loaded.", err)
	}
	alerts := make([]SoftQuotaAlert, 0, len(alertModels))
	for _, alert := range alertModels {
		alerts = append(alerts, SoftQuotaAlert{
			ID: alert.ID, Metric: alert.MetricKey, ThresholdPercent: alert.ThresholdPercent,
			Severity: alert.Severity, Limit: alert.LimitUnits, Observed: alert.ObservedUnits,
			FirstObservedAt: alert.FirstObservedAt.UTC().Format(time.RFC3339Nano),
			LastObservedAt:  alert.LastObservedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return TenantUsage{
		TenantID: tenantID, SubscriptionVersion: policy.Subscription.Version,
		PeriodStart: policy.Subscription.CurrentPeriodStart.UTC().Format(time.RFC3339Nano),
		PeriodEnd:   policy.Subscription.CurrentPeriodEnd.UTC().Format(time.RFC3339Nano),
		Usage: TenantPeriodUsage{
			InputTokens: totals.InputTokens, CachedInputTokens: totals.CachedInputTokens,
			OutputTokens: totals.OutputTokens, ReasoningTokens: totals.ReasoningTokens,
			TotalTokens: totals.TotalTokens, NetworkIngressBytes: totals.NetworkIngressBytes,
			NetworkEgressBytes: totals.NetworkEgressBytes, ExecutionSeconds: observedSeconds,
			ProviderCostByCurrency:    costs,
			ProviderCostReportedCount: reportingCounts.Reported,
			ProviderCostMissingCount:  reportingCounts.Total - reportingCounts.Reported,
			PlatformCharges:           platformCharges,
			PlatformCostByCurrency:    platformCosts,
			KnownCostByCurrency:       knownCosts,
		},
		SoftQuota: softQuota, Alerts: alerts,
	}, nil
}

func (s *Service) loadTenantPlatformCharges(
	ctx context.Context,
	tenantID uuid.UUID,
	periodStart time.Time,
	periodEnd time.Time,
) ([]PlatformCharge, error) {
	type chargeRow struct {
		Kind         string `gorm:"column:charge_kind"`
		CurrencyCode string `gorm:"column:currency_code"`
		AmountMicros int64  `gorm:"column:amount_micros"`
		Source       string `gorm:"column:source"`
	}
	var rows []chargeRow
	if err := s.db.WithContext(ctx).Table("billing_shared_estimated_charge_slices AS slice").
		Select(`slice.charge_kind, tariff.currency_code,
			CASE WHEN actual_run.id IS NULL THEN 'estimated' ELSE 'actual' END AS source,
			SUM(CASE WHEN actual_run.id IS NULL THEN slice.amount_micros ELSE actual_slice.amount_micros END) AS amount_micros`).
		Joins("JOIN worker_claim_facts AS claim ON claim.id = slice.claim_fact_id").
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = claim.tenant_id AND execution.id = claim.execution_id").
		Joins("JOIN billing_provider_tariffs AS tariff ON tariff.id = slice.tariff_id").
		Joins("LEFT JOIN billing_shared_actual_charge_slices AS actual_slice ON actual_slice.estimated_slice_id = slice.id").
		Joins("LEFT JOIN billing_shared_actual_allocation_lines AS actual_line ON actual_line.id = actual_slice.allocation_line_id").
		Joins("LEFT JOIN billing_shared_actual_allocation_runs AS actual_run ON actual_run.id = actual_line.run_id AND actual_run.state = ?", "sealed").
		Where("slice.tenant_id = ? AND execution.queued_at >= ? AND execution.queued_at < ?", tenantID, periodStart, periodEnd).
		Group("slice.charge_kind, tariff.currency_code, CASE WHEN actual_run.id IS NULL THEN 'estimated' ELSE 'actual' END").
		Order("tariff.currency_code, slice.charge_kind, CASE WHEN actual_run.id IS NULL THEN 'estimated' ELSE 'actual' END").
		Scan(&rows).Error; err != nil {
		return nil, problem.Wrap(500, "tenant_usage_platform_cost_load_failed", "Tenant platform cost allocation could not be loaded.", err)
	}
	charges := make([]PlatformCharge, 0, len(rows))
	for _, row := range rows {
		charges = append(charges, PlatformCharge{
			Kind: row.Kind, CurrencyCode: row.CurrencyCode, AmountMicros: row.AmountMicros, Source: row.Source,
		})
	}
	return charges, nil
}
