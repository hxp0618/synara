package observability

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
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

type warmCapacityAuthorityGroup struct {
	CapacityClass string `gorm:"column:capacity_class"`
	WarmSupported bool   `gorm:"column:warm_supported"`
	Count         int64  `gorm:"column:count"`
}

type warmCapacityUnitGroup struct {
	CapacityClass     string `gorm:"column:capacity_class"`
	DesiredIdleUnits  int64  `gorm:"column:desired_idle_units"`
	MinIdleUnits      int64  `gorm:"column:min_idle_units"`
	DesiredTotalUnits int64  `gorm:"column:desired_total_units"`
	ClaimedUnits      int64  `gorm:"column:claimed_units"`
	ReadyIdleUnits    int64  `gorm:"column:ready_idle_units"`
	DeficitUnits      int64  `gorm:"column:deficit_units"`
}

type warmCapacityAuthorityMetricKey struct {
	CapacityClass string
	Freshness     string
	WarmSupported bool
}

type sharedAllocationSchedulePeriodMetric struct {
	ScheduleKind string `gorm:"column:schedule_kind"`
	LastOutcome  string `gorm:"column:last_outcome"`
	DueState     string `gorm:"column:due_state"`
	Count        int64  `gorm:"column:count"`
}

type sharedActualAllocationMetric struct {
	Provider                string `gorm:"column:provider"`
	CurrencyCode            string `gorm:"column:currency_code"`
	State                   string `gorm:"column:state"`
	RunCount                int64  `gorm:"column:run_count"`
	SourceLineCount         int64  `gorm:"column:source_line_count"`
	UnallocatedLineCount    int64  `gorm:"column:unallocated_line_count"`
	SourceAmountMicros      int64  `gorm:"column:source_amount_micros"`
	UnallocatedAmountMicros int64  `gorm:"column:unallocated_amount_micros"`
}

type capacityAdmissionMetric struct {
	AdmissionMode string `gorm:"column:admission_mode"`
	Count         int64  `gorm:"column:count"`
}

