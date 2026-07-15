package credentialbindings

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/credentials"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestCredentialBindingLifecycleValidatesPurposeSelectorAndAuthorization(t *testing.T) {
	fixture := newBindingFixture(t)
	ctx := context.Background()
	repositoryURL := "https://git.example.com/team/private.git"
	projectID := uuid.New()
	if err := fixture.db.Create(&persistence.Project{
		ID: projectID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		Name: "Binding project", RepositoryURL: &repositoryURL, DefaultBranch: "main",
		Visibility: "private", CreatedBy: fixture.owner.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	gitCredential, err := fixture.credentials.Create(ctx, fixture.owner, fixture.tenantID, credentials.CreateInput{
		OrganizationID: &fixture.organizationID, Scope: "organization", Name: "Project Git",
		Purpose: credentials.PurposeGit, Provider: credentials.GitProvider,
		CredentialType: credentials.GitHTTPSCredentialType,
		Payload: map[string]any{
			"host": "git.example.com", "username": "git", "token": "binding-git-secret",
		},
	}, "binding-git-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		ProjectID: &projectID, CredentialID: gitCredential.ID, BindingKind: BindingGitFetch,
	}, "binding-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if binding.SelectorValue != repositoryURL || binding.ProjectID == nil || *binding.ProjectID != projectID {
		t.Fatalf("unexpected Git Binding: %#v", binding)
	}
	if _, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		ProjectID: &projectID, CredentialID: gitCredential.ID, BindingKind: BindingGitFetch,
	}, "binding-duplicate", "127.0.0.1"); err == nil {
		t.Fatal("duplicate active Git Binding was accepted")
	}

	registryCredential, err := fixture.credentials.Create(ctx, fixture.owner, fixture.tenantID, credentials.CreateInput{
		OrganizationID: &fixture.organizationID, Scope: "organization", Name: "Project Registry",
		Purpose: credentials.PurposeRegistry, Provider: credentials.RegistryProviderOci,
		CredentialType: credentials.RegistryBearerCredentialType,
		Payload:        map[string]any{"host": "registry.example.com", "token": "binding-registry-secret"},
	}, "binding-registry-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	registryBinding, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		ProjectID: &projectID, CredentialID: registryCredential.ID, BindingKind: BindingRegistryPull,
	}, "binding-registry", "127.0.0.1")
	if err != nil || registryBinding.SelectorValue != "registry.example.com" {
		t.Fatalf("Registry Binding = %#v err=%v", registryBinding, err)
	}
	if _, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		ProjectID: &projectID, CredentialID: registryCredential.ID, BindingKind: BindingGitPush,
	}, "binding-wrong-purpose", "127.0.0.1"); bindingProblemCode(err) != "credential_binding_kind_mismatch" {
		t.Fatalf("wrong-purpose Binding error = %v", err)
	}
	wrongSelector := "other.example.com"
	if _, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		ProjectID: &projectID, CredentialID: registryCredential.ID, BindingKind: BindingRegistryPush,
		SelectorValue: &wrongSelector,
	}, "binding-wrong-selector", "127.0.0.1"); bindingProblemCode(err) != "credential_binding_selector_mismatch" {
		t.Fatalf("wrong-selector Binding error = %v", err)
	}
	if _, err := fixture.service.Create(ctx, fixture.member, fixture.tenantID, CreateInput{
		ProjectID: &projectID, CredentialID: registryCredential.ID, BindingKind: BindingRegistryPush,
	}, "binding-member", "127.0.0.1"); bindingProblemCode(err) != "tenant_forbidden" {
		t.Fatalf("member Binding error = %v", err)
	}

	items, err := fixture.service.List(ctx, fixture.owner, fixture.tenantID, OwnerFilter{ProjectID: &projectID})
	if err != nil || len(items) != 2 {
		t.Fatalf("Binding list = %#v err=%v", items, err)
	}
	disabled, err := fixture.service.Disable(
		ctx, fixture.owner, fixture.tenantID, registryBinding.ID, "binding-disable", "127.0.0.1",
	)
	if err != nil || disabled.DisabledAt == nil || disabled.DisabledBy == nil || *disabled.DisabledBy != fixture.owner.UserID {
		t.Fatalf("disabled Binding = %#v err=%v", disabled, err)
	}
	replayed, err := fixture.service.Disable(
		ctx, fixture.owner, fixture.tenantID, registryBinding.ID, "binding-disable-replay", "127.0.0.1",
	)
	if err != nil || replayed.DisabledAt == nil {
		t.Fatalf("idempotent Binding disable = %#v err=%v", replayed, err)
	}
}

