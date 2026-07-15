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

func TestSQLiteCredentialScopeSafetyRejectsInvalidShapeAndPreservesEmergencyToggle(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-credential-scope-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"idx_provider_credentials_user_scope",
		"idx_provider_credentials_organization_scope",
		"idx_provider_credentials_tenant_platform_scope",
		"trg_provider_credentials_scope_shape_insert",
		"trg_provider_credentials_scope_membership_insert",
		"trg_provider_credentials_scope_shape_update",
		"trg_agent_sessions_provider_credential_scope_insert",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite safety object %s count = %d, want 1", name, count)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	credential := sqliteScopeCredential(domain.TenantID, domain.UserID, now)
	credential.Scope = "user"
	credential.ScopeUserID = &domain.UserID
	if err := store.DB().Create(&credential).Error; err != nil {
		t.Fatalf("create valid User Credential: %v", err)
	}
	beforePayload := bytes.Clone(credential.EncryptedPayload)
	beforeKey := bytes.Clone(credential.EncryptedDataKey)
	if err := store.DB().Model(&persistence.ProviderCredential{}).
		Where("id = ?", credential.ID).
		Update("auto_select_enabled", true).Error; err != nil {
		t.Fatalf("toggle auto-select without key rotation: %v", err)
	}
	var toggled persistence.ProviderCredential
	if err := store.DB().Where("id = ?", credential.ID).Take(&toggled).Error; err != nil {
		t.Fatal(err)
	}
	if !toggled.AutoSelectEnabled || !bytes.Equal(toggled.EncryptedPayload, beforePayload) ||
		!bytes.Equal(toggled.EncryptedDataKey, beforeKey) || toggled.Version != 1 || toggled.AADVersion != 3 {
		t.Fatalf("auto-select toggle mutated Credential material: %#v", toggled)
	}
	if err := store.DB().Model(&persistence.ProviderCredential{}).
		Where("id = ?", credential.ID).Update("scope", "tenant").Error; err == nil {
		t.Fatal("SQLite allowed immutable Credential scope to change")
	}
	if err := store.DB().Model(&persistence.ProviderCredential{}).
		Where("id = ?", credential.ID).
		Updates(map[string]any{"aad_version": 3, "version": 2}).Error; err == nil {
		t.Fatal("SQLite allowed a legacy-style AAD/version update without new ciphertext")
	}

	missingUserID := uuid.New()
	invalidUser := sqliteScopeCredential(domain.TenantID, domain.UserID, now)
	invalidUser.Scope = "user"
	invalidUser.ScopeUserID = &missingUserID
	if err := store.DB().Create(&invalidUser).Error; err == nil {
		t.Fatal("SQLite accepted User Credential scope outside active Tenant membership")
	}

	git := sqliteScopeCredential(domain.TenantID, domain.UserID, now)
	git.Purpose = "git"
	git.Provider = "git"
	git.CredentialType = "https_token"
	git.Scope = "tenant"
	git.AADVersion = 2
	git.AutoSelectEnabled = true
	if err := store.DB().Create(&git).Error; err == nil {
		t.Fatal("SQLite accepted automatic selection for a Git Credential")
	}

	policy := persistence.ProviderCredentialScopePolicy{
		TenantID: domain.TenantID, PlatformCredentialsEnabled: true,
		PlatformCredentialAutoSelect: true, UpdatedBy: domain.UserID,
	}
	if err := store.DB().Create(&policy).Error; err == nil {
		t.Fatal("SQLite enabled Platform Credentials outside an enterprise entitlement")
	}

	projectID := uuid.New()
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Credential scope project", DefaultBranch: "main", Visibility: "private",
		CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentSession{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Valid User binding",
		Status: "active", Visibility: "private", Provider: "codex",
		ProviderCredentialID: &credential.ID, ExecutionTargetID: domain.ExecutionTargetID,
	}).Error; err != nil {
		t.Fatalf("SQLite rejected valid User Credential binding: %v", err)
	}
	if err := store.DB().Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", domain.TenantID, domain.UserID).
		Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentSession{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Suspended User binding",
		Status: "active", Visibility: "private", Provider: "codex",
		ProviderCredentialID: &credential.ID, ExecutionTargetID: domain.ExecutionTargetID,
	}).Error; err == nil {
		t.Fatal("SQLite bound a User Credential after Tenant membership suspension")
	}
}

func sqliteScopeCredential(tenantID, actorID uuid.UUID, now time.Time) persistence.ProviderCredential {
	return persistence.ProviderCredential{
		ID: uuid.New(), TenantID: tenantID, Scope: "tenant",
		Name: "SQLite scope " + uuid.NewString(), Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: bytes.Repeat([]byte{0x11}, 32), EncryptedDataKey: bytes.Repeat([]byte{0x22}, 32),
		KMSProvider: "local", KMSKeyID: "sqlite-test", AADVersion: 3, Version: 1,
		CreatedBy: actorID, UpdatedBy: actorID, CreatedAt: now, UpdatedAt: now,
	}
}
