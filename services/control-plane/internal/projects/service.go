package projects

import (
	"context"
	"errors"
	"net/url"
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
	repository persistence.Repository[persistence.Project]
}

func NewService(db *gorm.DB) *Service {
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db),
		repository: persistence.NewRepository[persistence.Project](db),
	}
}

func toProject(model persistence.Project) Project {
	return Project{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		Name: model.Name, RepositoryURL: model.RepositoryURL, DefaultBranch: model.DefaultBranch,
		GitCredentialID: model.GitCredentialID, Visibility: model.Visibility,
		CreatedBy: model.CreatedBy, CreatedAt: model.CreatedAt,
		UpdatedAt: model.UpdatedAt, ArchivedAt: model.ArchivedAt,
	}
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

	gitCredentialID, err := s.resolveGitCredentialBinding(
		ctx, principal, tenantID, organizationID, repositoryURL, input.GitCredentialID,
	)
	if err != nil {
		return Project{}, false, err
	}

	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "project.create", SuccessStatus: 201,
		Request: map[string]any{
			"tenantId": tenantID, "organizationId": organizationID, "name": name,
			"repositoryUrl": repositoryURL, "defaultBranch": defaultBranch,
			"gitCredentialId": gitCredentialID, "visibility": visibility,
		},
	}, func(tx *gorm.DB) (Project, error) {
		gitCredentialID, err := s.validateGitCredentialBinding(
			ctx, tx, tenantID, organizationID, repositoryURL, gitCredentialID,
		)
		if err != nil {
			return Project{}, err
		}
		model := persistence.Project{
			ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID,
			Name: name, RepositoryURL: repositoryURL, DefaultBranch: defaultBranch,
			GitCredentialID: gitCredentialID,
			Visibility:      visibility, CreatedBy: principal.UserID,
		}
		if err := tx.Create(&model).Error; err != nil {
			return Project{}, problem.Wrap(409, "project_create_rejected", "Project creation was rejected by a tenant isolation constraint.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "project.created", ResourceType: "project", ResourceID: &model.ID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"visibility": visibility, "gitCredentialId": gitCredentialID},
		}); err != nil {
			return Project{}, err
		}
		return toProject(model), nil
	})
	if err != nil {
		return Project{}, false, err
	}
	return result.Value, result.Replayed, nil
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
	query := s.db.WithContext(ctx).Model(&persistence.Project{}).
		Where("tenant_id = ? AND organization_id = ? AND archived_at IS NULL", tenantID, organizationID)
	if !authorization.TenantAllows(access.TenantRole, authorization.ProjectRead) {
		query = query.Where("visibility <> ? OR created_by = ?", "private", principal.UserID)
	}
	models := make([]persistence.Project, 0)
	if err := query.Order("LOWER(name), id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "projects_load_failed", "Failed to load projects.", err)
	}
	items := make([]Project, 0, len(models))
	for _, model := range models {
		items = append(items, toProject(model))
	}
	return items, nil
}

func (s *Service) Get(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
) (Project, error) {
	model, err := s.repository.First(ctx,
		persistence.TenantScope(tenantID),
		func(db *gorm.DB) *gorm.DB { return db.Where("id = ? AND archived_at IS NULL", projectID) },
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Project{}, problem.New(404, "project_not_found", "Project not found.")
	}
	if err != nil {
		return Project{}, problem.Wrap(500, "project_load_failed", "Failed to load the project.", err)
	}
	access, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, model.OrganizationID, authorization.ProjectRead)
	if err != nil {
		return Project{}, err
	}
	if model.Visibility == "private" && model.CreatedBy != principal.UserID && !authorization.TenantAllows(access.TenantRole, authorization.ProjectRead) {
		return Project{}, problem.New(404, "project_not_found", "Project not found.")
	}
	return toProject(model), nil
}

