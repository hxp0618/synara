package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestCredentialAccessAndMetadataDoNotExposePayload(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	secret := "sk-credential-plaintext"

	created, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "OpenAI production",
		Provider:       "openai",
		CredentialType: "api_key",
		Payload:        map[string]any{"apiKey": secret, "baseUrl": "https://api.openai.com"},
	}, "credential-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	assertMetadataDoesNotContain(t, created, secret)

	items, err := fixture.service.List(ctx, fixture.securityAdmin, fixture.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("unexpected credential metadata list: %#v", items)
	}
	assertMetadataDoesNotContain(t, items, secret)

	resolved, err := fixture.service.Resolve(ctx, fixture.tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["apiKey"] != secret {
		t.Fatalf("unexpected resolved payload: %#v", resolved)
	}

	var stored persistence.ProviderCredential
	if err := fixture.db.Where("id = ?", created.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.EncryptedPayload, []byte(secret)) || bytes.Contains(stored.EncryptedDataKey, []byte(secret)) {
		t.Fatal("credential plaintext leaked into provider_credentials")
	}
	var audits []persistence.AuditLog
	if err := fixture.db.Where("resource_type = ? AND resource_id = ?", "provider_credential", created.ID).Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	assertMetadataDoesNotContain(t, audits, secret)

	_, err = fixture.service.Create(ctx, fixture.member, fixture.tenantID, CreateInput{
		Name: "Forbidden", Provider: "openai", CredentialType: "api_key", Payload: map[string]any{"apiKey": secret},
	}, "credential-forbidden", "127.0.0.1")
	assertCredentialProblemCode(t, err, "tenant_forbidden")
	_, err = fixture.service.List(ctx, fixture.member, fixture.tenantID)
	assertCredentialProblemCode(t, err, "tenant_forbidden")
}

func TestCredentialCiphertextIsBoundToCredentialAAD(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	first, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Name: "First", Provider: "openai", CredentialType: "api_key", Payload: map[string]any{"apiKey": "first-secret"},
	}, "credential-first", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Name: "Second", Provider: "openai", CredentialType: "api_key", Payload: map[string]any{"apiKey": "second-secret"},
	}, "credential-second", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var firstModel, secondModel persistence.ProviderCredential
	if err := fixture.db.Where("id = ?", first.ID).Take(&firstModel).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Where("id = ?", second.ID).Take(&secondModel).Error; err != nil {
		t.Fatal(err)
	}
	altered := firstModel
	modelSelector := "different-model"
	altered.SelectorModel = &modelSelector
	aad, err := credentialAAD(altered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.cipher.Decrypt(context.Background(), credentialkms.Envelope{
		EncryptedPayload: firstModel.EncryptedPayload, EncryptedDataKey: firstModel.EncryptedDataKey,
		KMSProvider: firstModel.KMSProvider, KMSKeyID: firstModel.KMSKeyID,
	}, aad); err == nil {
		t.Fatal("scope-aware AAD accepted ciphertext under different selector metadata")
	}
	if err := fixture.db.Model(&persistence.ProviderCredential{}).Where("id = ?", first.ID).Updates(map[string]any{
		"encrypted_payload":  secondModel.EncryptedPayload,
		"encrypted_data_key": secondModel.EncryptedDataKey,
	}).Error; err == nil {
		t.Fatal("database allowed encrypted Credential material to change without rotation")
	}
}