func TestWorkerImagePullBindingAllowsOnlyOneActiveBindingPerTarget(t *testing.T) {
	fixture := newBindingFixture(t)
	ctx := context.Background()
	credentialsByHost := make(map[string]uuid.UUID, 2)
	for _, host := range []string{"ghcr.io", "registry.example.com"} {
		credential, err := fixture.credentials.Create(ctx, fixture.owner, fixture.tenantID, credentials.CreateInput{
			OrganizationID: &fixture.organizationID, Scope: "organization", Name: host,
			Purpose: credentials.PurposeRegistry, Provider: credentials.RegistryProviderOci,
			CredentialType: credentials.RegistryBearerCredentialType,
			Payload:        map[string]any{"host": host, "token": "worker-image-secret-" + host},
		}, "worker-image-binding-create-"+host, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		credentialsByHost[host] = credential.ID
	}
	first, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		ExecutionTargetID: &fixture.targetID, CredentialID: credentialsByHost["ghcr.io"],
		BindingKind: BindingWorkerImage,
	}, "worker-image-binding-first", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if first.ExecutionTargetID == nil || *first.ExecutionTargetID != fixture.targetID {
		t.Fatalf("unexpected Worker image pull Binding: %#v", first)
	}
	if _, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		ExecutionTargetID: &fixture.targetID, CredentialID: credentialsByHost["registry.example.com"],
		BindingKind: BindingWorkerImage,
	}, "worker-image-binding-second", "127.0.0.1"); bindingProblemCode(err) != "credential_binding_conflict" {
		t.Fatalf("second active Worker image pull Binding error = %v", err)
	}
}

func TestGitCredentialRepositoryEndpointMatching(t *testing.T) {
	tests := []struct {
		name       string
		descriptor credentials.BindingDescriptor
		repository string
		want       bool
	}{
		{
			name: "https", descriptor: credentials.BindingDescriptor{
				CredentialType: credentials.GitHTTPSCredentialType,
				EndpointHost:   "git.example.com", EndpointPort: 443,
			},
			repository: "https://git.example.com/team/repo.git", want: true,
		},
		{
			name: "ssh", descriptor: credentials.BindingDescriptor{
				CredentialType: credentials.GitSSHCredentialType,
				EndpointHost:   "git.example.com", EndpointPort: 22, EndpointUser: "git",
			},
			repository: "ssh://git@git.example.com/team/repo.git", want: true,
		},
		{
			name: "ssh user mismatch", descriptor: credentials.BindingDescriptor{
				CredentialType: credentials.GitSSHCredentialType,
				EndpointHost:   "git.example.com", EndpointPort: 22, EndpointUser: "git",
			},
			repository: "ssh://root@git.example.com/team/repo.git", want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := gitCredentialMatchesRepository(test.descriptor, test.repository); got != test.want {
				t.Fatalf("match = %t, want %t", got, test.want)
			}
		})
	}
}

type bindingFixture struct {
	db             *gorm.DB
	service        *Service
	credentials    *credentials.Service
	tenantID       uuid.UUID
	organizationID uuid.UUID
	targetID       uuid.UUID
	owner          identity.Principal
	member         identity.Principal
}

func newBindingFixture(t *testing.T) bindingFixture {
	t.Helper()
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "credential-binding-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	memberID := uuid.New()
	if err := store.DB().Create(&persistence.User{
		ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Member",
		Status: "active", EmailVerifiedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	wrapper, err := credentialkms.NewLocalKeyWrapper("binding-test", bytes.Repeat([]byte{0x49}, 32))
	if err != nil {
		t.Fatal(err)
	}
	credentialService := credentials.NewService(store.DB(), credentialkms.NewEnvelopeCipher(wrapper))
	return bindingFixture{
		db: store.DB(), service: NewService(store.DB(), credentialService), credentials: credentialService,
		tenantID: domain.TenantID, organizationID: domain.OrganizationID,
		targetID: domain.ExecutionTargetID,
		owner:    identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		member:   identity.Principal{UserID: memberID, ActiveTenantID: &domain.TenantID},
	}
}

func bindingProblemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}