func (s *Service) Update(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
	input UpdateProjectInput,
	requestID, ipAddress string,
) (Project, error) {
	current, err := s.Get(ctx, principal, tenantID, projectID)
	if err != nil {
		return Project{}, err
	}
	if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, current.OrganizationID, authorization.ProjectUpdate); err != nil {
		return Project{}, err
	}
	updates := map[string]any{}
	repositoryURL := current.RepositoryURL
	gitCredentialID := current.GitCredentialID
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
	if input.GitCredentialID.Set {
		gitCredentialID = input.GitCredentialID.Value
		updates["git_credential_id"] = input.GitCredentialID.Value
	}
	if (input.RepositoryURL != nil || input.GitCredentialID.Set) && gitCredentialID != nil {
		resolved, resolveErr := s.resolveGitCredentialBinding(
			ctx, principal, tenantID, current.OrganizationID, repositoryURL, gitCredentialID,
		)
		if resolveErr != nil {
			return Project{}, resolveErr
		}
		updates["git_credential_id"] = *resolved
	}
	if len(updates) == 0 {
		return Project{}, problem.New(400, "empty_update", "Provide at least one project field to update.")
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if gitCredentialID != nil {
			if _, validateErr := s.validateGitCredentialBinding(
				ctx, tx, tenantID, current.OrganizationID, repositoryURL, gitCredentialID,
			); validateErr != nil {
				return validateErr
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

func (s *Service) resolveGitCredentialBinding(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID uuid.UUID,
	repositoryURL *string,
	credentialID *uuid.UUID,
) (*uuid.UUID, error) {
	if credentialID == nil || *credentialID == uuid.Nil {
		return nil, nil
	}
	if repositoryURL == nil || !isHTTPSRepository(*repositoryURL) {
		return nil, problem.New(409, "git_credential_requires_https_repository", "Git Credential requires an HTTPS Project repository URL.")
	}
	if _, err := s.authorizer.RequireOrganization(
		ctx, principal.UserID, tenantID, organizationID, authorization.CredentialsUse,
	); err != nil {
		return nil, err
	}
	return s.validateGitCredentialBinding(ctx, s.db, tenantID, organizationID, repositoryURL, credentialID)
}

func (s *Service) validateGitCredentialBinding(
	ctx context.Context,
	db *gorm.DB,
	tenantID, organizationID uuid.UUID,
	repositoryURL *string,
	credentialID *uuid.UUID,
) (*uuid.UUID, error) {
	if credentialID == nil || *credentialID == uuid.Nil {
		return nil, nil
	}
	if repositoryURL == nil || !isHTTPSRepository(*repositoryURL) {
		return nil, problem.New(409, "git_credential_requires_https_repository", "Git Credential requires an HTTPS Project repository URL.")
	}
	var credential persistence.ProviderCredential
	err := persistence.WithLocking(db.WithContext(ctx), "SHARE", "").
		Select("id", "tenant_id", "organization_id", "purpose", "provider", "credential_type", "expires_at", "revoked_at").
		Where("tenant_id = ? AND id = ?", tenantID, *credentialID).
		Take(&credential).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, problem.New(404, "credential_not_found", "Git Credential not found.")
	}
	if err != nil {
		return nil, problem.Wrap(500, "credential_load_failed", "Git Credential could not be loaded.", err)
	}
	if credential.Purpose != "git" || credential.Provider != "git" || credential.CredentialType != "https_token" {
		return nil, problem.New(409, "credential_purpose_mismatch", "Project requires a Git https_token Credential.")
	}
	if credential.OrganizationID != nil && *credential.OrganizationID != organizationID {
		return nil, problem.New(404, "credential_not_found", "Git Credential not found.")
	}
	if credential.RevokedAt != nil || (credential.ExpiresAt != nil && !credential.ExpiresAt.After(time.Now().UTC())) {
		return nil, problem.New(409, "credential_unavailable", "Git Credential is revoked or expired.")
	}
	value := credential.ID
	return &value, nil
}

func isHTTPSRepository(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.Opaque == "" && strings.EqualFold(parsed.Scheme, "https") &&
		parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
		(parsed.Port() == "" || parsed.Port() == "443")
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