func TestCredentialLegacyAADRemainsReadableAndRotationUpgradesToScopeAwareV3(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()

	for _, test := range []struct {
		name           string
		purpose        string
		provider       string
		credentialType string
		aadVersion     int
		payload        map[string]any
		wantKey        string
		wantValue      string
	}{
		{
			name: "legacy Provider v1", purpose: PurposeProvider, provider: "codex",
			credentialType: "api_key", aadVersion: 1,
			payload: map[string]any{"apiKey": "legacy-provider-secret"},
			wantKey: "apiKey", wantValue: "legacy-provider-secret",
		},
		{
			name: "legacy Git v2", purpose: PurposeGit, provider: GitProvider,
			credentialType: GitHTTPSCredentialType, aadVersion: 2,
			payload: map[string]any{"host": "github.com", "username": "git", "token": "legacy-git-secret"},
			wantKey: "token", wantValue: "legacy-git-secret",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			model := persistence.ProviderCredential{
				ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
				Scope: credentialscope.ScopeOrganization,
				Name:  test.name, Purpose: test.purpose, Provider: test.provider, CredentialType: test.credentialType,
				AADVersion: test.aadVersion, Version: 1,
				CreatedBy: fixture.owner.UserID, UpdatedBy: fixture.owner.UserID,
			}
			aad, err := credentialAAD(model)
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := fixture.service.cipher.Encrypt(ctx, payload, aad)
			if err != nil {
				t.Fatal(err)
			}
			model.EncryptedPayload = envelope.EncryptedPayload
			model.EncryptedDataKey = envelope.EncryptedDataKey
			model.KMSProvider = envelope.KMSProvider
			model.KMSKeyID = envelope.KMSKeyID
			if err := fixture.db.Create(&model).Error; err != nil {
				t.Fatal(err)
			}

			resolved, err := fixture.service.Resolve(ctx, fixture.tenantID, model.ID)
			if err != nil {
				t.Fatal(err)
			}
			if resolved[test.wantKey] != test.wantValue {
				t.Fatalf("legacy payload = %#v", resolved)
			}

			rotated, err := fixture.service.Rotate(ctx, fixture.owner, fixture.tenantID, model.ID, RotateInput{
				ExpectedVersion: 1, Payload: test.payload,
			}, "legacy-aad-rotate", "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			if rotated.Version != 2 {
				t.Fatalf("rotation version = %d, want 2", rotated.Version)
			}
			metadata, err := json.Marshal(rotated)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(metadata, []byte("aadVersion")) {
				t.Fatalf("internal AAD version leaked through metadata: %s", metadata)
			}
			var stored persistence.ProviderCredential
			if err := fixture.db.Where("id = ?", model.ID).Take(&stored).Error; err != nil {
				t.Fatal(err)
			}
			if stored.AADVersion != 3 {
				t.Fatalf("rotated AAD version = %d, want 3", stored.AADVersion)
			}
		})
	}
}

func TestCredentialAutoSelectToggleIsAuthorizedAuditedAndDoesNotRotateCiphertext(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	created, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Scope: credentialscope.ScopeUser, ScopeUserID: &fixture.owner.UserID,
		Name: "User automatic selection", Provider: "codex", CredentialType: "api_key",
		Payload: map[string]any{"apiKey": "auto-select-secret"},
	}, "auto-select-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if created.AutoSelectEnabled {
		t.Fatal("new Credential unexpectedly enabled automatic selection")
	}
	var before persistence.ProviderCredential
	if err := fixture.db.Where("id = ?", created.ID).Take(&before).Error; err != nil {
		t.Fatal(err)
	}

	updated, err := fixture.service.SetAutoSelect(
		ctx, fixture.owner, fixture.tenantID, created.ID, SetAutoSelectInput{Enabled: true},
		"auto-select-enable", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.AutoSelectEnabled || updated.Version != created.Version {
		t.Fatalf("auto-select update rotated or did not enable Credential: %#v", updated)
	}
	var after persistence.ProviderCredential
	if err := fixture.db.Where("id = ?", created.ID).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.EncryptedPayload, after.EncryptedPayload) ||
		!bytes.Equal(before.EncryptedDataKey, after.EncryptedDataKey) || before.AADVersion != after.AADVersion {
		t.Fatal("auto-select policy update rewrote encrypted Credential material")
	}

	_, err = fixture.service.SetAutoSelect(
		ctx, fixture.member, fixture.tenantID, created.ID, SetAutoSelectInput{Enabled: false},
		"auto-select-forbidden", "127.0.0.1",
	)
	assertCredentialProblemCode(t, err, "tenant_forbidden")

	var audits int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND resource_id = ? AND action = ?", fixture.tenantID, created.ID, "credential.auto_select.changed").
		Count(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("auto-select update audits = %d, want 1", audits)
	}
}

