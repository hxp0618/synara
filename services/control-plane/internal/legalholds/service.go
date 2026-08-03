package legalholds

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
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/retentiongate"
)

var validScopeTypes = map[string]struct{}{
	"tenant": {}, "user": {}, "organization": {}, "project": {}, "session": {},
}

type Hold struct {
	ID              uuid.UUID  `json:"id"`
	TenantID        uuid.UUID  `json:"tenantId"`
	ScopeType       string     `json:"scopeType"`
	ScopeID         uuid.UUID  `json:"scopeId"`
	Name            string     `json:"name"`
	MatterReference string     `json:"matterReference"`
	Reason          string     `json:"reason"`
	Status          string     `json:"status"`
	Version         int64      `json:"version"`
	CreatedBy       uuid.UUID  `json:"createdBy"`
	CreatedAt       time.Time  `json:"createdAt"`
	ReleasedBy      *uuid.UUID `json:"releasedBy"`
	ReleaseReason   *string    `json:"releaseReason"`
	ReleasedAt      *time.Time `json:"releasedAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type CreateInput struct {
	ScopeType       string    `json:"scopeType"`
	ScopeID         uuid.UUID `json:"scopeId"`
	Name            string    `json:"name"`
	MatterReference string    `json:"matterReference"`
	Reason          string    `json:"reason"`
}

type ReleaseInput struct {
	ExpectedVersion int64  `json:"expectedVersion"`
	Reason          string `json:"reason"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	now        func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db),
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) List(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
) ([]Hold, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.RetentionRead); err != nil {
		return nil, err
	}
	var models []persistence.LegalHold
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).
		Order("CASE status WHEN 'active' THEN 0 ELSE 1 END, created_at DESC, id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "legal_holds_load_failed", "Legal Holds could not be loaded.", err)
	}
	items := make([]Hold, 0, len(models))
	for _, model := range models {
		items = append(items, toHold(model))
	}
	return items, nil
}

func (s *Service) Create(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input CreateInput,
	requestID, ipAddress string,
) (Hold, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Hold{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.RetentionManage); err != nil {
		return Hold{}, err
	}
	scopeType := strings.ToLower(strings.TrimSpace(input.ScopeType))
	if _, valid := validScopeTypes[scopeType]; !valid {
		return Hold{}, problem.New(400, "invalid_legal_hold_scope", "Legal Hold scopeType must be tenant, user, organization, project, or session.")
	}
	scopeID := input.ScopeID
	if scopeType == "tenant" {
		if scopeID != uuid.Nil && scopeID != tenantID {
			return Hold{}, problem.New(400, "invalid_legal_hold_scope", "Tenant Legal Hold scopeId must match the active Tenant.")
		}
		scopeID = tenantID
	} else if scopeID == uuid.Nil {
		return Hold{}, problem.New(400, "invalid_legal_hold_scope", "A scoped Legal Hold requires scopeId.")
	}
	name, err := normalize(input.Name, 1, 160, "invalid_legal_hold_name", "Legal Hold name")
	if err != nil {
		return Hold{}, err
	}
	matterReference, err := normalize(input.MatterReference, 1, 160, "invalid_legal_hold_matter", "Legal Hold matterReference")
	if err != nil {
		return Hold{}, err
	}
	reason, err := normalize(input.Reason, 10, 2000, "invalid_legal_hold_reason", "Legal Hold reason")
	if err != nil {
		return Hold{}, err
	}
	now := s.now()
	model := persistence.LegalHold{
		ID: uuid.New(), TenantID: tenantID, ScopeType: scopeType, ScopeID: scopeID,
		Name: name, MatterReference: matterReference, Reason: reason, Status: "active", Version: 1,
		CreatedBy: principal.UserID, CreatedAt: now, UpdatedAt: now,
	}
	releaseMutation := retentiongate.AcquireHoldMutation(s.db, tenantID)
	defer releaseMutation()
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := retentiongate.LockHoldMutation(ctx, tx, tenantID); err != nil {
			return problem.Wrap(409, "legal_hold_scope_conflict", "Legal Hold could not lock the Tenant retention scope.", err)
		}
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return problem.Wrap(409, "legal_hold_create_rejected", "Legal Hold could not be created; verify the scope and active matter uniqueness.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "legal_hold.created", ResourceType: "legal_hold", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"scopeType": scopeType, "scopeId": scopeID, "matterReference": matterReference,
				"reason": reason, "version": model.Version,
			},
		})
	})
	if err != nil {
		return Hold{}, err
	}
	return toHold(model), nil
}

