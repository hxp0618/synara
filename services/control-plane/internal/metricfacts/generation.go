package metricfacts

import (
	"math"
	"sort"
	"time"
)

const (
	GenerationMetricKindOutcome         = "outcome"
	GenerationMetricKindWarmAcquisition = "warm-acquisition"
	GenerationMetricKindColdStart       = "cold-start-duration"
	GenerationMetricKindPodQueue        = "pod-queue-duration"
	GenerationMetricKindPodProvisioning = "pod-provisioning-duration"
	GenerationMetricKindPodFailure      = "pod-failure"

	GenerationMetricDimensionNone      = "none"
	GenerationMetricCategoricalBucket  = -1
	GenerationMetricMaxHistogramBucket = 4095
	generationHistogramGrowthDivisor   = int64(50) // at most 2% relative width, plus one microsecond
)

type ExecutionGenerationSample struct {
	RecoveryReason      string
	TargetKind          string
	WarmPoolMode        string
	WarmPoolResult      string
	DispatchRequestedAt *time.Time
	ProviderReadyAt     *time.Time
	PodProvisioningAt   *time.Time
	PodRunningAt        *time.Time
	TerminalOutcome     *string
}

type GenerationMetricContribution struct {
	MetricKind      string
	TargetKind      string
	RecoveryReason  string
	Outcome         string
	WarmPoolMode    string
	WarmPoolResult  string
	FailureClass    string
	HistogramBucket int
}

func ExecutionGenerationMetricContributions(sample ExecutionGenerationSample) []GenerationMetricContribution {
	targetKind := BoundedTargetKind(sample.TargetKind)
	recoveryReason := BoundedRecoveryReason(sample.RecoveryReason)
	warmPoolMode := BoundedWarmPoolMode(sample.WarmPoolMode)
	warmPoolResult := BoundedWarmPoolResult(sample.WarmPoolResult)
	outcome := "pending"
	if sample.TerminalOutcome != nil {
		outcome = BoundedGenerationOutcome(*sample.TerminalOutcome)
	}
	contributions := []GenerationMetricContribution{{
		MetricKind: GenerationMetricKindOutcome, TargetKind: targetKind,
		RecoveryReason: recoveryReason, Outcome: outcome,
		WarmPoolMode: GenerationMetricDimensionNone, WarmPoolResult: GenerationMetricDimensionNone,
		FailureClass: GenerationMetricDimensionNone, HistogramBucket: GenerationMetricCategoricalBucket,
	}}
	if warmPoolMode != "disabled" {
		contributions = append(contributions, GenerationMetricContribution{
			MetricKind: GenerationMetricKindWarmAcquisition, TargetKind: targetKind,
			RecoveryReason: GenerationMetricDimensionNone, Outcome: GenerationMetricDimensionNone,
			WarmPoolMode: warmPoolMode, WarmPoolResult: warmPoolResult,
			FailureClass: GenerationMetricDimensionNone, HistogramBucket: GenerationMetricCategoricalBucket,
		})
	}
	if durationMicros, ok := metricDurationMicros(sample.DispatchRequestedAt, sample.PodProvisioningAt); ok {
		contributions = append(contributions, durationContribution(
			GenerationMetricKindPodQueue, targetKind, recoveryReason,
			GenerationMetricDimensionNone, GenerationMetricDimensionNone, durationMicros,
		))
	}
	if durationMicros, ok := metricDurationMicros(sample.PodProvisioningAt, sample.PodRunningAt); ok {
		contributions = append(contributions, durationContribution(
			GenerationMetricKindPodProvisioning, targetKind, recoveryReason,
			GenerationMetricDimensionNone, GenerationMetricDimensionNone, durationMicros,
		))
	}
	if durationMicros, ok := metricDurationMicros(sample.DispatchRequestedAt, sample.ProviderReadyAt); ok {
		contributions = append(contributions, durationContribution(
			GenerationMetricKindColdStart, targetKind, recoveryReason,
			warmPoolMode, warmPoolResult, durationMicros,
		))
	}
	return contributions
}