func TestCredentialScopePolicyRequiresEnterpriseEntitlementAndAuthorizedUpdate(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	policy, err := fixture.service.GetScopePolicy(ctx, fixture.owner, fixture.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.PlatformCredentialsEnabled || policy.PlatformCredentialAutoSelect || policy.UpdatedBy != nil {
		t.Fatalf("default scope policy = %#v", policy)
	}

	_, err = fixture.service.UpdateScopePolicy(
		ctx, fixture.member, fixture.tenantID,
		UpdateScopePolicyInput{PlatformCredentialsEnabled: true},
		"scope-policy-forbidden", "127.0.0.1",
	)
	assertCredentialProblemCode(t, err, "tenant_forbidden")
	_, err = fixture.service.UpdateScopePolicy(
		ctx, fixture.owner, fixture.tenantID,
		UpdateScopePolicyInput{PlatformCredentialsEnabled: true},
		"scope-policy-not-entitled", "127.0.0.1",
	)
	assertCredentialProblemCode(t, err, "platform_credential_not_entitled")
	_, err = fixture.service.UpdateScopePolicy(
		ctx, fixture.owner, fixture.tenantID,
		UpdateScopePolicyInput{PlatformCredentialAutoSelect: true},
		"scope-policy-invalid", "127.0.0.1",
	)
	assertCredentialProblemCode(t, err, "invalid_credential_scope_policy")

	if err := fixture.db.Model(&persistence.Tenant{}).Where("id = ?", fixture.tenantID).
		Update("plan_code", "enterprise").Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.PlatformInstallation{}).Where("key = ?", "control-plane").
		Update("profile", "enterprise").Error; err != nil {
		t.Fatal(err)
	}
	policy, err = fixture.service.UpdateScopePolicy(
		ctx, fixture.owner, fixture.tenantID,
		UpdateScopePolicyInput{PlatformCredentialsEnabled: true, PlatformCredentialAutoSelect: true},
		"scope-policy-enable", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.PlatformCredentialsEnabled || !policy.PlatformCredentialAutoSelect ||
		policy.UpdatedBy == nil || *policy.UpdatedBy != fixture.owner.UserID {
		t.Fatalf("enabled scope policy = %#v", policy)
	}

	if err := fixture.db.Model(&persistence.PlatformInstallation{}).Where("key = ?", "control-plane").
		Update("profile", "personal").Error; err != nil {
		t.Fatal(err)
	}
	policy, err = fixture.service.UpdateScopePolicy(
		ctx, fixture.owner, fixture.tenantID, UpdateScopePolicyInput{},
		"scope-policy-emergency-disable", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if policy.PlatformCredentialsEnabled || policy.PlatformCredentialAutoSelect {
		t.Fatalf("disabled scope policy = %#v", policy)
	}

	var audits int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ?", fixture.tenantID, "credential.scope_policy.updated").
		Count(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if audits != 2 {
		t.Fatalf("scope policy update audits = %d, want 2", audits)
	}
}

func TestCredentialRotationUsesOptimisticVersionAndNullableExpiry(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	expiresAt := fixture.now.Add(2 * time.Hour)
	created, err := fixture.service.Create(ctx, fixture.securityAdmin, fixture.tenantID, CreateInput{
		Name: "Rotating", Provider: "anthropic", CredentialType: "api_key",
		Payload: map[string]any{"apiKey": "old-secret"}, ExpiresAt: &expiresAt,
	}, "credential-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := fixture.service.Rotate(ctx, fixture.securityAdmin, fixture.tenantID, created.ID, RotateInput{
		ExpectedVersion: created.Version,
		Payload:         map[string]any{"apiKey": "new-secret"},
		ExpiresAt:       nil,
	}, "credential-rotate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Version != 2 || rotated.ExpiresAt != nil {
		t.Fatalf("rotation did not increment version and clear expiry: %#v", rotated)
	}
	resolved, err := fixture.service.Resolve(ctx, fixture.tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["apiKey"] != "new-secret" {
		t.Fatalf("rotation did not replace payload: %#v", resolved)
	}
	_, err = fixture.service.Rotate(ctx, fixture.owner, fixture.tenantID, created.ID, RotateInput{
		ExpectedVersion: 1,
		Payload:         map[string]any{"apiKey": "stale-secret"},
	}, "credential-stale", "127.0.0.1")
	assertCredentialProblemCode(t, err, "credential_version_conflict")
}

func TestCredentialRevokeExpiryAndTenantIsolation(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	expiresAt := fixture.now.Add(time.Hour)
	expiring, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Name: "Expiring", Provider: "openai", CredentialType: "api_key",
		Payload: map[string]any{"apiKey": "expiring-secret"}, ExpiresAt: &expiresAt,
	}, "credential-expiring", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.now = func() time.Time { return fixture.now.Add(2 * time.Hour) }
	_, err = fixture.service.Resolve(ctx, fixture.tenantID, expiring.ID)
	assertCredentialProblemCode(t, err, "credential_unavailable")

	fixture.service.now = func() time.Time { return fixture.now }
	revoked, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Name: "Revoked", Provider: "openai", CredentialType: "api_key", Payload: map[string]any{"apiKey": "revoked-secret"},
	}, "credential-revoked", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Revoke(ctx, fixture.owner, fixture.tenantID, revoked.ID, "credential-revoke", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Revoke(ctx, fixture.owner, fixture.tenantID, revoked.ID, "credential-revoke-retry", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Resolve(ctx, fixture.tenantID, revoked.ID)
	assertCredentialProblemCode(t, err, "credential_unavailable")
	var revokeAudits int64
	if err := fixture.db.Model(&persistence.AuditLog{}).Where("resource_id = ? AND action = ?", revoked.ID, "credential.revoked").Count(&revokeAudits).Error; err != nil {
		t.Fatal(err)
	}
	if revokeAudits != 1 {
		t.Fatalf("idempotent revoke wrote %d audit records", revokeAudits)
	}

	otherTenantID := uuid.New()
	_, err = fixture.service.Resolve(ctx, otherTenantID, revoked.ID)
	assertCredentialProblemCode(t, err, "credential_not_found")
	otherPrincipal := fixture.owner
	otherPrincipal.ActiveTenantID = &otherTenantID
	_, err = fixture.service.List(ctx, otherPrincipal, fixture.tenantID)
	assertCredentialProblemCode(t, err, "tenant_not_found")
}

func TestCredentialResolveWithoutKMSFailsClosed(t *testing.T) {
	fixture := newCredentialFixture(t)
	service := NewService(fixture.db, nil)
	_, err := service.Resolve(context.Background(), fixture.tenantID, uuid.New())
	assertCredentialProblemCode(t, err, "credential_kms_unavailable")
}

func TestGitCredentialPayloadIsStrictAndHostIsImmutable(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	secret := "github-private-token"
	created, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "GitHub private repositories",
		Purpose:        PurposeGit,
		Provider:       GitProvider,
		CredentialType: GitHTTPSCredentialType,
		Payload: map[string]any{
			"host": "GITHUB.COM.", "username": "x-access-token", "token": secret,
		},
	}, "git-credential-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if created.Purpose != PurposeGit || created.Provider != GitProvider || created.CredentialType != GitHTTPSCredentialType {
		t.Fatalf("unexpected Git Credential metadata: %#v", created)
	}
	assertMetadataDoesNotContain(t, created, secret)
	payload, err := fixture.service.Resolve(ctx, fixture.tenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if payload["host"] != "github.com" || payload["username"] != "x-access-token" || payload["token"] != secret {
		t.Fatalf("unexpected normalized Git Credential payload: %#v", payload)
	}

	for name, input := range map[string]CreateInput{
		"unknown field": {
			Name: "Unknown field", Purpose: PurposeGit, Provider: GitProvider, CredentialType: GitHTTPSCredentialType,
			Payload: map[string]any{"host": "github.com", "username": "git", "token": "token", "extra": "forbidden"},
		},
		"wrong provider": {
			Name: "Wrong provider", Purpose: PurposeGit, Provider: "github", CredentialType: GitHTTPSCredentialType,
			Payload: map[string]any{"host": "github.com", "username": "git", "token": "token"},
		},
		"empty token": {
			Name: "Empty token", Purpose: PurposeGit, Provider: GitProvider, CredentialType: GitHTTPSCredentialType,
			Payload: map[string]any{"host": "github.com", "username": "git", "token": ""},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, createErr := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, input, "git-invalid-"+name, "127.0.0.1")
			if createErr == nil {
				t.Fatal("invalid Git Credential payload was accepted")
			}
		})
	}

	_, err = fixture.service.Rotate(ctx, fixture.owner, fixture.tenantID, created.ID, RotateInput{
		ExpectedVersion: created.Version,
		Payload:         map[string]any{"host": "gitlab.com", "username": "oauth2", "token": "replacement"},
	}, "git-credential-rebind", "127.0.0.1")
	assertCredentialProblemCode(t, err, "git_credential_host_immutable")

	rotated, err := fixture.service.Rotate(ctx, fixture.owner, fixture.tenantID, created.ID, RotateInput{
		ExpectedVersion: created.Version,
		Payload:         map[string]any{"host": "github.com", "username": "oauth2", "token": "replacement"},
	}, "git-credential-rotate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Version != created.Version+1 {
		t.Fatalf("Git Credential rotation did not advance the version: %#v", rotated)
	}
}