type reservationAuthorityMetric struct {
	Freshness         string `gorm:"column:freshness"`
	TargetCount       int64  `gorm:"column:target_count"`
	AcknowledgedUnits int64  `gorm:"column:acknowledged_units"`
	AllocatedUnits    int64  `gorm:"column:allocated_units"`
	Unacknowledged    int64  `gorm:"column:unacknowledged_units"`
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
	if err := r.writePlatformRoutingPublicationMetrics(ctx, output); err != nil {
		return err
	}
	if err := r.writeCapacityReservationMetrics(ctx, output, now); err != nil {
		return err
	}
	if err := r.writeWorkerPoolWarmCapacityMetrics(ctx, output, now); err != nil {
		return err
	}
	if err := r.writeBillingSharedAllocationScheduleMetrics(ctx, output, now); err != nil {
		return err
	}
	if err := r.writeBillingSharedActualAllocationMetrics(ctx, output); err != nil {
		return err
	}

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

func (r *Registry) writeCapacityReservationMetrics(
	ctx context.Context,
	output *bytes.Buffer,
	now time.Time,
) error {
	if !r.db.Migrator().HasTable("execution_capacity_admissions") ||
		!r.db.Migrator().HasTable("execution_target_reservation_acknowledgements") {
		return nil
	}
	var admissions []capacityAdmissionMetric
	if err := r.db.WithContext(ctx).Table("execution_capacity_admissions").
		Select("admission_mode, COUNT(*) AS count").
		Group("admission_mode").Order("admission_mode").Scan(&admissions).Error; err != nil {
		return fmt.Errorf("collect Execution capacity admission metrics: %w", err)
	}
	writeHelp(
		output,
		"synara_execution_capacity_admission_evidence",
		"Retained immutable Execution capacity admission snapshots by bounded authority mode.",
		"gauge",
	)
	for _, row := range admissions {
		fmt.Fprintf(
			output,
			"synara_execution_capacity_admission_evidence%s %d\n",
			labels(map[string]string{"mode": boundedCapacityAdmissionMode(row.AdmissionMode)}),
			row.Count,
		)
	}

	var authorities []reservationAuthorityMetric
	if err := r.db.WithContext(ctx).Table("execution_target_health AS health").
		Select(`CASE WHEN health.expires_at > ? THEN 'fresh' ELSE 'expired' END AS freshness,
			COUNT(*) AS target_count,
			COALESCE(SUM(health.reservation_acknowledged_units), 0) AS acknowledged_units,
			COALESCE(SUM(health.allocated_capacity_units), 0) AS allocated_units`, now).
		Where("health.reservation_authority_mode = ?", "exact-active-v1").
		Group("freshness").Order("freshness").Scan(&authorities).Error; err != nil {
		return fmt.Errorf("collect Target reservation authority metrics: %w", err)
	}
	type unacknowledgedMetric struct {
		Freshness string `gorm:"column:freshness"`
		Count     int64  `gorm:"column:count"`
	}
	var unacknowledged []unacknowledgedMetric
	if err := r.db.WithContext(ctx).Table("agent_executions AS execution").
		Select(`CASE WHEN health.expires_at > ? THEN 'fresh' ELSE 'expired' END AS freshness,
			COUNT(*) AS count`, now).
		Joins(`JOIN execution_target_health AS health
			ON health.execution_target_id = execution.execution_target_id
			AND health.reservation_authority_mode = 'exact-active-v1'`).
		Joins(`LEFT JOIN execution_target_reservation_acknowledgements AS acknowledgement
			ON acknowledgement.execution_target_id = execution.execution_target_id
			AND acknowledgement.execution_id = execution.id
			AND acknowledgement.execution_generation = execution.generation
			AND acknowledgement.health_version = health.version`).
		Where("execution.status IN ? AND acknowledgement.execution_id IS NULL", []string{"queued", "recovering"}).
		Group("freshness").Order("freshness").Scan(&unacknowledged).Error; err != nil {
		return fmt.Errorf("collect unacknowledged Target reservation metrics: %w", err)
	}
	unacknowledgedByFreshness := make(map[string]int64, len(unacknowledged))
	for _, row := range unacknowledged {
		unacknowledgedByFreshness[row.Freshness] = row.Count
	}
	for _, metric := range []struct {
		name string
		help string
	}{
		{"synara_execution_target_reservation_authorities", "Exact Target reservation authorities by bounded freshness state."},
		{"synara_execution_target_reservation_acknowledged_units", "Active reservation units proven to be reflected in published Target occupancy."},
		{"synara_execution_target_reservation_unacknowledged_units", "Active queued or recovering reservation units not reflected in the current Target occupancy authority."},
		{"synara_execution_target_strict_capacity_used_units", "Allocated Target occupancy plus current unacknowledged reservation units."},
	} {
		writeHelp(output, metric.name, metric.help, "gauge")
	}
	for _, row := range authorities {
		freshness := boundedFreshness(row.Freshness)
		metricLabels := labels(map[string]string{"freshness": freshness})
		unacknowledgedUnits := unacknowledgedByFreshness[row.Freshness]
		fmt.Fprintf(output, "synara_execution_target_reservation_authorities%s %d\n", metricLabels, row.TargetCount)
		fmt.Fprintf(output, "synara_execution_target_reservation_acknowledged_units%s %d\n", metricLabels, row.AcknowledgedUnits)
		fmt.Fprintf(output, "synara_execution_target_reservation_unacknowledged_units%s %d\n", metricLabels, unacknowledgedUnits)
		fmt.Fprintf(output, "synara_execution_target_strict_capacity_used_units%s %d\n", metricLabels, row.AllocatedUnits+unacknowledgedUnits)
	}
	return nil
}

func boundedCapacityAdmissionMode(value string) string {
	switch value {
	case "fixed-unbounded-v1", "publisher-health-v1", "exact-active-v1":
		return value
	default:
		return "other"
	}
}

func boundedFreshness(value string) string {
	if value == "fresh" {
		return "fresh"
	}
	return "expired"
}

func (r *Registry) writePlatformRoutingPublicationMetrics(ctx context.Context, output *bytes.Buffer) error {
	if !r.db.Migrator().HasTable("platform_routing_publications") {
		return nil
	}
	var publicationCount int64
	if err := r.db.WithContext(ctx).Model(&persistence.PlatformRoutingPublication{}).
		Count(&publicationCount).Error; err != nil {
		return fmt.Errorf("collect Platform routing publication metrics: %w", err)
	}
	writeHelp(
		output,
		"synara_platform_routing_publications_total",
		"Immutable authenticated Platform routing-authority publication receipts.",
		"counter",
	)
	fmt.Fprintf(output, "synara_platform_routing_publications_total %d\n", publicationCount)
	writeHelp(
		output,
		"synara_platform_routing_publication_latest_timestamp_seconds",
		"Receive timestamp of the latest authenticated Platform routing-authority publication.",
		"gauge",
	)
	latestTimestamp := float64(0)
	if publicationCount > 0 {
		var latest persistence.PlatformRoutingPublication
		if err := r.db.WithContext(ctx).Select("received_at").
			Order("received_at DESC, publisher_identity, nonce").Take(&latest).Error; err != nil {
			return fmt.Errorf("collect latest Platform routing publication timestamp: %w", err)
		}
		latestTimestamp = float64(latest.ReceivedAt.UnixNano()) / float64(time.Second)
	}
	fmt.Fprintf(output, "synara_platform_routing_publication_latest_timestamp_seconds %s\n", strconv.FormatFloat(latestTimestamp, 'f', 6, 64))
	return nil
}

func (r *Registry) writeBillingSharedActualAllocationMetrics(
	ctx context.Context,
	output *bytes.Buffer,
) error {
	if !r.db.Migrator().HasTable("billing_shared_actual_allocation_runs") {
		return nil
	}
	var rows []sharedActualAllocationMetric
	if err := r.db.WithContext(ctx).Table("billing_shared_actual_allocation_runs").
		Select(`provider, currency_code, state,
			COUNT(*) AS run_count,
			SUM(source_line_count) AS source_line_count,
			SUM(unallocated_line_count) AS unallocated_line_count,
			SUM(source_amount_micros) AS source_amount_micros,
			SUM(unallocated_amount_micros) AS unallocated_amount_micros`).
		Group("provider, currency_code, state").
		Order("provider, currency_code, state").
		Scan(&rows).Error; err != nil {
		return fmt.Errorf("collect billing shared actual allocation metrics: %w", err)
	}
	writeHelp(
		output,
		"synara_billing_shared_actual_allocation_runs",
		"Immutable account-invoice allocation runs by bounded provider, currency, and sealing state.",
		"gauge",
	)
	writeHelp(
		output,
		"synara_billing_shared_actual_allocation_lines",
		"Exact account-invoice line inventory by selected or still-unallocated scope.",
		"gauge",
	)
	writeHelp(
		output,
		"synara_billing_shared_actual_allocation_amount_micros",
		"Signed account-invoice micros by selected or still-unallocated scope; selected amounts conserve into actual slices.",
		"gauge",
	)
	for _, row := range rows {
		baseLabels := map[string]string{
			"provider": boundedBillingProvider(row.Provider),
			"currency": row.CurrencyCode,
			"state":    boundedSharedActualAllocationState(row.State),
		}
		fmt.Fprintf(
			output,
			"synara_billing_shared_actual_allocation_runs%s %d\n",
			labels(baseLabels),
			row.RunCount,
		)
		for _, sample := range []struct {
			kind   string
			lines  int64
			amount int64
		}{
			{kind: "selected", lines: row.SourceLineCount, amount: row.SourceAmountMicros},
			{kind: "unallocated", lines: row.UnallocatedLineCount, amount: row.UnallocatedAmountMicros},
		} {
			metricLabels := map[string]string{
				"provider": baseLabels["provider"],
				"currency": baseLabels["currency"],
				"state":    baseLabels["state"],
				"kind":     sample.kind,
			}
			fmt.Fprintf(
				output,
				"synara_billing_shared_actual_allocation_lines%s %d\n",
				labels(metricLabels),
				sample.lines,
			)
			fmt.Fprintf(
				output,
				"synara_billing_shared_actual_allocation_amount_micros%s %d\n",
				labels(metricLabels),
				sample.amount,
			)
		}
	}
	return nil
}

func boundedSharedActualAllocationState(value string) string {
	switch strings.TrimSpace(value) {
	case "building", "sealed":
		return strings.TrimSpace(value)
	default:
		return "other"
	}
}

func boundedBillingProvider(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return "other"
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '_' && character != '-' {
			return "other"
		}
	}
	return value
}

