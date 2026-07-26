package observability

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/metricfacts"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const executionGenerationMetricWindow = 30 * 24 * time.Hour

type durableGenerationMetricSample struct {
	RecoveryReason    string     `gorm:"column:recovery_reason"`
	TargetKind        string     `gorm:"column:target_kind"`
	WarmPoolMode      string     `gorm:"column:warm_pool_mode"`
	WarmPoolResult    string     `gorm:"column:warm_pool_result"`
	DispatchRequested *time.Time `gorm:"column:dispatch_requested_at"`
	ProviderReadyAt   *time.Time `gorm:"column:provider_ready_at"`
	PodProvisioningAt *time.Time `gorm:"column:pod_provisioning_started_at"`
	PodRunningAt      *time.Time `gorm:"column:pod_running_at"`
	TerminalOutcome   *string    `gorm:"column:terminal_outcome"`
}

func (sample durableGenerationMetricSample) metricSample() metricfacts.ExecutionGenerationSample {
	return metricfacts.ExecutionGenerationSample{
		RecoveryReason: sample.RecoveryReason, TargetKind: sample.TargetKind,
		WarmPoolMode: sample.WarmPoolMode, WarmPoolResult: sample.WarmPoolResult,
		DispatchRequestedAt: sample.DispatchRequested, ProviderReadyAt: sample.ProviderReadyAt,
		PodProvisioningAt: sample.PodProvisioningAt, PodRunningAt: sample.PodRunningAt,
		TerminalOutcome: sample.TerminalOutcome,
	}
}

type durablePodFailureMetricSample struct {
	FailureClass string `gorm:"column:failure_class"`
	TargetKind   string `gorm:"column:target_kind"`
}

type rollingDurationDistribution struct {
	exactValues []float64
	buckets     map[int]int64
	sampleCount int64
}

func (distribution *rollingDurationDistribution) addExact(value float64) {
	distribution.exactValues = append(distribution.exactValues, value)
	distribution.sampleCount++
}

func (distribution *rollingDurationDistribution) addBucket(bucket int, count int64) error {
	if count <= 0 || !metricfacts.ValidDurationHistogramBucket(bucket) {
		return fmt.Errorf("invalid duration histogram bucket %d count %d", bucket, count)
	}
	if distribution.buckets == nil {
		distribution.buckets = make(map[int]int64)
	}
	distribution.buckets[bucket] += count
	distribution.sampleCount += count
	return nil
}

func (distribution *rollingDurationDistribution) quantile(value float64) float64 {
	if distribution.sampleCount == 0 {
		return 0
	}
	if len(distribution.buckets) == 0 {
		sort.Float64s(distribution.exactValues)
		return nearestRankQuantile(distribution.exactValues, value)
	}
	rank := int64(math.Ceil(value * float64(distribution.sampleCount)))
	if rank < 1 {
		rank = 1
	}
	buckets := make([]int, 0, len(distribution.buckets))
	for bucket := range distribution.buckets {
		buckets = append(buckets, bucket)
	}
	sort.Ints(buckets)
	var cumulative int64
	for _, bucket := range buckets {
		cumulative += distribution.buckets[bucket]
		if cumulative < rank {
			continue
		}
		upperMicros, ok := metricfacts.DurationHistogramUpperBoundMicros(bucket)
		if !ok {
			return 0
		}
		return float64(upperMicros) / float64(time.Second/time.Microsecond)
	}
	return 0
}

type generationMetricAccumulator struct {
	outcomes         map[string]int64
	warmResults      map[string]int64
	podFailures      map[string]int64
	coldStarts       map[string]*rollingDurationDistribution
	podQueues        map[string]*rollingDurationDistribution
	podProvisionings map[string]*rollingDurationDistribution
}

func newGenerationMetricAccumulator() *generationMetricAccumulator {
	return &generationMetricAccumulator{
		outcomes: make(map[string]int64), warmResults: make(map[string]int64),
		podFailures: make(map[string]int64), coldStarts: make(map[string]*rollingDurationDistribution),
		podQueues:        make(map[string]*rollingDurationDistribution),
		podProvisionings: make(map[string]*rollingDurationDistribution),
	}
}