func TestCredentialWorkerResolutionRequiresCurrentLeaseAndOrganization(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	organizationCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "Organization", Provider: "codex", CredentialType: "api_key",
		Payload: map[string]any{"apiKey": "organization-secret"},
	}, "credential-organization", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	tenantCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Name: "Tenant", Provider: "codex", CredentialType: "api_key",
		Payload: map[string]any{"apiKey": "tenant-secret"},
	}, "credential-tenant", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, sessionID, leaseToken := seedCredentialExecution(t, fixture, fixture.organizationID, organizationCredential.ID)
	executionService := executions.NewService(fixture.db, nil, 30*time.Second, 90*time.Second, 24*time.Hour, nil, nil)
	lease := executions.LeaseInput{TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken}

	payload, err := fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, organizationCredential.ID, lease)
	if err != nil {
		t.Fatal(err)
	}
	if payload["apiKey"] != "organization-secret" {
		t.Fatalf("unexpected organization credential payload: %#v", payload)
	}
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, tenantCredential.ID, lease)
	assertCredentialProblemCode(t, err, "credential_not_found")
	if err := fixture.db.Model(&persistence.AgentSession{}).Where("id = ?", sessionID).
		Update("provider_credential_id", tenantCredential.ID).Error; err != nil {
		t.Fatal(err)
	}
	payload, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, organizationCredential.ID, lease)
	if err != nil {
		t.Fatal(err)
	}
	if payload["apiKey"] != "organization-secret" {
		t.Fatalf("Session rebind changed the in-flight Execution Credential: %#v", payload)
	}
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, tenantCredential.ID, lease)
	assertCredentialProblemCode(t, err, "credential_not_found")

	invalidToken := lease
	invalidToken.LeaseToken = "wrong-token"
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, organizationCredential.ID, invalidToken)
	assertCredentialProblemCode(t, err, "invalid_lease_token")
	fenced := lease
	fenced.Generation++
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, organizationCredential.ID, fenced)
	assertCredentialProblemCode(t, err, "generation_fenced")

	otherOrganizationID := uuid.New()
	if err := fixture.db.Create(&persistence.Organization{
		ID: otherOrganizationID, TenantID: fixture.tenantID, Slug: "other-organization", Name: "Other Organization",
		Kind: "workspace", Status: "active", Settings: map[string]any{}, CreatedBy: fixture.owner.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	otherCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &otherOrganizationID,
		Name:           "Other Organization", Provider: "codex", CredentialType: "api_key",
		Payload: map[string]any{"apiKey": "other-secret"},
	}, "credential-other-organization", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).Where("id = ?", sessionID).
		Update("provider_credential_id", otherCredential.ID).Error; err == nil {
		t.Fatal("database allowed an Agent Session to bind another Organization's Credential")
	}
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, otherCredential.ID, lease)
	assertCredentialProblemCode(t, err, "credential_not_found")

	rotated, err := fixture.service.Rotate(ctx, fixture.owner, fixture.tenantID, organizationCredential.ID, RotateInput{
		ExpectedVersion: organizationCredential.Version,
		Payload:         map[string]any{"apiKey": "rotated-organization-secret"},
	}, "credential-organization-rotate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Version != organizationCredential.Version+1 {
		t.Fatalf("rotated Credential version = %d", rotated.Version)
	}
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, organizationCredential.ID, lease)
	assertCredentialProblemCode(t, err, "credential_version_fenced")
}