func (r *Registry) writeBillingSharedAllocationScheduleMetrics(
	ctx context.Context,
	output *bytes.Buffer,
	now time.Time,
) error {
	if !r.db.Migrator().HasTable("billing_shared_allocation_schedule_periods") {
		return nil
	}
	var rows []sharedAllocationSchedulePeriodMetric
	if err := r.db.WithContext(ctx).Table("billing_shared_allocation_schedule_periods").
		Select(`schedule_kind, last_outcome,
			CASE WHEN next_attempt_at <= CURRENT_TIMESTAMP THEN 'due' ELSE 'waiting' END AS due_state,
			COUNT(*) AS count`).
		Group(`schedule_kind, last_outcome,
			CASE WHEN next_attempt_at <= CURRENT_TIMESTAMP THEN 'due' ELSE 'waiting' END`).
		Order("schedule_kind, last_outcome, due_state").
		Scan(&rows).Error; err != nil {
		return fmt.Errorf("collect billing shared allocation schedule period metrics: %w", err)
	}
	writeHelp(
		output,
		"synara_billing_shared_allocation_schedule_periods",
		"Durable shared allocation schedule period inventory by bounded schedule kind, last outcome, and due state.",
		"gauge",
	)
	for _, row := range rows {
		fmt.Fprintf(
			output,
			"synara_billing_shared_allocation_schedule_periods%s %d\n",
			labels(map[string]string{
				"schedule_kind": boundedSharedAllocationScheduleKind(row.ScheduleKind),
				"outcome":       boundedSharedAllocationScheduleOutcome(row.LastOutcome),
				"due_state":     boundedSharedAllocationScheduleDueState(row.DueState),
			}),
			row.Count,
		)
	}
	return nil
}