func (accumulator *generationMetricAccumulator) addGenerationSample(
	sample durableGenerationMetricSample,
	approximateDurations bool,
) error {
	for _, contribution := range metricfacts.ExecutionGenerationMetricContributions(sample.metricSample()) {
		if contribution.MetricKind == metricfacts.GenerationMetricKindOutcome ||
			contribution.MetricKind == metricfacts.GenerationMetricKindWarmAcquisition {
			if err := accumulator.addContribution(contribution, 1); err != nil {
				return err
			}
			continue
		}
		if approximateDurations {
			if err := accumulator.addContribution(contribution, 1); err != nil {
				return err
			}
			continue
		}
		distribution, labelSet := accumulator.durationDistribution(contribution)
		seconds, ok := exactGenerationDurationSeconds(sample, contribution.MetricKind)
		if !ok {
			return fmt.Errorf("duration contribution %q has no exact source", contribution.MetricKind)
		}
		accumulator.distribution(distribution, labelSet).addExact(seconds)
	}
	return nil
}

func (accumulator *generationMetricAccumulator) addContribution(
	contribution metricfacts.GenerationMetricContribution,
	count int64,
) error {
	if count <= 0 {
		return fmt.Errorf("invalid Generation metric contribution count %d", count)
	}
	switch contribution.MetricKind {
	case metricfacts.GenerationMetricKindOutcome:
		accumulator.outcomes[labels(map[string]string{
			"outcome": contribution.Outcome, "recovery_reason": contribution.RecoveryReason,
			"target_kind": contribution.TargetKind,
		})] += count
	case metricfacts.GenerationMetricKindWarmAcquisition:
		accumulator.warmResults[labels(map[string]string{
			"requested_mode": contribution.WarmPoolMode, "result": contribution.WarmPoolResult,
			"target_kind": contribution.TargetKind,
		})] += count
	case metricfacts.GenerationMetricKindPodFailure:
		accumulator.podFailures[labels(map[string]string{
			"failure_class": contribution.FailureClass, "target_kind": contribution.TargetKind,
		})] += count
	case metricfacts.GenerationMetricKindColdStart,
		metricfacts.GenerationMetricKindPodQueue,
		metricfacts.GenerationMetricKindPodProvisioning:
		distributions, labelSet := accumulator.durationDistribution(contribution)
		if err := accumulator.distribution(distributions, labelSet).
			addBucket(contribution.HistogramBucket, count); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported Generation metric kind %q", contribution.MetricKind)
	}
	return nil
}

func (accumulator *generationMetricAccumulator) durationDistribution(
	contribution metricfacts.GenerationMetricContribution,
) (map[string]*rollingDurationDistribution, string) {
	labelValues := map[string]string{
		"recovery_reason": contribution.RecoveryReason,
		"target_kind":     contribution.TargetKind,
	}
	switch contribution.MetricKind {
	case metricfacts.GenerationMetricKindColdStart:
		labelValues["warm_pool_mode"] = contribution.WarmPoolMode
		labelValues["warm_pool_result"] = contribution.WarmPoolResult
		return accumulator.coldStarts, labels(labelValues)
	case metricfacts.GenerationMetricKindPodQueue:
		return accumulator.podQueues, labels(labelValues)
	default:
		return accumulator.podProvisionings, labels(labelValues)
	}
}

func (accumulator *generationMetricAccumulator) distribution(
	distributions map[string]*rollingDurationDistribution,
	labelSet string,
) *rollingDurationDistribution {
	if distributions[labelSet] == nil {
		distributions[labelSet] = &rollingDurationDistribution{}
	}
	return distributions[labelSet]
}

