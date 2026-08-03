package lifecyclepolicy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type IntBounds struct {
	Minimum int `json:"min"`
	Maximum int `json:"max"`
}

type Bounds struct {
	WaitingKeepAliveSeconds        IntBounds `json:"waitingKeepAliveSeconds"`
	SuspendAfterIdleSeconds        IntBounds `json:"suspendAfterIdleSeconds"`
	AbsoluteSessionLifetimeSeconds IntBounds `json:"absoluteSessionLifetimeSeconds"`
	WorkspaceRetentionDays         IntBounds `json:"workspaceRetentionDays"`
	WarmPoolModes                  []string  `json:"warmPoolModes"`
}

type Overrides struct {
	WaitingKeepAliveSeconds        *int    `json:"waitingKeepAliveSeconds"`
	SuspendAfterIdleSeconds        *int    `json:"suspendAfterIdleSeconds"`
	AbsoluteSessionLifetimeSeconds *int    `json:"absoluteSessionLifetimeSeconds"`
	WorkspaceRetentionDays         *int    `json:"workspaceRetentionDays"`
	WarmPoolMode                   *string `json:"warmPoolMode"`
}

type Effective struct {
	WaitingKeepAliveSeconds        int    `json:"waitingKeepAliveSeconds"`
	SuspendAfterIdleSeconds        int    `json:"suspendAfterIdleSeconds"`
	AbsoluteSessionLifetimeSeconds *int   `json:"absoluteSessionLifetimeSeconds"`
	WorkspaceRetentionDays         int    `json:"workspaceRetentionDays"`
	WarmPoolMode                   string `json:"warmPoolMode"`
}

type Config struct {
	Defaults Effective `json:"defaults"`
	Bounds   Bounds    `json:"bounds"`
}

type UpdateInput struct {
	ExpectedVersion int64 `json:"expectedVersion"`
	Overrides
}

