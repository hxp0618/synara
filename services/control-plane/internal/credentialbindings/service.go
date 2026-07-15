package credentialbindings

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/credentials"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	BindingGitFetch       = "git_fetch"
	BindingGitPush        = "git_push"
	BindingRegistryPull   = "registry_pull"
	BindingRegistryPush   = "registry_push"
	BindingPackageRead    = "package_read"
	BindingPackagePublish = "package_publish"
	BindingWorkerImage    = "worker_image_pull"
)

type Service struct {
	db          *gorm.DB
	credentials *credentials.Service
	authorizer  *authorization.Authorizer
	now         func() time.Time
}

func NewService(db *gorm.DB, credentialService *credentials.Service) *Service {
	return &Service{
		db: db, credentials: credentialService, authorizer: authorization.NewAuthorizer(db),
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) Create(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input CreateInput,
	requestID, ipAddress string,
) (Binding, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Binding{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsManage); err != nil {
		return Binding{}, err
	}
	if s.credentials == nil {
		return Binding{}, problem.New(503, "credential_service_unavailable", "Credential service is unavailable.")
	}
	kind, err := normalizeBindingKind(input.BindingKind)
	if err != nil {
		return Binding{}, err
	}
	if input.CredentialID == uuid.Nil || (input.ProjectID == nil) == (input.ExecutionTargetID == nil) {
		return Binding{}, problem.New(400, "invalid_credential_binding", "Exactly one Binding owner and one Credential are required.")
	}
	descriptor, err := s.credentials.LoadBindingDescriptor(ctx, tenantID, input.CredentialID)
	if err != nil {
		return Binding{}, err
	}
	owner, err := s.loadAuthorizedOwner(ctx, principal, tenantID, input, kind, descriptor)
	if err != nil {
		return Binding{}, err
	}
	if input.SelectorValue != nil && strings.TrimSpace(*input.SelectorValue) != owner.selector {
		return Binding{}, problem.New(409, "credential_binding_selector_mismatch", "Credential Binding selector does not match its owner or Credential.")
	}
	now := s.now()
	model := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: owner.organizationID,
		ProjectID: input.ProjectID, ExecutionTargetID: input.ExecutionTargetID,
		CredentialID: input.CredentialID, BindingKind: kind, SelectorValue: owner.selector,
		CreatedBy: principal.UserID, CreatedAt: now,
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := lockBindingOwner(ctx, tx, tenantID, model); err != nil {
			return err
		}
		var current persistence.ProviderCredential
		err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND id = ?", tenantID, model.CredentialID).
			Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "credential_not_found", "Credential not found.")
		}
		if err != nil {
			return problem.Wrap(500, "credential_load_failed", "Credential could not be loaded.", err)
		}
		if current.Version != descriptor.Version || current.Purpose != descriptor.Purpose ||
			current.Provider != descriptor.Provider || current.CredentialType != descriptor.CredentialType ||
			current.Scope != descriptor.Scope || current.RevokedAt != nil ||
			(current.ExpiresAt != nil && !current.ExpiresAt.After(now)) {
			return problem.New(409, "credential_unavailable", "Credential changed or became unavailable before Binding creation.")
		}
		if current.Scope == "organization" && (model.OrganizationID == nil || current.OrganizationID == nil ||
			*current.OrganizationID != *model.OrganizationID) {
			return problem.New(409, "credential_binding_scope_mismatch", "Organization Credential does not match the Binding owner.")
		}
		if current.Scope != "organization" && current.Scope != "tenant" {
			return problem.New(409, "credential_binding_scope_mismatch", "Workspace Credentials require Organization or Tenant scope.")
		}
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return problem.Wrap(409, "credential_binding_conflict", "Credential Binding could not be created.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "credential.binding.created", ResourceType: "credential_binding", ResourceID: &model.ID,
			OrganizationID: model.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"bindingKind": model.BindingKind, "credentialId": model.CredentialID,
				"projectId": model.ProjectID, "executionTargetId": model.ExecutionTargetID,
			},
		})
	})
	if err != nil {
		return Binding{}, err
	}
	return toBinding(model), nil
}

