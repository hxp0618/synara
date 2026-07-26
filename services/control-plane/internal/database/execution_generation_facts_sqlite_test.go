package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/metricrollup"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteExecutionGenerationFactsBackfillAndDeleteFence(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "generation-fact-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	projectID := uuid.New()
	sessionID := uuid.New()
	turnID := uuid.New()
	executionID := uuid.New()
	workerID := uuid.New()
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Generation fact backfill", DefaultBranch: "main", Visibility: "private",
		CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Generation fact backfill",
		Status: "active", Visibility: "private", Provider: "codex",
		ExecutionTargetID: domain.ExecutionTargetID, ProviderResumeCursorState: "absent",
		ResourceState: "active", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900,
		SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30, WarmPoolMode: "disabled",
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentTurn{
		ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
		Status: "running", InputText: "continue", TurnKind: "message",
		RuntimeMode: "full-access", InteractionMode: "default", StartedAt: &now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	instanceUID := uuid.NewString()
	if err := store.DB().Create(&persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: instanceUID,
		ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", WorkerMode: "general-pool",
		RegistrationTrustMode: "shared-token", ClusterID: "sqlite", Namespace: "default",
		PodName: "generation-fact-worker", Version: "test", ProtocolVersion: 2,
		Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: []byte("generation-fact-token"), Status: "online", AdministrativeStatus: "active",
		RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	if err := store.DB().Create(&persistence.AgentExecution{
		ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "running", ExecutionTargetID: domain.ExecutionTargetID,
		TargetKind: "local", WarmPoolModeSnapshot: "disabled", Provider: &provider,
		WorkerID: &workerID, ProviderResumeStrategySnapshot: "authoritative-history",
		Generation: 2, RequestedBy: domain.UserID, QueuedAt: now, StartedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	bundleCreatedAt := now.Add(15 * time.Second)
	if err := store.DB().Create(&persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		ExecutionID: executionID, Generation: 2, SchemaVersion: 1,
		RecoveryReason: "legacy-adoption", AuthoritativeHistorySequence: 0,
		Payload: map[string]any{"schemaVersion": 1}, PayloadSHA256: strings.Repeat("a", 64),
		CreatedAt: bundleCreatedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var fact persistence.ExecutionGenerationFact
	if err := store.DB().Where(
		"tenant_id = ? AND execution_id = ? AND generation = ?",
		domain.TenantID, executionID, 2,
	).Take(&fact).Error; err != nil {
		t.Fatalf("load SQLite backfilled generation fact: %v", err)
	}
	if fact.RecoveryReason != "legacy-adoption" || fact.WarmPoolResult != "not-requested" ||
		fact.DispatchRequestedAt == nil || !fact.DispatchRequestedAt.Equal(bundleCreatedAt) {
		t.Fatalf("SQLite backfilled generation fact = %#v", fact)
	}
	podAppliedAt := bundleCreatedAt.Add(time.Second)
	podPendingAt := podAppliedAt.Add(time.Second)
	podRunningAt := podPendingAt.Add(time.Second)
	if err := store.DB().Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 2).
		Updates(map[string]any{
			"pod_provisioning_started_at": podAppliedAt,
			"pod_pending_since_at":        podPendingAt,
			"pod_running_at":              podRunningAt,
			"pod_last_observed_at":        podRunningAt,
			"updated_at":                  podRunningAt,
		}).Error; err != nil {
		t.Fatalf("advance SQLite Pod provisioning fact: %v", err)
	}
	if err := store.DB().Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 2).
		Update("pod_provisioning_started_at", podAppliedAt.Add(time.Second)).Error; err == nil || !strings.Contains(err.Error(), "Pod provisioning start is immutable") {
		t.Fatalf("SQLite Pod provisioning immutability fence = %v", err)
	}
	podUID := uuid.NewString()
	failure := persistence.ExecutionGenerationPodFailureFact{
		TenantID: domain.TenantID, ExecutionID: executionID, Generation: 2,
		FailureClass: "image-pull", ExecutionTargetID: fact.ExecutionTargetID,
		Namespace: "default", PodName: "generation-fact-worker", PodUID: &podUID,
		ReasonCode: "image-pull-backoff", FirstObservedAt: podPendingAt,
		LastObservedAt: podPendingAt, CreatedAt: podPendingAt, UpdatedAt: podPendingAt,
	}
	if err := store.DB().Create(&failure).Error; err != nil {
		t.Fatalf("create SQLite Pod failure fact: %v", err)
	}
	var failureRollupEntry persistence.ExecutionGenerationPodFailureMetricRollupEntry
	if err := store.DB().Where(
		"tenant_id = ? AND execution_id = ? AND generation = ? AND failure_class = ?",
		failure.TenantID, failure.ExecutionID, failure.Generation, failure.FailureClass,
	).Take(&failureRollupEntry).Error; err != nil {
		t.Fatalf("SQLite Pod failure metric rollup enqueue: %v", err)
	}
	readyAt := podRunningAt.Add(time.Second)
	terminalAt := readyAt.Add(time.Second)
	if err := store.DB().Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 2).
		Updates(map[string]any{
			"provider_ready_at": readyAt, "terminal_at": terminalAt,
			"terminal_outcome": "completed", "updated_at": terminalAt,
		}).Error; err != nil {
		t.Fatalf("seal SQLite Generation metric source: %v", err)
	}
	var generationRollupEntry persistence.ExecutionGenerationMetricRollupEntry
	if err := store.DB().Where(
		"tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 2,
	).Take(&generationRollupEntry).Error; err != nil {
		t.Fatalf("SQLite Generation metric rollup enqueue: %v", err)
	}
	rollupSummary, err := metricrollup.NewService(store.DB()).RunOnce(ctx, 100)
	if err != nil || rollupSummary.ProcessedGenerationFacts != 1 || rollupSummary.ProcessedPodFailureFacts != 1 {
		t.Fatalf("SQLite Generation metric rollup = %#v, %v", rollupSummary, err)
	}
	if err := store.DB().Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 2).
		Update("provider_ready_at", readyAt.Add(time.Second)).Error; err == nil || !strings.Contains(err.Error(), "metric source is sealed") {
		t.Fatalf("SQLite Generation metric source seal = %v", err)
	}
	if err := store.DB().Delete(
		&persistence.ExecutionGenerationMetricRollupEntry{},
		"tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 2,
	).Error; err == nil || !strings.Contains(err.Error(), "rollup entries cannot be deleted") {
		t.Fatalf("SQLite Generation metric rollup membership delete fence = %v", err)
	}
	var generationRollup persistence.ExecutionGenerationMetricRollup
	if err := store.DB().Where("metric_kind = ?", "outcome").Take(&generationRollup).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.ExecutionGenerationMetricRollup{}).
		Where(
			"bucket_day = ? AND metric_kind = ? AND target_kind = ? AND recovery_reason = ? AND outcome = ? AND warm_pool_mode = ? AND warm_pool_result = ? AND failure_class = ? AND histogram_bucket = ?",
			generationRollup.BucketDay, generationRollup.MetricKind, generationRollup.TargetKind,
			generationRollup.RecoveryReason, generationRollup.Outcome, generationRollup.WarmPoolMode,
			generationRollup.WarmPoolResult, generationRollup.FailureClass, generationRollup.HistogramBucket,
		).
		Update("sample_count", 0).Error; err == nil || !strings.Contains(err.Error(), "totals cannot regress") {
		t.Fatalf("SQLite Generation metric rollup monotonic fence = %v", err)
	}
	if err := store.DB().Model(&failure).Update("reason_code", "err-image-pull").Error; err == nil || !strings.Contains(err.Error(), "Pod failure fact identity is immutable") {
		t.Fatalf("SQLite Pod failure identity fence = %v", err)
	}
	if err := store.DB().Delete(&failure).Error; err == nil || !strings.Contains(err.Error(), "Pod failure facts cannot be deleted") {
		t.Fatalf("SQLite Pod failure delete fence = %v", err)
	}
	if err := store.DB().Delete(&persistence.ExecutionGenerationFact{},
		"tenant_id = ? AND execution_id = ? AND generation = ?",
		domain.TenantID, executionID, 2,
	).Error; err == nil || !strings.Contains(err.Error(), "cannot be deleted") {
		t.Fatalf("SQLite generation fact delete fence = %v", err)
	}
}
