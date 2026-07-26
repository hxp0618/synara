package observability

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/metricfacts"
	"github.com/synara-ai/synara/services/control-plane/internal/metricrollup"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestExecutionGenerationFactMetricsIncludeEndToEndColdStartAndWarmOutcome(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.ExecutionGenerationFact{},
		&persistence.ExecutionGenerationPodFailureFact{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	dispatchedAt := now.Add(-12 * time.Second)
	readyAt := now.Add(-2 * time.Second)
	podAppliedAt := now.Add(-11 * time.Second)
	podRunningAt := now.Add(-7 * time.Second)
	terminalAt := now.Add(-time.Second)
	completed := "completed"
	fact := persistence.ExecutionGenerationFact{
		TenantID: uuid.New(), ExecutionID: uuid.New(), Generation: 1,
		SessionID: uuid.New(), TurnID: uuid.New(), ExecutionTargetID: uuid.New(),
		TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
		WarmPoolMode: "low-latency", WarmPoolResult: "hit",
		DispatchRequestedAt: &dispatchedAt, ProviderReadyAt: &readyAt,
		PodProvisioningStartedAt: &podAppliedAt, PodPendingSinceAt: &podAppliedAt,
		PodRunningAt: &podRunningAt, PodLastObservedAt: &podRunningAt,
		TerminalAt: &terminalAt, TerminalOutcome: &completed,
		ProviderResumeStrategy: "native-cursor", CreatedAt: dispatchedAt, UpdatedAt: terminalAt,
	}
	if err := db.Create(&fact).Error; err != nil {
		t.Fatal(err)
	}
	podUID := uuid.NewString()
	if err := db.Create(&persistence.ExecutionGenerationPodFailureFact{
		TenantID: fact.TenantID, ExecutionID: fact.ExecutionID, Generation: fact.Generation,
		FailureClass: "image-pull", ExecutionTargetID: fact.ExecutionTargetID,
		Namespace: "synara-test", PodName: "worker", PodUID: &podUID,
		ReasonCode: "image-pull-backoff", FirstObservedAt: podAppliedAt,
		LastObservedAt: podAppliedAt, CreatedAt: podAppliedAt, UpdatedAt: podAppliedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := New(db).writeExecutionGenerationFactMetrics(context.Background(), &output, now); err != nil {
		t.Fatal(err)
	}
	metrics := output.String()
	for _, expected := range []string{
		`synara_execution_generation_outcomes_30d{outcome="completed",recovery_reason="initial-claim",target_kind="kubernetes"} 1`,
		`synara_warm_pool_acquisitions_30d{requested_mode="low-latency",result="hit",target_kind="kubernetes"} 1`,
		`synara_execution_cold_start_duration_seconds_30d{recovery_reason="initial-claim",target_kind="kubernetes",warm_pool_mode="low-latency",warm_pool_result="hit",quantile="0.5"} 10`,
		`synara_execution_cold_start_duration_seconds_30d{recovery_reason="initial-claim",target_kind="kubernetes",warm_pool_mode="low-latency",warm_pool_result="hit",quantile="0.95"} 10`,
		`synara_execution_cold_start_duration_seconds_30d{recovery_reason="initial-claim",target_kind="kubernetes",warm_pool_mode="low-latency",warm_pool_result="hit",quantile="0.99"} 10`,
		`synara_execution_cold_start_samples_30d{recovery_reason="initial-claim",target_kind="kubernetes",warm_pool_mode="low-latency",warm_pool_result="hit"} 1`,
		`synara_execution_pod_queue_duration_seconds_30d{recovery_reason="initial-claim",target_kind="kubernetes",quantile="0.5"} 1`,
		`synara_execution_pod_queue_samples_30d{recovery_reason="initial-claim",target_kind="kubernetes"} 1`,
		`synara_execution_pod_provisioning_duration_seconds_30d{recovery_reason="initial-claim",target_kind="kubernetes",quantile="0.95"} 4`,
		`synara_execution_pod_provisioning_samples_30d{recovery_reason="initial-claim",target_kind="kubernetes"} 1`,
		`synara_execution_pod_failure_generations_30d{failure_class="image-pull",target_kind="kubernetes"} 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
}

func TestExecutionGenerationFactMetricsMergeFullDayRollupsAndRawBoundaryFactsExactlyOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.ExecutionGenerationFact{},
		&persistence.ExecutionGenerationPodFailureFact{},
		&persistence.ExecutionGenerationMetricRollup{},
		&persistence.ExecutionGenerationMetricRollupEntry{},
		&persistence.ExecutionGenerationPodFailureMetricRollupEntry{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-executionGenerationMetricWindow)
	completed := "completed"
	type generationFixture struct {
		fact     persistence.ExecutionGenerationFact
		failure  persistence.ExecutionGenerationPodFailureFact
		terminal bool
	}
	makeFixture := func(dispatchedAt time.Time, duration time.Duration, terminal bool) generationFixture {
		readyAt := dispatchedAt.Add(duration)
		terminalAt := readyAt.Add(time.Second)
		fact := persistence.ExecutionGenerationFact{
			TenantID: uuid.New(), ExecutionID: uuid.New(), Generation: 1,
			SessionID: uuid.New(), TurnID: uuid.New(), ExecutionTargetID: uuid.New(),
			TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
			WarmPoolMode: "disabled", WarmPoolResult: "not-requested",
			DispatchRequestedAt: &dispatchedAt, ProviderReadyAt: &readyAt,
			ProviderResumeStrategy: "authoritative-history", CreatedAt: dispatchedAt, UpdatedAt: readyAt,
		}
		if terminal {
			fact.TerminalAt = &terminalAt
			fact.TerminalOutcome = &completed
			fact.UpdatedAt = terminalAt
		}
		podUID := uuid.NewString()
		return generationFixture{
			fact: fact, terminal: terminal,
			failure: persistence.ExecutionGenerationPodFailureFact{
				TenantID: fact.TenantID, ExecutionID: fact.ExecutionID, Generation: fact.Generation,
				FailureClass: "image-pull", ExecutionTargetID: fact.ExecutionTargetID,
				Namespace: "default", PodName: "worker", PodUID: &podUID, ReasonCode: "image-pull-backoff",
				FirstObservedAt: dispatchedAt, LastObservedAt: dispatchedAt,
				CreatedAt: dispatchedAt, UpdatedAt: dispatchedAt,
			},
		}
	}
	fixtures := []generationFixture{
		makeFixture(time.Date(2026, time.July, 25, 10, 0, 0, 0, time.UTC), 10*time.Second, true),
		makeFixture(windowStart.Add(time.Hour), 20*time.Second, true),
		makeFixture(time.Date(2026, time.July, 24, 10, 0, 0, 0, time.UTC), 30*time.Second, false),
	}
	for _, fixture := range fixtures {
		if err := db.Create(&fixture.fact).Error; err != nil {
			t.Fatal(err)
		}
		if fixture.terminal {
			if err := db.Create(&persistence.ExecutionGenerationMetricRollupEntry{
				TenantID: fixture.fact.TenantID, ExecutionID: fixture.fact.ExecutionID,
				Generation: fixture.fact.Generation, DispatchRequestedAt: *fixture.fact.DispatchRequestedAt,
				TerminalAt: *fixture.fact.TerminalAt, BucketDay: utcDayBoundary(*fixture.fact.DispatchRequestedAt),
				CreatedAt: *fixture.fact.TerminalAt, UpdatedAt: *fixture.fact.TerminalAt,
			}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&fixture.failure).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&persistence.ExecutionGenerationPodFailureMetricRollupEntry{
				TenantID: fixture.failure.TenantID, ExecutionID: fixture.failure.ExecutionID,
				Generation: fixture.failure.Generation, FailureClass: fixture.failure.FailureClass,
				FirstObservedAt: fixture.failure.FirstObservedAt,
				BucketDay:       utcDayBoundary(fixture.failure.FirstObservedAt),
				CreatedAt:       fixture.failure.FirstObservedAt, UpdatedAt: fixture.failure.FirstObservedAt,
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	rollupService := metricrollup.NewService(db)
	summary, err := rollupService.RunOnce(context.Background(), 100)
	if err != nil || summary.ProcessedGenerationFacts != 2 || summary.ProcessedPodFailureFacts != 2 {
		t.Fatalf("roll up Generation metric fixtures = %#v, %v", summary, err)
	}
	var output bytes.Buffer
	if err := New(db).writeExecutionGenerationFactMetrics(context.Background(), &output, now); err != nil {
		t.Fatal(err)
	}
	metrics := output.String()
	upperMicros, ok := metricfacts.DurationHistogramUpperBoundMicros(
		metricfacts.DurationHistogramBucket((20 * time.Second).Microseconds()),
	)
	if !ok {
		t.Fatal("20-second histogram bucket is unavailable")
	}
	for _, expected := range []string{
		`synara_execution_generation_outcomes_30d{outcome="completed",recovery_reason="initial-claim",target_kind="kubernetes"} 2`,
		`synara_execution_generation_outcomes_30d{outcome="pending",recovery_reason="initial-claim",target_kind="kubernetes"} 1`,
		`synara_execution_cold_start_samples_30d{recovery_reason="initial-claim",target_kind="kubernetes",warm_pool_mode="disabled",warm_pool_result="not-requested"} 3`,
		fmt.Sprintf(`synara_execution_cold_start_duration_seconds_30d{recovery_reason="initial-claim",target_kind="kubernetes",warm_pool_mode="disabled",warm_pool_result="not-requested",quantile="0.5"} %s`, formatFloat(float64(upperMicros)/1_000_000)),
		`synara_execution_pod_failure_generations_30d{failure_class="image-pull",target_kind="kubernetes"} 2`,
		`synara_execution_generation_metric_rollup_pending_facts{kind="generation"} 0`,
		`synara_execution_generation_metric_rollup_pending_facts{kind="pod-failure"} 0`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("merged Generation metrics omitted %q:\n%s", expected, metrics)
		}
	}
}

func TestProviderResumeMetricsDeduplicateFallbackByGenerationFact(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.SessionEvent{}, &persistence.ExecutionGenerationFact{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reason := "session_resume_expired"
	provider := "codex"
	fact := persistence.ExecutionGenerationFact{
		TenantID: uuid.New(), ExecutionID: uuid.New(), Generation: 2,
		SessionID: uuid.New(), TurnID: uuid.New(), ExecutionTargetID: uuid.New(),
		TargetKind: "kubernetes", Provider: provider, RecoveryReason: "suspend-resume",
		WarmPoolMode: "disabled", WarmPoolResult: "not-requested",
		ProviderResumeStrategy: "native-cursor", ResumeFallbackProvider: &provider,
		ResumeFallbackReasonCode: &reason, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&fact).Error; err != nil {
		t.Fatal(err)
	}
	registry := New(db)
	_, fallbacks, err := registry.providerResumeMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	key := providerResumeRuntimeFallbackMetricKey{Provider: provider, ReasonCode: reason}
	if fallbacks[key] != 1 || len(fallbacks) != 1 {
		t.Fatalf("durable fallback metrics = %#v, want one generation", fallbacks)
	}
}

func TestExecutionStartupSamplesUseDurableGenerationFacts(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.ExecutionRecoveryBundle{},
		&persistence.ExecutionGenerationFact{},
		&persistence.SessionEvent{},
	); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	tenantID := uuid.New()
	executionID := uuid.New()
	sessionID := uuid.New()
	turnID := uuid.New()
	bundleAt := now.Add(-20 * time.Second)
	leasedAt := now.Add(-10 * time.Second)
	startedAt := now.Add(-8 * time.Second)
	readyAt := now.Add(-5 * time.Second)
	if err := db.Create(&persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
		ExecutionID: executionID, Generation: 1, SchemaVersion: 1,
		RecoveryReason: "initial-claim", AuthoritativeHistorySequence: 0,
		Payload: map[string]any{"schemaVersion": 1}, PayloadSHA256: strings.Repeat("a", 64),
		CreatedAt: bundleAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ExecutionGenerationFact{
		TenantID: tenantID, ExecutionID: executionID, Generation: 1,
		SessionID: sessionID, TurnID: turnID, ExecutionTargetID: uuid.New(),
		TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
		WarmPoolMode: "balanced", WarmPoolResult: "hit", BundleCreatedAt: &bundleAt,
		LeasedAt: &leasedAt, ExecutionStartedAt: &startedAt, ProviderReadyAt: &readyAt,
		ProviderResumeStrategy: "authoritative-history", CreatedAt: bundleAt, UpdatedAt: readyAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// A duplicate or skewed legacy Event must not alter the durable sample.
	duplicateAt := now.Add(-30 * time.Second)
	if err := db.Create(&persistence.SessionEvent{
		TenantID: tenantID, SessionID: sessionID, Sequence: 1, EventID: uuid.New(),
		EventVersion: 2, EventType: "execution.started", ActorType: "worker",
		ExecutionID: &executionID, Generation: pointerInt64(1), Payload: map[string]any{},
		OccurredAt: duplicateAt,
	}).Error; err != nil {
		t.Fatal(err)
	}

	samples, err := New(db).executionStartupSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].StartedAt == nil || !samples[0].StartedAt.Equal(startedAt) ||
		samples[0].LeasedAt == nil || !samples[0].LeasedAt.Equal(leasedAt) {
		t.Fatalf("durable startup samples = %#v", samples)
	}
}

func TestExecutionStartupSamplesIncludeDispatchedRecoveryBeforeClaim(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.ExecutionRecoveryBundle{},
		&persistence.ExecutionGenerationFact{},
	); err != nil {
		t.Fatal(err)
	}

	dispatchedAt := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Second)
	fact := persistence.ExecutionGenerationFact{
		TenantID: uuid.New(), ExecutionID: uuid.New(), Generation: 2,
		SessionID: uuid.New(), TurnID: uuid.New(), ExecutionTargetID: uuid.New(),
		TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "execution-recovery",
		WarmPoolMode: "balanced", WarmPoolResult: "pending",
		DispatchRequestedAt: &dispatchedAt, ProviderResumeStrategy: "authoritative-history",
		CreatedAt: dispatchedAt, UpdatedAt: dispatchedAt,
	}
	if err := db.Create(&fact).Error; err != nil {
		t.Fatal(err)
	}

	samples, err := New(db).executionStartupSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Generation != 2 ||
		!samples[0].BundleCreatedAt.Equal(dispatchedAt) || samples[0].LeasedAt != nil {
		t.Fatalf("pre-claim durable startup samples = %#v", samples)
	}
}
