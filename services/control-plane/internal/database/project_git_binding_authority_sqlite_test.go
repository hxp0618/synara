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
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteProjectGitBindingAuthorityBackfillsClearsAndFencesLegacyColumn(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.DB().WithContext(ctx).AutoMigrate(persistence.AllModels()...); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-project-git-authority-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	credential := sqliteWorkspaceCredential(
		domain.TenantID, domain.OrganizationID, domain.UserID, "git", "git", "https_token", now,
	)
	if err := store.DB().Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	repositoryURL := "https://git.example.com/team/legacy.git"
	project := persistence.Project{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Legacy Project Git Credential", RepositoryURL: &repositoryURL, DefaultBranch: "main",
		GitCredentialID: &credential.ID, Visibility: "organization", CreatedBy: domain.UserID,
	}
	if err := store.DB().Create(&project).Error; err != nil {
		t.Fatalf("seed legacy Project Git Credential: %v", err)
	}

	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var migrated persistence.Project
	if err := store.DB().Where("tenant_id = ? AND id = ?", domain.TenantID, project.ID).Take(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if migrated.GitCredentialID != nil {
		t.Fatalf("legacy Project Git Credential column was not cleared: %#v", migrated.GitCredentialID)
	}
	var bindings []persistence.CredentialBinding
	if err := store.DB().Where(
		"tenant_id = ? AND project_id = ? AND binding_kind = ? AND disabled_at IS NULL",
		domain.TenantID, project.ID, "git_fetch",
	).Find(&bindings).Error; err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].CredentialID != credential.ID || bindings[0].SelectorValue != repositoryURL {
		t.Fatalf("unexpected backfilled Project Git Binding: %#v", bindings)
	}

	for _, trigger := range []string{
		"trg_projects_legacy_git_credential_insert",
		"trg_projects_legacy_git_credential_update",
		"trg_projects_repository_git_binding_selector",
		"trg_credential_bindings_git_selector_insert",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, trigger).
			Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite Project Git authority trigger %s count = %d, want 1", trigger, count)
		}
	}

	if err := store.DB().Model(&persistence.Project{}).
		Where("tenant_id = ? AND id = ?", domain.TenantID, project.ID).
		Update("git_credential_id", credential.ID).Error; err == nil {
		t.Fatal("SQLite accepted a non-NULL write to retired projects.git_credential_id")
	}
	changedRepository := "https://git.example.com/team/changed.git"
	if err := store.DB().Model(&persistence.Project{}).
		Where("tenant_id = ? AND id = ?", domain.TenantID, project.ID).
		Update("repository_url", changedRepository).Error; err == nil {
		t.Fatal("SQLite accepted Project repository selector drift")
	}
	invalidBinding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: &domain.OrganizationID,
		ProjectID: &project.ID, CredentialID: credential.ID, BindingKind: "git_push",
		SelectorValue: changedRepository, CreatedBy: domain.UserID, CreatedAt: now,
	}
	if err := store.DB().Create(&invalidBinding).Error; err == nil {
		t.Fatal("SQLite accepted a Project Git Binding selector mismatch")
	}

	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatalf("repeat SQLite Project Git authority migration: %v", err)
	}
	var bindingCount int64
	if err := store.DB().Model(&persistence.CredentialBinding{}).
		Where("tenant_id = ? AND project_id = ? AND binding_kind = ?", domain.TenantID, project.ID, "git_fetch").
		Count(&bindingCount).Error; err != nil {
		t.Fatal(err)
	}
	if bindingCount != 1 {
		t.Fatalf("repeat migration produced %d Project Git Bindings, want 1", bindingCount)
	}
}