func boundedSharedAllocationScheduleKind(value string) string {
	switch strings.TrimSpace(value) {
	case "static", "monthly-utc":
		return strings.TrimSpace(value)
	default:
		return "other"
	}
}

func boundedSharedAllocationScheduleOutcome(value string) string {
	switch strings.TrimSpace(value) {
	case "never", "running", "completed", "failed":
		return strings.TrimSpace(value)
	default:
		return "other"
	}
}

func boundedSharedAllocationScheduleDueState(value string) string {
	switch strings.TrimSpace(value) {
	case "due", "waiting":
		return strings.TrimSpace(value)
	default:
		return "other"
	}
}

func (r *Registry) writeWorkerPoolWarmCapacityMetrics(
	ctx context.Context,
	output *bytes.Buffer,
	now time.Time,
) error {
	authorities := make(map[warmCapacityAuthorityMetricKey]int64)
	for _, freshness := range []string{"fresh", "expired"} {
		var rows []warmCapacityAuthorityGroup
		query := r.db.WithContext(ctx).Table("worker_pool_warm_capacity").
			Select("capacity_class, warm_supported, COUNT(*) AS count").
			Group("capacity_class, warm_supported")
		if freshness == "fresh" {
			query = query.Where("expires_at > ?", now)
		} else {
			query = query.Where("expires_at <= ?", now)
		}
		if err := query.Scan(&rows).Error; err != nil {
			return fmt.Errorf("collect Worker Pool warm capacity %s authority metrics: %w", freshness, err)
		}
		for _, row := range rows {
			key := warmCapacityAuthorityMetricKey{
				CapacityClass: boundedWarmCapacityClass(row.CapacityClass),
				Freshness:     freshness,
				WarmSupported: row.WarmSupported,
			}
			authorities[key] += row.Count
		}
	}

	authorityKeys := make([]warmCapacityAuthorityMetricKey, 0, len(authorities))
	for key := range authorities {
		authorityKeys = append(authorityKeys, key)
	}
	sort.Slice(authorityKeys, func(left, right int) bool {
		leftKey := authorityKeys[left]
		rightKey := authorityKeys[right]
		if leftKey.CapacityClass != rightKey.CapacityClass {
			return leftKey.CapacityClass < rightKey.CapacityClass
		}
		if leftKey.Freshness != rightKey.Freshness {
			return leftKey.Freshness < rightKey.Freshness
		}
		return !leftKey.WarmSupported && rightKey.WarmSupported
	})
	writeHelp(
		output,
		"synara_worker_pool_warm_capacity_authorities",
		"Authoritative Worker Pool warm-capacity observations by bounded capacity class, freshness, and warm-support state.",
		"gauge",
	)
	for _, key := range authorityKeys {
		fmt.Fprintf(
			output,
			"synara_worker_pool_warm_capacity_authorities%s %d\n",
			labels(map[string]string{
				"capacity_class": key.CapacityClass,
				"freshness":      key.Freshness,
				"warm_supported": strconv.FormatBool(key.WarmSupported),
			}),
			authorities[key],
		)
	}

	var unitRows []warmCapacityUnitGroup
	if err := r.db.WithContext(ctx).Table("worker_pool_warm_capacity").
		Select(`capacity_class,
			SUM(desired_idle_units) AS desired_idle_units,
			SUM(min_idle_units) AS min_idle_units,
			SUM(desired_total_units) AS desired_total_units,
			SUM(claimed_units) AS claimed_units,
			SUM(ready_idle_units) AS ready_idle_units,
			SUM(CASE
				WHEN min_idle_units > ready_idle_units THEN min_idle_units - ready_idle_units
				ELSE 0
			END) AS deficit_units`).
		Where("expires_at > ?", now).
		Group("capacity_class").
		Scan(&unitRows).Error; err != nil {
		return fmt.Errorf("collect fresh Worker Pool warm capacity unit metrics: %w", err)
	}
	type unitTotals struct {
		desiredIdle  int64
		minIdle      int64
		desiredTotal int64
		claimed      int64
		readyIdle    int64
		deficit      int64
	}
	units := make(map[string]unitTotals)
	for _, row := range unitRows {
		capacityClass := boundedWarmCapacityClass(row.CapacityClass)
		totals := units[capacityClass]
		totals.desiredIdle += row.DesiredIdleUnits
		totals.minIdle += row.MinIdleUnits
		totals.desiredTotal += row.DesiredTotalUnits
		totals.claimed += row.ClaimedUnits
		totals.readyIdle += row.ReadyIdleUnits
		totals.deficit += row.DeficitUnits
		units[capacityClass] = totals
	}
	capacityClasses := make([]string, 0, len(units))
	for capacityClass := range units {
		capacityClasses = append(capacityClasses, capacityClass)
	}
	sort.Strings(capacityClasses)
	writeHelp(
		output,
		"synara_worker_pool_warm_capacity_units",
		"Fresh authoritative Worker Pool warm-capacity units by bounded capacity class and counter kind.",
		"gauge",
	)
	for _, capacityClass := range capacityClasses {
		totals := units[capacityClass]
		for _, sample := range []struct {
			kind  string
			value int64
		}{
			{kind: "desired_idle", value: totals.desiredIdle},
			{kind: "min_idle", value: totals.minIdle},
			{kind: "desired_total", value: totals.desiredTotal},
			{kind: "claimed", value: totals.claimed},
			{kind: "ready_idle", value: totals.readyIdle},
		} {
			fmt.Fprintf(
				output,
				"synara_worker_pool_warm_capacity_units%s %d\n",
				labels(map[string]string{"capacity_class": capacityClass, "kind": sample.kind}),
				sample.value,
			)
		}
	}
	writeHelp(
		output,
		"synara_worker_pool_warm_deficit",
		"Fresh authoritative guaranteed warm-floor deficit by bounded capacity class.",
		"gauge",
	)
	for _, capacityClass := range capacityClasses {
		fmt.Fprintf(
			output,
			"synara_worker_pool_warm_deficit%s %d\n",
			labels(map[string]string{"capacity_class": capacityClass}),
			units[capacityClass].deficit,
		)
	}
	return nil
}

func boundedWarmCapacityClass(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "standard":
		return "standard"
	case "interactive":
		return "interactive"
	default:
		return "other"
	}
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
	case "synara:metric-rollup":
		return "metric-rollup"
	case "synara:billing-import-scheduler":
		return "billing-import"
	case "synara:billing-shared-allocation-scheduler":
		return "billing-shared-allocation"
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