func exactGenerationDurationSeconds(sample durableGenerationMetricSample, metricKind string) (float64, bool) {
	var startAt, endAt *time.Time
	switch metricKind {
	case metricfacts.GenerationMetricKindColdStart:
		startAt, endAt = sample.DispatchRequested, sample.ProviderReadyAt
	case metricfacts.GenerationMetricKindPodQueue:
		startAt, endAt = sample.DispatchRequested, sample.PodProvisioningAt
	case metricfacts.GenerationMetricKindPodProvisioning:
		startAt, endAt = sample.PodProvisioningAt, sample.PodRunningAt
	default:
		return 0, false
	}
	if startAt == nil || endAt == nil || endAt.Before(*startAt) {
		return 0, false
	}
	return endAt.Sub(*startAt).Seconds(), true
}

func (r *Registry) writeExecutionGenerationFactMetrics(
	ctx context.Context,
	output *bytes.Buffer,
	now time.Time,
) error {
	if !r.db.Migrator().HasTable("execution_generation_facts") {
		return nil
	}
	var options []*sql.TxOptions
	if r.db.Dialector.Name() == "postgres" {
		options = append(options, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return writeExecutionGenerationFactMetricsSnapshot(ctx, tx, output, now)
	}, options...)
}

func writeExecutionGenerationFactMetricsSnapshot(
	ctx context.Context,
	db *gorm.DB,
	output *bytes.Buffer,
	now time.Time,
) error {
	accumulator := newGenerationMetricAccumulator()
	rollupsAvailable := db.Migrator().HasTable(&persistence.ExecutionGenerationMetricRollup{}) &&
		db.Migrator().HasTable(&persistence.ExecutionGenerationMetricRollupEntry{}) &&
		db.Migrator().HasTable(&persistence.ExecutionGenerationPodFailureMetricRollupEntry{})
	if rollupsAvailable {
		if err := collectGenerationMetricRollups(ctx, db, accumulator, now); err != nil {
			return err
		}
	} else if err := collectRawGenerationMetrics(ctx, db, accumulator, now, false); err != nil {
		return err
	}
	writeGenerationMetricAccumulator(output, accumulator)
	if rollupsAvailable {
		if err := writeGenerationMetricRollupInventory(ctx, db, output); err != nil {
			return err
		}
	}
	return nil
}

func collectGenerationMetricRollups(
	ctx context.Context,
	db *gorm.DB,
	accumulator *generationMetricAccumulator,
	now time.Time,
) error {
	windowStart := now.Add(-executionGenerationMetricWindow)
	fullBucketStart := nextUTCDayBoundary(windowStart)
	fullBucketEnd := utcDayBoundary(now)
	var rollups []persistence.ExecutionGenerationMetricRollup
	if fullBucketStart.Before(fullBucketEnd) {
		if err := db.WithContext(ctx).
			Where("bucket_day >= ? AND bucket_day < ?", fullBucketStart, fullBucketEnd).
			Order("bucket_day, metric_kind, target_kind").
			Find(&rollups).Error; err != nil {
			return fmt.Errorf("collect Execution generation metric rollups: %w", err)
		}
	}
	for _, rollup := range rollups {
		if err := accumulator.addContribution(metricfacts.GenerationMetricContribution{
			MetricKind: rollup.MetricKind, TargetKind: rollup.TargetKind,
			RecoveryReason: rollup.RecoveryReason, Outcome: rollup.Outcome,
			WarmPoolMode: rollup.WarmPoolMode, WarmPoolResult: rollup.WarmPoolResult,
			FailureClass: rollup.FailureClass, HistogramBucket: rollup.HistogramBucket,
		}, rollup.SampleCount); err != nil {
			return fmt.Errorf("merge Execution generation metric rollup: %w", err)
		}
	}
	if err := collectRawGenerationMetricsForRollupWindow(
		ctx, db, accumulator, windowStart, fullBucketStart, fullBucketEnd, now,
	); err != nil {
		return err
	}
	return collectRawPodFailureMetricsForRollupWindow(
		ctx, db, accumulator, windowStart, fullBucketStart, fullBucketEnd, now,
	)
}

