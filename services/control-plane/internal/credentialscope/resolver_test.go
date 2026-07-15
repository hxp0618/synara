package credentialscope

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestResolveUsesScopePriorityAndRejectsSameLevelAmbiguity(t *testing.T) {
	fixture := newResolverFixture(t, "enterprise", "enterprise")
	model := "gpt-5.6"
	tenantCredential := fixture.credential(ScopeTenant, nil, nil, true)
	organizationCredential := fixture.credential(ScopeOrganization, nil, &fixture.organizationID, true)
	userCredential := fixture.credential(ScopeUser, &fixture.userID, nil, true)
	for _, credential := range []*persistence.ProviderCredential{
		&tenantCredential, &organizationCredential, &userCredential,
	} {
		if err := fixture.db.Create(credential).Error; err != nil {
			t.Fatal(err)
		}
	}

	selection, err := Resolve(context.Background(), fixture.db, fixture.request(&model, nil))
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil || selection.Credential.ID != userCredential.ID || selection.Scope != ScopeUser || selection.Explicit {
		t.Fatalf("automatic selection = %#v, want User Credential", selection)
	}

	if err := fixture.db.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", fixture.tenantID, fixture.userID).
		Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	selection, err = Resolve(context.Background(), fixture.db, fixture.request(&model, nil))
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil || selection.Credential.ID != organizationCredential.ID || selection.Scope != ScopeOrganization {
		t.Fatalf("selection after User suspension = %#v, want Organization Credential", selection)
	}

	secondOrganization := fixture.credential(ScopeOrganization, nil, &fixture.organizationID, true)
	if err := fixture.db.Create(&secondOrganization).Error; err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(context.Background(), fixture.db, fixture.request(&model, nil))
	assertScopeProblem(t, err, "credential_scope_ambiguous")

	explicit := tenantCredential.ID
	selection, err = Resolve(context.Background(), fixture.db, fixture.request(&model, &explicit))
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil || selection.Credential.ID != tenantCredential.ID || !selection.Explicit {
		t.Fatalf("explicit selection = %#v, want Tenant Credential", selection)
	}
}

func TestResolveRequiresOptInAndAppliesTenantSelectors(t *testing.T) {
	fixture := newResolverFixture(t, "enterprise", "enterprise")
	model := "gpt-5.6"
	otherModel := "gpt-5.5"
	restricted := fixture.credential(ScopeTenant, nil, nil, true)
	restricted.SelectorOrganizationID = &fixture.organizationID
	restricted.SelectorModel = &model
	disabled := fixture.credential(ScopeUser, &fixture.userID, nil, false)
	for _, credential := range []*persistence.ProviderCredential{&restricted, &disabled} {
		if err := fixture.db.Create(credential).Error; err != nil {
			t.Fatal(err)
		}
	}

	selection, err := Resolve(context.Background(), fixture.db, fixture.request(&model, nil))
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil || selection.Credential.ID != restricted.ID {
		t.Fatalf("matching Tenant selector selection = %#v", selection)
	}

	selection, err = Resolve(context.Background(), fixture.db, fixture.request(&otherModel, nil))
	if err != nil {
		t.Fatal(err)
	}
	if selection != nil {
		t.Fatalf("mismatched Tenant model selected %#v", selection)
	}

	explicitDisabled := disabled.ID
	selection, err = Resolve(context.Background(), fixture.db, fixture.request(&model, &explicitDisabled))
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil || selection.Credential.ID != disabled.ID || !selection.Explicit {
		t.Fatalf("explicit disabled Credential selection = %#v", selection)
	}

	if err := fixture.db.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", fixture.tenantID, fixture.userID).
		Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(context.Background(), fixture.db, fixture.request(&model, &explicitDisabled))
	assertScopeProblem(t, err, "credential_not_found")
}

