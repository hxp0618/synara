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
	if err := fixture.db.Model(&persistence.ProviderCredential{}).Where("id = ?", first.ID).Updates(map[string]any{
		"encrypted_payload":  secondModel.EncryptedPayload,
		"encrypted_data_key": secondModel.EncryptedDataKey,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Resolve(ctx, fixture.tenantID, first.ID)
	assertCredentialProblemCode(t, err, "credential_decryption_failed")
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

func TestGitCredentialWorkerResolutionRequiresProjectBindingHostAndCurrentLease(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	gitCredential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		OrganizationID: &fixture.organizationID,
		Name:           "Private Git",
		Purpose:        PurposeGit,
		Provider:       GitProvider,
		CredentialType: GitHTTPSCredentialType,
		Payload: map[string]any{
			"host": "git.example.com", "username": "synara", "token": "git-secret-token",
		},
	}, "git-worker-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	worker, executionID, projectID, leaseToken := seedGitCredentialExecution(t, fixture, gitCredential.ID)
	executionService := executions.NewService(fixture.db, nil, 30*time.Second, 90*time.Second, 24*time.Hour, nil, nil)
	lease := executions.LeaseInput{TenantID: fixture.tenantID, Generation: 1, LeaseToken: leaseToken}

	payload, err := fixture.service.ResolveGitForExecution(ctx, executionService, worker, executionID, gitCredential.ID, lease)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Host != "git.example.com" || payload.Username != "synara" || payload.Token != "git-secret-token" {
		t.Fatalf("unexpected resolved Git Credential: %#v", payload)
	}

	fenced := lease
	fenced.Generation++
	_, err = fixture.service.ResolveGitForExecution(ctx, executionService, worker, executionID, gitCredential.ID, fenced)
	assertCredentialProblemCode(t, err, "generation_fenced")

	otherRepository := "https://other.example.com/team/repository.git"
	if err := fixture.db.Model(&persistence.Project{}).Where("id = ?", projectID).
		Update("repository_url", otherRepository).Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.ResolveGitForExecution(ctx, executionService, worker, executionID, gitCredential.ID, lease)
	assertCredentialProblemCode(t, err, "git_credential_host_mismatch")
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
	payload, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, tenantCredential.ID, lease)
	if err != nil {
		t.Fatal(err)
	}
	if payload["apiKey"] != "tenant-secret" {
		t.Fatalf("unexpected tenant credential payload: %#v", payload)
	}

	invalidToken := lease
	invalidToken.LeaseToken = "wrong-token"
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, tenantCredential.ID, invalidToken)
	assertCredentialProblemCode(t, err, "invalid_lease_token")
	fenced := lease
	fenced.Generation++
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, tenantCredential.ID, fenced)
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
		Update("provider_credential_id", otherCredential.ID).Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.ResolveForExecution(ctx, executionService, worker, executionID, otherCredential.ID, lease)
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
		&persistence.AgentExecution{ID: executionID, TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "running", ExecutionTargetID: fixture.targetID, TargetKind: "local", WorkerID: &workerID, Generation: 1, RequestedBy: fixture.owner.UserID, QueuedAt: now, StartedAt: &now},
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
	credentialID uuid.UUID,
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
			GitCredentialID: &credentialID, Visibility: "organization", CreatedBy: fixture.owner.UserID,
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