func PodFailureMetricContribution(targetKind, failureClass string) GenerationMetricContribution {
	return GenerationMetricContribution{
		MetricKind: GenerationMetricKindPodFailure, TargetKind: BoundedTargetKind(targetKind),
		RecoveryReason: GenerationMetricDimensionNone, Outcome: GenerationMetricDimensionNone,
		WarmPoolMode: GenerationMetricDimensionNone, WarmPoolResult: GenerationMetricDimensionNone,
		FailureClass: BoundedPodFailureClass(failureClass), HistogramBucket: GenerationMetricCategoricalBucket,
	}
}

func durationContribution(
	metricKind, targetKind, recoveryReason, warmPoolMode, warmPoolResult string,
	durationMicros int64,
) GenerationMetricContribution {
	return GenerationMetricContribution{
		MetricKind: metricKind, TargetKind: targetKind, RecoveryReason: recoveryReason,
		Outcome: GenerationMetricDimensionNone, WarmPoolMode: warmPoolMode,
		WarmPoolResult: warmPoolResult, FailureClass: GenerationMetricDimensionNone,
		HistogramBucket: DurationHistogramBucket(durationMicros),
	}
}

func metricDurationMicros(startAt, endAt *time.Time) (int64, bool) {
	if startAt == nil || endAt == nil || endAt.Before(*startAt) {
		return 0, false
	}
	return endAt.Sub(*startAt).Microseconds(), true
}

var durationHistogramUpperBoundsMicros = buildDurationHistogramUpperBounds()

func buildDurationHistogramUpperBounds() []int64 {
	// Index zero represents an exact zero duration. Positive bounds grow by at
	// most 2% and at least one microsecond, so sketches merge by integer bucket
	// without architecture-dependent floating point logarithms.
	bounds := []int64{0}
	for upper := int64(1); len(bounds) <= GenerationMetricMaxHistogramBucket; {
		bounds = append(bounds, upper)
		growth := (upper + generationHistogramGrowthDivisor - 1) / generationHistogramGrowthDivisor
		if growth < 1 {
			growth = 1
		}
		if upper > math.MaxInt64-growth {
			break
		}
		upper += growth
	}
	return bounds
}

func DurationHistogramBucket(durationMicros int64) int {
	if durationMicros <= 0 {
		return 0
	}
	index := sort.Search(len(durationHistogramUpperBoundsMicros), func(index int) bool {
		return durationHistogramUpperBoundsMicros[index] >= durationMicros
	})
	if index >= len(durationHistogramUpperBoundsMicros) {
		return len(durationHistogramUpperBoundsMicros) - 1
	}
	return index
}

func DurationHistogramUpperBoundMicros(bucket int) (int64, bool) {
	if bucket < 0 || bucket >= len(durationHistogramUpperBoundsMicros) {
		return 0, false
	}
	return durationHistogramUpperBoundsMicros[bucket], true
}

func ValidDurationHistogramBucket(bucket int) bool {
	_, ok := DurationHistogramUpperBoundMicros(bucket)
	return ok
}

func BoundedTargetKind(value string) string {
	switch value {
	case "local", "ssh", "docker", "kubernetes":
		return value
	default:
		return "other"
	}
}

func BoundedRecoveryReason(value string) string {
	switch value {
	case "initial-claim", "legacy-adoption", "execution-recovery", "suspend-resume":
		return value
	default:
		return "other"
	}
}

func BoundedWarmPoolMode(value string) string {
	switch value {
	case "disabled", "balanced", "low-latency":
		return value
	default:
		return "disabled"
	}
}

func BoundedWarmPoolResult(value string) string {
	switch value {
	case "pending", "not-requested", "hit", "fallback":
		return value
	default:
		return "pending"
	}
}

func BoundedGenerationOutcome(value string) string {
	switch value {
	case "completed", "failed", "cancelled", "interrupted", "recovering":
		return value
	default:
		return "other"
	}
}

func BoundedPodFailureClass(value string) string {
	switch value {
	case "pod-apply-failed", "pending-timeout", "unschedulable", "image-pull",
		"container-start", "evicted", "oom-killed", "pod-failed":
		return value
	default:
		return "other"
	}
}
