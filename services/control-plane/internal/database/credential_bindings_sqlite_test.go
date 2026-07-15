package database

import (
	"bytes"
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

func TestSQLiteCredentialBindingsEnforceOwnerPurposeAndGenerationFencing(t *testing.T) {
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
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-credential-binding-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"uq_credential_bindings_active_project_selector",
		"uq_credential_bindings_active_worker_image_target",
		"idx_credential_bindings_project_fk",
		"idx_credential_bindings_execution_target_fk",
		"idx_credential_bindings_created_by_fk",
		"idx_credential_bindings_disabled_by_fk",
		"trg_credential_bindings_validate_insert",
		"trg_credential_bindings_immutable_update",
		"trg_credential_bindings_no_delete",
		"trg_execution_credential_grants_validate_insert",
		"trg_execution_credential_grants_no_update",
		"trg_execution_credential_grants_no_delete",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite Credential Binding safety object %s count = %d, want 1", name, count)
		}
	}
	for _, name := range []string{
		"uq_credential_bindings_active_target_selector",
		"idx_credential_bindings_target_lookup",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("redundant SQLite Credential Binding index %s still exists", name)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	projectID := uuid.New()
	repositoryURL := "https://github.com/synara-ai/private.git"
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Private project", RepositoryURL: &repositoryURL, DefaultBranch: "main",
		Visibility: "private", CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	credential := sqliteWorkspaceCredential(
		domain.TenantID, domain.OrganizationID, domain.UserID, "git", "git", "https_token", now,
	)
	if err := store.DB().Create(&credential).Error; err != nil {
		t.Fatalf("create Git Credential: %v", err)
	}
	binding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: &domain.OrganizationID,
		ProjectID: &projectID, CredentialID: credential.ID, BindingKind: "git_fetch",
		SelectorValue: repositoryURL, CreatedBy: domain.UserID, CreatedAt: now,
	}
	if err := store.DB().Create(&binding).Error; err != nil {
		t.Fatalf("create valid Credential Binding: %v", err)
	}
	duplicate := binding
	duplicate.ID = uuid.New()
	if err := store.DB().Create(&duplicate).Error; err == nil {
		t.Fatal("SQLite accepted a duplicate active Project Credential Binding")
	}

	wrongPurpose := sqliteWorkspaceCredential(
		domain.TenantID, domain.OrganizationID, domain.UserID, "registry", "oci", "bearer_token", now,
	)
	if err := store.DB().Create(&wrongPurpose).Error; err != nil {
		t.Fatalf("create Registry Credential: %v", err)
	}
	workerBinding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: &domain.OrganizationID,
		ExecutionTargetID: &domain.ExecutionTargetID, CredentialID: wrongPurpose.ID,
		BindingKind: "worker_image_pull", SelectorValue: "registry.example.com",
		CreatedBy: domain.UserID, CreatedAt: now,
	}
	if err := store.DB().Create(&workerBinding).Error; err != nil {
		t.Fatalf("create Worker image pull Binding: %v", err)
	}
	secondRegistryCredential := sqliteWorkspaceCredential(
		domain.TenantID, domain.OrganizationID, domain.UserID, "registry", "oci", "bearer_token", now,
	)
	if err := store.DB().Create(&secondRegistryCredential).Error; err != nil {
		t.Fatalf("create second Registry Credential: %v", err)
	}
	secondWorkerBinding := workerBinding
	secondWorkerBinding.ID = uuid.New()
	secondWorkerBinding.CredentialID = secondRegistryCredential.ID
	secondWorkerBinding.SelectorValue = "registry-2.example.com"
	if err := store.DB().Create(&secondWorkerBinding).Error; err == nil {
		t.Fatal("SQLite accepted multiple active Worker image pull Bindings for one Execution Target")
	}
	wrongBinding := binding
	wrongBinding.ID = uuid.New()
	wrongBinding.CredentialID = wrongPurpose.ID
	wrongBinding.SelectorValue = "https://github.com/synara-ai/other.git"
	if err := store.DB().Create(&wrongBinding).Error; err == nil {
		t.Fatal("SQLite accepted a Credential Binding kind/purpose mismatch")
	}

	sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New()
	if err := store.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Grant Session", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentTurn{
		ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
		Status: "queued", InputText: "grant", TurnKind: "message", RuntimeMode: "full-access",
		InteractionMode: "default", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentExecution{
		ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: domain.ExecutionTargetID,
		TargetKind: "local", Generation: 1, RequestedBy: domain.UserID, QueuedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	grant := persistence.ExecutionCredentialGrant{
		ID: uuid.New(), TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1,
		BindingID: binding.ID, CredentialID: credential.ID, CredentialVersion: credential.Version,
		CreatedAt: now,
	}
	if err := store.DB().Create(&grant).Error; err != nil {
		t.Fatalf("create valid Execution Credential Grant: %v", err)
	}
	stale := grant
	stale.ID = uuid.New()
	stale.Generation = 2
	if err := store.DB().Create(&stale).Error; err == nil {
		t.Fatal("SQLite accepted a stale Execution Credential Grant generation")
	}
	if err := store.DB().Model(&persistence.ExecutionCredentialGrant{}).
		Where("id = ?", grant.ID).Update("credential_version", 2).Error; err == nil {
		t.Fatal("SQLite allowed an Execution Credential Grant to mutate")
	}
	if err := store.DB().Delete(&persistence.ExecutionCredentialGrant{}, "id = ?", grant.ID).Error; err == nil {
		t.Fatal("SQLite allowed an Execution Credential Grant to be deleted")
	}

	disabledAt := now.Add(time.Second)
	if err := store.DB().Model(&persistence.CredentialBinding{}).Where("id = ?", binding.ID).
		Updates(map[string]any{"disabled_at": disabledAt, "disabled_by": domain.UserID}).Error; err != nil {
		t.Fatalf("disable Credential Binding: %v", err)
	}
	if err := store.DB().Model(&persistence.CredentialBinding{}).Where("id = ?", binding.ID).
		Update("selector_value", "https://github.com/synara-ai/changed.git").Error; err == nil {
		t.Fatal("SQLite allowed a disabled Credential Binding to mutate")
	}
	if err := store.DB().Delete(&persistence.CredentialBinding{}, "id = ?", binding.ID).Error; err == nil {
		t.Fatal("SQLite allowed Credential Binding history to be deleted")
	}
}

func sqliteWorkspaceCredential(
	tenantID, organizationID, actorID uuid.UUID,
	purpose, provider, credentialType string,
	now time.Time,
) persistence.ProviderCredential {
	return persistence.ProviderCredential{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: &organizationID, Scope: "organization",
		Name: "SQLite Workspace Credential", Purpose: purpose, Provider: provider,
		CredentialType: credentialType, EncryptedPayload: bytes.Repeat([]byte{0x61}, 32),
		EncryptedDataKey: bytes.Repeat([]byte{0x62}, 32), KMSProvider: "local", KMSKeyID: "test",
		AADVersion: 3, Version: 1, CreatedBy: actorID, UpdatedBy: actorID, CreatedAt: now, UpdatedAt: now,
	}
}
