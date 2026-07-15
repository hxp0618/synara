package projects

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	apiidempotency "github.com/synara-ai/synara/services/control-plane/internal/idempotency"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
}

func NewService(db *gorm.DB) *Service {
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db),
	}
}

var projectSelectColumns = []string{
	"id", "tenant_id", "organization_id", "name", "repository_url", "default_branch",
	"visibility", "created_by", "created_at", "updated_at", "archived_at",
}

type activeGitFetchBinding struct {
	ProjectID     uuid.UUID `gorm:"column:project_id"`
	CredentialID  uuid.UUID `gorm:"column:credential_id"`
	SelectorValue string    `gorm:"column:selector_value"`
}

func toProject(model persistence.Project, gitCredentialID *uuid.UUID) Project {
	return Project{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		Name: model.Name, RepositoryURL: model.RepositoryURL, DefaultBranch: model.DefaultBranch,
		GitCredentialID: gitCredentialID, Visibility: model.Visibility,
		CreatedBy: model.CreatedBy, CreatedAt: model.CreatedAt,
		UpdatedAt: model.UpdatedAt, ArchivedAt: model.ArchivedAt,
	}
}

func credentialBindingAPIRequired() error {
	return problem.New(
		409,
		"credential_binding_api_required",
		"Project Git Credentials must be managed through the Credential Binding API.",
	)
}

func normalizeVisibility(value, fallback string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = fallback
	}
	if value != "private" && value != "organization" && value != "tenant" {
		return "", problem.New(400, "invalid_project_visibility", "Project visibility must be private, organization, or tenant.")
	}
	return value, nil
}

func normalizeBranch(value string) (string, error) {
	branch, err := gitpolicy.NormalizeBranch(value, "main")
	if err != nil {
		return "", problem.New(400, "invalid_default_branch", "Default branch is not a valid Git branch name.")
	}
	return branch, nil
}

func normalizeRepositoryURL(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil, nil
	}
	if len(normalized) > 2048 || strings.ContainsAny(normalized, "\r\n\t ") {
		return nil, problem.New(400, "invalid_repository_url", "Repository URL is invalid.")
	}
	return &normalized, nil
}

func (s *Service) Create(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID uuid.UUID,
	input CreateProjectInput,
	requestID, ipAddress string,
) (Project, error) {
	item, _, err := s.CreateWithIdempotency(
		ctx, principal, tenantID, organizationID, input, "", requestID, ipAddress,
	)
	return item, err
}

func (s *Service) CreateWithIdempotency(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID uuid.UUID,
	input CreateProjectInput,
	idempotencyKey, requestID, ipAddress string,
) (Project, bool, error) {
	if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, organizationID, authorization.ProjectCreate); err != nil {
		return Project{}, false, err
	}
	if input.GitCredentialID.Set {
		return Project{}, false, credentialBindingAPIRequired()
	}
	name, err := validation.Name(input.Name, "invalid_project_name", "Project name", 200)
	if err != nil {
		return Project{}, false, err
	}
	repositoryURL, err := normalizeRepositoryURL(input.RepositoryURL)
	if err != nil {
		return Project{}, false, err
	}
	defaultBranch, err := normalizeBranch(input.DefaultBranch)
	if err != nil {
		return Project{}, false, err
	}
	visibility, err := normalizeVisibility(input.Visibility, "organization")
	if err != nil {
		return Project{}, false, err
	}

	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "project.create", SuccessStatus: 201,
		Request: map[string]any{
			"tenantId": tenantID, "organizationId": organizationID, "name": name,
			"repositoryUrl": repositoryURL, "defaultBranch": defaultBranch,
			"visibility": visibility,
		},
	}, func(tx *gorm.DB) (Project, error) {
		model := persistence.Project{
			ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID,
			Name: name, RepositoryURL: repositoryURL, DefaultBranch: defaultBranch,
			Visibility: visibility, CreatedBy: principal.UserID,
		}
		if err := tx.Omit("GitCredentialID").Create(&model).Error; err != nil {
			return Project{}, problem.Wrap(409, "project_create_rejected", "Project creation was rejected by a tenant isolation constraint.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "project.created", ResourceType: "project", ResourceID: &model.ID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"visibility": visibility},
		}); err != nil {
			return Project{}, err
		}
		return toProject(model, nil), nil
	})
	if err != nil {
		return Project{}, false, err
	}
	item, err := s.projectWithGitFetchBinding(ctx, result.Value)
	if err != nil {
		return Project{}, false, err
	}
	return item, result.Replayed, nil
}

