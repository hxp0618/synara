package credentials

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestProviderCredentialGrantResolveUsesGrantAndCurrentLease(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	providerCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "Grant Provider",
		Purpose:        PurposeProvider,
		Provider:       "codex",
		CredentialType: ProviderAPIKeyCredentialType,
		Payload: map[string]any{
			"apiKey": "provider-grant-secret", "baseUrl": "https://api.example.com",
		},
	}, "grant-provider-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, sessionID, leaseToken := seedCredentialExecution(
		t, fixture, fixture.organizationID, providerCredential.ID,
	)
	markCredentialExecutionWaitingForAccess(t, fixture, sessionID)
	grantID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionProviderCredentialGrant{
		ID: grantID, TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
		CredentialID: providerCredential.ID, CredentialVersion: providerCredential.Version,
		CreatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatalf("seed Provider Credential Grant: %v", err)
	}
	executionService := newExecutionServiceForCredentialTests(fixture)
	lease := executions.LeaseInput{
		TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken,
	}

	resolved, err := fixture.service.ResolveProviderGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GrantID != grantID || resolved.Payload["apiKey"] != "provider-grant-secret" {
		t.Fatalf("unexpected resolved Provider Credential Grant: %#v", resolved)
	}
	if resolved.Access.Status != executions.ProviderCredentialAccessStatusActive ||
		resolved.Access.GrantID != grantID || resolved.Access.Serial != 1 ||
		resolved.Access.RenewAfterAt == nil ||
		!resolved.Access.RenewAfterAt.Before(resolved.Access.ExpiresAt) {
		t.Fatalf("resolved access = %#v", resolved.Access)
	}

	var leaseModel persistence.WorkerLease
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID,
	).Take(&leaseModel).Error; err != nil {
		t.Fatal(err)
	}
	if leaseModel.ProviderCredentialGrantID == nil || *leaseModel.ProviderCredentialGrantID != grantID ||
		leaseModel.ProviderCredentialAccessSerial == nil || *leaseModel.ProviderCredentialAccessSerial != 1 ||
		leaseModel.ProviderCredentialAccessIssuedAt == nil ||
		leaseModel.ProviderCredentialAccessRenewedAt == nil ||
		leaseModel.ProviderCredentialAccessExpiresAt == nil ||
		leaseModel.ProviderCredentialRefreshDeadlineAt == nil {
		t.Fatalf("persistent lease access bundle = %#v", leaseModel)
	}

	semanticActivityAt := time.Now().UTC()
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, sessionID).
		Updates(map[string]any{
			"last_event_sequence":          2,
			"meaningful_activity_sequence": 2,
			"meaningful_activity_at":       semanticActivityAt,
		}).Error; err != nil {
		t.Fatal(err)
	}
	renewed, err := executionService.Renew(
		ctx,
		worker,
		executionID,
		executions.RenewLeaseInput{LeaseInput: lease},
		"provider-credential-access-renew",
	)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Value.ProviderCredentialAccess == nil ||
		renewed.Value.ProviderCredentialAccess.Status != executions.ProviderCredentialAccessStatusActive ||
		renewed.Value.ProviderCredentialAccess.Serial != 2 ||
		renewed.Value.ProviderCredentialAccess.ActivitySequence != 2 ||
		!renewed.Value.ProviderCredentialAccess.ExpiresAt.After(resolved.Access.ExpiresAt) {
		t.Fatalf("semantic Lease renewal access = %#v", renewed.Value.ProviderCredentialAccess)
	}

	if _, err := fixture.service.ResolveForExecution(
		ctx, executionService, worker, executionID, providerCredential.ID, lease,
	); err == nil {
		t.Fatal("generation with an opaque Provider Credential Grant accepted the legacy Credential-ID route")
	} else {
		assertCredentialProblemCode(t, err, "provider_credential_grant_required")
	}

	fenced := lease
	fenced.Generation = 2
	_, err = fixture.service.ResolveProviderGrantForExecution(
		ctx, executionService, worker, executionID, grantID, fenced,
	)
	assertCredentialProblemCode(t, err, "generation_fenced")
}