func TestUserCredentialWorkerResolutionFailsClosedAfterMembershipSuspension(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	credential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Scope: credentialscope.ScopeUser, ScopeUserID: &fixture.owner.UserID,
		Name: "Owner Credential", Provider: "codex", CredentialType: "api_key",
		Payload: map[string]any{"apiKey": "owner-secret"},
	}, "credential-user", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, _, leaseToken := seedCredentialExecution(
		t, fixture, fixture.organizationID, credential.ID,
	)
	executionService := executions.NewService(fixture.db, nil, 30*time.Second, 90*time.Second, 24*time.Hour, nil, nil)
	lease := executions.LeaseInput{TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken}

	payload, err := fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, credential.ID, lease)
	if err != nil || payload["apiKey"] != "owner-secret" {
		t.Fatalf("active User Credential resolve = %#v, err=%v", payload, err)
	}
	if err := fixture.db.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", fixture.tenantID, fixture.owner.UserID).
		Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, credential.ID, lease)
	assertCredentialProblemCode(t, err, "credential_not_found")
}

type credentialFixture struct {
	db             *gorm.DB
	service        *Service
	tenantID       uuid.UUID
	organizationID uuid.UUID
	targetID       uuid.UUID
	owner          identity.Principal
	securityAdmin  identity.Principal
	member         identity.Principal
	now            time.Time
}

