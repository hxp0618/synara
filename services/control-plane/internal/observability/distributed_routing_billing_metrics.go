package observability

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type labeledAmount struct {
	Provider            string `gorm:"column:provider"`
	CurrencyCode        string `gorm:"column:currency_code"`
	ChargeKind          string `gorm:"column:charge_kind"`
	ReconciliationState string `gorm:"column:reconciliation_state"`
	AmountMicros        int64  `gorm:"column:amount_micros"`
	MatchedMicros       int64  `gorm:"column:matched_micros"`
}

type leaseStateMetric struct {
	LeaseName string    `gorm:"column:lease_name"`
	ExpiresAt time.Time `gorm:"column:expires_at"`
}

func (r *Registry) writeDistributedRoutingLeadershipBillingMetrics(
	ctx context.Context,
	output *bytes.Buffer,
	now time.Time,
) error {
	targetHealth, err := pairedCountsWhere(
		ctx, r.db, "execution_target_health", "status", "capacity_status", "",
	)
	if err != nil {
		return fmt.Errorf("collect Execution Target health metrics: %w", err)
	}
	failovers, err := pairedCountsWhere(
		ctx, r.db, "execution_failover_attempts", "status", "reason", "",
	)
	if err != nil {
		return fmt.Errorf("collect Execution failover metrics: %w", err)
	}
	writePairedGauge(
		output, "synara_execution_target_health",
		"Authoritative expiring Execution Target health observations by health and capacity state.",
		"status", "capacity_status", targetHealth,
	)
	writePairedGauge(
		output, "synara_execution_failover_attempts",
		"Authoritative cross-Target successor attempts by immutable status and reason.",
		"status", "reason", failovers,
	)

	var leases []leaseStateMetric
	if err := r.db.WithContext(ctx).Table("reconciler_leases").
		Select("lease_name, expires_at").Order("lease_name").Scan(&leases).Error; err != nil {
		return fmt.Errorf("collect Reconciler leadership metrics: %w", err)
	}
	writeHelp(
		output, "synara_reconciler_leadership",
		"Database-authoritative Reconciler lease inventory by bounded controller and active or expired state.",
		"gauge",
	)
	for _, lease := range leases {
		state := "expired"
		if lease.ExpiresAt.After(now) {
			state = "active"
		}
		fmt.Fprintf(
			output, "synara_reconciler_leadership%s 1\n",
			labels(map[string]string{"controller": boundedReconcilerLeaseName(lease.LeaseName), "state": state}),
		)
	}

	var estimates []labeledAmount
	if err := r.db.WithContext(ctx).Table("billing_estimated_usage_charges").
		Select("provider, currency_code, charge_kind, SUM(amount_micros) AS amount_micros").
		Group("provider, currency_code, charge_kind").Scan(&estimates).Error; err != nil {
		return fmt.Errorf("collect estimated cloud cost metrics: %w", err)
	}
	sortAmounts(estimates)
	writeHelp(
		output, "synara_cloud_cost_estimated_micros",
		"Tariff-rated infrastructure estimate in currency micros; this is not an actual cloud invoice amount.",
		"gauge",
	)
	for _, row := range estimates {
		fmt.Fprintf(
			output, "synara_cloud_cost_estimated_micros%s %d\n",
			labels(map[string]string{
				"provider": row.Provider, "currency": row.CurrencyCode, "charge_kind": row.ChargeKind,
			}),
			row.AmountMicros,
		)
	}

	var actual []labeledAmount
	if err := r.db.WithContext(ctx).Table("billing_actual_invoice_lines").
		Select(`provider, currency_code, charge_kind, reconciliation_state,
			SUM(amount_micros) AS amount_micros,
			SUM(matched_estimate_amount_micros) AS matched_micros`).
		Group("provider, currency_code, charge_kind, reconciliation_state").Scan(&actual).Error; err != nil {
		return fmt.Errorf("collect actual cloud invoice metrics: %w", err)
	}
	sortAmounts(actual)
	writeHelp(
		output, "synara_cloud_cost_actual_micros",
		"Imported actual cloud invoice amount in currency micros by reconciliation state.",
		"gauge",
	)
	writeHelp(
		output, "synara_cloud_cost_reconciliation_variance_micros",
		"Actual cloud invoice micros minus matched tariff estimate micros; non-zero values require FinOps review.",
		"gauge",
	)
	for _, row := range actual {
		metricLabels := labels(map[string]string{
			"provider": row.Provider, "currency": row.CurrencyCode,
			"charge_kind": row.ChargeKind, "reconciliation_state": row.ReconciliationState,
		})
		fmt.Fprintf(output, "synara_cloud_cost_actual_micros%s %d\n", metricLabels, row.AmountMicros)
		fmt.Fprintf(
			output, "synara_cloud_cost_reconciliation_variance_micros%s %d\n",
			metricLabels, row.AmountMicros-row.MatchedMicros,
		)
	}
	return nil
}

func boundedReconcilerLeaseName(value string) string {
	switch value {
	case "synara:docker-worker-pool-reconciler":
		return "docker"
	case "synara:kubernetes-execution-reconciler":
		return "kubernetes"
	case "synara:global-target-failover-sweep":
		return "target-failover"
	case "synara:session-resource-lifecycle":
		return "resource-lifecycle"
	case "synara:worker-release-auto-rollback":
		return "worker-release-auto-rollback"
	case "synara:tenant-retention-sweeper":
		return "retention"
	case "synara:billing-import-scheduler":
		return "billing-import"
	default:
		return "other"
	}
}

func sortAmounts(rows []labeledAmount) {
	sort.Slice(rows, func(left, right int) bool {
		leftKey := strings.Join([]string{
			rows[left].Provider, rows[left].CurrencyCode, rows[left].ChargeKind, rows[left].ReconciliationState,
		}, "\x00")
		rightKey := strings.Join([]string{
			rows[right].Provider, rows[right].CurrencyCode, rows[right].ChargeKind, rows[right].ReconciliationState,
		}, "\x00")
		return leftKey < rightKey
	})
}
