package database

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestActiveTurnSuspendCheckpointMigration(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}

	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(ctx, db, migrationsThrough(t, "000055_semantic_credential_access_refresh.sql")); err != nil {
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
		t.Fatalf("insert pre-000056 suspend attempt: %v", err)
	}

	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var execution persistence.AgentExecution
	if err := db.Select(
		"worker_id", "execution_target_id", "generation", "provider_runtime_binding_id", "requested_by",
	).Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.WorkerID == nil {
		t.Fatal("migration execution lost its Worker")
	}

	var legacy struct {
		Reason                   string
		ControlCommandID         *uuid.UUID
		ProviderRuntimeBindingID *uuid.UUID
		ResumeBundleID           *uuid.UUID
	}
	if err := db.Raw(`
		SELECT reason, control_command_id, provider_runtime_binding_id, resume_bundle_id
		FROM execution_suspend_attempts
		WHERE tenant_id = ? AND id = ?`,
		seed.tenantID, legacyAttemptID,
	).Scan(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.Reason != "waiting-keepalive" ||
		legacy.ControlCommandID != nil ||
		legacy.ProviderRuntimeBindingID != nil ||
		legacy.ResumeBundleID != nil {
		t.Fatalf("000056 did not preserve legacy waiting suspend attempt shape: %#v", legacy)
	}

	bindingID := activeSuspendMigrationBindingID(t, db, seed, execution)
	now := legacyRequestedAt.Add(10 * time.Minute)
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, legacyAttemptID).
		Updates(map[string]any{
			"status": "aborted", "aborted_at": now,
			"failure_code": "migration_fixture_closed", "failure_message": "Close the legacy waiting attempt before testing the active-turn uniqueness boundary.",
		}).Error; err != nil {
		t.Fatalf("close legacy suspend attempt after 000056: %v", err)
	}
	boundaryMeaningfulActivitySequence := int64(7)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
		Updates(map[string]any{
			"last_event_sequence":          boundaryMeaningfulActivitySequence,
			"meaningful_activity_sequence": boundaryMeaningfulActivitySequence,
		}).Error; err != nil {
		t.Fatalf("seed Session meaningful activity sequence after 000056: %v", err)
	}

	validCommand := activeSuspendMigrationCommand(seed, execution, *execution.WorkerID, "SuspendTurn", now)
	if err := db.Create(&validCommand).Error; err != nil {
		t.Fatalf("create valid SuspendTurn command after 000056: %v", err)
	}
	wrongTypeCommand := activeSuspendMigrationCommand(seed, execution, *execution.WorkerID, "SteerTurn", now.Add(time.Second))
	if err := db.Create(&wrongTypeCommand).Error; err != nil {
		t.Fatalf("create wrong-type control command after 000056: %v", err)
	}

	assertStage4MigrationRejected(
		t,
		db.Create(&persistence.ExecutionSuspendAttempt{
			ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: seed.turnID,
			ExecutionID: seed.executionID, WorkerID: *execution.WorkerID, ExecutionTargetID: execution.ExecutionTargetID,
			Generation: execution.Generation, Reason: "active-idle-timeout", Status: "checkpointing",
			CompletionMode: "worker-attested-v1", ControlCommandID: &wrongTypeCommand.ID,
			BoundaryMeaningfulActivitySequence: &boundaryMeaningfulActivitySequence,
			RequestedAt:                        now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
		}).Error,
		"chk_execution_suspend_attempts_active_suspend_receipt",
	)
	wrongBoundarySequence := boundaryMeaningfulActivitySequence - 1
	assertStage4MigrationRejected(
		t,
		db.Create(&persistence.ExecutionSuspendAttempt{
			ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: seed.turnID,
			ExecutionID: seed.executionID, WorkerID: *execution.WorkerID, ExecutionTargetID: execution.ExecutionTargetID,
			Generation: execution.Generation, Reason: "active-idle-timeout", Status: "checkpointing",
			CompletionMode: "worker-attested-v1", ControlCommandID: &validCommand.ID,
			BoundaryMeaningfulActivitySequence: &wrongBoundarySequence,
			RequestedAt:                        now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
		}).Error,
		"chk_execution_suspend_attempts_active_suspend_receipt",
	)

	attempt := persistence.ExecutionSuspendAttempt{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: seed.turnID,
		ExecutionID: seed.executionID, WorkerID: *execution.WorkerID, ExecutionTargetID: execution.ExecutionTargetID,
		Generation: execution.Generation, Reason: "active-idle-timeout", Status: "checkpointing",
		CompletionMode: "worker-attested-v1", ControlCommandID: &validCommand.ID,
		BoundaryMeaningfulActivitySequence: &boundaryMeaningfulActivitySequence,
		RequestedAt:                        now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatalf("create valid active suspend attempt after 000056: %v", err)
	}

	providerQuiescedAt := now.Add(30 * time.Second)
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Update("provider_quiesced_at", providerQuiescedAt).Error,
		"chk_execution_suspend_attempts_active_suspend_receipt",
	)

	checkpointHistorySequence := int64(7)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
		Updates(map[string]any{
			"last_event_sequence":          boundaryMeaningfulActivitySequence + 1,
			"meaningful_activity_sequence": boundaryMeaningfulActivitySequence + 1,
		}).Error; err != nil {
		t.Fatalf("advance Session activity after active suspend boundary: %v", err)
	}
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
		Updates(map[string]any{
			"provider_quiesced_at":                providerQuiescedAt,
			"active_command_id":                   "provider-host-active-1",
			"checkpoint_history_sequence":         checkpointHistorySequence,
			"current_turn_sequence":               int64(8),
			"provider_runtime_binding_id":         bindingID,
			"provider_checkpoint_protocol":        "provider-host-suspend-terminal-v1",
			"provider_cursor_source_execution_id": seed.executionID,
			"provider_cursor_source_generation":   execution.Generation,
			"provider_cursor_history_sequence":    checkpointHistorySequence,
			"provider_cursor_binding_version":     3,
			"provider_cursor_binding_digest":      activeSuspendMigrationDigest(0x11),
			"provider_cursor_sha256":              activeSuspendMigrationDigest(0x22),
			"checkpoint_receipt_sha256":           activeSuspendMigrationDigest(0x33),
		}).Error; err != nil {
		t.Fatalf("record active suspend receipt after 000056: %v", err)
	}

	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Update("provider_cursor_binding_version", 4).Error,
		"chk_execution_suspend_attempts_active_suspend_receipt",
	)

	completedAt := providerQuiescedAt.Add(20 * time.Second)
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
		Updates(map[string]any{"status": "completed", "completed_at": completedAt}).Error; err != nil {
		t.Fatalf("complete active suspend attempt after 000056: %v", err)
	}

	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Update("resume_outcome_unknown_at", completedAt.Add(time.Minute)).Error,
		"chk_execution_suspend_attempts_resume_binding",
	)

	initialBundle := activeSuspendMigrationBundle(seed, 1, "initial-claim", nil, now.Add(2*time.Second), "a")
	if err := db.Create(&initialBundle).Error; err != nil {
		t.Fatalf("create initial Recovery Bundle after 000056: %v", err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Update("generation", 2).Error; err != nil {
		t.Fatalf("advance execution generation to 2 for wrong-reason bundle: %v", err)
	}
	wrongReasonBundle := activeSuspendMigrationBundle(seed, 2, "execution-recovery", &initialBundle.ID, now.Add(3*time.Second), "b")
	if err := db.Create(&wrongReasonBundle).Error; err != nil {
		t.Fatalf("create wrong-reason Recovery Bundle after 000056: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Updates(map[string]any{
				"resume_bundle_id":  wrongReasonBundle.ID,
				"resume_generation": int64(2),
				"resume_bound_at":   completedAt.Add(2 * time.Minute),
			}).Error,
		"chk_execution_suspend_attempts_resume_binding",
	)

	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Update("generation", 3).Error; err != nil {
		t.Fatalf("advance execution generation to 3 for suspend-resume bundle: %v", err)
	}
	validResumeBundle := activeSuspendMigrationBundle(seed, 3, "suspend-resume", &wrongReasonBundle.ID, now.Add(4*time.Second), "c")
	if err := db.Create(&validResumeBundle).Error; err != nil {
		t.Fatalf("create suspend-resume Recovery Bundle after 000056: %v", err)
	}
	resumeBoundAt := completedAt.Add(3 * time.Minute)
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
		Updates(map[string]any{
			"resume_bundle_id":  validResumeBundle.ID,
			"resume_generation": int64(3),
			"resume_bound_at":   resumeBoundAt,
		}).Error; err != nil {
		t.Fatalf("bind active suspend attempt to suspend-resume bundle after 000056: %v", err)
	}

	outcomeUnknownAt := resumeBoundAt.Add(time.Minute)
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
		Update("resume_outcome_unknown_at", outcomeUnknownAt).Error; err != nil {
		t.Fatalf("record resume outcome unknown after 000056: %v", err)
	}
	assertStage4MigrationRejected(
		t,
		db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, attempt.ID).
			Update("resume_outcome_unknown_at", outcomeUnknownAt.Add(time.Second)).Error,
		"chk_execution_suspend_attempts_resume_binding",
	)
}

