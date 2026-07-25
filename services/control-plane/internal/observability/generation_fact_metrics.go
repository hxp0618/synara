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
	TerminalOutcome   *string    `gorm:"column:terminal_outcome"`
}

type durableGenerationMetricKey struct {
	recoveryReason string
	targetKind     string
	warmPoolMode   string
	warmPoolResult string
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
			dispatch_requested_at, provider_ready_at, terminal_outcome`).
		Where("dispatch_requested_at >= ?", now.Add(-executionGenerationMetricWindow)).
		Find(&samples).Error; err != nil {
		return fmt.Errorf("collect durable Execution generation metrics: %w", err)
	}

	durations := make(map[durableGenerationMetricKey][]float64)
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
		if sample.DispatchRequested == nil || sample.ProviderReadyAt == nil || sample.ProviderReadyAt.Before(*sample.DispatchRequested) {
			continue
		}
		key := durableGenerationMetricKey{
			recoveryReason: sample.RecoveryReason,
			targetKind:     sample.TargetKind,
			warmPoolMode:   sample.WarmPoolMode,
			warmPoolResult: sample.WarmPoolResult,
		}
		seconds := sample.ProviderReadyAt.Sub(*sample.DispatchRequested).Seconds()
		durations[key] = append(durations[key], seconds)
	}

	writeHelp(output, "synara_execution_generation_outcomes_30d", "Durable Execution generation outcomes in the trailing 30-day window.", "gauge")
	for _, labelSet := range sortedMetricLabelSets(outcomes) {
		fmt.Fprintf(output, "synara_execution_generation_outcomes_30d%s %d\n", labelSet, outcomes[labelSet])
	}
	writeHelp(output, "synara_warm_pool_acquisitions_30d", "Durable requested Warm Pool allocation results in the trailing 30-day window; requested Session inventory is reported separately.", "gauge")
	for _, labelSet := range sortedMetricLabelSets(warmResults) {
		fmt.Fprintf(output, "synara_warm_pool_acquisitions_30d%s %d\n", labelSet, warmResults[labelSet])
	}
	// These are rolling-window values and therefore gauges, not a classic
	// Prometheus histogram whose bucket counters must never decrease.
	writeHelp(output, "synara_execution_cold_start_duration_seconds_30d", "Durable trailing-30-day dispatch-request-to-Provider-ready latency quantiles, including pre-claim Pod provisioning.", "gauge")
	writeHelp(output, "synara_execution_cold_start_samples_30d", "Durable trailing-30-day cold-start sample count.", "gauge")
	keys := make([]durableGenerationMetricKey, 0, len(durations))
	for key := range durations {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].recoveryReason != keys[j].recoveryReason {
			return keys[i].recoveryReason < keys[j].recoveryReason
		}
		if keys[i].targetKind != keys[j].targetKind {
			return keys[i].targetKind < keys[j].targetKind
		}
		if keys[i].warmPoolMode != keys[j].warmPoolMode {
			return keys[i].warmPoolMode < keys[j].warmPoolMode
		}
		return keys[i].warmPoolResult < keys[j].warmPoolResult
	})
	for _, key := range keys {
		values := durations[key]
		sort.Float64s(values)
		labelSet := labels(map[string]string{
			"recovery_reason":  key.recoveryReason,
			"target_kind":      key.targetKind,
			"warm_pool_mode":   key.warmPoolMode,
			"warm_pool_result": key.warmPoolResult,
		})
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
				"synara_execution_cold_start_duration_seconds_30d%s %s\n",
				addLabel(labelSet, "quantile", quantile.label),
				formatFloat(nearestRankQuantile(values, quantile.value)),
			)
		}
		fmt.Fprintf(output, "synara_execution_cold_start_samples_30d%s %d\n", labelSet, len(values))
	}
	return nil
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
