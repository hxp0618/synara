package observability

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"
	"time"
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

type durablePodFailureMetricSample struct {
	FailureClass string `gorm:"column:failure_class"`
	TargetKind   string `gorm:"column:target_kind"`
}

func (r *Registry) writeExecutionGenerationFactMetrics(
	ctx context.Context,
	output *bytes.Buffer,
	now time.Time,
) error {
	if !r.db.Migrator().HasTable("execution_generation_facts") {
		return nil
	}
	samples := make([]durableGenerationMetricSample, 0)
	if err := r.db.WithContext(ctx).Table("execution_generation_facts").
		Select(`recovery_reason, target_kind, warm_pool_mode, warm_pool_result,
			dispatch_requested_at, provider_ready_at, pod_provisioning_started_at,
			pod_running_at, terminal_outcome`).
		Where("dispatch_requested_at >= ?", now.Add(-executionGenerationMetricWindow)).
		Find(&samples).Error; err != nil {
		return fmt.Errorf("collect durable Execution generation metrics: %w", err)
	}

	coldStartDurations := make(map[string][]float64)
	podQueueDurations := make(map[string][]float64)
	podProvisioningDurations := make(map[string][]float64)
	outcomes := make(map[string]int64)
	warmResults := make(map[string]int64)
	for _, sample := range samples {
		outcome := "pending"
		if sample.TerminalOutcome != nil {
			outcome = *sample.TerminalOutcome
		}
		outcomes[labels(map[string]string{
			"outcome": outcome, "recovery_reason": sample.RecoveryReason, "target_kind": sample.TargetKind,
		})]++
		if sample.WarmPoolMode != "disabled" {
			warmResults[labels(map[string]string{
				"requested_mode": sample.WarmPoolMode, "result": sample.WarmPoolResult,
				"target_kind": sample.TargetKind,
			})]++
		}
		podLabelSet := labels(map[string]string{
			"recovery_reason": sample.RecoveryReason,
			"target_kind":     sample.TargetKind,
		})
		if sample.DispatchRequested != nil && sample.PodProvisioningAt != nil &&
			!sample.PodProvisioningAt.Before(*sample.DispatchRequested) {
			podQueueDurations[podLabelSet] = append(
				podQueueDurations[podLabelSet],
				sample.PodProvisioningAt.Sub(*sample.DispatchRequested).Seconds(),
			)
		}
		if sample.PodProvisioningAt != nil && sample.PodRunningAt != nil &&
			!sample.PodRunningAt.Before(*sample.PodProvisioningAt) {
			podProvisioningDurations[podLabelSet] = append(
				podProvisioningDurations[podLabelSet],
				sample.PodRunningAt.Sub(*sample.PodProvisioningAt).Seconds(),
			)
		}
		if sample.DispatchRequested == nil || sample.ProviderReadyAt == nil ||
			sample.ProviderReadyAt.Before(*sample.DispatchRequested) {
			continue
		}
		coldStartLabelSet := labels(map[string]string{
			"recovery_reason":  sample.RecoveryReason,
			"target_kind":      sample.TargetKind,
			"warm_pool_mode":   sample.WarmPoolMode,
			"warm_pool_result": sample.WarmPoolResult,
		})
		coldStartDurations[coldStartLabelSet] = append(
			coldStartDurations[coldStartLabelSet],
			sample.ProviderReadyAt.Sub(*sample.DispatchRequested).Seconds(),
		)
	}

	writeHelp(output, "synara_execution_generation_outcomes_30d", "Durable Execution generation outcomes in the trailing 30-day window.", "gauge")
	for _, labelSet := range sortedMetricLabelSets(outcomes) {
		fmt.Fprintf(output, "synara_execution_generation_outcomes_30d%s %d\n", labelSet, outcomes[labelSet])
	}
	writeHelp(output, "synara_warm_pool_acquisitions_30d", "Durable requested Warm Pool allocation results in the trailing 30-day window; requested Session inventory is reported separately.", "gauge")
	for _, labelSet := range sortedMetricLabelSets(warmResults) {
		fmt.Fprintf(output, "synara_warm_pool_acquisitions_30d%s %d\n", labelSet, warmResults[labelSet])
	}
	writeRollingDurationQuantiles(
		output,
		"synara_execution_cold_start_duration_seconds_30d",
		"Durable trailing-30-day dispatch-request-to-Provider-ready latency quantiles, including pre-claim Pod provisioning.",
		"synara_execution_cold_start_samples_30d",
		"Durable trailing-30-day cold-start sample count.",
		coldStartDurations,
	)
	writeRollingDurationQuantiles(
		output,
		"synara_execution_pod_queue_duration_seconds_30d",
		"Durable trailing-30-day dispatch-request-to-first-Pod-apply latency quantiles.",
		"synara_execution_pod_queue_samples_30d",
		"Durable trailing-30-day Execution Pod queue sample count.",
		podQueueDurations,
	)
	writeRollingDurationQuantiles(
		output,
		"synara_execution_pod_provisioning_duration_seconds_30d",
		"Durable trailing-30-day first-Pod-apply-to-first-Running latency quantiles.",
		"synara_execution_pod_provisioning_samples_30d",
		"Durable trailing-30-day Execution Pod provisioning sample count.",
		podProvisioningDurations,
	)

	if r.db.Migrator().HasTable("execution_generation_pod_failure_facts") {
		failures := make([]durablePodFailureMetricSample, 0)
		if err := r.db.WithContext(ctx).
			Table("execution_generation_pod_failure_facts AS failure").
			Select("failure.failure_class, generation.target_kind").
			Joins(`JOIN execution_generation_facts AS generation
				ON generation.tenant_id = failure.tenant_id
				AND generation.execution_id = failure.execution_id
				AND generation.generation = failure.generation`).
			Where("failure.first_observed_at >= ?", now.Add(-executionGenerationMetricWindow)).
			Find(&failures).Error; err != nil {
			return fmt.Errorf("collect durable Execution Pod failure metrics: %w", err)
		}
		counts := make(map[string]int64)
		for _, failure := range failures {
			counts[labels(map[string]string{
				"failure_class": failure.FailureClass,
				"target_kind":   failure.TargetKind,
			})]++
		}
		writeHelp(output, "synara_execution_pod_failure_generations_30d", "Durable Execution generations with each Pod failure class in the trailing 30-day window.", "gauge")
		for _, labelSet := range sortedMetricLabelSets(counts) {
			fmt.Fprintf(output, "synara_execution_pod_failure_generations_30d%s %d\n", labelSet, counts[labelSet])
		}
	}
	return nil
}

func writeRollingDurationQuantiles(
	output *bytes.Buffer,
	metricName, metricHelp, sampleMetricName, sampleMetricHelp string,
	durations map[string][]float64,
) {
	// Rolling-window values are gauges, not classic Prometheus histogram
	// counters, because samples leave the window over time.
	writeHelp(output, metricName, metricHelp, "gauge")
	writeHelp(output, sampleMetricName, sampleMetricHelp, "gauge")
	labelSets := make([]string, 0, len(durations))
	for labelSet := range durations {
		labelSets = append(labelSets, labelSet)
	}
	sort.Strings(labelSets)
	for _, labelSet := range labelSets {
		values := durations[labelSet]
		sort.Float64s(values)
		for _, quantile := range []struct {
			label string
			value float64
		}{
			{label: "0.5", value: 0.5},
			{label: "0.95", value: 0.95},
			{label: "0.99", value: 0.99},
		} {
			fmt.Fprintf(
				output,
				"%s%s %s\n",
				metricName,
				addLabel(labelSet, "quantile", quantile.label),
				formatFloat(nearestRankQuantile(values, quantile.value)),
			)
		}
		fmt.Fprintf(output, "%s%s %d\n", sampleMetricName, labelSet, len(values))
	}
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