func collectRawGenerationMetricsForRollupWindow(
	ctx context.Context,
	db *gorm.DB,
	accumulator *generationMetricAccumulator,
	windowStart, fullBucketStart, fullBucketEnd, now time.Time,
) error {
	var samples []durableGenerationMetricSample
	if err := db.WithContext(ctx).Table("execution_generation_facts AS fact").
		Select(`fact.recovery_reason, fact.target_kind, fact.warm_pool_mode, fact.warm_pool_result,
			fact.dispatch_requested_at, fact.provider_ready_at, fact.pod_provisioning_started_at,
			fact.pod_running_at, fact.terminal_outcome`).
		Joins(`LEFT JOIN execution_generation_metric_rollup_entries AS entry
			ON entry.tenant_id = fact.tenant_id
			AND entry.execution_id = fact.execution_id
			AND entry.generation = fact.generation`).
		Where("fact.dispatch_requested_at >= ? AND fact.dispatch_requested_at <= ?", windowStart, now).
		Where(`fact.dispatch_requested_at < ?
			OR fact.dispatch_requested_at >= ?
			OR entry.rolled_up_at IS NULL`, fullBucketStart, fullBucketEnd).
		Find(&samples).Error; err != nil {
		return fmt.Errorf("collect pending and boundary Execution generation metrics: %w", err)
	}
	for _, sample := range samples {
		if err := accumulator.addGenerationSample(sample, true); err != nil {
			return fmt.Errorf("aggregate pending Execution generation metric: %w", err)
		}
	}
	return nil
}

func collectRawPodFailureMetricsForRollupWindow(
	ctx context.Context,
	db *gorm.DB,
	accumulator *generationMetricAccumulator,
	windowStart, fullBucketStart, fullBucketEnd, now time.Time,
) error {
	if !db.Migrator().HasTable("execution_generation_pod_failure_facts") {
		return nil
	}
	var failures []durablePodFailureMetricSample
	if err := db.WithContext(ctx).
		Table("execution_generation_pod_failure_facts AS failure").
		Select("failure.failure_class, generation.target_kind").
		Joins(`JOIN execution_generation_facts AS generation
			ON generation.tenant_id = failure.tenant_id
			AND generation.execution_id = failure.execution_id
			AND generation.generation = failure.generation`).
		Joins(`LEFT JOIN execution_generation_pod_failure_metric_rollup_entries AS entry
			ON entry.tenant_id = failure.tenant_id
			AND entry.execution_id = failure.execution_id
			AND entry.generation = failure.generation
			AND entry.failure_class = failure.failure_class`).
		Where("failure.first_observed_at >= ? AND failure.first_observed_at <= ?", windowStart, now).
		Where(`failure.first_observed_at < ?
			OR failure.first_observed_at >= ?
			OR entry.rolled_up_at IS NULL`, fullBucketStart, fullBucketEnd).
		Find(&failures).Error; err != nil {
		return fmt.Errorf("collect pending and boundary Execution Pod failure metrics: %w", err)
	}
	for _, failure := range failures {
		if err := accumulator.addContribution(
			metricfacts.PodFailureMetricContribution(failure.TargetKind, failure.FailureClass), 1,
		); err != nil {
			return err
		}
	}
	return nil
}

