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
	var credential persistence.ProviderCredential
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.credentialID).Take(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if credential.Purpose != "provider" {
		t.Fatalf("legacy Provider Credential purpose was not backfilled: %#v", credential)
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

func TestGitCredentialMigrationEnforcesBindingPurposeScopeAndAvailability(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantID := uuid.New(), uuid.New()
	organizationID, otherOrganizationID := uuid.New(), uuid.New()
	targetID := uuid.New()
	models := []any{
		&persistence.User{ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Git migration", Status: "active", EmailVerifiedAt: &now},
		&persistence.Tenant{ID: tenantID, Slug: "git-migration-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10], Name: "Git migration", Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now},
		&persistence.Organization{ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root", Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.Organization{ID: otherOrganizationID, TenantID: tenantID, Slug: "other", Name: "Other", Kind: "team", Status: "active", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.ExecutionTarget{ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID, Kind: "local", Name: "Git migration target", Status: "active", ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}},
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed Git Credential migration state: %v", err)
	}
	providerCredentialID := uuid.New()
	gitCredentialID := uuid.New()
	expiredGitCredentialID := uuid.New()
	revokedGitCredentialID := uuid.New()
	otherOrganizationGitCredentialID := uuid.New()
	expiredAt := now.Add(-time.Minute)
	revokedAt := now
	credentials := []persistence.ProviderCredential{
		migrationCredential(providerCredentialID, tenantID, &organizationID, userID, "provider", "codex", "api_key", now, nil),
		migrationCredential(gitCredentialID, tenantID, &organizationID, userID, "git", "git", "https_token", now, nil),
		migrationCredential(expiredGitCredentialID, tenantID, &organizationID, userID, "git", "git", "https_token", now.Add(-time.Hour), &expiredAt),
		migrationCredential(revokedGitCredentialID, tenantID, &organizationID, userID, "git", "git", "https_token", now, nil),
		migrationCredential(otherOrganizationGitCredentialID, tenantID, &otherOrganizationID, userID, "git", "git", "https_token", now, nil),
	}
	credentials[3].RevokedAt = &revokedAt
	credentials[3].RevokedBy = &userID
	for index := range credentials {
		if err := db.Create(&credentials[index]).Error; err != nil {
			t.Fatal(err)
		}
	}
	repositoryURL := "https://git.example.com/team/private.git"
	wrongPurposeProject := persistence.Project{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID, Name: "Wrong purpose",
		RepositoryURL: &repositoryURL, DefaultBranch: "main", GitCredentialID: &providerCredentialID,
		Visibility: "organization", CreatedBy: userID,
	}
	if err := db.Create(&wrongPurposeProject).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Provider Credential as a Project Git Credential")
	}
	projectID := uuid.New()
	project := persistence.Project{
		ID: projectID, TenantID: tenantID, OrganizationID: organizationID, Name: "Valid Git binding",
		RepositoryURL: &repositoryURL, DefaultBranch: "main", GitCredentialID: &gitCredentialID,
		Visibility: "organization", CreatedBy: userID,
	}
	if err := db.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	invalidSession := persistence.AgentSession{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID,
		CreatedBy: userID, Title: "Wrong purpose", Status: "active", Visibility: "private",
		Provider: "codex", ProviderCredentialID: &gitCredentialID, ExecutionTargetID: targetID,
	}
	if err := db.Create(&invalidSession).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Git Credential as an Agent Session Provider Credential")
	}
	validSession := invalidSession
	validSession.ID = uuid.New()
	validSession.Title = "Valid provider binding"
	validSession.ProviderCredentialID = &providerCredentialID
	if err := db.Create(&validSession).Error; err != nil {
		t.Fatal(err)
	}
	for name, credentialID := range map[string]uuid.UUID{
		"expired":            expiredGitCredentialID,
		"revoked":            revokedGitCredentialID,
		"cross-organization": otherOrganizationGitCredentialID,
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.Model(&persistence.Project{}).Where("tenant_id = ? AND id = ?", tenantID, projectID).
				Update("git_credential_id", credentialID).Error; err == nil {
				t.Fatalf("PostgreSQL accepted %s Git Credential binding", name)
			}
		})
	}
	if err := db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND id = ?", tenantID, gitCredentialID).
		Update("organization_id", otherOrganizationID).Error; err == nil {
		t.Fatal("PostgreSQL allowed Git Credential identity metadata to mutate")
	}
}

func TestCheckpointMigrationUpgradesExistingGitReferenceAndReadyPointers(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL is not configured")
	}
	db, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000023_git_credentials.sql")); err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.RemoteWorkspaceID == nil {
		t.Fatal("000020 did not bind the legacy Execution to a logical Workspace")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	baseCommit, headCommit := strings.Repeat("a", 40), strings.Repeat("b", 40)
	branch := "synara/session-migration"
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: seed.tenantID, WorkspaceID: *execution.RemoteWorkspaceID,
		SessionID: seed.sessionID, TurnID: &seed.turnID, ExecutionID: seed.executionID,
		Generation: execution.Generation, IdempotencyKey: "migration-git-reference",
		Strategy: "git-reference", Status: "pending", BaseCommit: &baseCommit,
		HeadCommit: &headCommit, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-git-reference-v1", "headCommit": headCommit},
		CreatedAt: now,
	}
	if err := db.Create(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	legacySnapshotID := uuid.New()
	legacySnapshotBytes := int64(18)
	legacySnapshotSHA := strings.Repeat("c", 64)
	legacySnapshotContentType := "application/x-tar"
	legacySnapshotReadyAt := now.Add(time.Second)
	legacySnapshot := persistence.Artifact{
		ID: legacySnapshotID, TenantID: seed.tenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: seed.sessionID, ExecutionID: &seed.executionID,
		Kind: "workspace_snapshot", Status: "ready", Bucket: "migration-artifacts",
		ObjectKey: "checkpoint-migration/" + legacySnapshotID.String(), ContentType: &legacySnapshotContentType,
		SizeBytes: &legacySnapshotBytes, SHA256: &legacySnapshotSHA, CreatedByType: "system",
		CreatedByID: seed.tenantID, ReadyAt: &legacySnapshotReadyAt, CreatedAt: now,
	}
	legacySnapshotCheckpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: seed.tenantID, WorkspaceID: *execution.RemoteWorkspaceID,
		SessionID: seed.sessionID, TurnID: &seed.turnID, ExecutionID: seed.executionID,
		Generation: execution.Generation, IdempotencyKey: "migration-snapshot-reference",
		Strategy: "snapshot", Status: "ready", ArtifactID: &legacySnapshotID,
		Manifest: map[string]any{"format": "synara-workspace-snapshot-v1"}, FileCount: pointerInt(1),
		TotalBytes: &legacySnapshotBytes, SHA256: &legacySnapshotSHA, CreatedAt: now,
		ReadyAt: &legacySnapshotReadyAt,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("WorkspaceCheckpointID", "UploadCleanupAt").Create(&legacySnapshot).Error; err != nil {
			return err
		}
		return tx.Create(&legacySnapshotCheckpoint).Error
	}); err != nil {
		t.Fatalf("seed pre-000025 ready Checkpoint Artifact: %v", err)
	}
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var migratedSnapshot persistence.Artifact
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, legacySnapshotID).Take(&migratedSnapshot).Error; err != nil {
		t.Fatal(err)
	}
	if migratedSnapshot.WorkspaceCheckpointID == nil || *migratedSnapshot.WorkspaceCheckpointID != legacySnapshotCheckpoint.ID {
		t.Fatalf("000025 did not backfill the reverse Checkpoint binding: %#v", migratedSnapshot)
	}
	readyAt := now.Add(time.Second)
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", checkpoint.TenantID, checkpoint.ID).
			Updates(map[string]any{"status": "ready", "ready_at": readyAt}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", checkpoint.TenantID, checkpoint.WorkspaceID).
			Update("current_checkpoint_id", checkpoint.ID).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", checkpoint.TenantID, checkpoint.ExecutionID).
			Update("restore_checkpoint_id", checkpoint.ID).Error
	}); err != nil {
		t.Fatalf("000024 did not allow an Artifact-free ready Git-reference Checkpoint: %v", err)
	}
	pending := checkpoint
	pending.ID = uuid.New()
	pending.IdempotencyKey = "migration-pending-reference"
	pending.Status = "pending"
	pending.ReadyAt = nil
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", checkpoint.TenantID, checkpoint.WorkspaceID).
			Update("current_checkpoint_id", pending.ID).Error
	}); err == nil {
		t.Fatal("000024 allowed current_checkpoint_id to reference a pending Checkpoint")
	}
	failedAt := readyAt.Add(time.Second)
	if err := db.Model(&persistence.WorkspaceCheckpoint{}).
		Where("tenant_id = ? AND id = ?", pending.TenantID, pending.ID).
		Updates(map[string]any{
			"status": "failed", "failure_code": "migration_test_complete",
			"failure_message": "release the active Checkpoint slot", "failed_at": failedAt,
		}).Error; err != nil {
		t.Fatal(err)
	}

	boundCheckpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: seed.tenantID, WorkspaceID: *execution.RemoteWorkspaceID,
		SessionID: seed.sessionID, TurnID: &seed.turnID, ExecutionID: seed.executionID,
		Generation: execution.Generation, IdempotencyKey: "migration-reverse-binding-enforcement",
		Strategy: "snapshot", Status: "pending", Manifest: map[string]any{"format": "synara-workspace-snapshot-v1"},
		FileCount: pointerInt(1), TotalBytes: &legacySnapshotBytes, CreatedAt: failedAt,
	}
	if err := db.Create(&boundCheckpoint).Error; err != nil {
		t.Fatal(err)
	}
	unboundArtifactID := uuid.New()
	unboundArtifact := legacySnapshot
	unboundArtifact.ID = unboundArtifactID
	unboundArtifact.ObjectKey = "checkpoint-migration/" + unboundArtifactID.String()
	unboundArtifact.WorkspaceCheckpointID = nil
	if err := db.Create(&unboundArtifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", boundCheckpoint.TenantID, boundCheckpoint.ID).
			Updates(map[string]any{
				"status": "ready", "artifact_id": unboundArtifactID,
				"sha256": legacySnapshotSHA, "ready_at": failedAt.Add(time.Second),
			}).Error
	}); err == nil {
		t.Fatal("000025 allowed a ready Checkpoint to use an Artifact without the reverse binding")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.Artifact{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, unboundArtifactID).
			Update("workspace_checkpoint_id", boundCheckpoint.ID).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", boundCheckpoint.TenantID, boundCheckpoint.ID).
			Updates(map[string]any{
				"status": "ready", "artifact_id": unboundArtifactID,
				"sha256": legacySnapshotSHA, "ready_at": failedAt.Add(time.Second),
			}).Error
	}); err != nil {
		t.Fatalf("000025 rejected a correctly reverse-bound ready Checkpoint Artifact: %v", err)
	}
	if err := db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, unboundArtifactID).
		Update("workspace_checkpoint_id", nil).Error; err == nil {
		t.Fatal("000025 allowed a ready Checkpoint Artifact reverse binding to be removed")
	}

	scopeCheckpoint := boundCheckpoint
	scopeCheckpoint.ID = uuid.New()
	scopeCheckpoint.IdempotencyKey = "migration-artifact-scope"
	scopeCheckpoint.Status = "pending"
	scopeCheckpoint.ArtifactID = nil
	scopeCheckpoint.SHA256 = nil
	scopeCheckpoint.ReadyAt = nil
	scopeCheckpoint.CreatedAt = failedAt.Add(2 * time.Second)
	if err := db.Create(&scopeCheckpoint).Error; err != nil {
		t.Fatal(err)
	}
	wrongKind := legacySnapshot
	wrongKind.ID = uuid.New()
	wrongKind.ObjectKey = "checkpoint-migration/" + wrongKind.ID.String()
	wrongKind.Kind = "generated_file"
	wrongKind.Status = "pending"
	wrongKind.ContentType = nil
	wrongKind.SizeBytes = nil
	wrongKind.SHA256 = nil
	wrongKind.ReadyAt = nil
	wrongKind.WorkspaceCheckpointID = &scopeCheckpoint.ID
	if err := db.Create(&wrongKind).Error; err == nil {
		t.Fatal("000025 allowed a non-Checkpoint Artifact kind to bind a Workspace Checkpoint")
	}

	otherTurnID, otherExecutionID := uuid.New(), uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.AgentTurn{
			ID: otherTurnID, TenantID: seed.tenantID, SessionID: seed.sessionID,
			CreatedBy: session.CreatedBy, Status: "queued", InputText: "Scope test", CreatedAt: failedAt,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.AgentExecution{
			ID: otherExecutionID, TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: otherTurnID,
			Attempt: 1, Status: "queued", ExecutionTargetID: execution.ExecutionTargetID,
			TargetKind: execution.TargetKind, RequestedBy: session.CreatedBy, QueuedAt: failedAt,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	wrongExecution := legacySnapshot
	wrongExecution.ID = uuid.New()
	wrongExecution.ObjectKey = "checkpoint-migration/" + wrongExecution.ID.String()
	wrongExecution.Status = "pending"
	wrongExecution.ContentType = nil
	wrongExecution.SizeBytes = nil
	wrongExecution.SHA256 = nil
	wrongExecution.ReadyAt = nil
	wrongExecution.ExecutionID = &otherExecutionID
	wrongExecution.WorkspaceCheckpointID = &scopeCheckpoint.ID
	if err := db.Create(&wrongExecution).Error; err == nil {
		t.Fatal("000025 allowed a Checkpoint Artifact from another Execution")
	}

	otherSession := session
	otherSession.ID = uuid.New()
	otherSession.Title = "Checkpoint Artifact scope"
	otherSession.CurrentRuntimeBindingID = nil
	otherSession.ProviderResumeCursorEncrypted = nil
	otherSession.LastEventSequence = 0
	otherSession.CreatedAt = failedAt
	otherSession.UpdatedAt = failedAt
	if err := db.Create(&otherSession).Error; err != nil {
		t.Fatal(err)
	}
	wrongSession := legacySnapshot
	wrongSession.ID = uuid.New()
	wrongSession.ObjectKey = "checkpoint-migration/" + wrongSession.ID.String()
	wrongSession.Status = "pending"
	wrongSession.ContentType = nil
	wrongSession.SizeBytes = nil
	wrongSession.SHA256 = nil
	wrongSession.ReadyAt = nil
	wrongSession.SessionID = otherSession.ID
	wrongSession.WorkspaceCheckpointID = &scopeCheckpoint.ID
	if err := db.Create(&wrongSession).Error; err == nil {
		t.Fatal("000025 allowed a Checkpoint Artifact from another Session")
	}

	validPending := legacySnapshot
	validPending.ID = uuid.New()
	validPending.ObjectKey = "checkpoint-migration/" + validPending.ID.String()
	validPending.Status = "pending"
	validPending.ContentType = nil
	validPending.SizeBytes = nil
	validPending.SHA256 = nil
	validPending.ReadyAt = nil
	validPending.WorkspaceCheckpointID = &scopeCheckpoint.ID
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&validPending).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", scopeCheckpoint.TenantID, scopeCheckpoint.ID).
			Updates(map[string]any{"status": "uploading", "artifact_id": validPending.ID}).Error
	}); err != nil {
		t.Fatalf("000025 rejected a valid pending Checkpoint Artifact binding: %v", err)
	}
	duplicateBinding := validPending
	duplicateBinding.ID = uuid.New()
	duplicateBinding.ObjectKey = "checkpoint-migration/" + duplicateBinding.ID.String()
	if err := db.Create(&duplicateBinding).Error; err == nil {
		t.Fatal("000025 allowed multiple Artifacts to bind the same Workspace Checkpoint")
	}
	for name, updates := range map[string]map[string]any{
		"reverse binding removal": {"workspace_checkpoint_id": nil},
		"kind change":             {"kind": "checkpoint"},
		"execution change":        {"execution_id": otherExecutionID},
		"session change":          {"session_id": otherSession.ID},
		"premature failure":       {"status": "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.Model(&persistence.Artifact{}).
				Where("tenant_id = ? AND id = ?", seed.tenantID, validPending.ID).
				Updates(updates).Error; err == nil {
				t.Fatalf("000025 allowed active Checkpoint Artifact mutation: %#v", updates)
			}
		})
	}
	activeFailureAt := failedAt.Add(3 * time.Second)
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", scopeCheckpoint.TenantID, scopeCheckpoint.ID).
			Updates(map[string]any{
				"status": "failed", "failure_code": "migration_test_failed",
				"failure_message": "active binding failure transaction", "failed_at": activeFailureAt,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.Artifact{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, validPending.ID).
			Update("status", "failed").Error
	}); err != nil {
		t.Fatalf("000025 rejected an atomic Checkpoint and Artifact failure: %v", err)
	}

	var foreignKeyDefinition string
	if err := db.Raw(`
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'artifacts'::regclass
		  AND conname = 'fk_artifacts_workspace_checkpoint'
	`).Scan(&foreignKeyDefinition).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(foreignKeyDefinition, "FOREIGN KEY (workspace_checkpoint_id)") ||
		strings.Contains(foreignKeyDefinition, "tenant_id") {
		t.Fatalf("000025 Checkpoint Artifact foreign key should reuse the global Checkpoint primary key: %q", foreignKeyDefinition)
	}
	for tableName, constraintName := range map[string]string{
		"provider_runtime_bindings": "fk_provider_runtime_bindings_last_execution",
		"remote_workspaces":         "fk_remote_workspaces_last_execution",
	} {
		var definition string
		if err := db.Raw(`
			SELECT pg_get_constraintdef(oid)
			FROM pg_constraint
			WHERE conrelid = ?::regclass AND conname = ?
		`, tableName, constraintName).Scan(&definition).Error; err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(definition, "ON DELETE SET NULL (last_execution_id)") {
			t.Fatalf("000026 %s tenant-preserving Execution cleanup foreign key is invalid: %q", tableName, definition)
		}
	}
}

