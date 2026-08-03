package quotas

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type Quota struct {
	TenantID                    uuid.UUID `json:"tenantId"`
	MaxConcurrentExecutions     *int      `json:"maxConcurrentExecutions"`
	MaxQueuedExecutions         *int      `json:"maxQueuedExecutions"`
	MaxConcurrentExecutionUnits *int64    `json:"maxConcurrentExecutionUnits"`
	MaxArtifactBytes            *int64    `json:"maxArtifactBytes"`
}

type PutInput struct {
	MaxConcurrentExecutions     *int   `json:"maxConcurrentExecutions"`
	MaxQueuedExecutions         *int   `json:"maxQueuedExecutions"`
	MaxConcurrentExecutionUnits *int64 `json:"maxConcurrentExecutionUnits"`
	MaxArtifactBytes            *int64 `json:"maxArtifactBytes"`
}

const (
	ScopeProject    = "project"
	ScopeSession    = "session"
	ScopeAutomation = "automation"
)

type ScopedQuota struct {
	TenantID                    uuid.UUID `json:"tenantId"`
	ScopeKind                   string    `json:"scopeKind"`
	ScopeID                     uuid.UUID `json:"scopeId"`
	MaxConcurrentExecutions     *int      `json:"maxConcurrentExecutions"`
	MaxQueuedExecutions         *int      `json:"maxQueuedExecutions"`
	MaxConcurrentExecutionUnits *int64    `json:"maxConcurrentExecutionUnits"`
	Version                     int64     `json:"version"`
}