func (s *Service) List(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	filter OwnerFilter,
) ([]Binding, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsRead); err != nil {
		return nil, err
	}
	if (filter.ProjectID == nil) == (filter.ExecutionTargetID == nil) {
		return nil, problem.New(400, "invalid_credential_binding_filter", "Exactly one Credential Binding owner filter is required.")
	}
	query := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if filter.ProjectID != nil {
		query = query.Where("project_id = ?", *filter.ProjectID)
	} else {
		query = query.Where("execution_target_id = ?", *filter.ExecutionTargetID)
	}
	var models []persistence.CredentialBinding
	if err := query.Order("disabled_at IS NOT NULL, created_at DESC, id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "credential_bindings_load_failed", "Credential Bindings could not be loaded.", err)
	}
	items := make([]Binding, 0, len(models))
	for _, model := range models {
		items = append(items, toBinding(model))
	}
	return items, nil
}

func (s *Service) Disable(
	ctx context.Context,
	principal identity.Principal,
	tenantID, bindingID uuid.UUID,
	requestID, ipAddress string,
) (Binding, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Binding{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsManage); err != nil {
		return Binding{}, err
	}
	var model persistence.CredentialBinding
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, bindingID).
			Take(&model).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "credential_binding_not_found", "Credential Binding not found.")
		}
		if err != nil {
			return problem.Wrap(500, "credential_binding_load_failed", "Credential Binding could not be loaded.", err)
		}
		if model.DisabledAt != nil {
			return nil
		}
		now := s.now()
		if err := tx.Model(&persistence.CredentialBinding{}).
			Where("tenant_id = ? AND id = ? AND disabled_at IS NULL", tenantID, bindingID).
			Updates(map[string]any{"disabled_at": now, "disabled_by": principal.UserID}).Error; err != nil {
			return problem.Wrap(409, "credential_binding_disable_rejected", "Credential Binding could not be disabled.", err)
		}
		model.DisabledAt, model.DisabledBy = &now, &principal.UserID
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "credential.binding.disabled", ResourceType: "credential_binding", ResourceID: &bindingID,
			OrganizationID: model.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"bindingKind": model.BindingKind},
		})
	})
	if err != nil {
		return Binding{}, err
	}
	return toBinding(model), nil
}

type bindingOwner struct {
	organizationID *uuid.UUID
	selector       string
}

func (s *Service) loadAuthorizedOwner(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input CreateInput,
	kind string,
	descriptor credentials.BindingDescriptor,
) (bindingOwner, error) {
	if input.ProjectID != nil {
		if kind == BindingWorkerImage {
			return bindingOwner{}, problem.New(400, "credential_binding_kind_mismatch", "Worker image Credentials bind to an Execution Target.")
		}
		var project persistence.Project
		err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, *input.ProjectID).Take(&project).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return bindingOwner{}, problem.New(404, "project_not_found", "Project not found.")
		}
		if err != nil {
			return bindingOwner{}, problem.Wrap(500, "project_load_failed", "Project could not be loaded.", err)
		}
		if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, project.OrganizationID, authorization.OrganizationRead); err != nil {
			return bindingOwner{}, err
		}
		organizationID := project.OrganizationID
		if strings.HasPrefix(kind, "git_") {
			if descriptor.Purpose != credentials.PurposeGit {
				return bindingOwner{}, problem.New(409, "credential_binding_kind_mismatch", "Git Binding requires a Git Credential.")
			}
			if project.RepositoryURL == nil || !gitCredentialMatchesRepository(descriptor, *project.RepositoryURL) {
				return bindingOwner{}, problem.New(409, "credential_binding_selector_mismatch", "Git Credential does not match the Project repository.")
			}
			return bindingOwner{organizationID: &organizationID, selector: strings.TrimSpace(*project.RepositoryURL)}, nil
		}
		if !bindingKindMatchesPurpose(kind, descriptor.Purpose) {
			return bindingOwner{}, problem.New(409, "credential_binding_kind_mismatch", "Credential purpose does not match the Binding stage.")
		}
		return bindingOwner{organizationID: &organizationID, selector: descriptor.Selector}, nil
	}

	if kind != BindingWorkerImage {
		return bindingOwner{}, problem.New(400, "credential_binding_kind_mismatch", "This Credential Binding kind requires a Project.")
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return bindingOwner{}, err
	}
	var target persistence.ExecutionTarget
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, *input.ExecutionTargetID).
		Take(&target).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return bindingOwner{}, problem.New(404, "execution_target_not_found", "Execution Target not found.")
	}
	if err != nil {
		return bindingOwner{}, problem.Wrap(500, "execution_target_load_failed", "Execution Target could not be loaded.", err)
	}
	if descriptor.Purpose != credentials.PurposeRegistry {
		return bindingOwner{}, problem.New(409, "credential_binding_kind_mismatch", "Worker image pull requires a Registry Credential.")
	}
	return bindingOwner{organizationID: target.OrganizationID, selector: descriptor.Selector}, nil
}