func newCredentialFixture(t *testing.T) credentialFixture {
	t.Helper()
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "credential-test-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	securityAdminID := uuid.New()
	memberID := uuid.New()
	models := []any{
		&persistence.User{ID: securityAdminID, Email: uuid.NewString() + "@example.com", DisplayName: "Security Admin", Status: "active", EmailVerifiedAt: &now},
		&persistence.User{ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Member", Status: "active", EmailVerifiedAt: &now},
		&persistence.TenantMembership{TenantID: domain.TenantID, UserID: securityAdminID, Role: "security_admin", Status: "active", JoinedAt: &now},
		&persistence.TenantMembership{TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active", JoinedAt: &now},
	}
	for _, model := range models {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed credential fixture %T: %v", model, err)
		}
	}
	wrapper, err := credentialkms.NewLocalKeyWrapper("credential-test-v1", bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), credentialkms.NewEnvelopeCipher(wrapper))
	service.now = func() time.Time { return now }
	return credentialFixture{
		db: store.DB(), service: service, tenantID: domain.TenantID, organizationID: domain.OrganizationID,
		targetID:      domain.ExecutionTargetID,
		owner:         identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		securityAdmin: identity.Principal{UserID: securityAdminID, ActiveTenantID: &domain.TenantID},
		member:        identity.Principal{UserID: memberID, ActiveTenantID: &domain.TenantID},
		now:           now,
	}
}