func TestCheckpointMigrationRejectsInvalidLegacyReadyArtifactKind(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000023_git_credentials.sql")); err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if execution.RemoteWorkspaceID == nil {
		t.Fatal("000020 did not bind the legacy Execution to a logical Workspace")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	artifactID := uuid.New()
	sizeBytes := int64(18)
	sha := strings.Repeat("d", 64)
	contentType := "application/x-tar"
	readyAt := now.Add(time.Second)
	baseCommit := strings.Repeat("a", 40)
	branch := "main"
	artifact := persistence.Artifact{
		ID: artifactID, TenantID: seed.tenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: seed.sessionID, ExecutionID: &seed.executionID,
		Kind: "workspace_snapshot", Status: "ready", Bucket: "invalid-checkpoint-migration",
		ObjectKey: "invalid-checkpoint-migration/" + artifactID.String(), ContentType: &contentType,
		SizeBytes: &sizeBytes, SHA256: &sha, CreatedByType: "system", CreatedByID: seed.tenantID,
		ReadyAt: &readyAt, CreatedAt: now,
	}
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: seed.tenantID, WorkspaceID: *execution.RemoteWorkspaceID,
		SessionID: seed.sessionID, TurnID: &seed.turnID, ExecutionID: seed.executionID,
		Generation: execution.Generation, IdempotencyKey: "invalid-legacy-ready-kind",
		Strategy: "patch", Status: "ready", BaseCommit: &baseCommit, HeadCommit: &baseCommit,
		CurrentBranch: &branch, ArtifactID: &artifactID, Manifest: map[string]any{"format": "synara-workspace-patch-v1"},
		FileCount: pointerInt(1), TotalBytes: &sizeBytes, SHA256: &sha, CreatedAt: now, ReadyAt: &readyAt,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("WorkspaceCheckpointID", "UploadCleanupAt").Create(&artifact).Error; err != nil {
			return err
		}
		return tx.Create(&checkpoint).Error
	}); err != nil {
		t.Fatalf("seed legacy kind-mismatched ready Checkpoint: %v", err)
	}
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000024_checkpoint_lifecycle.sql")); err != nil {
		t.Fatalf("000024 should preserve the legacy row for 000025 validation: %v", err)
	}
	err := Migrate(context.Background(), db, migrations.Files)
	if err == nil {
		t.Fatalf("000025 did not reject the invalid legacy ready Checkpoint: %v", err)
	}
	var applied int64
	if countErr := db.Table("control_plane_schema_migrations").Where("version = ?", 25).Count(&applied).Error; countErr != nil {
		t.Fatal(countErr)
	}
	if applied != 0 {
		t.Fatal("000025 recorded a failed invalid legacy Checkpoint migration as applied")
	}
}