func lockBindingOwner(ctx context.Context, tx *gorm.DB, tenantID uuid.UUID, binding persistence.CredentialBinding) error {
	if binding.ProjectID != nil {
		var project persistence.Project
		if err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND id = ? AND organization_id = ?", tenantID, *binding.ProjectID, binding.OrganizationID).
			Take(&project).Error; err != nil {
			return problem.Wrap(409, "credential_binding_owner_unavailable", "Credential Binding Project became unavailable.", err)
		}
		return nil
	}
	var target persistence.ExecutionTarget
	if err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
		Where("tenant_id = ? AND id = ?", tenantID, *binding.ExecutionTargetID).
		Take(&target).Error; err != nil {
		return problem.Wrap(409, "credential_binding_owner_unavailable", "Credential Binding Execution Target became unavailable.", err)
	}
	if target.OrganizationID == nil && binding.OrganizationID != nil || target.OrganizationID != nil && binding.OrganizationID == nil ||
		(target.OrganizationID != nil && binding.OrganizationID != nil && *target.OrganizationID != *binding.OrganizationID) {
		return problem.New(409, "credential_binding_scope_mismatch", "Execution Target Organization changed before Binding creation.")
	}
	return nil
}

func normalizeBindingKind(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !bindingKindMatchesPurpose(value, "git") && !bindingKindMatchesPurpose(value, "registry") &&
		!bindingKindMatchesPurpose(value, "package") {
		return "", problem.New(400, "invalid_credential_binding_kind", "Credential Binding kind is invalid.")
	}
	return value, nil
}

func bindingKindMatchesPurpose(kind, purpose string) bool {
	switch purpose {
	case credentials.PurposeGit:
		return kind == BindingGitFetch || kind == BindingGitPush
	case credentials.PurposeRegistry:
		return kind == BindingRegistryPull || kind == BindingRegistryPush || kind == BindingWorkerImage
	case credentials.PurposePackage:
		return kind == BindingPackageRead || kind == BindingPackagePublish
	default:
		return false
	}
}

func gitCredentialMatchesRepository(descriptor credentials.BindingDescriptor, raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path == "" || parsed.Path == "/" {
		return false
	}
	host, err := gitpolicy.NormalizeHostname(parsed.Hostname())
	if err != nil || host != descriptor.EndpointHost {
		return false
	}
	switch descriptor.CredentialType {
	case credentials.GitHTTPSCredentialType:
		return strings.EqualFold(parsed.Scheme, "https") && parsed.User == nil &&
			(parsed.Port() == "" || parsed.Port() == strconv.Itoa(descriptor.EndpointPort))
	case credentials.GitSSHCredentialType:
		if !strings.EqualFold(parsed.Scheme, "ssh") || parsed.User == nil || parsed.User.Username() != descriptor.EndpointUser {
			return false
		}
		if _, present := parsed.User.Password(); present {
			return false
		}
		port := parsed.Port()
		if port == "" {
			port = "22"
		}
		return port == strconv.Itoa(descriptor.EndpointPort)
	default:
		return false
	}
}

func requireActiveTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(403, "tenant_context_required", "The requested Tenant is not active for this session.")
	}
	return nil
}

func toBinding(model persistence.CredentialBinding) Binding {
	return Binding{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		ProjectID: model.ProjectID, ExecutionTargetID: model.ExecutionTargetID,
		CredentialID: model.CredentialID, BindingKind: model.BindingKind,
		SelectorValue: model.SelectorValue, CreatedBy: model.CreatedBy, CreatedAt: model.CreatedAt,
		DisabledAt: model.DisabledAt, DisabledBy: model.DisabledBy,
	}
}