func (s *Service) Release(
	ctx context.Context,
	principal identity.Principal,
	tenantID, holdID uuid.UUID,
	input ReleaseInput,
	requestID, ipAddress string,
) (Hold, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Hold{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.RetentionManage); err != nil {
		return Hold{}, err
	}
	if input.ExpectedVersion < 1 {
		return Hold{}, problem.New(400, "invalid_legal_hold_version", "expectedVersion must be a positive integer.")
	}
	reason, err := normalize(input.Reason, 10, 2000, "invalid_legal_hold_release_reason", "Legal Hold release reason")
	if err != nil {
		return Hold{}, err
	}
	releaseMutation := retentiongate.AcquireHoldMutation(s.db, tenantID)
	defer releaseMutation()
	var model persistence.LegalHold
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := retentiongate.LockHoldMutation(ctx, tx, tenantID); err != nil {
			return problem.Wrap(409, "legal_hold_scope_conflict", "Legal Hold could not lock the Tenant retention scope.", err)
		}
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, holdID).Take(&model).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return problem.New(404, "legal_hold_not_found", "Legal Hold not found.")
		}
		if loadErr != nil {
			return problem.Wrap(500, "legal_hold_load_failed", "Legal Hold could not be loaded.", loadErr)
		}
		if model.Status != "active" || model.Version != input.ExpectedVersion {
			return problem.New(409, "legal_hold_version_conflict", "Legal Hold changed; reload it before releasing.")
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.LegalHold{}).
			Where("tenant_id = ? AND id = ? AND status = ? AND version = ?", tenantID, holdID, "active", model.Version).
			Updates(map[string]any{
				"status": "released", "version": model.Version + 1, "released_by": principal.UserID,
				"release_reason": reason, "released_at": now, "updated_at": now,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "legal_hold_version_conflict", "Legal Hold changed; reload it before releasing.", updated.Error)
		}
		model.Status = "released"
		model.Version++
		model.ReleasedBy = &principal.UserID
		model.ReleaseReason = &reason
		model.ReleasedAt = &now
		model.UpdatedAt = now
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "legal_hold.released", ResourceType: "legal_hold", ResourceID: &holdID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"scopeType": model.ScopeType, "scopeId": model.ScopeID,
				"matterReference": model.MatterReference, "reason": reason, "version": model.Version,
			},
		})
	})
	if err != nil {
		return Hold{}, err
	}
	return toHold(model), nil
}

func toHold(model persistence.LegalHold) Hold {
	return Hold{
		ID: model.ID, TenantID: model.TenantID, ScopeType: model.ScopeType, ScopeID: model.ScopeID,
		Name: model.Name, MatterReference: model.MatterReference, Reason: model.Reason,
		Status: model.Status, Version: model.Version, CreatedBy: model.CreatedBy, CreatedAt: model.CreatedAt,
		ReleasedBy: model.ReleasedBy, ReleaseReason: model.ReleaseReason, ReleasedAt: model.ReleasedAt,
		UpdatedAt: model.UpdatedAt,
	}
}

func normalize(value string, minimum, maximum int, code, label string) (string, error) {
	normalized := strings.TrimSpace(value)
	if len(normalized) < minimum || len(normalized) > maximum {
		return "", problem.New(400, code, label+" length is invalid.")
	}
	return normalized, nil
}
