package executiontargets

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Target struct {
	ID             uuid.UUID      `json:"id"`
	TenantID       *uuid.UUID     `json:"tenantId"`
	OrganizationID *uuid.UUID     `json:"organizationId"`
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	Status         string         `json:"status"`
	Capabilities   map[string]any `json:"capabilities"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
}

type CreateInput struct {
	OrganizationID *uuid.UUID     `json:"organizationId"`
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	Configuration  map[string]any `json:"configuration"`
	Capabilities   map[string]any `json:"capabilities"`
}

type Binding struct {
	ID   uuid.UUID
	Kind platform.ExecutionTargetKind
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	platform   platform.Config
	cipher     *secret.CursorCipher
}

func NewService(db *gorm.DB, platformConfig platform.Config, cipher *secret.CursorCipher) *Service {
	return &Service{db: db, authorizer: authorization.NewAuthorizer(db), platform: platformConfig, cipher: cipher}
}

func (s *Service) List(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]Target, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return nil, err
	}
	models := make([]persistence.ExecutionTarget, 0)
	err := s.db.WithContext(ctx).
		Where("status <> ? AND (tenant_id = ? OR tenant_id IS NULL)", "disabled", tenantID).
		Order("CASE WHEN tenant_id IS NULL THEN 1 ELSE 0 END, LOWER(name), id").
		Find(&models).Error
	if err != nil {
		return nil, problem.Wrap(500, "execution_targets_load_failed", "Failed to load execution targets.", err)
	}
	items := make([]Target, 0, len(models))
	for _, model := range models {
		items = append(items, toTarget(model))
	}
	return items, nil
}

func (s *Service) Get(ctx context.Context, principal identity.Principal, tenantID, targetID uuid.UUID) (Target, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Target{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return Target{}, err
	}
	model, err := s.loadAccessible(ctx, tenantID, targetID, false)
	if err != nil {
		return Target{}, err
	}
	return toTarget(model), nil
}

func (s *Service) Create(ctx context.Context, principal identity.Principal, tenantID uuid.UUID, input CreateInput) (Target, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Target{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return Target{}, err
	}
	kind, err := platform.ParseExecutionTargetKind(input.Kind)
	if err != nil {
		return Target{}, problem.New(400, "invalid_execution_target_kind", err.Error()+".")
	}
	if platform.IsRemoteTarget(kind) && (!s.platform.LeaseEnabled || !s.platform.FencingEnabled) {
		return Target{}, problem.New(409, "remote_target_protocol_unsupported", "Remote execution targets require lease and fencing support.")
	}
	name, err := validation.Name(input.Name, "invalid_execution_target_name", "Execution target name", 160)
	if err != nil {
		return Target{}, err
	}
	if s.platform.Profile == platform.ProfilePersonal && input.OrganizationID == nil {
		return Target{}, problem.New(400, "personal_execution_target_organization_required", "Personal execution targets must belong to the Personal organization.")
	}
	if input.OrganizationID != nil {
		if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, *input.OrganizationID, authorization.OrganizationRead); err != nil {
			return Target{}, err
		}
	}
	configuration, err := encryptConfiguration(s.cipher, input.Configuration)
	if err != nil {
		return Target{}, err
	}
	capabilities := input.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	capabilities, err = normalizeProviderPolicyCapabilities(capabilities)
	if err != nil {
		return Target{}, err
	}
	if err := validatePublicCapabilities(capabilities); err != nil {
		return Target{}, err
	}
	model := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &tenantID, OrganizationID: input.OrganizationID,
		Kind: string(kind), Name: name, Status: "active", ConfigurationEncrypted: configuration,
		Capabilities: capabilities,
	}
	if err := s.db.WithContext(ctx).Create(&model).Error; err != nil {
		return Target{}, problem.Wrap(409, "execution_target_create_rejected", "Execution target creation was rejected.", err)
	}
	return toTarget(model), nil
}

func (s *Service) UpdateProviderPolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	rawPolicy map[string]any,
) (Target, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Target{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return Target{}, err
	}

	var updated persistence.ExecutionTarget
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.ExecutionTarget
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND (tenant_id = ? OR tenant_id IS NULL)", targetID, tenantID).
			Take(&model).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "execution_target_not_found", "Execution target not found.")
		}
		if err != nil {
			return problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the execution target.", err)
		}
		if model.TenantID == nil {
			return problem.New(403, "shared_execution_target_provider_policy_immutable", "Platform-shared execution target Provider Policy cannot be changed by a tenant.")
		}

		capabilities := make(map[string]any, len(model.Capabilities)+1)
		for key, value := range model.Capabilities {
			capabilities[key] = value
		}
		capabilities["providerPolicy"] = rawPolicy
		normalized, err := normalizeProviderPolicyCapabilities(capabilities)
		if err != nil {
			return err
		}
		requestedPolicy, err := ParseProviderPolicy(normalized)
		if err != nil {
			return err
		}
		currentPolicy, currentErr := ParseProviderPolicy(model.Capabilities)
		if currentErr == nil && slices.Equal(currentPolicy.ExperimentalProviders, requestedPolicy.ExperimentalProviders) {
			updated = model
			return nil
		}

		now := time.Now().UTC()
		model.Capabilities = normalized
		model.UpdatedAt = now
		if err := tx.WithContext(ctx).Model(&model).
			Where("id = ? AND tenant_id = ?", targetID, tenantID).
			Select("capabilities", "updated_at").Updates(&model).Error; err != nil {
			return problem.Wrap(500, "execution_target_provider_policy_update_failed", "Failed to update the execution target Provider Policy.", err)
		}
		reason := "Execution Target Provider Policy changed; re-register the Worker before claiming more executions."
		if err := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("execution_target_id = ? AND current_manifest_id IS NOT NULL AND compatibility_status <> ? AND terminated_at IS NULL", targetID, "revoked").
			Updates(map[string]any{
				"compatibility_status":     "incompatible",
				"compatibility_reason":     reason,
				"compatibility_checked_at": now,
			}).Error; err != nil {
			return problem.Wrap(500, "worker_manifest_invalidation_failed", "Failed to invalidate Workers after the Provider Policy changed.", err)
		}
		updated = model
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	return toTarget(updated), nil
}

func (s *Service) ResolveForSession(ctx context.Context, tenantID, organizationID uuid.UUID, requested *uuid.UUID) (Binding, error) {
	var model persistence.ExecutionTarget
	query := s.db.WithContext(ctx).
		Where("status = ?", "active").
		Where("(tenant_id IS NULL OR tenant_id = ?) AND (organization_id IS NULL OR organization_id = ?)", tenantID, organizationID)
	if requested != nil && *requested != uuid.Nil {
		query = query.Where("id = ?", *requested)
	} else {
		query = query.Order("CASE WHEN tenant_id IS NOT NULL AND organization_id IS NOT NULL THEN 0 WHEN tenant_id IS NOT NULL THEN 1 ELSE 2 END").
			Order("CASE WHEN name = 'local-default' OR name = 'platform-local' THEN 0 ELSE 1 END, LOWER(name), id")
	}
	if err := query.Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Binding{}, problem.New(409, "execution_target_required", "Create or select an active execution target before creating this session.")
	} else if err != nil {
		return Binding{}, problem.Wrap(500, "execution_target_lookup_failed", "Failed to resolve the execution target.", err)
	}
	kind, err := platform.ParseExecutionTargetKind(model.Kind)
	if err != nil {
		return Binding{}, problem.Wrap(500, "invalid_persisted_execution_target", "The persisted execution target kind is invalid.", err)
	}
	return Binding{ID: model.ID, Kind: kind}, nil
}

func (s *Service) ResolveWorkerTarget(ctx context.Context, targetID uuid.UUID, targetKind string) (persistence.ExecutionTarget, platform.ExecutionTargetKind, error) {
	kind, err := platform.ParseExecutionTargetKind(targetKind)
	if err != nil {
		return persistence.ExecutionTarget{}, "", problem.New(400, "invalid_execution_target_kind", err.Error()+".")
	}
	var model persistence.ExecutionTarget
	if err := s.db.WithContext(ctx).Where("id = ? AND status = ?", targetID, "active").Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, "", problem.New(404, "execution_target_not_found", "Execution target not found.")
	} else if err != nil {
		return persistence.ExecutionTarget{}, "", problem.Wrap(500, "execution_target_lookup_failed", "Failed to resolve the execution target.", err)
	}
	if model.Kind != string(kind) {
		return persistence.ExecutionTarget{}, "", problem.New(409, "execution_target_kind_mismatch", "targetKind does not match the persisted execution target.")
	}
	return model, kind, nil
}

func (s *Service) loadAccessible(ctx context.Context, tenantID, targetID uuid.UUID, activeOnly bool) (persistence.ExecutionTarget, error) {
	var model persistence.ExecutionTarget
	query := s.db.WithContext(ctx).Where("id = ? AND (tenant_id = ? OR tenant_id IS NULL)", targetID, tenantID)
	if activeOnly {
		query = query.Where("status = ?", "active")
	}
	if err := query.Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, problem.New(404, "execution_target_not_found", "Execution target not found.")
	} else if err != nil {
		return persistence.ExecutionTarget{}, problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the execution target.", err)
	}
	return model, nil
}

func encryptConfiguration(cipher *secret.CursorCipher, configuration map[string]any) ([]byte, error) {
	if len(configuration) == 0 {
		return []byte{}, nil
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return nil, problem.Wrap(400, "invalid_execution_target_configuration", "Execution target configuration is not valid JSON.", err)
	}
	if cipher == nil {
		return nil, problem.New(503, "execution_target_encryption_unavailable", "Execution target configuration encryption is not configured.")
	}
	encrypted, err := cipher.Encrypt(string(encoded))
	if err != nil {
		return nil, problem.Wrap(503, "execution_target_encryption_unavailable", "Execution target configuration encryption is unavailable.", err)
	}
	return encrypted, nil
}

func toTarget(model persistence.ExecutionTarget) Target {
	capabilities := model.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	return Target{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		Kind: model.Kind, Name: model.Name, Status: model.Status, Capabilities: capabilities,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func requireActiveTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}

func validatePublicCapabilities(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			for _, sensitive := range []string{"secret", "password", "token", "credential", "privatekey", "private_key"} {
				if strings.Contains(normalized, sensitive) {
					return problem.New(400, "unsafe_execution_target_capability", "Secrets belong in configuration, not public capabilities.")
				}
			}
			if err := validatePublicCapabilities(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validatePublicCapabilities(child); err != nil {
				return err
			}
		}
	}
	return nil
}
