package database

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestKubernetesTerminalSuspendProofMigration(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(
		context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql"),
	); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(
		context.Background(), db, migrationsThrough(t, "000053_stage4_recovery_observability.sql"),
	); err != nil {
		t.Fatal(err)
	}

	legacyAttemptID := uuid.New()
	legacyRequestedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.Exec(`
		INSERT INTO execution_suspend_attempts (
			id, tenant_id, session_id, turn_id, execution_id, worker_id,
			generation, reason, status, requested_at, checkpoint_deadline_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'waiting-keepalive', 'checkpointing', ?, ?)`,
		legacyAttemptID, seed.tenantID, seed.sessionID, seed.turnID, seed.executionID, workerIDForSeed(t, db, seed),
		1, legacyRequestedAt, legacyRequestedAt.Add(2*time.Minute),
	).Error; err != nil {
		t.Fatalf("insert pre-000054 suspend attempt: %v", err)
	}

	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.WorkerID == nil {
		t.Fatal("migration fixture lost its Worker before pod-terminal verification")
	}
	var worker persistence.WorkerInstance
	if err := db.Where("id = ?", *execution.WorkerID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if worker.RegistrationTrustMode != "shared-token" {
		t.Fatalf("000054 did not safely backfill worker registration trust: %#v", worker)
	}

	var legacy struct {
		CompletionMode    string
		ExecutionTargetID uuid.UUID
		CheckpointStatus  sql.NullString
		CheckpointReadyAt sql.NullTime
		PodTerminalPhase  sql.NullString
	}
	if err := db.Raw(`
		SELECT completion_mode, execution_target_id, checkpoint_status, checkpoint_ready_at, pod_terminal_phase
		FROM execution_suspend_attempts
		WHERE tenant_id = ? AND id = ?`,
		seed.tenantID, legacyAttemptID,
	).Scan(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.CompletionMode != "worker-attested-v1" ||
		legacy.ExecutionTargetID != execution.ExecutionTargetID ||
		legacy.CheckpointStatus.Valid || legacy.CheckpointReadyAt.Valid || legacy.PodTerminalPhase.Valid {
		t.Fatalf("000054 did not safely preserve legacy suspend attempts: %#v", legacy)
	}
	legacyAbortedAt := legacyRequestedAt.Add(time.Minute)
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, legacyAttemptID).
		Updates(map[string]any{
			"status": "aborted", "aborted_at": legacyAbortedAt,
			"failure_code": "test_replaced", "failure_message": "Replaced by the pod-terminal migration test.",
		}).Error; err != nil {
		t.Fatalf("finish legacy attempt before pod-terminal verification: %v", err)
	}

	if err := db.Model(&persistence.WorkerInstance{}).
		Where("id = ?", worker.ID).
		Update("registration_trust_mode", "kubernetes-pod-bound-v1").Error; err != nil {
		t.Fatalf("promote worker to pod-bound trust for verification: %v", err)
	}

	now := legacyRequestedAt.Add(10 * time.Minute)
	invalidAttempt := persistence.ExecutionSuspendAttempt{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: seed.turnID,
		ExecutionID: seed.executionID, WorkerID: worker.ID, Generation: execution.Generation,
		Reason: "waiting-keepalive", Status: "checkpointing", CompletionMode: "kubernetes-pod-terminal-v1",
		RequestedAt: now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
	}
	assertStage4MigrationRejected(
		t,
		db.Create(&invalidAttempt).Error,
		"chk_execution_suspend_attempts_kubernetes_identity",
	)

	attempt := persistence.ExecutionSuspendAttempt{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: seed.turnID,
		ExecutionID: seed.executionID, WorkerID: worker.ID, ExecutionTargetID: execution.ExecutionTargetID,
		Generation: execution.Generation, Reason: "waiting-keepalive", Status: "checkpointing",
		CompletionMode: "kubernetes-pod-terminal-v1", WorkerIncarnation: worker.Incarnation,
		WorkerInstanceUID: worker.InstanceUID, WorkerClusterID: worker.ClusterID,
		WorkerNamespace: worker.Namespace, WorkerPodName: worker.PodName,
		RequestedAt: now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatalf("insert valid pod-terminal suspend attempt: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Update("worker_pod_name", "rewritten-pod").Error,
		"chk_execution_suspend_attempt_lineage_immutable",
	)

	providerQuiescedAt := now.Add(30 * time.Second)
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
		Update("provider_quiesced_at", providerQuiescedAt).Error; err != nil {
		t.Fatalf("record pod-terminal provider quiesce: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Updates(map[string]any{"status": "completed", "completed_at": now.Add(time.Minute)}).Error,
		"chk_execution_suspend_attempts_pod_terminal",
	)

	checkpointReadyAt := providerQuiescedAt.Add(10 * time.Second)
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
		Updates(map[string]any{"checkpoint_status": "ready", "checkpoint_ready_at": checkpointReadyAt}).Error; err != nil {
		t.Fatalf("record checkpoint-ready handoff: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Update("checkpoint_status", "unchanged").Error,
		"chk_execution_suspend_attempt_checkpoint_immutable",
	)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Updates(map[string]any{"status": "completed", "completed_at": checkpointReadyAt.Add(time.Second)}).Error,
		"chk_execution_suspend_attempts_pod_terminal",
	)

	terminalObservedAt := checkpointReadyAt.Add(20 * time.Second)
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
		Updates(map[string]any{
			"status": "completed", "completed_at": terminalObservedAt,
			"pod_terminal_observed_at": terminalObservedAt, "pod_terminal_phase": "Succeeded",
		}).Error; err != nil {
		t.Fatalf("complete pod-terminal suspend attempt: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Update("pod_terminal_phase", "Failed").Error,
		"chk_execution_suspend_attempt_finished_immutable",
	)
}

func workerIDForSeed(t *testing.T, db *gorm.DB, seed stage3MigrationSeed) uuid.UUID {
	t.Helper()
	var execution persistence.AgentExecution
	if err := db.Select("worker_id").Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.WorkerID == nil {
		t.Fatal("seed execution does not have a worker")
	}
	return *execution.WorkerID
}