type PutScopedInput struct {
	ExpectedVersion             *int64 `json:"expectedVersion,omitempty"`
	MaxConcurrentExecutions     *int   `json:"maxConcurrentExecutions"`
	MaxQueuedExecutions         *int   `json:"maxQueuedExecutions"`
	MaxConcurrentExecutionUnits *int64 `json:"maxConcurrentExecutionUnits"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, authorizer: authorization.NewAuthorizer(db)}
}

func (s *Service) Get(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (Quota, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Quota{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.QuotaRead); err != nil {
		return Quota{}, err
	}
	var model persistence.TenantQuota
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Quota{TenantID: tenantID}, nil
	}
	if err != nil {
		return Quota{}, problem.Wrap(500, "tenant_quota_load_failed", "Failed to load tenant quota.", err)
	}
	return toQuota(model), nil
}

func (s *Service) Put(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input PutInput,
	requestID, ipAddress string,
) (Quota, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Quota{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.QuotaManage); err != nil {
		return Quota{}, err
	}
	if err := validate(input); err != nil {
		return Quota{}, err
	}
	model := persistence.TenantQuota{
		TenantID: tenantID, MaxConcurrentExecutions: input.MaxConcurrentExecutions,
		MaxQueuedExecutions:         input.MaxQueuedExecutions,
		MaxConcurrentExecutionUnits: input.MaxConcurrentExecutionUnits,
		MaxArtifactBytes:            input.MaxArtifactBytes, UpdatedBy: principal.UserID,
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; err != nil {
			return problem.Wrap(404, "tenant_not_found", "Tenant not found.", err)
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "tenant_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"max_concurrent_executions":      input.MaxConcurrentExecutions,
				"max_queued_executions":          input.MaxQueuedExecutions,
				"max_concurrent_execution_units": input.MaxConcurrentExecutionUnits,
				"max_artifact_bytes":             input.MaxArtifactBytes,
				"updated_by":                     principal.UserID,
			}),
		}).Create(&model).Error; err != nil {
			return problem.Wrap(409, "tenant_quota_update_failed", "Failed to update tenant quota.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.quota_updated", ResourceType: "tenant_quota", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"maxConcurrentExecutions":     input.MaxConcurrentExecutions,
				"maxQueuedExecutions":         input.MaxQueuedExecutions,
				"maxConcurrentExecutionUnits": input.MaxConcurrentExecutionUnits,
				"maxArtifactBytes":            input.MaxArtifactBytes,
			},
		})
	})
	if err != nil {
		return Quota{}, err
	}
	return s.Get(ctx, principal, tenantID)
}

func (s *Service) GetScoped(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	scopeKind string,
	scopeID uuid.UUID,
) (ScopedQuota, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return ScopedQuota{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.QuotaRead); err != nil {
		return ScopedQuota{}, err
	}
	scopeKind, err := normalizeScope(scopeKind, scopeID)
	if err != nil {
		return ScopedQuota{}, err
	}
	if err := requireScopeOwned(ctx, s.db, tenantID, scopeKind, scopeID, false); err != nil {
		return ScopedQuota{}, err
	}
	var model persistence.ExecutionQuotaPolicy
	err = s.db.WithContext(ctx).
		Where("tenant_id = ? AND scope_kind = ? AND scope_id = ?", tenantID, scopeKind, scopeID).
		Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ScopedQuota{TenantID: tenantID, ScopeKind: scopeKind, ScopeID: scopeID}, nil
	}
	if err != nil {
		return ScopedQuota{}, problem.Wrap(500, "execution_quota_policy_load_failed", "Execution quota policy could not be loaded.", err)
	}
	return toScopedQuota(model), nil
}

func (s *Service) PutScoped(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	scopeKind string,
	scopeID uuid.UUID,
	input PutScopedInput,
	requestID, ipAddress string,
) (ScopedQuota, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return ScopedQuota{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.QuotaManage); err != nil {
		return ScopedQuota{}, err
	}
	scopeKind, err := normalizeScope(scopeKind, scopeID)
	if err != nil {
		return ScopedQuota{}, err
	}
	if err := validateScoped(input); err != nil {
		return ScopedQuota{}, err
	}
	var result ScopedQuota
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; err != nil {
			return problem.Wrap(404, "tenant_not_found", "Tenant not found.", err)
		}
		if err := requireScopeOwned(ctx, tx, tenantID, scopeKind, scopeID, true); err != nil {
			return err
		}
		var current persistence.ExecutionQuotaPolicy
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND scope_kind = ? AND scope_id = ?", tenantID, scopeKind, scopeID).
			Take(&current).Error
		if loadErr != nil && !errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "execution_quota_policy_load_failed", "Execution quota policy could not be loaded.", loadErr)
		}

		clearing := input.MaxConcurrentExecutions == nil && input.MaxQueuedExecutions == nil &&
			input.MaxConcurrentExecutionUnits == nil
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			if input.ExpectedVersion != nil && *input.ExpectedVersion != 0 {
				return problem.New(409, "execution_quota_policy_version_conflict", "Execution quota policy changed; reload it before saving.")
			}
			if clearing {
				result = ScopedQuota{TenantID: tenantID, ScopeKind: scopeKind, ScopeID: scopeID}
				return nil
			}
			current = persistence.ExecutionQuotaPolicy{
				TenantID: tenantID, ScopeKind: scopeKind, ScopeID: scopeID,
				MaxConcurrentExecutions:     input.MaxConcurrentExecutions,
				MaxQueuedExecutions:         input.MaxQueuedExecutions,
				MaxConcurrentExecutionUnits: input.MaxConcurrentExecutionUnits,
				Version:                     1, UpdatedBy: principal.UserID,
			}
			if err := tx.WithContext(ctx).Create(&current).Error; err != nil {
				return problem.Wrap(409, "execution_quota_policy_create_rejected", "Execution quota policy could not be created.", err)
			}
			result = toScopedQuota(current)
		} else {
			if input.ExpectedVersion == nil || *input.ExpectedVersion != current.Version {
				return problem.New(409, "execution_quota_policy_version_conflict", "Execution quota policy changed; reload it before saving.")
			}
			if clearing {
				deleted := tx.WithContext(ctx).
					Where("tenant_id = ? AND scope_kind = ? AND scope_id = ? AND version = ?", tenantID, scopeKind, scopeID, current.Version).
					Delete(&persistence.ExecutionQuotaPolicy{})
				if deleted.Error != nil || deleted.RowsAffected != 1 {
					return problem.Wrap(409, "execution_quota_policy_version_conflict", "Execution quota policy changed; reload it before saving.", deleted.Error)
				}
				result = ScopedQuota{TenantID: tenantID, ScopeKind: scopeKind, ScopeID: scopeID}
			} else {
				updates := map[string]any{
					"max_concurrent_executions":      input.MaxConcurrentExecutions,
					"max_queued_executions":          input.MaxQueuedExecutions,
					"max_concurrent_execution_units": input.MaxConcurrentExecutionUnits,
					"version":                        current.Version + 1, "updated_by": principal.UserID,
				}
				updated := tx.WithContext(ctx).Model(&persistence.ExecutionQuotaPolicy{}).
					Where("tenant_id = ? AND scope_kind = ? AND scope_id = ? AND version = ?", tenantID, scopeKind, scopeID, current.Version).
					Updates(updates)
				if updated.Error != nil || updated.RowsAffected != 1 {
					return problem.Wrap(409, "execution_quota_policy_version_conflict", "Execution quota policy changed; reload it before saving.", updated.Error)
				}
				current.MaxConcurrentExecutions = input.MaxConcurrentExecutions
				current.MaxQueuedExecutions = input.MaxQueuedExecutions
				current.MaxConcurrentExecutionUnits = input.MaxConcurrentExecutionUnits
				current.Version++
				result = toScopedQuota(current)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "execution_quota_policy.updated", ResourceType: "execution_quota_policy", ResourceID: &scopeID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"scopeKind": scopeKind, "cleared": clearing, "version": result.Version,
				"maxConcurrentExecutions":     input.MaxConcurrentExecutions,
				"maxQueuedExecutions":         input.MaxQueuedExecutions,
				"maxConcurrentExecutionUnits": input.MaxConcurrentExecutionUnits,
			},
		})
	})
	return result, err
}

func normalizeScope(scopeKind string, scopeID uuid.UUID) (string, error) {
	scopeKind = strings.ToLower(strings.TrimSpace(scopeKind))
	if scopeID == uuid.Nil {
		return "", problem.New(400, "invalid_execution_quota_scope", "Execution quota scope ID is required.")
	}
	switch scopeKind {
	case ScopeProject, ScopeSession, ScopeAutomation:
		return scopeKind, nil
	default:
		return "", problem.New(400, "invalid_execution_quota_scope", "Execution quota scope must be project, session, or automation.")
	}
}

func requireScopeOwned(ctx context.Context, db *gorm.DB, tenantID uuid.UUID, scopeKind string, scopeID uuid.UUID, lock bool) error {
	query := db.WithContext(ctx)
	if lock {
		query = persistence.WithLocking(query, "UPDATE", "")
	}
	table := ""
	switch scopeKind {
	case ScopeProject:
		table = persistence.Project{}.TableName()
	case ScopeSession:
		table = persistence.AgentSession{}.TableName()
	case ScopeAutomation:
		table = persistence.Automation{}.TableName()
	default:
		return problem.New(400, "invalid_execution_quota_scope", "Execution quota scope is invalid.")
	}
	var row struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	err := query.Table(table).Select("id").
		Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, scopeID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(404, "execution_quota_scope_not_found", "Execution quota scope not found.")
	}
	if err != nil {
		return problem.Wrap(500, "execution_quota_scope_check_failed", "Execution quota scope could not be verified.", err)
	}
	return nil
}

func validateScoped(input PutScopedInput) error {
	return validate(PutInput{
		MaxConcurrentExecutions:     input.MaxConcurrentExecutions,
		MaxQueuedExecutions:         input.MaxQueuedExecutions,
		MaxConcurrentExecutionUnits: input.MaxConcurrentExecutionUnits,
	})
}

func toScopedQuota(model persistence.ExecutionQuotaPolicy) ScopedQuota {
	return ScopedQuota{
		TenantID: model.TenantID, ScopeKind: model.ScopeKind, ScopeID: model.ScopeID,
		MaxConcurrentExecutions:     model.MaxConcurrentExecutions,
		MaxQueuedExecutions:         model.MaxQueuedExecutions,
		MaxConcurrentExecutionUnits: model.MaxConcurrentExecutionUnits,
		Version:                     model.Version,
	}
}

func validate(input PutInput) error {
	if input.MaxConcurrentExecutions != nil && (*input.MaxConcurrentExecutions <= 0 || *input.MaxConcurrentExecutions > 1_000_000) {
		return problem.New(400, "invalid_execution_quota", "maxConcurrentExecutions must be null or between 1 and 1000000.")
	}
	if input.MaxQueuedExecutions != nil && (*input.MaxQueuedExecutions <= 0 || *input.MaxQueuedExecutions > 1_000_000) {
		return problem.New(400, "invalid_execution_queue_quota", "maxQueuedExecutions must be null or between 1 and 1000000.")
	}
	if input.MaxConcurrentExecutionUnits != nil && (*input.MaxConcurrentExecutionUnits <= 0 || *input.MaxConcurrentExecutionUnits > 1_000_000_000_000) {
		return problem.New(400, "invalid_execution_unit_quota", "maxConcurrentExecutionUnits must be null or between 1 and 1000000000000.")
	}
	if input.MaxArtifactBytes != nil && (*input.MaxArtifactBytes <= 0 || *input.MaxArtifactBytes > 1<<60) {
		return problem.New(400, "invalid_artifact_quota", "maxArtifactBytes must be null or between 1 and 1152921504606846976.")
	}
	return nil
}

func toQuota(model persistence.TenantQuota) Quota {
	return Quota{
		TenantID: model.TenantID, MaxConcurrentExecutions: model.MaxConcurrentExecutions,
		MaxQueuedExecutions:         model.MaxQueuedExecutions,
		MaxConcurrentExecutionUnits: model.MaxConcurrentExecutionUnits,
		MaxArtifactBytes:            model.MaxArtifactBytes,
	}
}
