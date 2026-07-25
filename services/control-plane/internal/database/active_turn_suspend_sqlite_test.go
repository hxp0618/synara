package database

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSQLiteActiveTurnSuspendCheckpointSafety(t *testing.T) {
	fixture := openStage4SQLiteFixture(t)

	for _, name := range []string{
		"trg_execution_suspend_attempts_shape_insert",
		"trg_execution_suspend_attempts_shape_update",
		"trg_execution_suspend_attempts_finished_update",
		"trg_execution_suspend_attempts_resume_reference_update",
	} {
		var count int64
		if err := fixture.db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite active suspend object %s count = %d, want 1", name, count)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	sessionID := fixture.createSession(t, fixture.tenantID, fixture.organizationID, fixture.projectID, "SQLite active suspend", now)
	turnID := fixture.createTurn(t, fixture.tenantID, sessionID, now)
	workerID, _ := fixture.createWorker(t, "sqlite-active-suspend", now)
	executionID := fixture.createExecution(t, sessionID, turnID, workerID, 1, "waiting-for-approval", now)
	boundaryMeaningfulActivitySequence := int64(5)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, sessionID).
		Updates(map[string]any{
			"last_event_sequence":          boundaryMeaningfulActivitySequence,
			"meaningful_activity_sequence": boundaryMeaningfulActivitySequence,
		}).Error; err != nil {
		t.Fatalf("seed sqlite Session meaningful activity sequence: %v", err)
	}

	bindingID := uuid.New()
	if err := fixture.db.Create(&persistence.ProviderRuntimeBinding{
		ID: bindingID, TenantID: fixture.tenantID, SessionID: sessionID,
		Provider: "codex", Revision: 1, Status: "active", ResumeStrategy: "native-cursor",
		AuthoritativeHistorySequence: 7, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create sqlite Provider Runtime binding: %v", err)
	}

	validCommand := activeSuspendSQLiteCommand(fixture, executionID, sessionID, turnID, workerID, 1, "SuspendTurn", now)
	if err := fixture.db.Create(&validCommand).Error; err != nil {
		t.Fatalf("create valid sqlite SuspendTurn command: %v", err)
	}
	wrongTypeCommand := activeSuspendSQLiteCommand(fixture, executionID, sessionID, turnID, workerID, 1, "SteerTurn", now.Add(time.Second))
	if err := fixture.db.Create(&wrongTypeCommand).Error; err != nil {
		t.Fatalf("create sqlite wrong-type control command: %v", err)
	}

	assertSQLiteStage4Rejected(
		t,
		fixture.db.Create(&persistence.ExecutionSuspendAttempt{
			ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
			ExecutionID: executionID, WorkerID: workerID, ExecutionTargetID: fixture.targetID,
			Generation: 1, Reason: "waiting-keepalive", Status: "checkpointing",
			CompletionMode: "worker-attested-v1", ControlCommandID: &validCommand.ID,
			RequestedAt: now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
		}).Error,
		"invalid Execution suspend attempt shape",
	)

	assertSQLiteStage4Rejected(
		t,
		fixture.db.Create(&persistence.ExecutionSuspendAttempt{
			ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
			ExecutionID: executionID, WorkerID: workerID, ExecutionTargetID: fixture.targetID,
			Generation: 1, Reason: "active-idle-timeout", Status: "checkpointing",
			CompletionMode: "worker-attested-v1", ControlCommandID: &wrongTypeCommand.ID,
			BoundaryMeaningfulActivitySequence: &boundaryMeaningfulActivitySequence,
			RequestedAt:                        now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
		}).Error,
		"exact bound SuspendTurn Control command",
	)
	wrongBoundarySequence := boundaryMeaningfulActivitySequence - 1
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Create(&persistence.ExecutionSuspendAttempt{
			ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
			ExecutionID: executionID, WorkerID: workerID, ExecutionTargetID: fixture.targetID,
			Generation: 1, Reason: "active-idle-timeout", Status: "checkpointing",
			CompletionMode: "worker-attested-v1", ControlCommandID: &validCommand.ID,
			BoundaryMeaningfulActivitySequence: &wrongBoundarySequence,
			RequestedAt:                        now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
		}).Error,
		"freeze the Session meaningful activity sequence",
	)

	attempt := persistence.ExecutionSuspendAttempt{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
		ExecutionID: executionID, WorkerID: workerID, ExecutionTargetID: fixture.targetID,
		Generation: 1, Reason: "active-idle-timeout", Status: "checkpointing",
		CompletionMode: "worker-attested-v1", ControlCommandID: &validCommand.ID,
		BoundaryMeaningfulActivitySequence: &boundaryMeaningfulActivitySequence,
		RequestedAt:                        now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
	}
	if err := fixture.db.Create(&attempt).Error; err != nil {
		t.Fatalf("create valid sqlite active suspend attempt: %v", err)
	}

	providerQuiescedAt := now.Add(30 * time.Second)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Update("provider_quiesced_at", providerQuiescedAt).Error,
		"invalid Execution suspend attempt shape",
	)

	checkpointHistorySequence := int64(7)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, sessionID).
		Updates(map[string]any{
			"last_event_sequence":          boundaryMeaningfulActivitySequence + 1,
			"meaningful_activity_sequence": boundaryMeaningfulActivitySequence + 1,
		}).Error; err != nil {
		t.Fatalf("advance sqlite Session activity after active suspend boundary: %v", err)
	}
	receipt := map[string]any{
		"provider_quiesced_at":                providerQuiescedAt,
		"active_command_id":                   "provider-host-active-1",
		"checkpoint_history_sequence":         checkpointHistorySequence,
		"current_turn_sequence":               int64(8),
		"provider_runtime_binding_id":         bindingID,
		"provider_checkpoint_protocol":        "provider-host-suspend-terminal-v1",
		"provider_cursor_source_execution_id": executionID,
		"provider_cursor_source_generation":   int64(1),
		"provider_cursor_history_sequence":    checkpointHistorySequence,
		"provider_cursor_binding_version":     3,
		"provider_cursor_binding_digest":      activeSuspendSQLiteDigest(0x11),
		"provider_cursor_sha256":              activeSuspendSQLiteDigest(0x22),
		"checkpoint_receipt_sha256":           activeSuspendSQLiteDigest(0x33),
	}
	if err := fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
		Updates(receipt).Error; err != nil {
		t.Fatalf("record sqlite active suspend receipt: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Update("provider_cursor_binding_version", 4).Error,
		"invalid Execution suspend attempt shape",
	)

	completedAt := providerQuiescedAt.Add(20 * time.Second)
	if err := fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
		Updates(map[string]any{"status": "completed", "completed_at": completedAt}).Error; err != nil {
		t.Fatalf("complete sqlite active suspend attempt: %v", err)
	}

	outcomeUnknownAt := completedAt.Add(time.Minute)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Update("resume_outcome_unknown_at", outcomeUnknownAt).Error,
		"Finished Execution suspend attempts are immutable",
	)

	initialBundle := activeSuspendSQLiteBundle(fixture, sessionID, turnID, executionID, 1, "initial-claim", nil, now.Add(2*time.Second), "a")
	if err := fixture.db.Create(&initialBundle).Error; err != nil {
		t.Fatalf("create sqlite initial Recovery Bundle: %v", err)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, executionID).
		Update("generation", 2).Error; err != nil {
		t.Fatalf("advance sqlite execution generation to 2: %v", err)
	}
	wrongReasonBundle := activeSuspendSQLiteBundle(fixture, sessionID, turnID, executionID, 2, "execution-recovery", &initialBundle.ID, now.Add(3*time.Second), "b")
	if err := fixture.db.Create(&wrongReasonBundle).Error; err != nil {
		t.Fatalf("create sqlite wrong-reason Recovery Bundle: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Updates(map[string]any{
				"resume_bundle_id":  wrongReasonBundle.ID,
				"resume_generation": int64(2),
				"resume_bound_at":   completedAt.Add(2 * time.Minute),
			}).Error,
		"suspend-resume Recovery Bundle",
	)

	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, executionID).
		Update("generation", 3).Error; err != nil {
		t.Fatalf("advance sqlite execution generation to 3: %v", err)
	}
	validResumeBundle := activeSuspendSQLiteBundle(fixture, sessionID, turnID, executionID, 3, "suspend-resume", &wrongReasonBundle.ID, now.Add(4*time.Second), "c")
	if err := fixture.db.Create(&validResumeBundle).Error; err != nil {
		t.Fatalf("create sqlite suspend-resume Recovery Bundle: %v", err)
	}
	resumeBoundAt := completedAt.Add(3 * time.Minute)
	if err := fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
		Updates(map[string]any{
			"resume_bundle_id":  validResumeBundle.ID,
			"resume_generation": int64(3),
			"resume_bound_at":   resumeBoundAt,
		}).Error; err != nil {
		t.Fatalf("bind sqlite suspend attempt to suspend-resume bundle: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Update("resume_bound_at", resumeBoundAt.Add(time.Second)).Error,
		"Finished Execution suspend attempts are immutable",
	)

	boundOutcomeUnknownAt := resumeBoundAt.Add(time.Minute)
	if err := fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
		Update("resume_outcome_unknown_at", boundOutcomeUnknownAt).Error; err != nil {
		t.Fatalf("record sqlite resume outcome unknown: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Update("resume_outcome_unknown_at", boundOutcomeUnknownAt.Add(time.Second)).Error,
		"Finished Execution suspend attempts are immutable",
	)
}

func activeSuspendSQLiteCommand(
	fixture stage4SQLiteFixture,
	executionID, sessionID, turnID, workerID uuid.UUID,
	generation int64,
	commandType string,
	now time.Time,
) persistence.ExecutionControlCommand {
	commandID := uuid.New()
	return persistence.ExecutionControlCommand{
		ID: commandID, TenantID: fixture.tenantID, ExecutionID: executionID, SessionID: sessionID,
		TurnID: turnID, Provider: "codex", CommandType: commandType,
		CommandID: "sqlite-active:" + commandID.String(), Payload: map[string]any{},
		Status: "pending", RequestedBy: fixture.userID, RequestedAt: now,
		DeliveryWorkerID: &workerID, DeliveryGeneration: &generation,
		DeliveryAvailableAt: now,
	}
}

func activeSuspendSQLiteBundle(
	fixture stage4SQLiteFixture,
	sessionID, turnID, executionID uuid.UUID,
	generation int64,
	recoveryReason string,
	previousBundleID *uuid.UUID,
	now time.Time,
	shaCharacter string,
) persistence.ExecutionRecoveryBundle {
	return persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID, ExecutionID: executionID,
		Generation: generation, SchemaVersion: 1, RecoveryReason: recoveryReason,
		PreviousBundleID: previousBundleID, AuthoritativeHistorySequence: generation + 6,
		Payload:       map[string]any{"schemaVersion": 1, "generation": generation},
		PayloadSHA256: strings.Repeat(shaCharacter, 64), CreatedAt: now,
	}
}

func activeSuspendSQLiteDigest(value byte) []byte {
	return bytes.Repeat([]byte{value}, 32)
}
