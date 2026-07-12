package sessions

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSuspendedTenantCannotCreateTurnExecution(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	if err := fixture.db.Model(&persistence.Tenant{}).Where("id = ?", fixture.tenantID).Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	_, err := fixture.service.CreateTurn(context.Background(), fixture.principal, fixture.sessionID,
		CreateTurnInput{InputText: "must not execute"}, "suspended-turn", "127.0.0.1")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "tenant_suspended" {
		t.Fatalf("expected tenant_suspended, got %v", err)
	}
	var executions int64
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("tenant_id = ?", fixture.tenantID).Count(&executions).Error; err != nil {
		t.Fatal(err)
	}
	if executions != 0 {
		t.Fatalf("suspended tenant created %d executions", executions)
	}
}

func TestConcurrentExecutionQuotaRejectsSecondActiveExecution(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	maxExecutions := 1
	if err := fixture.db.Create(&persistence.TenantQuota{
		TenantID: fixture.tenantID, MaxConcurrentExecutions: &maxExecutions, UpdatedBy: fixture.principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := fixture.service.CreateTurn(ctx, fixture.principal, fixture.sessionID,
		CreateTurnInput{InputText: "first execution"}, "quota-turn-first", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.service.CreateTurn(ctx, fixture.principal, fixture.sessionID,
		CreateTurnInput{InputText: "second execution"}, "quota-turn-second", "127.0.0.1")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "execution_quota_exceeded" {
		t.Fatalf("expected execution_quota_exceeded, got %v", err)
	}
	var executions int64
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("tenant_id = ?", fixture.tenantID).Count(&executions).Error; err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatalf("quota rejection persisted %d executions", executions)
	}
}

func TestSessionProviderCredentialBindingValidatesProviderAndAvailability(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	credentialID := uuid.New()
	if err := fixture.db.Create(&persistence.ProviderCredential{
		ID: credentialID, TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Name: "Codex API", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: []byte("encrypted-payload-placeholder"), EncryptedDataKey: []byte("encrypted-data-key-placeholder"),
		KMSProvider: "local", KMSKeyID: "test", Version: 1,
		CreatedBy: fixture.principal.UserID, UpdatedBy: fixture.principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	created, err := fixture.service.Create(ctx, fixture.principal, fixture.projectID, CreateSessionInput{
		Title: "Credential-bound", Provider: "codex", ProviderCredentialID: &credentialID,
	}, "credential-session", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if created.ProviderCredentialID == nil || *created.ProviderCredentialID != credentialID {
		t.Fatalf("session omitted Provider Credential binding: %#v", created)
	}

	_, err = fixture.service.Create(ctx, fixture.principal, fixture.projectID, CreateSessionInput{
		Title: "Wrong provider", Provider: "claudeAgent", ProviderCredentialID: &credentialID,
	}, "credential-mismatch", "127.0.0.1")
	assertSessionProblemCode(t, err, "credential_provider_mismatch")
	if err := fixture.db.Model(&persistence.ProviderCredential{}).Where("id = ?", credentialID).
		Updates(map[string]any{"revoked_at": time.Now().UTC(), "revoked_by": fixture.principal.UserID}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Create(ctx, fixture.principal, fixture.projectID, CreateSessionInput{
		Title: "Revoked credential", Provider: "codex", ProviderCredentialID: &credentialID,
	}, "credential-revoked", "127.0.0.1")
	assertSessionProblemCode(t, err, "credential_unavailable")
}

type tenantExecutionPolicyFixture struct {
	db             *gorm.DB
	service        *Service
	principal      identity.Principal
	tenantID       uuid.UUID
	organizationID uuid.UUID
	projectID      uuid.UUID
	sessionID      uuid.UUID
}

func newTenantExecutionPolicyFixture(t *testing.T) tenantExecutionPolicyFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-policy-test-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	projectID := uuid.New()
	sessionID := uuid.New()
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Execution Policy", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Execution Policy", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), platformConfig, nil)
	return tenantExecutionPolicyFixture{
		db: store.DB(), service: NewService(store.DB(), projects.NewService(store.DB()), targetService),
		principal: identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		tenantID:  domain.TenantID, organizationID: domain.OrganizationID,
		projectID: projectID, sessionID: sessionID,
	}
}

func assertSessionProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}
