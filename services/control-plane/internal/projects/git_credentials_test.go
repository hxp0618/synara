package projects

import (
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
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProjectGitCredentialUsesCredentialBindingAuthority(t *testing.T) {
	fixture := newProjectGitFixture(t)
	legacyCredentialID := uuid.New()

	_, err := fixture.service.Create(
		fixture.ctx,
		fixture.principal,
		fixture.tenantID,
		fixture.organizationID,
		CreateProjectInput{
			Name: "Legacy bind", RepositoryURL: &fixture.repositoryURL, DefaultBranch: "main",
			GitCredentialID: OptionalNullableUUID{Set: true, Value: &legacyCredentialID},
			Visibility:      "organization",
		},
		"project-legacy-create",
		"127.0.0.1",
	)
	assertProjectProblemCode(t, err, "credential_binding_api_required")

	var createWithNull CreateProjectInput
	if err := json.Unmarshal([]byte(`{"name":"Legacy null","gitCredentialId":null}`), &createWithNull); err != nil {
		t.Fatal(err)
	}
	if !createWithNull.GitCredentialID.Set || createWithNull.GitCredentialID.Value != nil {
		t.Fatalf("legacy create field presence was not retained: %#v", createWithNull.GitCredentialID)
	}
	_, err = fixture.service.Create(
		fixture.ctx,
		fixture.principal,
		fixture.tenantID,
		fixture.organizationID,
		createWithNull,
		"project-legacy-null-create",
		"127.0.0.1",
	)
	assertProjectProblemCode(t, err, "credential_binding_api_required")

	project := fixture.createProject(t, "Binding authority")
	credential := fixture.createGitCredential(t, "Primary Git")
	binding := fixture.createGitFetchBinding(t, project.ID, credential.ID, fixture.repositoryURL)

	loaded, err := fixture.service.Get(fixture.ctx, fixture.principal, fixture.tenantID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GitCredentialID == nil || *loaded.GitCredentialID != credential.ID {
		t.Fatalf("Project response did not derive Git Credential from Binding: %#v", loaded)
	}
	items, err := fixture.service.List(fixture.ctx, fixture.principal, fixture.tenantID, fixture.organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].GitCredentialID == nil || *items[0].GitCredentialID != credential.ID {
		t.Fatalf("Project list did not derive Git Credential from Binding: %#v", items)
	}

	var stored persistence.Project
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, project.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.GitCredentialID != nil {
		t.Fatalf("legacy Project Git Credential column was populated: %#v", stored.GitCredentialID)
	}

	_, err = fixture.service.Update(
		fixture.ctx,
		fixture.principal,
		fixture.tenantID,
		project.ID,
		UpdateProjectInput{GitCredentialID: OptionalNullableUUID{Set: true}},
		"project-legacy-unbind",
		"127.0.0.1",
	)
	assertProjectProblemCode(t, err, "credential_binding_api_required")

	sameRepository := fixture.repositoryURL
	if _, err := fixture.service.Update(
		fixture.ctx,
		fixture.principal,
		fixture.tenantID,
		project.ID,
		UpdateProjectInput{RepositoryURL: &sameRepository},
		"project-same-repository",
		"127.0.0.1",
	); err != nil {
		t.Fatalf("same repository selector was rejected: %v", err)
	}
	changedRepository := "https://git.example.com/team/changed.git"
	_, err = fixture.service.Update(
		fixture.ctx,
		fixture.principal,
		fixture.tenantID,
		project.ID,
		UpdateProjectInput{RepositoryURL: &changedRepository},
		"project-changed-repository",
		"127.0.0.1",
	)
	assertProjectProblemCode(t, err, "credential_binding_disable_required")

	if err := fixture.db.Model(&persistence.Project{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, project.ID).
		Update("git_credential_id", credential.ID).Error; err == nil {
		t.Fatal("SQLite accepted a non-NULL write to retired projects.git_credential_id")
	}
	if err := fixture.db.Model(&persistence.Project{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, project.ID).
		Update("repository_url", changedRepository).Error; err == nil {
		t.Fatal("SQLite allowed repository selector drift while a Git Binding was active")
	}

	if binding.ProjectID == nil || *binding.ProjectID != project.ID {
		t.Fatalf("unexpected Binding owner: %#v", binding)
	}
}

func TestProjectOperationsRejectCrossTenantNestedIDSubstitution(t *testing.T) {
	fixture := newProjectGitFixture(t)
	project := fixture.createProject(t, "Tenant A project")
	otherTenant, err := tenancy.NewService(fixture.db).CreateTenant(
		fixture.ctx,
		fixture.principal,
		tenancy.CreateTenantInput{
			Slug: "project-other-" + uuid.NewString()[:8], Name: "Project Other Tenant",
			PlanCode: "free", Status: "active",
		},
		"project-other-tenant",
		"127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	otherPrincipal := fixture.principal
	otherPrincipal.ActiveTenantID = &otherTenant.ID

	_, err = fixture.service.Get(fixture.ctx, otherPrincipal, otherTenant.ID, project.ID)
	assertProjectProblemCode(t, err, "project_not_found")
	updatedName := "Cross-Tenant Rename"
	_, err = fixture.service.Update(
		fixture.ctx, otherPrincipal, otherTenant.ID, project.ID,
		UpdateProjectInput{Name: &updatedName}, "project-cross-tenant-update", "127.0.0.1",
	)
	assertProjectProblemCode(t, err, "project_not_found")
	err = fixture.service.Archive(
		fixture.ctx, otherPrincipal, otherTenant.ID, project.ID,
		"project-cross-tenant-archive", "127.0.0.1",
	)
	assertProjectProblemCode(t, err, "project_not_found")
	_, err = fixture.service.List(
		fixture.ctx, otherPrincipal, otherTenant.ID, fixture.organizationID,
	)
	assertProjectProblemCode(t, err, "organization_not_found")
	_, err = fixture.service.ListPage(
		fixture.ctx, otherPrincipal, otherTenant.ID, fixture.organizationID, ProjectListQuery{},
	)
	assertProjectProblemCode(t, err, "organization_not_found")
	_, err = fixture.service.Create(
		fixture.ctx, otherPrincipal, otherTenant.ID, fixture.organizationID,
		CreateProjectInput{Name: "Cross-Tenant Create"},
		"project-cross-tenant-create", "127.0.0.1",
	)
	assertProjectProblemCode(t, err, "organization_not_found")
	_, _, err = fixture.service.UpdateWithIdempotency(
		fixture.ctx, otherPrincipal, otherTenant.ID, project.ID,
		UpdateProjectInput{Name: &updatedName}, "project-cross-tenant-update-idempotent",
		"project-cross-tenant-update-idempotent", "127.0.0.1",
	)
	assertProjectProblemCode(t, err, "project_not_found")
	_, _, err = fixture.service.ArchiveWithIdempotency(
		fixture.ctx, otherPrincipal, otherTenant.ID, project.ID,
		"project-cross-tenant-archive-idempotent", "project-cross-tenant-archive-idempotent", "127.0.0.1",
	)
	assertProjectProblemCode(t, err, "project_not_found")
	_, _, err = fixture.service.CreateWithIdempotency(
		fixture.ctx, otherPrincipal, otherTenant.ID, fixture.organizationID,
		CreateProjectInput{Name: "Cross-Tenant Idempotent Create"},
		"project-cross-tenant-create-idempotent", "project-cross-tenant-create-idempotent", "127.0.0.1",
	)
	assertProjectProblemCode(t, err, "organization_not_found")

	var original persistence.Project
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, project.ID).
		Take(&original).Error; err != nil {
		t.Fatal(err)
	}
	if original.Name != "Tenant A project" || original.ArchivedAt != nil {
		t.Fatalf("cross-Tenant operations mutated original Project: %#v", original)
	}
}

func TestProjectOperationsRejectInactiveTenantContextWithoutMutation(t *testing.T) {
	fixture := newProjectGitFixture(t)
	otherTenant, err := tenancy.NewService(fixture.db).CreateTenant(
		fixture.ctx,
		fixture.principal,
		tenancy.CreateTenantInput{
			Slug: "project-inactive-" + uuid.NewString()[:8], Name: "Project Inactive Tenant",
			PlanCode: "free", Status: "active",
		},
		"project-inactive-tenant",
		"127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	var root persistence.Organization
	if err := fixture.db.Where("tenant_id = ? AND kind = ?", otherTenant.ID, "root").Take(&root).Error; err != nil {
		t.Fatal(err)
	}
	otherPrincipal := fixture.principal
	otherPrincipal.ActiveTenantID = &otherTenant.ID
	project, err := fixture.service.Create(
		fixture.ctx, otherPrincipal, otherTenant.ID, root.ID,
		CreateProjectInput{Name: "Inactive context target"},
		"project-inactive-setup", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}

	var projectsBefore, auditsBefore int64
	if err := fixture.db.Model(&persistence.Project{}).Where("tenant_id = ?", otherTenant.ID).
		Count(&projectsBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AuditLog{}).Where("tenant_id = ?", otherTenant.ID).
		Count(&auditsBefore).Error; err != nil {
		t.Fatal(err)
	}

	_, err = fixture.service.Get(fixture.ctx, fixture.principal, otherTenant.ID, project.ID)
	assertProjectProblemCode(t, err, "tenant_not_found")
	_, err = fixture.service.List(fixture.ctx, fixture.principal, otherTenant.ID, root.ID)
	assertProjectProblemCode(t, err, "tenant_not_found")
	_, err = fixture.service.Create(
		fixture.ctx, fixture.principal, otherTenant.ID, root.ID,
		CreateProjectInput{Name: "Inactive context create"},
		"project-inactive-create", "127.0.0.1",
	)
	assertProjectProblemCode(t, err, "tenant_not_found")
	updatedName := "Inactive context update"
	_, err = fixture.service.Update(
		fixture.ctx, fixture.principal, otherTenant.ID, project.ID,
		UpdateProjectInput{Name: &updatedName}, "project-inactive-update", "127.0.0.1",
	)
	assertProjectProblemCode(t, err, "tenant_not_found")
	err = fixture.service.Archive(
		fixture.ctx, fixture.principal, otherTenant.ID, project.ID,
		"project-inactive-archive", "127.0.0.1",
	)
	assertProjectProblemCode(t, err, "tenant_not_found")

	var persisted persistence.Project
	if err := fixture.db.Where("tenant_id = ? AND id = ?", otherTenant.ID, project.ID).
		Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Name != "Inactive context target" || persisted.ArchivedAt != nil {
		t.Fatalf("inactive Tenant context mutated Project: %#v", persisted)
	}
	var projectsAfter, auditsAfter int64
	if err := fixture.db.Model(&persistence.Project{}).Where("tenant_id = ?", otherTenant.ID).
		Count(&projectsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AuditLog{}).Where("tenant_id = ?", otherTenant.ID).
		Count(&auditsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if projectsAfter != projectsBefore || auditsAfter != auditsBefore {
		t.Fatalf(
			"inactive Tenant context changed durable counts: projects %d->%d audits %d->%d",
			projectsBefore, projectsAfter, auditsBefore, auditsAfter,
		)
	}
}

func TestProjectGitCredentialProjectionFailsClosedOnDriftAndAmbiguity(t *testing.T) {
	fixture := newProjectGitFixture(t)
	project := fixture.createProject(t, "Fail closed")
	firstCredential := fixture.createGitCredential(t, "First Git")
	driftRepository := "https://git.example.com/team/drift.git"

	fixture.dropGitSelectorTrigger(t)
	fixture.createGitFetchBinding(t, project.ID, firstCredential.ID, driftRepository)
	fixture.remigrate(t)
	_, err := fixture.service.Get(fixture.ctx, fixture.principal, fixture.tenantID, project.ID)
	assertProjectProblemCode(t, err, "project_git_binding_selector_drift")
	repaired, err := fixture.service.Update(
		fixture.ctx,
		fixture.principal,
		fixture.tenantID,
		project.ID,
		UpdateProjectInput{RepositoryURL: &driftRepository},
		"project-repair-selector-drift",
		"127.0.0.1",
	)
	if err != nil {
		t.Fatalf("repository update matching the exact active selector was rejected: %v", err)
	}
	if repaired.GitCredentialID == nil || *repaired.GitCredentialID != firstCredential.ID {
		t.Fatalf("repaired Project did not project its active Git Binding: %#v", repaired)
	}

	secondCredential := fixture.createGitCredential(t, "Second Git")
	fixture.dropGitSelectorTrigger(t)
	fixture.createGitFetchBinding(t, project.ID, secondCredential.ID, "https://git.example.com/team/other.git")
	fixture.remigrate(t)
	_, err = fixture.service.Get(fixture.ctx, fixture.principal, fixture.tenantID, project.ID)
	assertProjectProblemCode(t, err, "project_git_binding_ambiguous")
	_, err = fixture.service.List(fixture.ctx, fixture.principal, fixture.tenantID, fixture.organizationID)
	assertProjectProblemCode(t, err, "project_git_binding_ambiguous")
}

type projectGitFixture struct {
	ctx            context.Context
	db             *gorm.DB
	store          database.MetadataStore
	service        *Service
	principal      identity.Principal
	tenantID       uuid.UUID
	organizationID uuid.UUID
	userID         uuid.UUID
	repositoryURL  string
}

func newProjectGitFixture(t *testing.T) projectGitFixture {
	t.Helper()
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "project-git-binding-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	return projectGitFixture{
		ctx: ctx, db: store.DB(), store: store, service: NewService(store.DB()), principal: principal,
		tenantID: domain.TenantID, organizationID: domain.OrganizationID, userID: domain.UserID,
		repositoryURL: "https://git.example.com/team/private.git",
	}
}

func (fixture projectGitFixture) createProject(t *testing.T, name string) Project {
	t.Helper()
	project, err := fixture.service.Create(
		fixture.ctx,
		fixture.principal,
		fixture.tenantID,
		fixture.organizationID,
		CreateProjectInput{
			Name: name, RepositoryURL: &fixture.repositoryURL, DefaultBranch: "main", Visibility: "organization",
		},
		"project-create-"+uuid.NewString(),
		"127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func (fixture projectGitFixture) createGitCredential(t *testing.T, name string) persistence.ProviderCredential {
	t.Helper()
	now := time.Now().UTC()
	credential := persistence.ProviderCredential{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Scope: "organization", Name: name, Purpose: "git", Provider: "git", CredentialType: "https_token",
		EncryptedPayload: []byte("encrypted-git-payload"), EncryptedDataKey: []byte("encrypted-git-data-key"),
		KMSProvider: "local", KMSKeyID: "test", AADVersion: 3, Version: 1,
		CreatedBy: fixture.userID, UpdatedBy: fixture.userID, CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.store.DB().Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	return credential
}

func (fixture projectGitFixture) createGitFetchBinding(
	t *testing.T,
	projectID, credentialID uuid.UUID,
	selector string,
) persistence.CredentialBinding {
	t.Helper()
	binding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		ProjectID: &projectID, CredentialID: credentialID, BindingKind: "git_fetch",
		SelectorValue: selector, CreatedBy: fixture.userID, CreatedAt: time.Now().UTC(),
	}
	if err := fixture.store.DB().Create(&binding).Error; err != nil {
		t.Fatal(err)
	}
	return binding
}

func (fixture projectGitFixture) dropGitSelectorTrigger(t *testing.T) {
	t.Helper()
	if err := fixture.store.DB().Exec(`DROP TRIGGER IF EXISTS trg_credential_bindings_git_selector_insert`).Error; err != nil {
		t.Fatal(err)
	}
}

func (fixture projectGitFixture) remigrate(t *testing.T) {
	t.Helper()
	if err := fixture.store.Migrate(fixture.ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
}

func assertProjectProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}
