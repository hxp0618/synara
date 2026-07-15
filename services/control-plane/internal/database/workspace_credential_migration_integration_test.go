package database

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkspaceCredentialMigrationBackfillsGitBindingAndFencesGrants(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	gitCredential := postgresWorkspaceCredential(
		seed.tenantID, session.OrganizationID, session.CreatedBy, "git", "git", "https_token", now,
	)
	if err := db.Create(&gitCredential).Error; err != nil {
		t.Fatalf("create pre-000035 Git Credential: %v", err)
	}
	if err := db.Model(&persistence.Project{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, project.ID).
		Update("git_credential_id", gitCredential.ID).Error; err != nil {
		t.Fatalf("bind legacy Project Git Credential: %v", err)
	}

	if err := Migrate(ctx, db, migrationsThrough(t, "000035_workspace_credential_bindings.sql")); err != nil {
		t.Fatal(err)
	}

	var backfilled persistence.CredentialBinding
	if err := db.Where(
		"tenant_id = ? AND project_id = ? AND binding_kind = ? AND disabled_at IS NULL",
		seed.tenantID, project.ID, "git_fetch",
	).Take(&backfilled).Error; err != nil {
		t.Fatalf("load backfilled Git Binding: %v", err)
	}
	if backfilled.CredentialID != gitCredential.ID || backfilled.OrganizationID == nil ||
		*backfilled.OrganizationID != session.OrganizationID || project.RepositoryURL == nil ||
		backfilled.SelectorValue != *project.RepositoryURL {
		t.Fatalf("unexpected backfilled Git Binding: %#v", backfilled)
	}

	registryCredential := postgresWorkspaceCredential(
		seed.tenantID, session.OrganizationID, session.CreatedBy,
		"registry", "oci", "bearer_token", now,
	)
	if err := db.Create(&registryCredential).Error; err != nil {
		t.Fatalf("create Registry Credential: %v", err)
	}
	registryBinding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: seed.tenantID, OrganizationID: &session.OrganizationID,
		ProjectID: &project.ID, CredentialID: registryCredential.ID, BindingKind: "registry_pull",
		SelectorValue: "registry.example.com", CreatedBy: session.CreatedBy, CreatedAt: now,
	}
	if err := db.Create(&registryBinding).Error; err != nil {
		t.Fatalf("create Registry Binding: %v", err)
	}
	wrongPurpose := registryBinding
	wrongPurpose.ID = uuid.New()
	wrongPurpose.CredentialID = gitCredential.ID
	wrongPurpose.SelectorValue = "other-registry.example.com"
	if err := db.Create(&wrongPurpose).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Credential Binding kind/purpose mismatch")
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	workerImageBinding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: seed.tenantID, OrganizationID: &session.OrganizationID,
		ExecutionTargetID: &session.ExecutionTargetID, CredentialID: registryCredential.ID,
		BindingKind: "worker_image_pull", SelectorValue: "registry.example.com",
		CreatedBy: session.CreatedBy, CreatedAt: now,
	}
	if err := db.Create(&workerImageBinding).Error; err != nil {
		t.Fatalf("create Worker image pull Binding: %v", err)
	}
	secondWorkerImageBinding := workerImageBinding
	secondWorkerImageBinding.ID = uuid.New()
	secondWorkerImageBinding.SelectorValue = "registry-2.example.com"
	if err := db.Create(&secondWorkerImageBinding).Error; err == nil {
		t.Fatal("PostgreSQL accepted multiple active Worker image pull Bindings for one Execution Target")
	}

	grant := persistence.ExecutionCredentialGrant{
		ID: uuid.New(), TenantID: seed.tenantID, ExecutionID: seed.executionID, Generation: 1,
		BindingID: registryBinding.ID, CredentialID: registryCredential.ID,
		CredentialVersion: registryCredential.Version, CreatedAt: now,
	}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatalf("create valid Execution Credential Grant: %v", err)
	}
	stale := grant
	stale.ID = uuid.New()
	stale.Generation = 2
	if err := db.Create(&stale).Error; err == nil {
		t.Fatal("PostgreSQL accepted a stale Execution Credential Grant generation")
	}
	if err := db.Model(&persistence.ExecutionCredentialGrant{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, grant.ID).
		Update("credential_version", 2).Error; err == nil {
		t.Fatal("PostgreSQL allowed an Execution Credential Grant to mutate")
	}
	if err := db.Delete(&persistence.ExecutionCredentialGrant{},
		"tenant_id = ? AND id = ?", seed.tenantID, grant.ID).Error; err == nil {
		t.Fatal("PostgreSQL allowed an Execution Credential Grant to be deleted")
	}

	disabledAt := now.Add(time.Second)
	if err := db.Model(&persistence.CredentialBinding{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, registryBinding.ID).
		Updates(map[string]any{"disabled_at": disabledAt, "disabled_by": session.CreatedBy}).Error; err != nil {
		t.Fatalf("disable Registry Binding: %v", err)
	}
	if err := db.Model(&persistence.CredentialBinding{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, registryBinding.ID).
		Update("selector_value", "changed.example.com").Error; err == nil {
		t.Fatal("PostgreSQL allowed a disabled Credential Binding to mutate")
	}
	if err := db.Delete(&persistence.CredentialBinding{},
		"tenant_id = ? AND id = ?", seed.tenantID, registryBinding.ID).Error; err == nil {
		t.Fatal("PostgreSQL allowed Credential Binding history to be deleted")
	}

	invalidShape := postgresWorkspaceCredential(
		seed.tenantID, session.OrganizationID, session.CreatedBy,
		"registry", "git", "https_token", now,
	)
	if err := db.Create(&invalidShape).Error; err == nil {
		t.Fatal("PostgreSQL accepted an invalid Registry Credential provider/type shape")
	}

	assertMigrationIndex(
		t, db, "uq_credential_bindings_active_project_selector",
		"tenant_id,project_id,binding_kind,selector_value", "disabled_at",
	)
	assertMigrationIndex(
		t, db, "idx_execution_credential_grants_execution",
		"tenant_id,execution_id,generation,id", "",
	)
}

func TestWorkerImagePullBindingUniquenessMigrationFailsClosedAndEnforcesTargetScope(t *testing.T) {
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
	if err := Migrate(ctx, db, migrationsThrough(t, "000038_credential_binding_fk_indexes.sql")); err != nil {
		t.Fatal(err)
	}

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	firstCredential := postgresWorkspaceCredential(
		seed.tenantID, session.OrganizationID, session.CreatedBy,
		"registry", "oci", "basic", now,
	)
	secondCredential := postgresWorkspaceCredential(
		seed.tenantID, session.OrganizationID, session.CreatedBy,
		"registry", "oci", "bearer_token", now,
	)
	if err := db.Create(&firstCredential).Error; err != nil {
		t.Fatalf("create first Registry Credential: %v", err)
	}
	if err := db.Create(&secondCredential).Error; err != nil {
		t.Fatalf("create second Registry Credential: %v", err)
	}
	firstBinding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: seed.tenantID, OrganizationID: &session.OrganizationID,
		ExecutionTargetID: &session.ExecutionTargetID, CredentialID: firstCredential.ID,
		BindingKind: "worker_image_pull", SelectorValue: "ghcr.io",
		CreatedBy: session.CreatedBy, CreatedAt: now,
	}
	secondBinding := firstBinding
	secondBinding.ID = uuid.New()
	secondBinding.CredentialID = secondCredential.ID
	secondBinding.SelectorValue = "registry.example.com"
	if err := db.Create(&firstBinding).Error; err != nil {
		t.Fatalf("create first Worker image pull Binding: %v", err)
	}
	if err := db.Create(&secondBinding).Error; err != nil {
		t.Fatalf("000038 unexpectedly rejected a different-selector Target Binding: %v", err)
	}

	err := Migrate(ctx, db, migrations.Files)
	if err == nil || !strings.Contains(err.Error(), "Multiple active worker_image_pull Credential Bindings") {
		t.Fatalf("000039 duplicate migration error = %v", err)
	}
	var applied int64
	if err := db.Table("control_plane_schema_migrations").Where("version = ?", 39).Count(&applied).Error; err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("failed 000039 migration record count = %d, want 0", applied)
	}

	disabledAt := now.Add(time.Second)
	if err := db.Model(&persistence.CredentialBinding{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, secondBinding.ID).
		Updates(map[string]any{"disabled_at": disabledAt, "disabled_by": session.CreatedBy}).Error; err != nil {
		t.Fatalf("disable ambiguous Worker image pull Binding: %v", err)
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatalf("apply repaired 000039 migration: %v", err)
	}
	assertMigrationIndex(
		t, db, "uq_credential_bindings_active_worker_image_target",
		"tenant_id,execution_target_id", "worker_image_pull",
	)
	for _, name := range []string{
		"uq_credential_bindings_active_target_selector",
		"idx_credential_bindings_target_lookup",
	} {
		var count int64
		if err := db.Raw("SELECT count(*) FROM pg_class WHERE oid = to_regclass(?)", name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("redundant Target Binding index %s still exists", name)
		}
	}

	thirdBinding := firstBinding
	thirdBinding.ID = uuid.New()
	thirdBinding.CredentialID = secondCredential.ID
	thirdBinding.SelectorValue = "quay.io"
	if err := db.Create(&thirdBinding).Error; err == nil {
		t.Fatal("PostgreSQL accepted multiple active Worker image pull Bindings for one Execution Target")
	}
}

func postgresWorkspaceCredential(
	tenantID, organizationID, actorID uuid.UUID,
	purpose, provider, credentialType string,
	now time.Time,
) persistence.ProviderCredential {
	return persistence.ProviderCredential{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: &organizationID, Scope: "organization",
		Name: "PostgreSQL Workspace Credential", Purpose: purpose, Provider: provider,
		CredentialType: credentialType, EncryptedPayload: bytes.Repeat([]byte{0x71}, 32),
		EncryptedDataKey: bytes.Repeat([]byte{0x72}, 32), KMSProvider: "local", KMSKeyID: "test",
		AADVersion: 3, Version: 1, CreatedBy: actorID, UpdatedBy: actorID, CreatedAt: now, UpdatedAt: now,
	}
}
