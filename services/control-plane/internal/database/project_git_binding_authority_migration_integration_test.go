package database

import (
	"context"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	controlplanemigrations "github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProjectGitBindingAuthorityMigrationBackfillsClearsAndFencesLegacyColumn(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_WORKSPACE_CREDENTIAL_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_WORKSPACE_CREDENTIAL_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(ctx, db, migrationsThrough(t, "000034_worker_revocation_fencing.sql")); err != nil {
		t.Fatal(err)
	}

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	var project persistence.Project
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, session.ProjectID).Take(&project).Error; err != nil {
		t.Fatal(err)
	}
	if project.RepositoryURL == nil {
		t.Fatal("migration fixture Project has no repository URL")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	credential := postgresWorkspaceCredential(
		seed.tenantID, session.OrganizationID, session.CreatedBy, "git", "git", "https_token", now,
	)
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.Project{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, project.ID).
		Update("git_credential_id", credential.ID).Error; err != nil {
		t.Fatalf("seed legacy Project Git Credential write: %v", err)
	}
	if err := Migrate(ctx, db, migrationsThrough(t, "000035_workspace_credential_bindings.sql")); err != nil {
		t.Fatal(err)
	}
	var firstBackfill persistence.CredentialBinding
	if err := db.Where(
		"tenant_id = ? AND project_id = ? AND binding_kind = ? AND disabled_at IS NULL",
		seed.tenantID, project.ID, "git_fetch",
	).Take(&firstBackfill).Error; err != nil {
		t.Fatal(err)
	}
	disabledAt := now.Add(time.Second)
	if err := db.Model(&persistence.CredentialBinding{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, firstBackfill.ID).
		Updates(map[string]any{"disabled_at": disabledAt, "disabled_by": session.CreatedBy}).Error; err != nil {
		t.Fatalf("disable the 000035 legacy backfill: %v", err)
	}

	if err := Migrate(ctx, db, migrationsThrough(t, "000036_project_git_binding_authority.sql")); err != nil {
		t.Fatal(err)
	}
	var migrated persistence.Project
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, project.ID).Take(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if migrated.GitCredentialID != nil {
		t.Fatalf("legacy Project Git Credential column was not cleared: %#v", migrated.GitCredentialID)
	}
	var binding persistence.CredentialBinding
	if err := db.Where(
		"tenant_id = ? AND project_id = ? AND binding_kind = ? AND disabled_at IS NULL",
		seed.tenantID, project.ID, "git_fetch",
	).Take(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if binding.CredentialID != credential.ID || binding.SelectorValue != *project.RepositoryURL {
		t.Fatalf("unexpected backfilled Project Git Binding: %#v", binding)
	}

	if err := db.Model(&persistence.Project{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, project.ID).
		Update("git_credential_id", credential.ID).Error; err == nil {
		t.Fatal("PostgreSQL accepted a non-NULL write to retired projects.git_credential_id")
	}
	changedRepository := "https://git.example.com/team/changed.git"
	if err := db.Model(&persistence.Project{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, project.ID).
		Update("repository_url", changedRepository).Error; err == nil {
		t.Fatal("PostgreSQL accepted Project repository selector drift")
	}
	invalidBinding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: seed.tenantID, OrganizationID: &session.OrganizationID,
		ProjectID: &project.ID, CredentialID: credential.ID, BindingKind: "git_push",
		SelectorValue: changedRepository, CreatedBy: session.CreatedBy, CreatedAt: now,
	}
	if err := db.Create(&invalidBinding).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Project Git Binding selector mismatch")
	}

	for _, trigger := range []string{
		"trg_projects_legacy_git_credential_insert",
		"trg_projects_legacy_git_credential_update",
		"trg_projects_repository_git_binding_selector",
		"trg_credential_bindings_git_selector",
	} {
		var count int64
		if err := db.Raw(`SELECT count(*) FROM pg_trigger WHERE tgname = ? AND NOT tgisinternal`, trigger).
			Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("PostgreSQL Project Git authority trigger %s count = %d, want 1", trigger, count)
		}
	}
	var legacyIndex *string
	if err := db.Raw(`SELECT to_regclass('idx_projects_git_credential')::text`).Scan(&legacyIndex).Error; err != nil {
		t.Fatal(err)
	}
	if legacyIndex != nil {
		t.Fatalf("retired legacy Project Git Credential index still exists: %s", *legacyIndex)
	}

	script, err := fs.ReadFile(controlplanemigrations.Files, "000036_project_git_binding_authority.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(string(script)).Error; err != nil {
		t.Fatalf("repeat Project Git Binding authority DDL: %v", err)
	}
	var bindingCount int64
	if err := db.Model(&persistence.CredentialBinding{}).
		Where(
			"tenant_id = ? AND project_id = ? AND binding_kind = ? AND disabled_at IS NULL",
			seed.tenantID, project.ID, "git_fetch",
		).
		Count(&bindingCount).Error; err != nil {
		t.Fatal(err)
	}
	if bindingCount != 1 {
		t.Fatalf("repeat migration produced %d Project Git Bindings, want 1", bindingCount)
	}
}
