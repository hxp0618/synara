package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteExecutionSchedulingPolicySnapshotCannotUseV0DefaultsUnderRestriction(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "policy-snapshot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}

	columns := []string{
		`tenant_scheduling_policy_version INTEGER NOT NULL DEFAULT 0`,
		`tenant_scheduling_policy_digest TEXT NOT NULL DEFAULT '48646d468c45b8a2257c2080fce3ef0697f7ec181ca7e085eb49a194a92a90b2'`,
		`organization_scheduling_policy_version INTEGER NOT NULL DEFAULT 0`,
		`organization_scheduling_policy_digest TEXT NOT NULL DEFAULT '48646d468c45b8a2257c2080fce3ef0697f7ec181ca7e085eb49a194a92a90b2'`,
		`placement_region TEXT NOT NULL DEFAULT ''`,
		`placement_cluster_id TEXT NOT NULL DEFAULT ''`,
	}
	columnNames := []string{"tenant_scheduling_policy_version", "tenant_scheduling_policy_digest", "organization_scheduling_policy_version", "organization_scheduling_policy_digest", "placement_region", "placement_cluster_id"}
	for index, definition := range columns {
		if !store.DB().Migrator().HasColumn("agent_executions", columnNames[index]) {
			if err := store.DB().Exec(`ALTER TABLE agent_executions ADD COLUMN ` + definition).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-policy-snapshot-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	document := schedulingpolicy.UnrestrictedDocument()
	document.Provider = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"codex"}}
	snapshot, err := schedulingpolicy.NewService(store.DB()).UpdateTenant(ctx, domain.TenantID, schedulingpolicy.UpdateInput{ExpectedVersion: 0, Document: document, ActorID: domain.UserID})
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
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	base := map[string]any{"id": uuid.New(), "tenant_id": domain.TenantID, "session_id": sessionID, "turn_id": turnID, "attempt": 1, "status": "queued", "execution_target_id": domain.ExecutionTargetID, "target_kind": "local", "requested_by": domain.UserID, "queued_at": now}
	if err := store.DB().Table("agent_executions").Create(base).Error; err == nil {
		t.Fatal("SQLite accepted v0 defaults under a restricted Tenant Policy")
	}
	base["id"] = uuid.New()
	base["tenant_scheduling_policy_version"] = snapshot.Tenant.Version
	base["tenant_scheduling_policy_digest"] = snapshot.Tenant.Digest
	base["organization_scheduling_policy_version"] = int64(0)
	base["organization_scheduling_policy_digest"] = schedulingpolicy.UnrestrictedDigest
	base["placement_region"] = "forged-region"
	if err := store.DB().Table("agent_executions").Create(base).Error; err == nil {
		t.Fatal("SQLite accepted a forged fixed-Target placement location")
	}
	base["id"] = uuid.New()
	base["placement_region"] = ""
	if err := store.DB().Table("agent_executions").Create(base).Error; err != nil {
		t.Fatalf("exact SQLite Policy snapshot was rejected: %v", err)
	}
}