func seedCredentialExecution(
	t *testing.T,
	fixture credentialFixture,
	organizationID, credentialID uuid.UUID,
) (persistence.WorkerInstance, uuid.UUID, uuid.UUID, string) {
	t.Helper()
	now := fixture.now
	projectID := uuid.New()
	sessionID := uuid.New()
	turnID := uuid.New()
	executionID := uuid.New()
	workerID := uuid.New()
	leaseToken := "credential-worker-lease-token"
	provider := "codex"
	credentialVersion := 1
	worker := persistence.WorkerInstance{
		ID: workerID, ExecutionTargetID: fixture.targetID, TargetKind: "local",
		ClusterID: "credential-test", Namespace: "credential-test", PodName: "credential-test", Version: "test", ProtocolVersion: 1,
		Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: secret.HashToken("credential-worker-token"), Status: "online",
		RegisteredAt: now, LastHeartbeatAt: now,
	}
	models := []any{
		&persistence.Project{ID: projectID, TenantID: fixture.tenantID, OrganizationID: organizationID, Name: "Credential project", DefaultBranch: "main", Visibility: "organization", CreatedBy: fixture.owner.UserID},
		&persistence.AgentSession{ID: sessionID, TenantID: fixture.tenantID, OrganizationID: organizationID, ProjectID: projectID, CreatedBy: fixture.owner.UserID, Title: "Credential session", Status: "active", Visibility: "organization", Provider: "codex", ProviderCredentialID: &credentialID, ExecutionTargetID: fixture.targetID},
		&persistence.AgentTurn{ID: turnID, TenantID: fixture.tenantID, SessionID: sessionID, CreatedBy: fixture.owner.UserID, Status: "running", InputText: "test"},
		&worker,
		&persistence.AgentExecution{
			ID: executionID, TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
			Attempt: 1, Status: "running", ExecutionTargetID: fixture.targetID, TargetKind: "local",
			Provider: &provider, WorkerID: &workerID,
			ProviderCredentialIDSnapshot: &credentialID, ProviderCredentialVersionSnapshot: &credentialVersion,
			ProviderResumeStrategySnapshot: "authoritative-history",
			Generation:                     1, RequestedBy: fixture.owner.UserID, QueuedAt: now, StartedAt: &now,
		},
		&persistence.WorkerLease{ExecutionID: executionID, TenantID: fixture.tenantID, WorkerID: workerID, Generation: 1, LeaseTokenHash: secret.HashToken(leaseToken), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	for _, model := range models {
		if err := fixture.db.Create(model).Error; err != nil {
			t.Fatalf("seed credential execution %T: %v", model, err)
		}
	}
	return worker, executionID, sessionID, leaseToken
}

func seedGitCredentialExecution(
	t *testing.T,
	fixture credentialFixture,
) (persistence.WorkerInstance, uuid.UUID, uuid.UUID, string) {
	t.Helper()
	now := fixture.now
	projectID := uuid.New()
	sessionID := uuid.New()
	turnID := uuid.New()
	executionID := uuid.New()
	workerID := uuid.New()
	leaseToken := "git-credential-worker-lease-token"
	repositoryURL := "https://git.example.com/team/repository.git"
	worker := persistence.WorkerInstance{
		ID: workerID, ExecutionTargetID: fixture.targetID, TargetKind: "local",
		ClusterID: "git-credential-test", Namespace: "git-credential-test", PodName: "git-credential-test",
		Version: "test", ProtocolVersion: 1, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: secret.HashToken("git-credential-worker-token"), Status: "online",
		RegisteredAt: now, LastHeartbeatAt: now,
	}
	models := []any{
		&persistence.Project{
			ID: projectID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
			Name: "Git Credential project", RepositoryURL: &repositoryURL, DefaultBranch: "main",
			Visibility: "organization", CreatedBy: fixture.owner.UserID,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
			ProjectID: projectID, CreatedBy: fixture.owner.UserID, Title: "Git Credential session",
			Status: "active", Visibility: "organization", Provider: "codex", ExecutionTargetID: fixture.targetID,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: fixture.tenantID, SessionID: sessionID,
			CreatedBy: fixture.owner.UserID, Status: "running", InputText: "test",
		},
		&worker,
		&persistence.AgentExecution{
			ID: executionID, TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
			Attempt: 1, Status: "running", ExecutionTargetID: fixture.targetID, TargetKind: "local",
			WorkerID: &workerID, Generation: 1, RequestedBy: fixture.owner.UserID, QueuedAt: now, StartedAt: &now,
		},
		&persistence.WorkerLease{
			ExecutionID: executionID, TenantID: fixture.tenantID, WorkerID: workerID, Generation: 1,
			LeaseTokenHash: secret.HashToken(leaseToken), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Hour),
		},
	}
	for _, model := range models {
		if err := fixture.db.Create(model).Error; err != nil {
			t.Fatalf("seed Git Credential execution %T: %v", model, err)
		}
	}
	return worker, executionID, projectID, leaseToken
}

func assertMetadataDoesNotContain(t *testing.T, value any, secret string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(secret)) {
		t.Fatalf("credential secret leaked through metadata: %s", encoded)
	}
}

func assertCredentialProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}