func TestProviderCredentialGrantResolveReturnsUnavailableAfterCredentialRotation(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	providerCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "Rotating Provider",
		Purpose:        PurposeProvider,
		Provider:       "codex",
		CredentialType: ProviderAPIKeyCredentialType,
		Payload: map[string]any{
			"apiKey": "provider-grant-secret", "baseUrl": "https://api.example.com",
		},
	}, "grant-provider-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, sessionID, leaseToken := seedCredentialExecution(
		t, fixture, fixture.organizationID, providerCredential.ID,
	)
	markCredentialExecutionWaitingForAccess(t, fixture, sessionID)
	grantID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionProviderCredentialGrant{
		ID: grantID, TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
		CredentialID: providerCredential.ID, CredentialVersion: providerCredential.Version,
		CreatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatalf("seed Provider Credential Grant: %v", err)
	}
	executionService := newExecutionServiceForCredentialTests(fixture)
	lease := executions.LeaseInput{
		TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken,
	}

	if _, err := fixture.service.ResolveProviderGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	); err != nil {
		t.Fatal(err)
	}
	rotated, err := fixture.service.Rotate(ctx, fixture.owner, fixture.tenantID, providerCredential.ID, RotateInput{
		ExpectedVersion: providerCredential.Version,
		Payload: map[string]any{
			"apiKey": "provider-grant-rotated", "baseUrl": "https://api.example.com",
		},
	}, "grant-provider-rotate", "127.0.0.1")
	if err != nil || rotated.Version != providerCredential.Version+1 {
		t.Fatalf("rotate Provider Credential: item=%#v err=%v", rotated, err)
	}

	_, err = fixture.service.ResolveProviderGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	)
	assertCredentialProblemCode(t, err, "provider_credential_access_unavailable")
	renewed, err := executionService.Renew(
		ctx,
		worker,
		executionID,
		executions.RenewLeaseInput{LeaseInput: lease},
		"provider-credential-access-rotated-renew",
	)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Value.ProviderCredentialAccess == nil ||
		renewed.Value.ProviderCredentialAccess.Status != executions.ProviderCredentialAccessStatusCredentialUnavailable {
		t.Fatalf("rotated Credential renewal access = %#v", renewed.Value.ProviderCredentialAccess)
	}
}

func TestProviderCredentialGrantResolveReturnsUnavailableWhenPlatformCredentialLosesEntitlement(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.Tenant{}).Where("id = ?", fixture.tenantID).
			Update("plan_code", "enterprise").Error; err != nil {
			return err
		}
		return tx.Model(&persistence.TenantSubscription{}).Where("tenant_id = ?", fixture.tenantID).
			Updates(map[string]any{"plan_code": "enterprise", "status": "active"}).Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.PlatformInstallation{}).Where("key = ?", "control-plane").
		Update("profile", "enterprise").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.UpdateScopePolicy(
		ctx, fixture.owner, fixture.tenantID,
		UpdateScopePolicyInput{PlatformCredentialsEnabled: true},
		"platform-scope-enable", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	platformCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Scope:          credentialscope.ScopePlatform,
		Name:           "Platform Provider",
		Purpose:        PurposeProvider,
		Provider:       "codex",
		CredentialType: ProviderAPIKeyCredentialType,
		Payload: map[string]any{
			"apiKey": "platform-provider-secret", "baseUrl": "https://api.example.com",
		},
	}, "platform-provider-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, sessionID, leaseToken := seedCredentialExecution(
		t, fixture, fixture.organizationID, platformCredential.ID,
	)
	markCredentialExecutionWaitingForAccess(t, fixture, sessionID)
	grantID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionProviderCredentialGrant{
		ID: grantID, TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
		CredentialID: platformCredential.ID, CredentialVersion: platformCredential.Version,
		CreatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatalf("seed Provider Credential Grant: %v", err)
	}
	if err := fixture.db.Model(&persistence.PlatformInstallation{}).Where("key = ?", "control-plane").
		Update("profile", "personal").Error; err != nil {
		t.Fatal(err)
	}
	executionService := newExecutionServiceForCredentialTests(fixture)
	lease := executions.LeaseInput{
		TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken,
	}

	_, err = fixture.service.ResolveProviderGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	)
	assertCredentialProblemCode(t, err, "provider_credential_access_unavailable")
}

