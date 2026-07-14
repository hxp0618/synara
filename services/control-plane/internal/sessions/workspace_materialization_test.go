package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestTurnReplacesLegacyUnknownPodMaterializationBeforeClaim(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	target := configureSessionKubernetesTarget(t, fixture)
	workspace, legacy := seedCurrentPodMaterialization(t, fixture, target, 2, "ready")

	if _, err := fixture.service.CreateTurn(
		context.Background(), fixture.principal, fixture.sessionID,
		CreateTurnInput{InputText: "replace legacy pod placement"}, "replace-legacy-pod", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	execution := loadSessionExecution(t, fixture, "replace legacy pod placement")
	if execution.WorkspaceMaterializationID == nil || *execution.WorkspaceMaterializationID == legacy.ID {
		t.Fatalf("legacy Pod placement was reused: execution=%#v legacy=%s", execution, legacy.ID)
	}

	var retired persistence.WorkspaceMaterialization
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, legacy.ID).Take(&retired).Error; err != nil {
		t.Fatal(err)
	}
	if retired.State != "retired" || retired.CleanupReason == nil ||
		*retired.CleanupReason != "worker-instance-replaced" || retired.CleanupRequestedAt == nil {
		t.Fatalf("legacy Pod placement was not durably retired: %#v", retired)
	}
	var current persistence.WorkspaceMaterialization
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, *execution.WorkspaceMaterializationID).
		Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.WorkspaceID != workspace.ID || current.LayoutVersion != currentWorkspaceLayoutVersion ||
		current.StorageScope != "pod" || current.WorkerInstanceUID != nil || current.State != "active" {
		t.Fatalf("replacement Pod materialization is invalid: %#v", current)
	}
	var updatedWorkspace persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, workspace.ID).
		Take(&updatedWorkspace).Error; err != nil {
		t.Fatal(err)
	}
	if updatedWorkspace.State != "recovering" || updatedWorkspace.CurrentMaterializationID == nil ||
		*updatedWorkspace.CurrentMaterializationID != current.ID {
		t.Fatalf("logical Workspace did not move to the replacement Pod materialization: %#v", updatedWorkspace)
	}
}