type Policy struct {
	Scope     string     `json:"scope"`
	TenantID  uuid.UUID  `json:"tenantId"`
	ProjectID *uuid.UUID `json:"projectId,omitempty"`
	Overrides Overrides  `json:"overrides"`
	Effective Effective  `json:"effective"`
	Version   int64      `json:"version"`
	UpdatedBy *uuid.UUID `json:"updatedBy,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	config     Config
	now        func() time.Time
}

func DefaultConfig(profile platform.DeploymentProfile) Config {
	warmPoolMode := "balanced"
	if profile == platform.ProfilePersonal {
		warmPoolMode = "disabled"
	}
	return Config{
		Defaults: Effective{
			WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800,
			WorkspaceRetentionDays: 30, WarmPoolMode: warmPoolMode,
		},
		Bounds: Bounds{
			WaitingKeepAliveSeconds:        IntBounds{Minimum: 60, Maximum: 86400},
			SuspendAfterIdleSeconds:        IntBounds{Minimum: 60, Maximum: 604800},
			AbsoluteSessionLifetimeSeconds: IntBounds{Minimum: 3600, Maximum: 31536000},
			WorkspaceRetentionDays:         IntBounds{Minimum: 1, Maximum: 3650},
			WarmPoolModes:                  []string{"disabled", "balanced", "low-latency"},
		},
	}
}

func (config Config) Validate() error {
	if err := validateBounds(config.Bounds.WaitingKeepAliveSeconds); err != nil {
		return errors.New("waitingKeepAliveSeconds bounds are invalid")
	}
	if err := validateBounds(config.Bounds.SuspendAfterIdleSeconds); err != nil {
		return errors.New("suspendAfterIdleSeconds bounds are invalid")
	}
	if err := validateBounds(config.Bounds.AbsoluteSessionLifetimeSeconds); err != nil {
		return errors.New("absoluteSessionLifetimeSeconds bounds are invalid")
	}
	if err := validateBounds(config.Bounds.WorkspaceRetentionDays); err != nil {
		return errors.New("workspaceRetentionDays bounds are invalid")
	}
	if err := validateEffective(config.Defaults, config.Bounds); err != nil {
		return err
	}
	return nil
}

func NewService(db *gorm.DB, config Config) (*Service, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), config: config,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (s *Service) PublicConfig() Config {
	config := s.config
	config.Bounds.WarmPoolModes = append([]string(nil), s.config.Bounds.WarmPoolModes...)
	return config
}

func (s *Service) ResolveForSession(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, projectID uuid.UUID,
	sessionOverride *Overrides,
) (Effective, error) {
	effective := s.config.Defaults
	var tenant persistence.TenantResourceLifecyclePolicy
	err := tx.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&tenant).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return Effective{}, problem.Wrap(500, "lifecycle_policy_load_failed", "The Tenant Resource Lifecycle Policy could not be loaded.", err)
	}
	if err == nil {
		effective = applyOverrides(effective, overridesFromTenant(tenant))
	}
	var project persistence.ProjectResourceLifecyclePolicy
	err = tx.WithContext(ctx).Where("tenant_id = ? AND project_id = ?", tenantID, projectID).Take(&project).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return Effective{}, problem.Wrap(500, "lifecycle_policy_load_failed", "The Project Resource Lifecycle Policy could not be loaded.", err)
	}
	if err == nil {
		effective = applyOverrides(effective, overridesFromProject(project))
	}
	if sessionOverride != nil {
		normalized, err := s.normalizeOverrides(*sessionOverride)
		if err != nil {
			return Effective{}, err
		}
		effective = applyOverrides(effective, normalized)
	}
	if err := validateEffective(effective, s.config.Bounds); err != nil {
		return Effective{}, problem.New(409, "lifecycle_policy_invalid", err.Error())
	}
	return cloneEffective(effective), nil
}

func (s *Service) GetTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
) (Policy, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Policy{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.LifecycleRead); err != nil {
		return Policy{}, err
	}
	return s.tenantPolicy(ctx, s.db, tenantID)
}

func (s *Service) UpdateTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input UpdateInput,
	requestID, ipAddress string,
) (Policy, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Policy{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.LifecycleManage); err != nil {
		return Policy{}, err
	}
	overrides, err := s.normalizeOverrides(input.Overrides)
	if err != nil {
		return Policy{}, err
	}
	if input.ExpectedVersion < 0 {
		return Policy{}, problem.New(400, "invalid_lifecycle_policy_version", "expectedVersion must not be negative.")
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		now := s.now()
		var current persistence.TenantResourceLifecyclePolicy
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ?", tenantID).Take(&current).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			if input.ExpectedVersion != 0 {
				return problem.New(409, "lifecycle_policy_version_conflict", "The Resource Lifecycle Policy changed; reload it before saving.")
			}
			current = persistence.TenantResourceLifecyclePolicy{
				TenantID: tenantID, Version: 1, UpdatedBy: principal.UserID,
				CreatedAt: now, UpdatedAt: now,
			}
			applyTenantModelOverrides(&current, overrides)
			if err := tx.WithContext(ctx).Create(&current).Error; err != nil {
				return problem.Wrap(409, "lifecycle_policy_update_rejected", "The Tenant Resource Lifecycle Policy could not be created.", err)
			}
		} else if loadErr != nil {
			return problem.Wrap(500, "lifecycle_policy_load_failed", "The Tenant Resource Lifecycle Policy could not be locked.", loadErr)
		} else {
			if current.Version != input.ExpectedVersion {
				return problem.New(409, "lifecycle_policy_version_conflict", "The Resource Lifecycle Policy changed; reload it before saving.")
			}
			updates := overrideUpdates(overrides, current.Version+1, principal.UserID, now)
			result := tx.WithContext(ctx).Model(&persistence.TenantResourceLifecyclePolicy{}).
				Where("tenant_id = ? AND version = ?", tenantID, current.Version).Updates(updates)
			if result.Error != nil || result.RowsAffected != 1 {
				return problem.Wrap(409, "lifecycle_policy_version_conflict", "The Resource Lifecycle Policy changed while saving.", result.Error)
			}
		}
		return recordPolicyAudit(ctx, tx, tenantID, nil, principal.UserID, overrides, requestID, ipAddress)
	})
	if err != nil {
		return Policy{}, err
	}
	return s.tenantPolicy(ctx, s.db, tenantID)
}

func (s *Service) GetProject(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
) (Policy, error) {
	project, err := s.authorizeProject(ctx, principal, tenantID, projectID, authorization.ProjectRead)
	if err != nil {
		return Policy{}, err
	}
	return s.projectPolicy(ctx, s.db, project)
}

func (s *Service) UpdateProject(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
	input UpdateInput,
	requestID, ipAddress string,
) (Policy, error) {
	project, err := s.authorizeProject(ctx, principal, tenantID, projectID, authorization.ProjectUpdate)
	if err != nil {
		return Policy{}, err
	}
	overrides, err := s.normalizeOverrides(input.Overrides)
	if err != nil {
		return Policy{}, err
	}
	if input.ExpectedVersion < 0 {
		return Policy{}, problem.New(400, "invalid_lifecycle_policy_version", "expectedVersion must not be negative.")
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		now := s.now()
		var current persistence.ProjectResourceLifecyclePolicy
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND project_id = ?", tenantID, projectID).Take(&current).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			if input.ExpectedVersion != 0 {
				return problem.New(409, "lifecycle_policy_version_conflict", "The Resource Lifecycle Policy changed; reload it before saving.")
			}
			current = persistence.ProjectResourceLifecyclePolicy{
				ProjectID: projectID, TenantID: tenantID, Version: 1, UpdatedBy: principal.UserID,
				CreatedAt: now, UpdatedAt: now,
			}
			applyProjectModelOverrides(&current, overrides)
			if err := tx.WithContext(ctx).Create(&current).Error; err != nil {
				return problem.Wrap(409, "lifecycle_policy_update_rejected", "The Project Resource Lifecycle Policy could not be created.", err)
			}
		} else if loadErr != nil {
			return problem.Wrap(500, "lifecycle_policy_load_failed", "The Project Resource Lifecycle Policy could not be locked.", loadErr)
		} else {
			if current.Version != input.ExpectedVersion {
				return problem.New(409, "lifecycle_policy_version_conflict", "The Resource Lifecycle Policy changed; reload it before saving.")
			}
			updates := overrideUpdates(overrides, current.Version+1, principal.UserID, now)
			result := tx.WithContext(ctx).Model(&persistence.ProjectResourceLifecyclePolicy{}).
				Where("tenant_id = ? AND project_id = ? AND version = ?", tenantID, projectID, current.Version).
				Updates(updates)
			if result.Error != nil || result.RowsAffected != 1 {
				return problem.Wrap(409, "lifecycle_policy_version_conflict", "The Resource Lifecycle Policy changed while saving.", result.Error)
			}
		}
		return recordPolicyAudit(ctx, tx, tenantID, &projectID, principal.UserID, overrides, requestID, ipAddress)
	})
	if err != nil {
		return Policy{}, err
	}
	return s.projectPolicy(ctx, s.db, project)
}

func (s *Service) tenantPolicy(ctx context.Context, tx *gorm.DB, tenantID uuid.UUID) (Policy, error) {
	var model persistence.TenantResourceLifecyclePolicy
	err := tx.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Policy{
			Scope: "tenant", TenantID: tenantID, Overrides: Overrides{},
			Effective: cloneEffective(s.config.Defaults), Version: 0,
		}, nil
	}
	if err != nil {
		return Policy{}, problem.Wrap(500, "lifecycle_policy_load_failed", "The Tenant Resource Lifecycle Policy could not be loaded.", err)
	}
	overrides := overridesFromTenant(model)
	return Policy{
		Scope: "tenant", TenantID: tenantID, Overrides: overrides,
		Effective: applyOverrides(cloneEffective(s.config.Defaults), overrides),
		Version:   model.Version, UpdatedBy: &model.UpdatedBy,
		CreatedAt: &model.CreatedAt, UpdatedAt: &model.UpdatedAt,
	}, nil
}

func (s *Service) projectPolicy(
	ctx context.Context,
	tx *gorm.DB,
	project persistence.Project,
) (Policy, error) {
	tenantPolicy, err := s.tenantPolicy(ctx, tx, project.TenantID)
	if err != nil {
		return Policy{}, err
	}
	var model persistence.ProjectResourceLifecyclePolicy
	err = tx.WithContext(ctx).
		Where("tenant_id = ? AND project_id = ?", project.TenantID, project.ID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Policy{
			Scope: "project", TenantID: project.TenantID, ProjectID: &project.ID,
			Overrides: Overrides{}, Effective: tenantPolicy.Effective, Version: 0,
		}, nil
	}
	if err != nil {
		return Policy{}, problem.Wrap(500, "lifecycle_policy_load_failed", "The Project Resource Lifecycle Policy could not be loaded.", err)
	}
	overrides := overridesFromProject(model)
	return Policy{
		Scope: "project", TenantID: project.TenantID, ProjectID: &project.ID,
		Overrides: overrides, Effective: applyOverrides(tenantPolicy.Effective, overrides),
		Version: model.Version, UpdatedBy: &model.UpdatedBy,
		CreatedAt: &model.CreatedAt, UpdatedAt: &model.UpdatedAt,
	}, nil
}

func (s *Service) authorizeProject(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
	permission authorization.Permission,
) (persistence.Project, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return persistence.Project{}, err
	}
	var project persistence.Project
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, projectID).
		Take(&project).Error; err != nil {
		return persistence.Project{}, problem.Wrap(404, "project_not_found", "Project not found.", err)
	}
	if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, project.OrganizationID, permission); err != nil {
		return persistence.Project{}, err
	}
	return project, nil
}

func (s *Service) normalizeOverrides(overrides Overrides) (Overrides, error) {
	if overrides.WarmPoolMode != nil {
		normalized := strings.ToLower(strings.TrimSpace(*overrides.WarmPoolMode))
		overrides.WarmPoolMode = &normalized
	}
	if err := validateOptional(overrides.WaitingKeepAliveSeconds, s.config.Bounds.WaitingKeepAliveSeconds); err != nil {
		return Overrides{}, problem.New(400, "invalid_waiting_keep_alive", "waitingKeepAliveSeconds is outside the operator bounds.")
	}
	if err := validateOptional(overrides.SuspendAfterIdleSeconds, s.config.Bounds.SuspendAfterIdleSeconds); err != nil {
		return Overrides{}, problem.New(400, "invalid_suspend_after_idle", "suspendAfterIdleSeconds is outside the operator bounds.")
	}
	if err := validateOptional(overrides.AbsoluteSessionLifetimeSeconds, s.config.Bounds.AbsoluteSessionLifetimeSeconds); err != nil {
		return Overrides{}, problem.New(400, "invalid_absolute_session_lifetime", "absoluteSessionLifetimeSeconds is outside the operator bounds.")
	}
	if err := validateOptional(overrides.WorkspaceRetentionDays, s.config.Bounds.WorkspaceRetentionDays); err != nil {
		return Overrides{}, problem.New(400, "invalid_workspace_retention", "workspaceRetentionDays is outside the operator bounds.")
	}
	if overrides.WarmPoolMode != nil && !contains(s.config.Bounds.WarmPoolModes, *overrides.WarmPoolMode) {
		return Overrides{}, problem.New(400, "invalid_warm_pool_mode", "warmPoolMode is outside the operator allowlist.")
	}
	return cloneOverrides(overrides), nil
}

func validateBounds(bounds IntBounds) error {
	if bounds.Minimum <= 0 || bounds.Maximum < bounds.Minimum {
		return errors.New("invalid bounds")
	}
	return nil
}

func validateOptional(value *int, bounds IntBounds) error {
	if value != nil && (*value < bounds.Minimum || *value > bounds.Maximum) {
		return errors.New("outside bounds")
	}
	return nil
}

func validateEffective(effective Effective, bounds Bounds) error {
	if err := validateOptional(&effective.WaitingKeepAliveSeconds, bounds.WaitingKeepAliveSeconds); err != nil {
		return errors.New("default waitingKeepAliveSeconds is outside the operator bounds")
	}
	if err := validateOptional(&effective.SuspendAfterIdleSeconds, bounds.SuspendAfterIdleSeconds); err != nil {
		return errors.New("default suspendAfterIdleSeconds is outside the operator bounds")
	}
	if err := validateOptional(effective.AbsoluteSessionLifetimeSeconds, bounds.AbsoluteSessionLifetimeSeconds); err != nil {
		return errors.New("default absoluteSessionLifetimeSeconds is outside the operator bounds")
	}
	if err := validateOptional(&effective.WorkspaceRetentionDays, bounds.WorkspaceRetentionDays); err != nil {
		return errors.New("default workspaceRetentionDays is outside the operator bounds")
	}
	if !contains(bounds.WarmPoolModes, effective.WarmPoolMode) {
		return errors.New("default warmPoolMode is outside the operator allowlist")
	}
	return nil
}

func applyOverrides(effective Effective, overrides Overrides) Effective {
	if overrides.WaitingKeepAliveSeconds != nil {
		effective.WaitingKeepAliveSeconds = *overrides.WaitingKeepAliveSeconds
	}
	if overrides.SuspendAfterIdleSeconds != nil {
		effective.SuspendAfterIdleSeconds = *overrides.SuspendAfterIdleSeconds
	}
	if overrides.AbsoluteSessionLifetimeSeconds != nil {
		value := *overrides.AbsoluteSessionLifetimeSeconds
		effective.AbsoluteSessionLifetimeSeconds = &value
	}
	if overrides.WorkspaceRetentionDays != nil {
		effective.WorkspaceRetentionDays = *overrides.WorkspaceRetentionDays
	}
	if overrides.WarmPoolMode != nil {
		effective.WarmPoolMode = *overrides.WarmPoolMode
	}
	return effective
}

func overridesFromTenant(model persistence.TenantResourceLifecyclePolicy) Overrides {
	return Overrides{
		WaitingKeepAliveSeconds:        model.WaitingKeepAliveSeconds,
		SuspendAfterIdleSeconds:        model.SuspendAfterIdleSeconds,
		AbsoluteSessionLifetimeSeconds: model.AbsoluteSessionLifetimeSeconds,
		WorkspaceRetentionDays:         model.WorkspaceRetentionDays, WarmPoolMode: model.WarmPoolMode,
	}
}

func overridesFromProject(model persistence.ProjectResourceLifecyclePolicy) Overrides {
	return Overrides{
		WaitingKeepAliveSeconds:        model.WaitingKeepAliveSeconds,
		SuspendAfterIdleSeconds:        model.SuspendAfterIdleSeconds,
		AbsoluteSessionLifetimeSeconds: model.AbsoluteSessionLifetimeSeconds,
		WorkspaceRetentionDays:         model.WorkspaceRetentionDays, WarmPoolMode: model.WarmPoolMode,
	}
}

func applyTenantModelOverrides(model *persistence.TenantResourceLifecyclePolicy, overrides Overrides) {
	model.WaitingKeepAliveSeconds = overrides.WaitingKeepAliveSeconds
	model.SuspendAfterIdleSeconds = overrides.SuspendAfterIdleSeconds
	model.AbsoluteSessionLifetimeSeconds = overrides.AbsoluteSessionLifetimeSeconds
	model.WorkspaceRetentionDays = overrides.WorkspaceRetentionDays
	model.WarmPoolMode = overrides.WarmPoolMode
}

func applyProjectModelOverrides(model *persistence.ProjectResourceLifecyclePolicy, overrides Overrides) {
	model.WaitingKeepAliveSeconds = overrides.WaitingKeepAliveSeconds
	model.SuspendAfterIdleSeconds = overrides.SuspendAfterIdleSeconds
	model.AbsoluteSessionLifetimeSeconds = overrides.AbsoluteSessionLifetimeSeconds
	model.WorkspaceRetentionDays = overrides.WorkspaceRetentionDays
	model.WarmPoolMode = overrides.WarmPoolMode
}

func overrideUpdates(overrides Overrides, version int64, userID uuid.UUID, now time.Time) map[string]any {
	return map[string]any{
		"waiting_keep_alive_seconds":        overrides.WaitingKeepAliveSeconds,
		"suspend_after_idle_seconds":        overrides.SuspendAfterIdleSeconds,
		"absolute_session_lifetime_seconds": overrides.AbsoluteSessionLifetimeSeconds,
		"workspace_retention_days":          overrides.WorkspaceRetentionDays,
		"warm_pool_mode":                    overrides.WarmPoolMode,
		"version":                           version, "updated_by": userID, "updated_at": now,
	}
}

func recordPolicyAudit(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	projectID *uuid.UUID,
	userID uuid.UUID,
	overrides Overrides,
	requestID, ipAddress string,
) error {
	resourceType := "tenant_resource_lifecycle_policy"
	resourceID := tenantID
	action := "tenant_resource_lifecycle_policy.updated"
	if projectID != nil {
		resourceType = "project_resource_lifecycle_policy"
		resourceID = *projectID
		action = "project_resource_lifecycle_policy.updated"
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: "user", ActorID: &userID,
		Action: action, ResourceType: resourceType, ResourceID: &resourceID,
		RequestID: requestID, IPAddress: ipAddress,
		Metadata: map[string]any{
			"projectId": projectID, "overrides": overrides,
		},
	})
}

func cloneOverrides(value Overrides) Overrides {
	return Overrides{
		WaitingKeepAliveSeconds:        cloneInt(value.WaitingKeepAliveSeconds),
		SuspendAfterIdleSeconds:        cloneInt(value.SuspendAfterIdleSeconds),
		AbsoluteSessionLifetimeSeconds: cloneInt(value.AbsoluteSessionLifetimeSeconds),
		WorkspaceRetentionDays:         cloneInt(value.WorkspaceRetentionDays),
		WarmPoolMode:                   cloneString(value.WarmPoolMode),
	}
}

func cloneEffective(value Effective) Effective {
	value.AbsoluteSessionLifetimeSeconds = cloneInt(value.AbsoluteSessionLifetimeSeconds)
	return value
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
