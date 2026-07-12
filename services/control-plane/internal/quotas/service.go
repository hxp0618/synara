package quotas

import (
	"context"
	"errors"

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
	TenantID                uuid.UUID `json:"tenantId"`
	MaxConcurrentExecutions *int      `json:"maxConcurrentExecutions"`
	MaxArtifactBytes        *int64    `json:"maxArtifactBytes"`
}

type PutInput struct {
	MaxConcurrentExecutions *int   `json:"maxConcurrentExecutions"`
	MaxArtifactBytes        *int64 `json:"maxArtifactBytes"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, authorizer: authorization.NewAuthorizer(db)}
}

func (s *Service) Get(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (Quota, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
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
	if err := requireActiveTenant(principal, tenantID); err != nil {
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
		MaxArtifactBytes: input.MaxArtifactBytes, UpdatedBy: principal.UserID,
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
				"max_concurrent_executions": input.MaxConcurrentExecutions,
				"max_artifact_bytes":        input.MaxArtifactBytes,
				"updated_by":                principal.UserID,
			}),
		}).Create(&model).Error; err != nil {
			return problem.Wrap(409, "tenant_quota_update_failed", "Failed to update tenant quota.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.quota_updated", ResourceType: "tenant_quota", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"maxConcurrentExecutions": input.MaxConcurrentExecutions,
				"maxArtifactBytes":        input.MaxArtifactBytes,
			},
		})
	})
	if err != nil {
		return Quota{}, err
	}
	return s.Get(ctx, principal, tenantID)
}

func validate(input PutInput) error {
	if input.MaxConcurrentExecutions != nil && (*input.MaxConcurrentExecutions <= 0 || *input.MaxConcurrentExecutions > 1_000_000) {
		return problem.New(400, "invalid_execution_quota", "maxConcurrentExecutions must be null or between 1 and 1000000.")
	}
	if input.MaxArtifactBytes != nil && (*input.MaxArtifactBytes <= 0 || *input.MaxArtifactBytes > 1<<60) {
		return problem.New(400, "invalid_artifact_quota", "maxArtifactBytes must be null or between 1 and 1152921504606846976.")
	}
	return nil
}

func toQuota(model persistence.TenantQuota) Quota {
	return Quota{
		TenantID: model.TenantID, MaxConcurrentExecutions: model.MaxConcurrentExecutions,
		MaxArtifactBytes: model.MaxArtifactBytes,
	}
}

func requireActiveTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}
