package tenancy

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestDeleteTenantSchedulesAllNonCleanedWorkspaceMaterializationsImmediately(t *testing.T) {
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "tenant-cleanup-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	projectID, sessionID, workspaceID := uuid.New(), uuid.New(), uuid.New()
	for _, model := range []any{
		&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Tenant cleanup", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Tenant cleanup", Status: "active",
			Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
		},
		&persistence.RemoteWorkspace{
			ID: workspaceID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, SessionID: sessionID, ExecutionTargetID: domain.ExecutionTargetID,
			WorkspaceMode: "empty", State: "ready", DefaultBranch: "main", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		},
	} {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	activeID, cleanedID := uuid.New(), uuid.New()
	cleanedAt := now.Add(-time.Minute)
	for _, materialization := range []persistence.WorkspaceMaterialization{
		{
			ID: activeID, TenantID: domain.TenantID, WorkspaceID: workspaceID,
			OrganizationID: domain.OrganizationID, ProjectID: projectID, SessionID: sessionID,
			ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", StorageScope: "target",
			LayoutVersion: 3, IncarnationID: uuid.New(), State: "active", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		},
		{
			ID: cleanedID, TenantID: domain.TenantID, WorkspaceID: workspaceID,
			OrganizationID: domain.OrganizationID, ProjectID: projectID, SessionID: sessionID,
			ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", StorageScope: "target",
			LayoutVersion: 3, IncarnationID: uuid.New(), State: "cleaned", CreatedAt: now.Add(-time.Hour),
			UpdatedAt: cleanedAt, CleanedAt: &cleanedAt,
		},
	} {
		if err := store.DB().Create(&materialization).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DB().Model(&persistence.RemoteWorkspace{}).Where("id = ?", workspaceID).
		Update("current_materialization_id", activeID).Error; err != nil {
		t.Fatal(err)
	}
	turnID, executionID := uuid.New(), uuid.New()
	if err := store.DB().Create(&persistence.AgentTurn{
		ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
		Status: "queued", InputText: "Tenant deletion", RuntimeMode: "approval-required", InteractionMode: "default",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentExecution{
		ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
		RemoteWorkspaceID: &workspaceID, WorkspaceMaterializationID: &activeID,
		Generation: 0, RequestedBy: domain.UserID, QueuedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), platformConfig, cipher)
	sessionService := sessions.NewService(store.DB(), projects.NewService(store.DB()), targetService)
	executionService := executions.NewService(
		store.DB(), sessionService, 30*time.Second, 2*time.Minute, time.Hour, cipher, targetService,
	)
	service := NewService(store.DB(), executionService)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	if err := service.DeleteTenant(ctx, principal, domain.TenantID, "tenant-delete", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var active, cleaned persistence.WorkspaceMaterialization
	if err := store.DB().Where("id = ?", activeID).Take(&active).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Where("id = ?", cleanedID).Take(&cleaned).Error; err != nil {
		t.Fatal(err)
	}
	if active.CleanupReason == nil || *active.CleanupReason != "tenant-delete" || active.CleanupRequestedAt == nil ||
		active.CleanupRequestedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("Tenant deletion omitted immediate cleanup intent: %#v", active)
	}
	if cleaned.CleanupReason != nil || cleaned.CleanupRequestedAt != nil || cleaned.State != "cleaned" {
		t.Fatalf("Tenant deletion rewrote an already-cleaned materialization: %#v", cleaned)
	}
	var execution persistence.AgentExecution
	if err := store.DB().Where("id = ?", executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Status != "cancelled" || execution.FinishedAt == nil || execution.WorkerID != nil {
		t.Fatalf("queued Tenant Execution was not cancelled atomically: %#v", execution)
	}
	var turn persistence.AgentTurn
	if err := store.DB().Where("id = ?", turnID).Take(&turn).Error; err != nil {
		t.Fatal(err)
	}
	if turn.Status != "cancelled" || turn.CompletedAt == nil {
		t.Fatalf("queued Tenant Turn was not cancelled atomically: %#v", turn)
	}
	var events, messages int64
	if err := store.DB().Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND execution_id = ? AND event_type = ?", domain.TenantID, executionID, "execution.cancelled").
		Count(&events).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.OutboxMessage{}).
		Where("tenant_id = ? AND topic = ? AND message_key = ?", domain.TenantID, "execution.cancelled", executionID.String()).
		Count(&messages).Error; err != nil {
		t.Fatal(err)
	}
	if events != 1 || messages != 1 {
		t.Fatalf("Tenant cancellation durability evidence: events=%d outbox=%d", events, messages)
	}
}
