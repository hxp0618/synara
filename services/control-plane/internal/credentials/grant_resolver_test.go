package credentials

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestWorkspaceCredentialGrantResolveUsesGrantAndCurrentLease(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	gitCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "Grant Git", Purpose: PurposeGit, Provider: GitProvider,
		CredentialType: GitHTTPSCredentialType,
		Payload: map[string]any{
			"host": "git.example.com", "username": "synara", "token": "git-grant-secret",
		},
	}, "grant-git-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, projectID, leaseToken := seedGitCredentialExecution(
		t, fixture,
	)
	grantID := seedWorkspaceCredentialGrant(
		t, fixture, executionID, projectID, gitCredential, "git_fetch",
		"https://git.example.com/team/repository.git",
	)
	executionService := executions.NewService(
		fixture.db, nil, 30*time.Second, 90*time.Second, 24*time.Hour, nil, nil,
	)
	lease := executions.LeaseInput{
		TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken,
	}

	resolved, err := fixture.service.ResolveGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GrantID != grantID || resolved.BindingKind != "git_fetch" ||
		resolved.CredentialType != GitHTTPSCredentialType ||
		resolved.Payload["token"] != "git-grant-secret" {
		t.Fatalf("unexpected resolved Workspace Credential: %#v", resolved)
	}

	fenced := lease
	fenced.Generation = 2
	_, err = fixture.service.ResolveGrantForExecution(
		ctx, executionService, worker, executionID, grantID, fenced,
	)
	assertCredentialProblemCode(t, err, "generation_fenced")

	rotated, err := fixture.service.Rotate(ctx, fixture.owner, fixture.tenantID, gitCredential.ID, RotateInput{
		ExpectedVersion: gitCredential.Version,
		Payload: map[string]any{
			"host": "git.example.com", "username": "synara", "token": "rotated-git-grant-secret",
		},
	}, "grant-git-rotate", "127.0.0.1")
	if err != nil || rotated.Version != gitCredential.Version+1 {
		t.Fatalf("rotate Workspace Credential: item=%#v err=%v", rotated, err)
	}
	_, err = fixture.service.ResolveGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	)
	assertCredentialProblemCode(t, err, "credential_grant_version_fenced")
}

func TestRegistryCredentialGrantResolveValidatesImmutableSelector(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	registryCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "Registry", Purpose: PurposeRegistry, Provider: RegistryProviderOci,
		CredentialType: RegistryBearerCredentialType,
		Payload: map[string]any{
			"host": "registry.example.com", "token": "registry-grant-secret",
		},
	}, "grant-registry-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, projectID, leaseToken := seedGitCredentialExecution(
		t, fixture,
	)
	grantID := seedWorkspaceCredentialGrant(
		t, fixture, executionID, projectID, registryCredential, "registry_pull",
		"registry.example.com",
	)
	executionService := executions.NewService(
		fixture.db, nil, 30*time.Second, 90*time.Second, 24*time.Hour, nil, nil,
	)
	lease := executions.LeaseInput{
		TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken,
	}

	resolved, err := fixture.service.ResolveGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	)
	if err != nil || resolved.Payload["token"] != "registry-grant-secret" {
		t.Fatalf("resolve Registry Grant: %#v, err=%v", resolved, err)
	}
	var grant persistence.ExecutionCredentialGrant
	if err := fixture.db.Where("id = ?", grantID).Take(&grant).Error; err != nil {
		t.Fatal(err)
	}
	now := fixture.now.Add(time.Minute)
	if err := fixture.db.Model(&persistence.CredentialBinding{}).
		Where("id = ?", grant.BindingID).
		Updates(map[string]any{"disabled_at": now, "disabled_by": fixture.owner.UserID}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.ResolveGrantForExecution(
		ctx, executionService, worker, executionID, grantID, lease,
	)
	assertCredentialProblemCode(t, err, "credential_grant_unavailable")
	if err := fixture.db.Model(&persistence.CredentialBinding{}).
		Where("id = ?", grant.BindingID).
		Update("selector_value", "other.example.com").Error; err == nil {
		t.Fatal("database allowed immutable Binding selector mutation")
	}
}

func seedWorkspaceCredentialGrant(
	t *testing.T,
	fixture credentialFixture,
	executionID, projectID uuid.UUID,
	credential Credential,
	bindingKind, selector string,
) uuid.UUID {
	t.Helper()
	now := fixture.now
	bindingID := uuid.New()
	grantID := uuid.New()
	models := []any{
		&persistence.CredentialBinding{
			ID: bindingID, TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
			ProjectID: &projectID, CredentialID: credential.ID, BindingKind: bindingKind,
			SelectorValue: selector, CreatedBy: fixture.owner.UserID, CreatedAt: now,
		},
		&persistence.ExecutionCredentialGrant{
			ID: grantID, TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
			BindingID: bindingID, CredentialID: credential.ID,
			CredentialVersion: credential.Version, CreatedAt: now,
		},
	}
	for _, model := range models {
		if err := fixture.db.Create(model).Error; err != nil {
			t.Fatalf("seed Workspace Credential Grant %T: %v", model, err)
		}
	}
	return grantID
}
