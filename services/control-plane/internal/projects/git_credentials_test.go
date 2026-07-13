package projects

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProjectGitCredentialBindingIsPurposeAndScopeSeparated(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "project-git-credential-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	now := time.Now().UTC()
	gitCredentialID := uuid.New()
	providerCredentialID := uuid.New()
	for _, credential := range []persistence.ProviderCredential{
		{
			ID: gitCredentialID, TenantID: domain.TenantID, OrganizationID: &domain.OrganizationID,
			Name: "Git", Purpose: "git", Provider: "git", CredentialType: "https_token",
			EncryptedPayload: []byte("encrypted-git-payload"), EncryptedDataKey: []byte("encrypted-git-data-key"),
			KMSProvider: "local", KMSKeyID: "test", Version: 1,
			CreatedBy: domain.UserID, UpdatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: providerCredentialID, TenantID: domain.TenantID, OrganizationID: &domain.OrganizationID,
			Name: "Provider", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
			EncryptedPayload: []byte("encrypted-provider-payload"), EncryptedDataKey: []byte("encrypted-provider-data-key"),
			KMSProvider: "local", KMSKeyID: "test", Version: 1,
			CreatedBy: domain.UserID, UpdatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := store.DB().Create(&credential).Error; err != nil {
			t.Fatal(err)
		}
	}

	service := NewService(store.DB())
	repositoryURL := "https://git.example.com/team/private.git"
	created, err := service.Create(ctx, principal, domain.TenantID, domain.OrganizationID, CreateProjectInput{
		Name: "Private repository", RepositoryURL: &repositoryURL, DefaultBranch: "main",
		GitCredentialID: &gitCredentialID, Visibility: "organization",
	}, "project-git-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if created.GitCredentialID == nil || *created.GitCredentialID != gitCredentialID {
		t.Fatalf("Project omitted the Git Credential binding: %#v", created)
	}

	_, err = service.Create(ctx, principal, domain.TenantID, domain.OrganizationID, CreateProjectInput{
		Name: "Wrong purpose", RepositoryURL: &repositoryURL, DefaultBranch: "main",
		GitCredentialID: &providerCredentialID, Visibility: "organization",
	}, "project-provider-credential", "127.0.0.1")
	assertProjectProblemCode(t, err, "credential_purpose_mismatch")

	_, err = service.Create(ctx, principal, domain.TenantID, domain.OrganizationID, CreateProjectInput{
		Name: "No repository", DefaultBranch: "main", GitCredentialID: &gitCredentialID, Visibility: "organization",
	}, "project-no-repository", "127.0.0.1")
	assertProjectProblemCode(t, err, "git_credential_requires_https_repository")

	updated, err := service.Update(ctx, principal, domain.TenantID, created.ID, UpdateProjectInput{
		GitCredentialID: OptionalNullableUUID{Set: true},
	}, "project-git-unbind", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.GitCredentialID != nil {
		t.Fatalf("Project Git Credential was not unbound: %#v", updated)
	}
}

func assertProjectProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}