func TestResolvePlatformRequiresEnterprisePolicyAndSeparateAutoSelect(t *testing.T) {
	fixture := newResolverFixture(t, "enterprise", "enterprise")
	policy := persistence.ProviderCredentialScopePolicy{
		TenantID: fixture.tenantID, PlatformCredentialsEnabled: true,
		PlatformCredentialAutoSelect: false, UpdatedBy: fixture.userID,
	}
	if err := fixture.db.Create(&policy).Error; err != nil {
		t.Fatal(err)
	}
	platformCredential := fixture.credential(ScopePlatform, nil, nil, true)
	if err := fixture.db.Create(&platformCredential).Error; err != nil {
		t.Fatal(err)
	}
	model := "gpt-5.6"

	selection, err := Resolve(context.Background(), fixture.db, fixture.request(&model, nil))
	if err != nil {
		t.Fatal(err)
	}
	if selection != nil {
		t.Fatalf("Platform Credential auto-selected without Tenant auto policy: %#v", selection)
	}

	explicit := platformCredential.ID
	selection, err = Resolve(context.Background(), fixture.db, fixture.request(&model, &explicit))
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil || selection.Credential.ID != platformCredential.ID || !selection.Explicit {
		t.Fatalf("explicit Platform selection = %#v", selection)
	}

	if err := fixture.db.Model(&persistence.ProviderCredentialScopePolicy{}).
		Where("tenant_id = ?", fixture.tenantID).
		Update("platform_credential_auto_select", true).Error; err != nil {
		t.Fatal(err)
	}
	selection, err = Resolve(context.Background(), fixture.db, fixture.request(&model, nil))
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil || selection.Credential.ID != platformCredential.ID || selection.Explicit {
		t.Fatalf("enabled Platform auto-selection = %#v", selection)
	}

	if err := fixture.db.Model(&persistence.PlatformInstallation{}).
		Where("key = ?", "control-plane").Update("profile", "single-node").Error; err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(context.Background(), fixture.db, fixture.request(&model, &explicit))
	assertScopeProblem(t, err, "platform_credential_not_entitled")
}

type resolverFixture struct {
	db             *gorm.DB
	tenantID       uuid.UUID
	organizationID uuid.UUID
	userID         uuid.UUID
	now            time.Time
}

func newResolverFixture(t *testing.T, profile, planCode string) resolverFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{
		SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.PlatformInstallation{}, &persistence.User{}, &persistence.Tenant{},
		&persistence.TenantMembership{}, &persistence.Organization{},
		&persistence.ProviderCredential{}, &persistence.ProviderCredentialScopePolicy{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	tenantID, organizationID, userID := uuid.New(), uuid.New(), uuid.New()
	models := []any{
		&persistence.PlatformInstallation{Key: "control-plane", InstallationID: uuid.NewString(), Profile: profile},
		&persistence.User{ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Owner", Status: "active"},
		&persistence.Tenant{ID: tenantID, Slug: "scope-test", Name: "Scope Test", Status: "active", PlanCode: planCode, Region: "default", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now},
		&persistence.Organization{ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root", Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID},
	}
	for _, model := range models {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed resolver fixture %T: %v", model, err)
		}
	}
	return resolverFixture{db: db, tenantID: tenantID, organizationID: organizationID, userID: userID, now: now}
}

func (fixture resolverFixture) credential(scope string, userID, organizationID *uuid.UUID, autoSelect bool) persistence.ProviderCredential {
	return persistence.ProviderCredential{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: organizationID,
		Scope: scope, ScopeUserID: userID, AutoSelectEnabled: autoSelect,
		Name: scope + " Credential", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: []byte("encrypted-payload"), EncryptedDataKey: []byte("encrypted-data-key"),
		KMSProvider: "local", KMSKeyID: "test", AADVersion: 3, Version: 1,
		CreatedBy: fixture.userID, UpdatedBy: fixture.userID,
	}
}

func (fixture resolverFixture) request(model *string, explicit *uuid.UUID) Request {
	return Request{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		SessionOwnerUserID: fixture.userID, Provider: "codex", Model: model,
		ExplicitCredentialID: explicit, Now: fixture.now,
	}
}

func assertScopeProblem(t *testing.T, err error, code string) {
	t.Helper()
	var value *problem.Error
	if !errors.As(err, &value) || value.Code != code {
		t.Fatalf("error = %v, want problem code %q", err, code)
	}
}