func TestTurnReusesFreshUnboundPodMaterialization(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	target := configureSessionKubernetesTarget(t, fixture)
	_, fresh := seedCurrentPodMaterialization(t, fixture, target, currentWorkspaceLayoutVersion, "pending")

	if _, err := fixture.service.CreateTurn(
		context.Background(), fixture.principal, fixture.sessionID,
		CreateTurnInput{InputText: "bind fresh pod placement"}, "bind-fresh-pod", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	execution := loadSessionExecution(t, fixture, "bind fresh pod placement")
	if execution.WorkspaceMaterializationID == nil || *execution.WorkspaceMaterializationID != fresh.ID {
		t.Fatalf("fresh unbound Pod materialization was replaced: execution=%#v fresh=%s", execution, fresh.ID)
	}
	var persisted persistence.WorkspaceMaterialization
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fresh.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.State != "active" || persisted.CleanupReason != nil || persisted.CleanupRequestedAt != nil {
		t.Fatalf("fresh unbound Pod materialization was retired: %#v", persisted)
	}
}

func TestTurnRejectsDirtyLegacyUnknownPodMaterializationWithoutLatestReadyCheckpoint(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	target := configureSessionKubernetesTarget(t, fixture)
	workspace, legacy := seedCurrentPodMaterialization(t, fixture, target, 2, "dirty")

	_, err := fixture.service.CreateTurn(
		context.Background(), fixture.principal, fixture.sessionID,
		CreateTurnInput{InputText: "unsafe legacy pod replacement"}, "unsafe-legacy-pod", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "dirty_workspace_checkpoint_required")
	var persisted persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, workspace.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.State != "dirty" || persisted.CurrentMaterializationID == nil ||
		*persisted.CurrentMaterializationID != legacy.ID {
		t.Fatalf("rejected legacy Pod replacement mutated the logical Workspace: %#v", persisted)
	}
	assertCount(t, fixture, &persistence.WorkspaceMaterialization{}, "workspace_id = ?", 1, workspace.ID)
}

func TestTurnMovesWorkspaceToNewTargetWithoutLosingOldMaterialization(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	if _, err := fixture.service.CreateTurn(
		ctx, fixture.principal, fixture.sessionID, CreateTurnInput{InputText: "first target"}, "first-target", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	first := loadSessionExecution(t, fixture, "first target")
	if first.WorkspaceMaterializationID == nil {
		t.Fatal("first Execution omitted its materialization")
	}
	completeSessionExecutionForNextTurn(t, fixture, first)

	targetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
		Kind: "docker", Name: "replacement", Status: "active", ConfigurationEncrypted: []byte{},
		Capabilities: enabledProviderPolicyTestCapabilities(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Update("execution_target_id", targetID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateTurn(
		ctx, fixture.principal, fixture.sessionID, CreateTurnInput{InputText: "second target"}, "second-target", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	second := loadSessionExecution(t, fixture, "second target")
	if second.WorkspaceMaterializationID == nil || *second.WorkspaceMaterializationID == *first.WorkspaceMaterializationID {
		t.Fatalf("target move reused the old physical materialization: first=%#v second=%#v", first, second)
	}

	var oldMaterialization persistence.WorkspaceMaterialization
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, *first.WorkspaceMaterializationID).
		Take(&oldMaterialization).Error; err != nil {
		t.Fatal(err)
	}
	if oldMaterialization.State != "retired" || oldMaterialization.CleanupReason == nil ||
		*oldMaterialization.CleanupReason != "target-move" || oldMaterialization.CleanupRequestedAt == nil {
		t.Fatalf("old Target materialization was not durably retired: %#v", oldMaterialization)
	}
	var workspace persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.ExecutionTargetID != targetID || workspace.State != "recovering" ||
		workspace.CurrentMaterializationID == nil || *workspace.CurrentMaterializationID != *second.WorkspaceMaterializationID {
		t.Fatalf("logical Workspace did not move to the new materialization: %#v", workspace)
	}
	var current persistence.WorkspaceMaterialization
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, *second.WorkspaceMaterializationID).
		Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.ExecutionTargetID != targetID || current.TargetKind != "docker" || current.StorageScope != "target" ||
		current.LayoutVersion != currentWorkspaceLayoutVersion || current.State != "active" {
		t.Fatalf("new Target materialization is invalid: %#v", current)
	}
}

func TestTurnReplacesCleanupPendingCurrentMaterialization(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	if _, err := fixture.service.CreateTurn(
		ctx, fixture.principal, fixture.sessionID, CreateTurnInput{InputText: "before cleanup"}, "before-cleanup", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	first := loadSessionExecution(t, fixture, "before cleanup")
	completeSessionExecutionForNextTurn(t, fixture, first)
	now := time.Now().UTC()
	if err := fixture.db.Model(&persistence.WorkspaceMaterialization{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, *first.WorkspaceMaterializationID).
		Updates(map[string]any{
			"state": "cleanup-pending", "cleanup_reason": "test-cleanup", "cleanup_requested_at": now,
		}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Update("state", "cleanup-pending").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateTurn(
		ctx, fixture.principal, fixture.sessionID, CreateTurnInput{InputText: "after cleanup"}, "after-cleanup", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	second := loadSessionExecution(t, fixture, "after cleanup")
	if second.WorkspaceMaterializationID == nil || *second.WorkspaceMaterializationID == *first.WorkspaceMaterializationID {
		t.Fatalf("cleanup-pending materialization was reused: first=%#v second=%#v", first, second)
	}
	var workspace persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.State != "recovering" || workspace.CurrentMaterializationID == nil ||
		*workspace.CurrentMaterializationID != *second.WorkspaceMaterializationID {
		t.Fatalf("logical Workspace did not recover onto a fresh incarnation: %#v", workspace)
	}
}

func TestTurnRejectsDirtyWorkspaceTargetMoveWithoutLatestReadyCheckpoint(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	if _, err := fixture.service.CreateTurn(
		ctx, fixture.principal, fixture.sessionID, CreateTurnInput{InputText: "dirty target"}, "dirty-target", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	execution := loadSessionExecution(t, fixture, "dirty target")
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, execution.ID).
		Update("generation", 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"state": "dirty", "last_execution_id": execution.ID, "last_generation": 1,
		}).Error; err != nil {
		t.Fatal(err)
	}
	completeSessionExecutionForNextTurn(t, fixture, execution)
	var before persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	targetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
		Kind: "docker", Name: "unsafe-replacement", Status: "active", ConfigurationEncrypted: []byte{},
		Capabilities: enabledProviderPolicyTestCapabilities(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Update("execution_target_id", targetID).Error; err != nil {
		t.Fatal(err)
	}
	_, err := fixture.service.CreateTurn(
		ctx, fixture.principal, fixture.sessionID, CreateTurnInput{InputText: "must not move"}, "unsafe-move", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "dirty_workspace_checkpoint_required")
	var after persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.ExecutionTargetID != before.ExecutionTargetID || after.CurrentMaterializationID == nil ||
		before.CurrentMaterializationID == nil || *after.CurrentMaterializationID != *before.CurrentMaterializationID ||
		after.State != "dirty" {
		t.Fatalf("failed dirty target move mutated the logical Workspace: before=%#v after=%#v", before, after)
	}
}

func TestManualArchiveSchedulesWorkspaceCleanupFromTenantPolicy(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	if _, err := fixture.service.CreateTurn(
		ctx, fixture.principal, fixture.sessionID, CreateTurnInput{InputText: "archive me"}, "archive-turn", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	days := 7
	if err := fixture.db.Save(&persistence.TenantRetentionPolicy{
		TenantID: fixture.tenantID, WorkspaceCleanupAfterDays: &days, UpdatedBy: fixture.principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	archivedAt := time.Now().UTC()
	if _, err := fixture.service.Archive(
		ctx, fixture.principal, fixture.sessionID, "archive-policy", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	var workspace persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.RetentionUntil == nil || workspace.CurrentMaterializationID == nil {
		t.Fatalf("archive omitted Workspace retention metadata: %#v", workspace)
	}
	expected := archivedAt.AddDate(0, 0, days)
	if workspace.RetentionUntil.Before(expected.Add(-time.Minute)) || workspace.RetentionUntil.After(expected.Add(time.Minute)) {
		t.Fatalf("unexpected Workspace retention deadline: got=%s expected~=%s", workspace.RetentionUntil, expected)
	}
	var materialization persistence.WorkspaceMaterialization
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, *workspace.CurrentMaterializationID).
		Take(&materialization).Error; err != nil {
		t.Fatal(err)
	}
	if materialization.CleanupReason == nil || *materialization.CleanupReason != "session-archive" ||
		materialization.CleanupRequestedAt == nil || !materialization.CleanupRequestedAt.Equal(*workspace.RetentionUntil) {
		t.Fatalf("archive cleanup intent does not match retention deadline: workspace=%#v materialization=%#v", workspace, materialization)
	}
}

func loadSessionExecution(t *testing.T, fixture tenantExecutionPolicyFixture, inputText string) persistence.AgentExecution {
	t.Helper()
	var execution persistence.AgentExecution
	if err := fixture.db.Table("agent_executions AS execution").
		Joins("JOIN agent_turns AS turn ON turn.tenant_id = execution.tenant_id AND turn.id = execution.turn_id").
		Where("execution.tenant_id = ? AND execution.session_id = ? AND turn.input_text = ?", fixture.tenantID, fixture.sessionID, inputText).
		Select("execution.*").Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	return execution
}

func completeSessionExecutionForNextTurn(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	execution persistence.AgentExecution,
) {
	t.Helper()
	now := time.Now().UTC()
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, execution.ID).
			Updates(map[string]any{"status": "completed", "finished_at": now, "worker_id": nil}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND session_id = ? AND id = ?", fixture.tenantID, fixture.sessionID, execution.TurnID).
			Updates(map[string]any{"status": "completed", "completed_at": now}).Error
	}); err != nil {
		t.Fatal(err)
	}
}

func configureSessionKubernetesTarget(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
) persistence.ExecutionTarget {
	t.Helper()
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
		Kind: "kubernetes", Name: "workspace-pod-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: enabledProviderPolicyTestCapabilities(),
	}
	if err := fixture.db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Update("execution_target_id", target.ID).Error; err != nil {
		t.Fatal(err)
	}
	return target
}

func enabledProviderPolicyTestCapabilities() map[string]any {
	return map[string]any{
		"providerPolicy": map[string]any{
			"experimentalProviders": []string{"codex", "claudeAgent"},
		},
	}
}

func seedCurrentPodMaterialization(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	target persistence.ExecutionTarget,
	layoutVersion int,
	workspaceState string,
) (persistence.RemoteWorkspace, persistence.WorkspaceMaterialization) {
	t.Helper()
	now := time.Now().UTC()
	workspace := persistence.RemoteWorkspace{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		ProjectID: fixture.projectID, SessionID: fixture.sessionID, ExecutionTargetID: target.ID,
		WorkspaceMode: "empty", State: workspaceState, DefaultBranch: "main", CreatedAt: now, UpdatedAt: now,
	}
	materialization := persistence.WorkspaceMaterialization{
		ID: uuid.New(), TenantID: fixture.tenantID, WorkspaceID: workspace.ID,
		OrganizationID: fixture.organizationID, ProjectID: fixture.projectID, SessionID: fixture.sessionID,
		ExecutionTargetID: target.ID, TargetKind: target.Kind, StorageScope: "pod",
		LayoutVersion: layoutVersion, IncarnationID: uuid.New(), State: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&workspace).Error; err != nil {
			return err
		}
		if err := tx.Create(&materialization).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, workspace.ID).
			Update("current_materialization_id", materialization.ID).Error
	}); err != nil {
		t.Fatal(err)
	}
	workspace.CurrentMaterializationID = &materialization.ID
	return workspace, materialization
}