func openIsolatedMigrationSchema(t *testing.T, databaseURL string) *gorm.DB {
	t.Helper()
	admin, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schemaName := "checkpoint_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec(`CREATE SCHEMA "` + schemaName + `"`).Error; err != nil {
		t.Fatal(err)
	}
	options := DefaultOptions()
	options.MaxOpenConnections = 1
	options.MaxIdleConnections = 1
	db, err := Open(context.Background(), databaseURL, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`SET search_path TO "` + schemaName + `"`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec(`DROP SCHEMA IF EXISTS "` + schemaName + `" CASCADE`).Error
		if sqlDB, dbErr := admin.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func pointerInt(value int) *int { return &value }

func migrationCredential(
	id, tenantID uuid.UUID,
	organizationID *uuid.UUID,
	userID uuid.UUID,
	purpose, provider, credentialType string,
	createdAt time.Time,
	expiresAt *time.Time,
) persistence.ProviderCredential {
	return persistence.ProviderCredential{
		ID: id, TenantID: tenantID, OrganizationID: organizationID,
		Name: purpose + " credential", Purpose: purpose, Provider: provider, CredentialType: credentialType,
		EncryptedPayload: []byte("encrypted-credential-payload"), EncryptedDataKey: []byte("encrypted-credential-data-key"),
		KMSProvider: "local", KMSKeyID: "migration", Version: 1,
		CreatedBy: userID, UpdatedBy: userID, CreatedAt: createdAt, UpdatedAt: createdAt, ExpiresAt: expiresAt,
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
	tenantID, sessionID, turnID, executionID, interactionID, credentialID uuid.UUID
}

func seedStage3MigrationState(t *testing.T, db *gorm.DB) stage3MigrationSeed {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantID, organizationID := uuid.New(), uuid.New(), uuid.New()
	projectID, targetID := uuid.New(), uuid.New()
	sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New()
	workerID, interactionID := uuid.New(), uuid.New()
	credentialID := uuid.New()
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
		{&persistence.ProviderCredential{
			ID: credentialID, TenantID: tenantID, OrganizationID: &organizationID,
			Name: "Legacy provider", Provider: "codex", CredentialType: "api_key",
			EncryptedPayload: []byte("encrypted-provider-payload"), EncryptedDataKey: []byte("encrypted-provider-data-key"),
			KMSProvider: "local", KMSKeyID: "migration", Version: 1,
			CreatedBy: userID, UpdatedBy: userID, CreatedAt: now, UpdatedAt: now,
		}, []string{
			"id", "tenant_id", "organization_id", "name", "provider", "credential_type",
			"encrypted_payload", "encrypted_data_key", "kms_provider", "kms_key_id", "version",
			"created_by", "updated_by", "created_at", "updated_at",
		}},
		{&persistence.Project{ID: projectID, TenantID: tenantID, OrganizationID: organizationID, Name: "Migration project", RepositoryURL: &repositoryURL, DefaultBranch: "main", Visibility: "organization", CreatedBy: userID},
			[]string{"id", "tenant_id", "organization_id", "name", "repository_url", "default_branch", "visibility", "created_by", "created_at", "updated_at"}},
		{&persistence.ExecutionTarget{ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID, Kind: "kubernetes", Name: "Migration target", Status: "active", ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}}, nil},
		{&persistence.AgentSession{ID: sessionID, TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID, CreatedBy: userID, Title: "Migration session", Status: "active", Visibility: "private", Provider: "codex", ProviderCredentialID: &credentialID, ExecutionTargetID: targetID, ProviderResumeCursorEncrypted: []byte("encrypted-cursor"), CreatedAt: now, UpdatedAt: now},
			[]string{"id", "tenant_id", "organization_id", "project_id", "created_by", "title", "status", "visibility", "provider", "provider_credential_id", "execution_target_id", "provider_resume_cursor_encrypted", "created_at", "updated_at"}},
		{&persistence.AgentTurn{ID: turnID, TenantID: tenantID, SessionID: sessionID, CreatedBy: userID, Status: "running", InputText: "Continue", StartedAt: &now, CreatedAt: now},
			[]string{"id", "tenant_id", "session_id", "created_by", "status", "input_text", "started_at", "completed_at", "created_at"}},
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
	return stage3MigrationSeed{
		tenantID: tenantID, sessionID: sessionID, turnID: turnID, executionID: executionID,
		interactionID: interactionID, credentialID: credentialID,
	}
}