func collectRawGenerationMetrics(
	ctx context.Context,
	db *gorm.DB,
	accumulator *generationMetricAccumulator,
	now time.Time,
	approximateDurations bool,
) error {
	var samples []durableGenerationMetricSample
	if err := db.WithContext(ctx).Table("execution_generation_facts").
		Select(`recovery_reason, target_kind, warm_pool_mode, warm_pool_result,
			dispatch_requested_at, provider_ready_at, pod_provisioning_started_at,
			pod_running_at, terminal_outcome`).
		Where("dispatch_requested_at >= ? AND dispatch_requested_at <= ?", now.Add(-executionGenerationMetricWindow), now).
		Find(&samples).Error; err != nil {
		return fmt.Errorf("collect durable Execution generation metrics: %w", err)
	}
	for _, sample := range samples {
		if err := accumulator.addGenerationSample(sample, approximateDurations); err != nil {
			return err
		}
	}
	if !db.Migrator().HasTable("execution_generation_pod_failure_facts") {
		return nil
	}
	var failures []durablePodFailureMetricSample
	if err := db.WithContext(ctx).
		Table("execution_generation_pod_failure_facts AS failure").
		Select("failure.failure_class, generation.target_kind").
		Joins(`JOIN execution_generation_facts AS generation
			ON generation.tenant_id = failure.tenant_id
			AND generation.execution_id = failure.execution_id
			AND generation.generation = failure.generation`).
		Where("failure.first_observed_at >= ? AND failure.first_observed_at <= ?", now.Add(-executionGenerationMetricWindow), now).
		Find(&failures).Error; err != nil {
		return fmt.Errorf("collect durable Execution Pod failure metrics: %w", err)
	}
	for _, failure := range failures {
		if err := accumulator.addContribution(
			metricfacts.PodFailureMetricContribution(failure.TargetKind, failure.FailureClass), 1,
		); err != nil {
			return err
		}
	}
	return nil
}

func writeGenerationMetricAccumulator(output *bytes.Buffer, accumulator *generationMetricAccumulator) {
	writeHelp(output, "synara_execution_generation_outcomes_30d", "Durable Execution generation outcomes in the trailing 30-day window.", "gauge")
	for _, labelSet := range sortedMetricLabelSets(accumulator.outcomes) {
		fmt.Fprintf(output, "synara_execution_generation_outcomes_30d%s %d\n", labelSet, accumulator.outcomes[labelSet])
	}
	writeHelp(output, "synara_warm_pool_acquisitions_30d", "Durable requested Warm Pool allocation results in the trailing 30-day window; requested Session inventory is reported separately.", "gauge")
	for _, labelSet := range sortedMetricLabelSets(accumulator.warmResults) {
		fmt.Fprintf(output, "synara_warm_pool_acquisitions_30d%s %d\n", labelSet, accumulator.warmResults[labelSet])
	}
	writeRollingDurationDistributions(
		output, "synara_execution_cold_start_duration_seconds_30d",
		"Durable trailing-30-day dispatch-request-to-Provider-ready latency quantiles; sealed full UTC days use a mergeable histogram with at most two-percent relative bucket width.",
		"synara_execution_cold_start_samples_30d", "Durable trailing-30-day cold-start sample count.",
		accumulator.coldStarts,
	)
	writeRollingDurationDistributions(
		output, "synara_execution_pod_queue_duration_seconds_30d",
		"Durable trailing-30-day dispatch-request-to-first-Pod-apply latency quantiles; sealed full UTC days use a mergeable histogram with at most two-percent relative bucket width.",
		"synara_execution_pod_queue_samples_30d", "Durable trailing-30-day Execution Pod queue sample count.",
		accumulator.podQueues,
	)
	writeRollingDurationDistributions(
		output, "synara_execution_pod_provisioning_duration_seconds_30d",
		"Durable trailing-30-day first-Pod-apply-to-first-Running latency quantiles; sealed full UTC days use a mergeable histogram with at most two-percent relative bucket width.",
		"synara_execution_pod_provisioning_samples_30d", "Durable trailing-30-day Execution Pod provisioning sample count.",
		accumulator.podProvisionings,
	)
	writeHelp(output, "synara_execution_pod_failure_generations_30d", "Durable Execution generations with each Pod failure class in the trailing 30-day window.", "gauge")
	for _, labelSet := range sortedMetricLabelSets(accumulator.podFailures) {
		fmt.Fprintf(output, "synara_execution_pod_failure_generations_30d%s %d\n", labelSet, accumulator.podFailures[labelSet])
	}
}