func (s *Service) List(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID uuid.UUID,
) ([]Project, error) {
	access, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, organizationID, authorization.ProjectRead)
	if err != nil {
		return nil, err
	}
	query := s.db.WithContext(ctx).Model(&persistence.Project{}).Select(projectSelectColumns).
		Where("tenant_id = ? AND organization_id = ? AND archived_at IS NULL", tenantID, organizationID)
	if !authorization.TenantAllows(access.TenantRole, authorization.ProjectRead) {
		query = query.Where("visibility <> ? OR created_by = ?", "private", principal.UserID)
	}
	models := make([]persistence.Project, 0)
	if err := query.Order("LOWER(name), id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "projects_load_failed", "Failed to load projects.", err)
	}
	return s.projectsWithGitFetchBindings(ctx, models)
}

func (s *Service) Get(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
) (Project, error) {
	model, err := s.loadAuthorizedProjectModel(ctx, principal, tenantID, projectID)
	if err != nil {
		return Project{}, err
	}
	return s.projectModelWithGitFetchBinding(ctx, model)
}

func (s *Service) loadAuthorizedProjectModel(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
) (persistence.Project, error) {
	var model persistence.Project
	err := s.db.WithContext(ctx).Select(projectSelectColumns).
		Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, projectID).
		Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.Project{}, problem.New(404, "project_not_found", "Project not found.")
	}
	if err != nil {
		return persistence.Project{}, problem.Wrap(500, "project_load_failed", "Failed to load the project.", err)
	}
	access, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, model.OrganizationID, authorization.ProjectRead)
	if err != nil {
		return persistence.Project{}, err
	}
	if model.Visibility == "private" && model.CreatedBy != principal.UserID && !authorization.TenantAllows(access.TenantRole, authorization.ProjectRead) {
		return persistence.Project{}, problem.New(404, "project_not_found", "Project not found.")
	}
	return model, nil
}

func (s *Service) Update(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
	input UpdateProjectInput,
	requestID, ipAddress string,
) (Project, error) {
	currentModel, err := s.loadAuthorizedProjectModel(ctx, principal, tenantID, projectID)
	if err != nil {
		return Project{}, err
	}
	current := toProject(currentModel, nil)
	if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, current.OrganizationID, authorization.ProjectUpdate); err != nil {
		return Project{}, err
	}
	if input.GitCredentialID.Set {
		return Project{}, credentialBindingAPIRequired()
	}
	if input.RepositoryURL == nil {
		if _, err := s.projectModelWithGitFetchBinding(ctx, currentModel); err != nil {
			return Project{}, err
		}
	}
	updates := map[string]any{}
	repositoryURL := current.RepositoryURL
	if input.Name != nil {
		name, err := validation.Name(*input.Name, "invalid_project_name", "Project name", 200)
		if err != nil {
			return Project{}, err
		}
		updates["name"] = name
	}
	if input.RepositoryURL != nil {
		normalizedRepositoryURL, err := normalizeRepositoryURL(input.RepositoryURL)
		if err != nil {
			return Project{}, err
		}
		repositoryURL = normalizedRepositoryURL
		updates["repository_url"] = normalizedRepositoryURL
	}
	if input.DefaultBranch != nil {
		branch, err := normalizeBranch(*input.DefaultBranch)
		if err != nil {
			return Project{}, err
		}
		updates["default_branch"] = branch
	}
	if input.Visibility != nil {
		visibility, err := normalizeVisibility(*input.Visibility, "")
		if err != nil {
			return Project{}, err
		}
		updates["visibility"] = visibility
	}
	if len(updates) == 0 {
		return Project{}, problem.New(400, "empty_update", "Provide at least one project field to update.")
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var locked persistence.Project
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Select(projectSelectColumns).
			Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, projectID).
			Take(&locked).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "project_not_found", "Project not found.")
		} else if err != nil {
			return problem.Wrap(500, "project_load_failed", "Failed to lock the Project for update.", err)
		}
		if input.RepositoryURL != nil {
			if err := s.ensureRepositoryMatchesActiveGitBindings(ctx, tx, tenantID, projectID, repositoryURL); err != nil {
				return err
			}
		}
		result := tx.Model(&persistence.Project{}).
			Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, projectID).
			Updates(updates)
		if result.Error != nil {
			return problem.Wrap(409, "project_update_rejected", "Project update was rejected.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(404, "project_not_found", "Project not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "project.updated", ResourceType: "project", ResourceID: &projectID,
			OrganizationID: &current.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		})
	})
	if err != nil {
		return Project{}, err
	}
	return s.Get(ctx, principal, tenantID, projectID)
}

func (s *Service) projectWithGitFetchBinding(ctx context.Context, project Project) (Project, error) {
	project.GitCredentialID = nil
	bindings, err := s.loadActiveGitFetchBindings(ctx, s.db, project.TenantID, []uuid.UUID{project.ID})
	if err != nil {
		return Project{}, err
	}
	if err := applyGitFetchBinding(&project, bindings[project.ID]); err != nil {
		return Project{}, err
	}
	return project, nil
}

