package database

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProviderCredentialScopeMigrationBackfillsLegacyAADAndEnforcesScopePolicy(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_CREDENTIAL_SCOPE_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_CREDENTIAL_SCOPE_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(ctx, db, migrationsThrough(t, "000032_session_advanced_operations.sql")); err != nil {
		t.Fatal(err)
	}

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	legacyTenantID, legacyGitID := uuid.New(), uuid.New()
	insertLegacyCredential := func(id uuid.UUID, purpose, provider, credentialType string) {
		t.Helper()
		if err := db.Exec(`
			INSERT INTO provider_credentials (
			  id, tenant_id, organization_id, name, purpose, provider, credential_type,
			  encrypted_payload, encrypted_data_key, kms_provider, kms_key_id, version,
			  created_by, updated_by, created_at, updated_at
			) VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, 'local', 'migration', 1, ?, ?, ?, ?)
		`, id, seed.tenantID, purpose+" legacy", purpose, provider, credentialType,
			bytes.Repeat([]byte{0x31}, 32), bytes.Repeat([]byte{0x32}, 32),
			session.CreatedBy, session.CreatedBy, now, now).Error; err != nil {
			t.Fatalf("insert legacy %s Credential: %v", purpose, err)
		}
	}
	insertLegacyCredential(legacyTenantID, "provider", "codex", "api_key")
	insertLegacyCredential(legacyGitID, "git", "git", "https_token")

	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	for _, expected := range []struct {
		id         uuid.UUID
		scope      string
		aadVersion int
	}{
		{id: seed.credentialID, scope: "organization", aadVersion: 1},
		{id: legacyTenantID, scope: "tenant", aadVersion: 1},
		{id: legacyGitID, scope: "tenant", aadVersion: 2},
	} {
		var credential persistence.ProviderCredential
		if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, expected.id).Take(&credential).Error; err != nil {
			t.Fatal(err)
		}
		if credential.Scope != expected.scope || credential.AADVersion != expected.aadVersion || credential.AutoSelectEnabled {
			t.Fatalf("legacy Credential backfill = %#v, want scope=%s aad=%d auto=false", credential, expected.scope, expected.aadVersion)
		}
	}

	assertMigrationIndex(
		t, db, "idx_provider_credentials_user_scope",
		"tenant_id,purpose,provider,scope,scope_user_id,id", "auto_select_enabled",
	)
	assertMigrationIndex(
		t, db, "idx_provider_credentials_organization_scope",
		"tenant_id,purpose,provider,scope,organization_id,id", "auto_select_enabled",
	)
	assertMigrationIndex(
		t, db, "idx_provider_credentials_tenant_platform_scope",
		"tenant_id,purpose,provider,scope,id", "auto_select_enabled",
	)

	if err := db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.credentialID).
		Updates(map[string]any{"aad_version": 3, "version": 2}).Error; err == nil {
		t.Fatal("PostgreSQL allowed legacy AAD upgrade without new ciphertext and wrapped data key")
	}
	if err := db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.credentialID).
		Updates(map[string]any{
			"aad_version": 3, "version": 2,
			"encrypted_payload":  bytes.Repeat([]byte{0x41}, 32),
			"encrypted_data_key": bytes.Repeat([]byte{0x42}, 32),
		}).Error; err != nil {
		t.Fatalf("PostgreSQL rejected valid legacy AAD v3 rotation: %v", err)
	}

	userCredential := postgresScopeCredential(seed.tenantID, session.CreatedBy, now)
	userCredential.Scope = "user"
	userCredential.ScopeUserID = &session.CreatedBy
	if err := db.Create(&userCredential).Error; err != nil {
		t.Fatalf("create User Credential: %v", err)
	}
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
		Update("provider_credential_id", userCredential.ID).Error; err != nil {
		t.Fatalf("bind Session owner's User Credential: %v", err)
	}
	replacementOwnerID := uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: replacementOwnerID, Email: uuid.NewString() + "@example.com",
			DisplayName: "Replacement owner", Status: "active", EmailVerifiedAt: &now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: seed.tenantID, UserID: replacementOwnerID, Role: "owner",
			Status: "active", JoinedAt: &now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.OrganizationMembership{}).
		Where("tenant_id = ? AND user_id = ?", seed.tenantID, session.CreatedBy).
		Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", seed.tenantID, session.CreatedBy).
		Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
		Update("provider_credential_id", userCredential.ID).Error; err == nil {
		t.Fatal("PostgreSQL retained a writable User Credential binding after membership suspension")
	}
	if err := db.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", seed.tenantID, session.CreatedBy).
		Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.OrganizationMembership{}).
		Where("tenant_id = ? AND user_id = ?", seed.tenantID, session.CreatedBy).
		Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}

	modelSelector := "model-restricted"
	tenantRestricted := postgresScopeCredential(seed.tenantID, session.CreatedBy, now)
	tenantRestricted.Scope = "tenant"
	tenantRestricted.SelectorOrganizationID = &session.OrganizationID
	tenantRestricted.SelectorModel = &modelSelector
	if err := db.Create(&tenantRestricted).Error; err != nil {
		t.Fatalf("create restricted Tenant Credential: %v", err)
	}
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
		Update("provider_credential_id", tenantRestricted.ID).Error; err == nil {
		t.Fatal("PostgreSQL bound a Tenant Credential whose model selector did not match")
	}

	gitAuto := postgresScopeCredential(seed.tenantID, session.CreatedBy, now)
	gitAuto.Purpose, gitAuto.Provider, gitAuto.CredentialType = "git", "git", "https_token"
	gitAuto.Scope, gitAuto.AADVersion, gitAuto.AutoSelectEnabled = "tenant", 2, true
	if err := db.Create(&gitAuto).Error; err == nil {
		t.Fatal("PostgreSQL accepted automatic Provider selection for a Git Credential")
	}

	if err := db.Model(&persistence.Tenant{}).Where("id = ?", seed.tenantID).
		Update("plan_code", "enterprise").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"profile", "updated_at"}),
	}).Create(&persistence.PlatformInstallation{
		Key: "control-plane", InstallationID: uuid.NewString(), Profile: "enterprise",
	}).Error; err != nil {
		t.Fatal(err)
	}
	policy := persistence.ProviderCredentialScopePolicy{
		TenantID: seed.tenantID, PlatformCredentialsEnabled: true,
		PlatformCredentialAutoSelect: false, UpdatedBy: session.CreatedBy,
	}
	if err := db.Create(&policy).Error; err != nil {
		t.Fatalf("create enterprise Platform Credential policy: %v", err)
	}
	platformCredential := postgresScopeCredential(seed.tenantID, session.CreatedBy, now)
	platformCredential.Scope = "platform"
	if err := db.Create(&platformCredential).Error; err != nil {
		t.Fatalf("create explicitly enabled Platform Credential: %v", err)
	}
	if err := db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, platformCredential.ID).
		Update("auto_select_enabled", true).Error; err == nil {
		t.Fatal("PostgreSQL enabled Platform auto-select without Tenant auto-select policy")
	}
	if err := db.Model(&persistence.ProviderCredentialScopePolicy{}).
		Where("tenant_id = ?", seed.tenantID).
		Update("platform_credential_auto_select", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, platformCredential.ID).
		Update("auto_select_enabled", true).Error; err != nil {
		t.Fatalf("enable Platform auto-select after explicit policy: %v", err)
	}
	if err := db.Model(&persistence.ProviderCredentialScopePolicy{}).
		Where("tenant_id = ?", seed.tenantID).
		Updates(map[string]any{
			"platform_credentials_enabled": false, "platform_credential_auto_select": false,
		}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, platformCredential.ID).
		Update("auto_select_enabled", false).Error; err != nil {
		t.Fatalf("emergency Platform auto-select disable after policy revocation: %v", err)
	}
}

func postgresScopeCredential(tenantID, actorID uuid.UUID, now time.Time) persistence.ProviderCredential {
	return persistence.ProviderCredential{
		ID: uuid.New(), TenantID: tenantID, Scope: "tenant",
		Name: "PostgreSQL scope " + uuid.NewString(), Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: bytes.Repeat([]byte{0x51}, 32), EncryptedDataKey: bytes.Repeat([]byte{0x52}, 32),
		KMSProvider: "local", KMSKeyID: "migration", AADVersion: 3, Version: 1,
		CreatedBy: actorID, UpdatedBy: actorID, CreatedAt: now, UpdatedAt: now,
	}
}