func activeSuspendMigrationBindingID(
	t *testing.T,
	db *gorm.DB,
	seed stage3MigrationSeed,
	execution persistence.AgentExecution,
) uuid.UUID {
	t.Helper()
	if execution.ProviderRuntimeBindingID != nil {
		return *execution.ProviderRuntimeBindingID
	}

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.CurrentRuntimeBindingID != nil {
		return *session.CurrentRuntimeBindingID
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	binding := persistence.ProviderRuntimeBinding{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID,
		Provider: "codex", Revision: 99, Status: "active", ResumeStrategy: "native-cursor",
		AuthoritativeHistorySequence: 7, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&binding).Error; err != nil {
		t.Fatalf("create fallback Provider Runtime binding: %v", err)
	}
	return binding.ID
}

func activeSuspendMigrationCommand(
	seed stage3MigrationSeed,
	execution persistence.AgentExecution,
	workerID uuid.UUID,
	commandType string,
	now time.Time,
) persistence.ExecutionControlCommand {
	id := uuid.New()
	generation := execution.Generation
	return persistence.ExecutionControlCommand{
		ID: id, TenantID: seed.tenantID, ExecutionID: seed.executionID, SessionID: seed.sessionID,
		TurnID: seed.turnID, Provider: "codex", CommandType: commandType,
		CommandID: "pg-active:" + id.String(), Payload: map[string]any{}, Status: "pending",
		RequestedBy: execution.RequestedBy, RequestedAt: now,
		DeliveryWorkerID: &workerID, DeliveryGeneration: &generation, DeliveryAvailableAt: now,
	}
}

func activeSuspendMigrationBundle(
	seed stage3MigrationSeed,
	generation int64,
	recoveryReason string,
	previousBundleID *uuid.UUID,
	now time.Time,
	shaCharacter string,
) persistence.ExecutionRecoveryBundle {
	return persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: seed.turnID,
		ExecutionID: seed.executionID, Generation: generation, SchemaVersion: 1,
		RecoveryReason: recoveryReason, PreviousBundleID: previousBundleID,
		AuthoritativeHistorySequence: generation + 6,
		Payload:                      map[string]any{"schemaVersion": 1, "generation": generation},
		PayloadSHA256:                strings.Repeat(shaCharacter, 64), CreatedAt: now,
	}
}

func activeSuspendMigrationDigest(value byte) []byte {
	return bytes.Repeat([]byte{value}, 32)
}