func TestProviderCredentialGrantResolveRequiresCurrentWorkerIncarnation(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	providerCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "Grant Provider",
		Purpose:        PurposeProvider,
		Provider:       "codex",
		CredentialType: ProviderAPIKeyCredentialType,
		Payload: map[string]any{
			"apiKey": "provider-grant-secret", "baseUrl": "https://api.example.com",
		},
	}, "grant-provider-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, sessionID, leaseToken := seedCredentialExecution(
		t, fixture, fixture.organizationID, providerCredential.ID,
	)
	markCredentialExecutionWaitingForAccess(t, fixture, sessionID)
	grantID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionProviderCredentialGrant{
		ID: grantID, TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
		CredentialID: providerCredential.ID, CredentialVersion: providerCredential.Version,
		CreatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatalf("seed Provider Credential Grant: %v", err)
	}
	if err := fixture.db.Model(&persistence.WorkerInstance{}).
		Where("id = ?", worker.ID).
		Update("incarnation", worker.Incarnation+1).Error; err != nil {
		t.Fatal(err)
	}
	executionService := newExecutionServiceForCredentialTests(fixture)
	lease := executions.LeaseInput{
		TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken,
	}

	_, err = fixture.service.ResolveProviderGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	)
	assertCredentialProblemCode(t, err, "worker_incarnation_fenced")
}

func TestExecutionSecretResolutionRejectsTenantDeletingForRawAndOpaqueRoutes(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	providerCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "Tenant deleting provider",
		Purpose:        PurposeProvider,
		Provider:       "codex",
		CredentialType: ProviderAPIKeyCredentialType,
		Payload: map[string]any{
			"apiKey": "tenant-deleting-secret", "baseUrl": "https://api.example.com",
		},
	}, "tenant-deleting-provider-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, sessionID, leaseToken := seedCredentialExecution(
		t, fixture, fixture.organizationID, providerCredential.ID,
	)
	markCredentialExecutionWaitingForAccess(t, fixture, sessionID)
	executionService := newExecutionServiceForCredentialTests(fixture)
	lease := executions.LeaseInput{
		TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken,
	}
	now := fixture.now.Add(time.Minute)
	if err := fixture.db.Model(&persistence.Tenant{}).
		Where("id = ?", fixture.tenantID).
		Updates(map[string]any{"status": "deleting", "deleted_at": now}).Error; err != nil {
		t.Fatal(err)
	}

	_, err = fixture.service.ResolveForExecution(
		ctx, executionService, worker, executionID, providerCredential.ID, lease,
	)
	assertCredentialProblemCode(t, err, "tenant_deleting")

	grantID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionProviderCredentialGrant{
		ID: grantID, TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
		CredentialID: providerCredential.ID, CredentialVersion: providerCredential.Version,
		CreatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatalf("seed Provider Credential Grant: %v", err)
	}
	_, err = fixture.service.ResolveProviderGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	)
	assertCredentialProblemCode(t, err, "tenant_deleting")
}

func newExecutionServiceForCredentialTests(fixture credentialFixture) *executions.Service {
	return executions.NewService(
		fixture.db, nil, 30*time.Second, 90*time.Second, 24*time.Hour, nil, nil,
	)
}

func markCredentialExecutionWaitingForAccess(t *testing.T, fixture credentialFixture, sessionID uuid.UUID) {
	t.Helper()
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, sessionID).
		Updates(map[string]any{
			"resource_state":               "active",
			"meaningful_activity_at":       fixture.now,
			"meaningful_activity_sequence": 1,
			"last_event_sequence":          1,
		}).Error; err != nil {
		t.Fatalf("mark Session waiting for access: %v", err)
	}
}
