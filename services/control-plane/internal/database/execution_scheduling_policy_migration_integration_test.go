package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresExecutionSchedulingPolicyCASAndImmutableRevision(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "scheduling-policy-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	document := schedulingpolicy.UnrestrictedDocument()
	document.Provider = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"codex"}}
	services := []*schedulingpolicy.Service{schedulingpolicy.NewService(db), schedulingpolicy.NewService(db)}
	errorsByUpdate := make(chan error, len(services))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for _, service := range services {
		wait.Add(1)
		go func(service *schedulingpolicy.Service) {
			defer wait.Done()
			<-start
			_, updateErr := service.UpdateTenant(ctx, domain.TenantID, schedulingpolicy.UpdateInput{ExpectedVersion: 0, Document: document, ActorID: domain.UserID})
			errorsByUpdate <- updateErr
		}(service)
	}
	close(start)
	wait.Wait()
	close(errorsByUpdate)
	succeeded, conflicted := 0, 0
	for updateErr := range errorsByUpdate {
		switch {
		case updateErr == nil:
			succeeded++
		case errors.Is(updateErr, schedulingpolicy.ErrVersionConflict):
			conflicted++
		default:
			t.Fatalf("unexpected concurrent update error: %v", updateErr)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent outcomes: succeeded=%d conflicted=%d", succeeded, conflicted)
	}

	var revisions []persistence.ExecutionSchedulingPolicyRevision
	if err := db.Where("tenant_id = ?", domain.TenantID).Find(&revisions).Error; err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 {
		t.Fatalf("CAS loser leaked immutable revision: %d", len(revisions))
	}
	if err := db.Model(&persistence.ExecutionSchedulingPolicyRevision{}).Where("id = ?", revisions[0].ID).Update("deny_all", true).Error; err == nil {
		t.Fatal("PostgreSQL allowed immutable Revision update")
	}

	snapshot, err := schedulingpolicy.NewService(db).GetTenant(ctx, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	projectID, sessionID, turnID := uuid.New(), uuid.New(), uuid.New()
	for _, model := range []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Policy snapshot", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now},
		&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Policy snapshot", Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID, CreatedAt: now, UpdatedAt: now},
		&persistence.AgentTurn{ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "queued", InputText: "policy snapshot", TurnKind: "message", RuntimeMode: "approval-required", InteractionMode: "default", CreatedAt: now},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	baseExecution := map[string]any{
		"tenant_id": domain.TenantID, "session_id": sessionID, "turn_id": turnID,
		"attempt": 1, "status": "queued", "execution_target_id": domain.ExecutionTargetID,
		"target_kind": "local", "requested_by": domain.UserID, "queued_at": now,
	}
	baseExecution["id"] = uuid.New()
	if err := db.Table("agent_executions").Create(baseExecution).Error; err == nil {
		t.Fatal("PostgreSQL accepted v0 defaults under a restricted Tenant Policy")
	}
	baseExecution["id"] = uuid.New()
	baseExecution["tenant_scheduling_policy_version"] = snapshot.Tenant.Version
	baseExecution["tenant_scheduling_policy_digest"] = snapshot.Tenant.Digest
	baseExecution["organization_scheduling_policy_version"] = int64(0)
	baseExecution["organization_scheduling_policy_digest"] = schedulingpolicy.UnrestrictedDigest
	baseExecution["placement_region"] = "forged-region"
	if err := db.Table("agent_executions").Create(baseExecution).Error; err == nil {
		t.Fatal("PostgreSQL accepted a forged fixed-Target placement location")
	}
	baseExecution["id"] = uuid.New()
	baseExecution["placement_region"] = ""
	if err := db.Table("agent_executions").Create(baseExecution).Error; err != nil {
		t.Fatalf("exact PostgreSQL Policy snapshot was rejected: %v", err)
	}
}
