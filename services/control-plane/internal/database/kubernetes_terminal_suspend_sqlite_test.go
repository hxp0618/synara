package database

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSQLiteKubernetesTerminalSuspendProofSafety(t *testing.T) {
	fixture := openStage4SQLiteFixture(t)

	for _, name := range []string{
		"trg_worker_instances_registration_trust_insert",
		"trg_worker_instances_registration_trust_update",
		"trg_execution_suspend_attempts_shape_insert",
		"trg_execution_suspend_attempts_shape_update",
		"trg_execution_suspend_attempts_lineage_insert",
		"trg_execution_suspend_attempts_lineage_update",
	} {
		var count int64
		if err := fixture.db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite safety object %s count = %d, want 1", name, count)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	targetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
		Kind: "kubernetes", Name: "sqlite-kubernetes", Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create kubernetes target: %v", err)
	}

	sessionID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
		CreatedBy: fixture.userID, Title: "SQLite pod-terminal session", Status: "active", Visibility: "private",
		Provider: "codex", ExecutionTargetID: targetID, ProviderResumeCursorState: "absent",
		ResourceState: "waiting", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800,
		WorkspaceRetentionDays: 30, WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create kubernetes session: %v", err)
	}
	turnID := fixture.createTurn(t, fixture.tenantID, sessionID, now)

	workerID := uuid.New()
	instanceUID := uuid.NewString()
	if err := fixture.db.Create(&persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: instanceUID, ExecutionTargetID: targetID,
		TargetKind: "kubernetes", RegistrationTrustMode: "shared-token",
		ClusterID: "sqlite-kubernetes", Namespace: "default", PodName: "sqlite-pod-terminal",
		Version: "stage4", ProtocolVersion: 1, Capabilities: map[string]any{}, LeaseSupported: true,
		FencingSupported: true, AuthTokenHash: []byte("token-" + workerID.String()), Status: "online",
		AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatalf("create kubernetes worker: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerInstance{}).
			Where("id = ?", workerID).
			Update("registration_trust_mode", "bogus").Error,
		"invalid Worker registration trust mode",
	)

	executionID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentExecution{
		ID: executionID, TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "waiting-for-approval", ExecutionTargetID: targetID, TargetKind: "kubernetes",
		WorkerID: &workerID, Generation: 1, RequestedBy: fixture.userID, QueuedAt: now, StartedAt: timePointer(now),
	}).Error; err != nil {
		t.Fatalf("create kubernetes execution: %v", err)
	}

	sharedTrustAttempt := persistence.ExecutionSuspendAttempt{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
		ExecutionID: executionID, WorkerID: workerID, ExecutionTargetID: targetID,
		Generation: 1, Reason: "waiting-keepalive", Status: "checkpointing",
		CompletionMode: "kubernetes-pod-terminal-v1", WorkerIncarnation: 1,
		WorkerInstanceUID: instanceUID, WorkerClusterID: "sqlite-kubernetes",
		WorkerNamespace: "default", WorkerPodName: "sqlite-pod-terminal",
		RequestedAt: now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Create(&sharedTrustAttempt).Error,
		"exact Pod-bound Worker snapshot",
	)

	if err := fixture.db.Model(&persistence.WorkerInstance{}).
		Where("id = ?", workerID).
		Update("registration_trust_mode", "kubernetes-pod-bound-v1").Error; err != nil {
		t.Fatalf("promote worker to pod-bound trust: %v", err)
	}

	attempt := persistence.ExecutionSuspendAttempt{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
		ExecutionID: executionID, WorkerID: workerID, ExecutionTargetID: targetID,
		Generation: 1, Reason: "waiting-keepalive", Status: "checkpointing",
		CompletionMode: "kubernetes-pod-terminal-v1", WorkerIncarnation: 1,
		WorkerInstanceUID: instanceUID, WorkerClusterID: "sqlite-kubernetes",
		WorkerNamespace: "default", WorkerPodName: "sqlite-pod-terminal",
		RequestedAt: now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
	}
	if err := fixture.db.Create(&attempt).Error; err != nil {
		t.Fatalf("create valid sqlite pod-terminal suspend attempt: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Update("worker_namespace", "rewritten").Error,
		"exact Pod-bound Worker snapshot",
	)

	providerQuiescedAt := now.Add(30 * time.Second)
	if err := fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
		Update("provider_quiesced_at", providerQuiescedAt).Error; err != nil {
		t.Fatalf("record sqlite pod-terminal provider quiesce: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Updates(map[string]any{"status": "completed", "completed_at": now.Add(time.Minute)}).Error,
		"invalid Execution suspend attempt shape",
	)

	checkpointReadyAt := providerQuiescedAt.Add(10 * time.Second)
	if err := fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
		Updates(map[string]any{"checkpoint_status": "ready", "checkpoint_ready_at": checkpointReadyAt}).Error; err != nil {
		t.Fatalf("record sqlite checkpoint-ready handoff: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Update("checkpoint_status", "unchanged").Error,
		"invalid Execution suspend attempt shape",
	)

	terminalObservedAt := checkpointReadyAt.Add(20 * time.Second)
	if err := fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
		Updates(map[string]any{
			"status": "completed", "completed_at": terminalObservedAt,
			"pod_terminal_observed_at": terminalObservedAt, "pod_terminal_phase": "Succeeded",
		}).Error; err != nil {
		t.Fatalf("complete sqlite pod-terminal suspend attempt: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, attempt.ID).
			Update("pod_terminal_phase", "Failed").Error,
		"Finished Execution suspend attempts are immutable",
	)
}