func (s *Service) projectModelWithGitFetchBinding(ctx context.Context, model persistence.Project) (Project, error) {
	return s.projectWithGitFetchBinding(ctx, toProject(model, nil))
}

func (s *Service) projectsWithGitFetchBindings(ctx context.Context, models []persistence.Project) ([]Project, error) {
	if len(models) == 0 {
		return []Project{}, nil
	}
	projectIDs := make([]uuid.UUID, 0, len(models))
	for _, model := range models {
		projectIDs = append(projectIDs, model.ID)
	}
	bindings, err := s.loadActiveGitFetchBindings(ctx, s.db, models[0].TenantID, projectIDs)
	if err != nil {
		return nil, err
	}
	items := make([]Project, 0, len(models))
	for _, model := range models {
		item := toProject(model, nil)
		if err := applyGitFetchBinding(&item, bindings[model.ID]); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) loadActiveGitFetchBindings(
	ctx context.Context,
	db *gorm.DB,
	tenantID uuid.UUID,
	projectIDs []uuid.UUID,
) (map[uuid.UUID][]activeGitFetchBinding, error) {
	result := make(map[uuid.UUID][]activeGitFetchBinding, len(projectIDs))
	if len(projectIDs) == 0 {
		return result, nil
	}
	var bindings []activeGitFetchBinding
	err := db.WithContext(ctx).Model(&persistence.CredentialBinding{}).
		Select("project_id", "credential_id", "selector_value").
		Where(
			"tenant_id = ? AND project_id IN ? AND binding_kind = ? AND disabled_at IS NULL",
			tenantID, projectIDs, "git_fetch",
		).
		Order("project_id, id").Find(&bindings).Error
	if err != nil {
		return nil, problem.Wrap(500, "project_git_binding_load_failed", "Project Git Credential Binding could not be loaded.", err)
	}
	for _, binding := range bindings {
		result[binding.ProjectID] = append(result[binding.ProjectID], binding)
	}
	return result, nil
}

func applyGitFetchBinding(project *Project, bindings []activeGitFetchBinding) error {
	if len(bindings) > 1 {
		return problem.New(409, "project_git_binding_ambiguous", "Project has multiple active Git fetch Credential Bindings.")
	}
	if len(bindings) == 0 {
		project.GitCredentialID = nil
		return nil
	}
	binding := bindings[0]
	if project.RepositoryURL == nil || binding.SelectorValue != *project.RepositoryURL {
		return problem.New(409, "project_git_binding_selector_drift", "Project repository does not match its active Git fetch Credential Binding selector.")
	}
	credentialID := binding.CredentialID
	project.GitCredentialID = &credentialID
	return nil
}

func (s *Service) ensureRepositoryMatchesActiveGitBindings(
	ctx context.Context,
	db *gorm.DB,
	tenantID, projectID uuid.UUID,
	repositoryURL *string,
) error {
	var selectors []string
	err := persistence.WithLocking(db.WithContext(ctx), "SHARE", "").Model(&persistence.CredentialBinding{}).
		Where(
			"tenant_id = ? AND project_id = ? AND binding_kind IN ? AND disabled_at IS NULL",
			tenantID, projectID, []string{"git_fetch", "git_push"},
		).
		Order("binding_kind, id").Pluck("selector_value", &selectors).Error
	if err != nil {
		return problem.Wrap(500, "project_git_binding_load_failed", "Project Git Credential Bindings could not be loaded.", err)
	}
	for _, selector := range selectors {
		if repositoryURL == nil || selector != *repositoryURL {
			return problem.New(
				409,
				"credential_binding_disable_required",
				"Disable active Project Git Credential Bindings before changing the repository URL.",
			)
		}
	}
	return nil
}

func (s *Service) Archive(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
	requestID, ipAddress string,
) error {
	current, err := s.Get(ctx, principal, tenantID, projectID)
	if err != nil {
		return err
	}
	if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, current.OrganizationID, authorization.ProjectDelete); err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var activeSessions int64
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND project_id = ? AND archived_at IS NULL", tenantID, projectID).
			Count(&activeSessions).Error; err != nil {
			return problem.Wrap(500, "project_archive_failed", "Failed to inspect project sessions.", err)
		}
		if activeSessions > 0 {
			return problem.New(409, "project_has_active_sessions", "Archive active project sessions before archiving the project.")
		}
		now := time.Now().UTC()
		result := tx.Model(&persistence.Project{}).
			Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, projectID).
			Update("archived_at", now)
		if result.Error != nil {
			return problem.Wrap(500, "project_archive_failed", "Failed to archive the project.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(404, "project_not_found", "Project not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "project.archived", ResourceType: "project", ResourceID: &projectID,
			OrganizationID: &current.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		})
	})
}
