package metricfacts

import (
	"testing"
	"time"
)

func TestDurationHistogramBoundsAreDeterministicAndCoverDurations(t *testing.T) {
	for _, durationMicros := range []int64{0, 1, 49, 50, 51, 999, 1_000_000, 10_000_000, 86_400_000_000} {
		bucket := DurationHistogramBucket(durationMicros)
		upper, ok := DurationHistogramUpperBoundMicros(bucket)
		if !ok || upper < durationMicros {
			t.Fatalf("duration %d bucket %d upper = %d, %v", durationMicros, bucket, upper, ok)
		}
		if bucket > 0 {
			previous, previousOK := DurationHistogramUpperBoundMicros(bucket - 1)
			if !previousOK || previous >= durationMicros {
				t.Fatalf("duration %d bucket %d previous upper = %d, %v", durationMicros, bucket, previous, previousOK)
			}
		}
	}
	for bucket := 1; bucket < len(durationHistogramUpperBoundsMicros); bucket++ {
		if durationHistogramUpperBoundsMicros[bucket] <= durationHistogramUpperBoundsMicros[bucket-1] {
			t.Fatalf("duration histogram bounds regress at %d", bucket)
		}
	}
}

func TestExecutionGenerationMetricContributionsUseMergeableDurationBuckets(t *testing.T) {
	dispatchedAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	podAppliedAt := dispatchedAt.Add(time.Second)
	podRunningAt := podAppliedAt.Add(4 * time.Second)
	readyAt := dispatchedAt.Add(10 * time.Second)
	completed := "completed"
	contributions := ExecutionGenerationMetricContributions(ExecutionGenerationSample{
		RecoveryReason: "initial-claim", TargetKind: "kubernetes",
		WarmPoolMode: "low-latency", WarmPoolResult: "hit",
		DispatchRequestedAt: &dispatchedAt, ProviderReadyAt: &readyAt,
		PodProvisioningAt: &podAppliedAt, PodRunningAt: &podRunningAt,
		TerminalOutcome: &completed,
	})
	if len(contributions) != 5 {
		t.Fatalf("Generation metric contributions = %#v", contributions)
	}
	wantedKinds := map[string]bool{
		GenerationMetricKindOutcome: true, GenerationMetricKindWarmAcquisition: true,
		GenerationMetricKindColdStart: true, GenerationMetricKindPodQueue: true,
		GenerationMetricKindPodProvisioning: true,
	}
	for _, contribution := range contributions {
		if !wantedKinds[contribution.MetricKind] {
			t.Fatalf("unexpected Generation metric contribution %#v", contribution)
		}
		delete(wantedKinds, contribution.MetricKind)
		if contribution.HistogramBucket >= 0 && !ValidDurationHistogramBucket(contribution.HistogramBucket) {
			t.Fatalf("invalid Generation duration bucket %#v", contribution)
		}
	}
	if len(wantedKinds) != 0 {
		t.Fatalf("missing Generation metric contributions %#v", wantedKinds)
	}
}
