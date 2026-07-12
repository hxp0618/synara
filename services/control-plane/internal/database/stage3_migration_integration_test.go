package database

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestStage3MigrationsBackfillExistingRuntimeState(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	legacyFiles := migrationsThrough(t, "000016_sse_connection_leases.sql")
	if err := Migrate(context.Background(), db, legacyFiles); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.CurrentRuntimeBindingID == nil {
		t.Fatal("Stage 3 migration did not attach a Provider runtime binding")
	}
	var binding persistence.ProviderRuntimeBinding
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, *session.CurrentRuntimeBindingID).Take(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if binding.Provider != "codex" || binding.ResumeStrategy != "native-cursor" || binding.AuthoritativeHistorySequence != 1 {
		t.Fatalf("unexpected backfilled Provider runtime binding: %#v", binding)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Provider == nil || *execution.Provider != "codex" || execution.ProviderRuntimeBindingID == nil ||
		execution.RemoteWorkspaceID == nil {
		t.Fatalf("Stage 3 migration did not bind the existing Execution: %#v", execution)
	}
	var workspace persistence.RemoteWorkspace
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, *execution.RemoteWorkspaceID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.SessionID != seed.sessionID || workspace.WorkspaceMode != "clone" || workspace.DefaultBranch != "main" {
		t.Fatalf("unexpected backfilled remote Workspace: %#v", workspace)
	}
	var interaction persistence.ExecutionInteraction
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.interactionID).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	if interaction.TurnID != seed.turnID || interaction.Provider != "codex" ||
		interaction.ResolutionKind == nil || *interaction.ResolutionKind != "approved" ||
		interaction.DeliveryStatus != "pending" || interaction.DeliveryWorkerID == nil ||
		interaction.DeliveryGeneration == nil || interaction.DeliveryAvailableAt == nil {
		t.Fatalf("unexpected backfilled interaction delivery: %#v", interaction)
	}
}

func migrationsThrough(t *testing.T, maximum string) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	result := fstest.MapFS{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") || name > maximum {
			continue
		}
		payload, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		result[name] = &fstest.MapFile{Data: payload}
	}
	return result
}

type stage3MigrationSeed struct {
	tenantID, sessionID, turnID, executionID, interactionID uuid.UUID
}

func seedStage3MigrationState(t *testing.T, db *gorm.DB) stage3MigrationSeed {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantID, organizationID := uuid.New(), uuid.New(), uuid.New()
	projectID, targetID := uuid.New(), uuid.New()
	sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New()
	workerID, interactionID := uuid.New(), uuid.New()
	repositoryURL := "https://example.com/synara.git"
	resolvedAt := now.Add(time.Minute)
	models := []struct {
		value  any
		fields []string
	}{
		{&persistence.User{ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Migration test", Status: "active", EmailVerifiedAt: &now}, nil},
		{&persistence.Tenant{ID: tenantID, Slug: "migration-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10], Name: "Migration tenant", Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID}, nil},
		{&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now}, nil},
		{&persistence.Organization{ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root", Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID}, nil},
		{&persistence.OrganizationMembership{TenantID: tenantID, OrganizationID: organizationID, UserID: userID, Role: "owner", Status: "active"}, nil},
		{&persistence.Project{ID: projectID, TenantID: tenantID, OrganizationID: organizationID, Name: "Migration project", RepositoryURL: &repositoryURL, DefaultBranch: "main", Visibility: "organization", CreatedBy: userID}, nil},
		{&persistence.ExecutionTarget{ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID, Kind: "kubernetes", Name: "Migration target", Status: "active", ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}}, nil},
		{&persistence.AgentSession{ID: sessionID, TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID, CreatedBy: userID, Title: "Migration session", Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: targetID, ProviderResumeCursorEncrypted: []byte("encrypted-cursor"), CreatedAt: now, UpdatedAt: now},
			[]string{"id", "tenant_id", "organization_id", "project_id", "created_by", "title", "status", "visibility", "provider", "execution_target_id", "provider_resume_cursor_encrypted", "created_at", "updated_at"}},
		{&persistence.AgentTurn{ID: turnID, TenantID: tenantID, SessionID: sessionID, CreatedBy: userID, Status: "running", InputText: "Continue", StartedAt: &now, CreatedAt: now}, nil},
		{&persistence.WorkerInstance{ID: workerID, ExecutionTargetID: targetID, TargetKind: "kubernetes", ClusterID: "migration", Namespace: "default", PodName: "migration-worker", Version: "legacy", ProtocolVersion: 1, Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("migration-worker-token"), Status: "online", RegisteredAt: now, LastHeartbeatAt: now},
			[]string{"id", "execution_target_id", "target_kind", "cluster_id", "namespace", "pod_name", "version", "protocol_version", "capabilities", "lease_supported", "fencing_supported", "auth_token_hash", "status", "registered_at", "last_heartbeat_at"}},
		{&persistence.AgentExecution{ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "waiting-for-approval", ExecutionTargetID: targetID, TargetKind: "kubernetes", WorkerID: &workerID, Generation: 1, RequestedBy: userID, QueuedAt: now, StartedAt: &now},
			[]string{"id", "tenant_id", "session_id", "turn_id", "attempt", "status", "execution_target_id", "target_kind", "worker_id", "generation", "requested_by", "queued_at", "started_at"}},
		{&persistence.WorkerLease{ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID, Generation: 1, LeaseTokenHash: []byte("migration-lease-token"), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Hour)}, nil},
		{&persistence.SessionEvent{TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID, Sequence: 1, EventID: uuid.New(), EventVersion: 1, EventType: "turn.created", ActorType: "user", ActorID: &userID, ExecutionID: &executionID, Payload: map[string]any{"inputText": "Continue"}, OccurredAt: now}, nil},
		{&persistence.ExecutionInteraction{ID: interactionID, TenantID: tenantID, ExecutionID: executionID, SessionID: sessionID, WorkerID: workerID, Generation: 1, RequestID: "approval-migration", Kind: "approval", Status: "resolved", Payload: map[string]any{"summary": "Run"}, Resolution: map[string]any{"decision": "accept"}, RequestedAt: now, ResolvedAt: &resolvedAt, ResolvedBy: &userID},
			[]string{"id", "tenant_id", "execution_id", "session_id", "worker_id", "generation", "request_id", "kind", "status", "payload", "resolution", "requested_at", "resolved_at", "resolved_by"}},
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			query := tx
			if len(model.fields) > 0 {
				query = query.Select(model.fields)
			}
			if err := query.Create(model.value).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed Stage 3 migration state: %v", err)
	}
	return stage3MigrationSeed{tenantID: tenantID, sessionID: sessionID, turnID: turnID, executionID: executionID, interactionID: interactionID}
}
