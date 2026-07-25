package observability

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestExecutionGenerationFactMetricsIncludeEndToEndColdStartAndWarmOutcome(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.ExecutionGenerationFact{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	dispatchedAt := now.Add(-12 * time.Second)
	readyAt := now.Add(-2 * time.Second)
	terminalAt := now.Add(-time.Second)
	completed := "completed"
	fact := persistence.ExecutionGenerationFact{
		TenantID: uuid.New(), ExecutionID: uuid.New(), Generation: 1,
		SessionID: uuid.New(), TurnID: uuid.New(), ExecutionTargetID: uuid.New(),
		TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
		WarmPoolMode: "low-latency", WarmPoolResult: "hit",
		DispatchRequestedAt: &dispatchedAt, ProviderReadyAt: &readyAt,
		TerminalAt: &terminalAt, TerminalOutcome: &completed,
		ProviderResumeStrategy: "native-cursor", CreatedAt: dispatchedAt, UpdatedAt: terminalAt,
	}
	if err := db.Create(&fact).Error; err != nil {
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
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
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
