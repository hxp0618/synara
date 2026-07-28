package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresExecutionKubernetesAllocationIdentityAndLifecycleFencing(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "kubernetes-allocation-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID, sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, model := range []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Kubernetes allocation", DefaultBranch: "main", Visibility: "private", CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now},
		&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Kubernetes allocation", Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID, ProviderResumeCursorState: "absent", ResourceState: "provisioning", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30, WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now},
		&persistence.AgentTurn{ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "queued", InputText: "allocation", TurnKind: "message", RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now},
		&persistence.AgentExecution{ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "queued", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", RequestedBy: domain.UserID, QueuedAt: now},
		&persistence.ExecutionGenerationFact{TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1, SessionID: sessionID, TurnID: turnID, ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", Provider: "codex", RecoveryReason: "initial-claim", WarmPoolMode: "disabled", WarmPoolResult: "not-requested", DispatchRequestedAt: &now, CreatedAt: now, UpdatedAt: now},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	allocation := persistence.ExecutionKubernetesAllocation{
		TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1,
		ExecutionTargetID: domain.ExecutionTargetID, Backend: "sandbox-operator-standard",
		Namespace: "synara-workers", SandboxTemplateName: "synara-worker", SandboxWarmPoolName: "synara-interactive",
		ConfigurationDigest: strings.Repeat("a", 64), ClaimName: "synara-claim-test", Status: "materializing",
		MaterializationStartedAt: now, LastObservedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	boundAt := now.Add(time.Second)
	if err := db.Model(&allocation).Updates(map[string]any{
		"claim_uid": "claim-uid", "sandbox_name": "sandbox-one", "sandbox_uid": "sandbox-uid",
		"pod_name": "pod-one", "pod_uid": "pod-uid", "status": "bound", "bound_at": boundAt,
		"claim_ready_at":   boundAt,
		"last_observed_at": boundAt, "updated_at": boundAt,
	}).Error; err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	if err := db.Model(&allocation).Update("claim_uid", "replacement-uid").Error; err == nil {
		t.Fatal("bound Claim UID mutation unexpectedly succeeded")
	}
	deleteAt := boundAt.Add(time.Second)
	if err := db.Model(&allocation).Updates(map[string]any{
		"status": "deleting", "delete_requested_at": deleteAt,
		"last_observed_at": deleteAt, "updated_at": deleteAt,
	}).Error; err != nil {
		t.Fatalf("request deletion: %v", err)
	}
	deletedAt := deleteAt.Add(time.Second)
	if err := db.Model(&allocation).Updates(map[string]any{
		"status": "deleted", "deleted_at": deletedAt,
		"last_observed_at": deletedAt, "updated_at": deletedAt,
	}).Error; err != nil {
		t.Fatalf("finish deletion: %v", err)
	}
	if err := db.Model(&allocation).Update("status", "bound").Error; err == nil {
		t.Fatal("deleted allocation revival unexpectedly succeeded")
	}
	if err := db.Delete(&allocation).Error; err == nil {
		t.Fatal("allocation history deletion unexpectedly succeeded")
	}
}