func writeRollingDurationDistributions(
	output *bytes.Buffer,
	metricName, metricHelp, sampleMetricName, sampleMetricHelp string,
	distributions map[string]*rollingDurationDistribution,
) {
	writeHelp(output, metricName, metricHelp, "gauge")
	writeHelp(output, sampleMetricName, sampleMetricHelp, "gauge")
	labelSets := make([]string, 0, len(distributions))
	for labelSet := range distributions {
		labelSets = append(labelSets, labelSet)
	}
	sort.Strings(labelSets)
	for _, labelSet := range labelSets {
		distribution := distributions[labelSet]
		for _, quantile := range []struct {
			label string
			value float64
		}{{"0.5", 0.5}, {"0.95", 0.95}, {"0.99", 0.99}} {
			fmt.Fprintf(
				output, "%s%s %s\n", metricName,
				addLabel(labelSet, "quantile", quantile.label),
				formatFloat(distribution.quantile(quantile.value)),
			)
		}
		fmt.Fprintf(output, "%s%s %d\n", sampleMetricName, labelSet, distribution.sampleCount)
	}
}

func writeGenerationMetricRollupInventory(ctx context.Context, db *gorm.DB, output *bytes.Buffer) error {
	var pendingGenerations, pendingFailures, generationBuckets, failureBuckets int64
	if err := db.WithContext(ctx).Model(&persistence.ExecutionGenerationMetricRollupEntry{}).
		Where("rolled_up_at IS NULL").Count(&pendingGenerations).Error; err != nil {
		return fmt.Errorf("count pending Execution generation metric rollups: %w", err)
	}
	if err := db.WithContext(ctx).Model(&persistence.ExecutionGenerationPodFailureMetricRollupEntry{}).
		Where("rolled_up_at IS NULL").Count(&pendingFailures).Error; err != nil {
		return fmt.Errorf("count pending Execution Pod failure metric rollups: %w", err)
	}
	if err := db.WithContext(ctx).Model(&persistence.ExecutionGenerationMetricRollup{}).
		Where("metric_kind <> ?", metricfacts.GenerationMetricKindPodFailure).
		Count(&generationBuckets).Error; err != nil {
		return fmt.Errorf("count Execution generation metric rollup buckets: %w", err)
	}
	if err := db.WithContext(ctx).Model(&persistence.ExecutionGenerationMetricRollup{}).
		Where("metric_kind = ?", metricfacts.GenerationMetricKindPodFailure).
		Count(&failureBuckets).Error; err != nil {
		return fmt.Errorf("count Execution Pod failure metric rollup buckets: %w", err)
	}
	writeHelp(output, "synara_execution_generation_metric_rollup_pending_facts", "Durable Execution Generation facts still awaiting metric rollup membership completion.", "gauge")
	fmt.Fprintf(output, "synara_execution_generation_metric_rollup_pending_facts%s %d\n", labels(map[string]string{"kind": "generation"}), pendingGenerations)
	fmt.Fprintf(output, "synara_execution_generation_metric_rollup_pending_facts%s %d\n", labels(map[string]string{"kind": "pod-failure"}), pendingFailures)
	writeHelp(output, "synara_execution_generation_metric_rollup_buckets", "Durable Execution Generation metric rollup row inventory.", "gauge")
	fmt.Fprintf(output, "synara_execution_generation_metric_rollup_buckets%s %d\n", labels(map[string]string{"kind": "generation"}), generationBuckets)
	fmt.Fprintf(output, "synara_execution_generation_metric_rollup_buckets%s %d\n", labels(map[string]string{"kind": "pod-failure"}), failureBuckets)
	return nil
}

func utcDayBoundary(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func nextUTCDayBoundary(value time.Time) time.Time {
	day := utcDayBoundary(value)
	if value.UTC().Equal(day) {
		return day
	}
	return day.AddDate(0, 0, 1)
}

func nearestRankQuantile(sortedValues []float64, quantile float64) float64 {
	if len(sortedValues) == 0 {
		return 0
	}
	index := int(math.Ceil(quantile*float64(len(sortedValues)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sortedValues) {
		index = len(sortedValues) - 1
	}
	return sortedValues[index]
}

func sortedMetricLabelSets(values map[string]int64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
