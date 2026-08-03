package tenancy

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
	"github.com/synara-ai/synara/services/control-plane/internal/productprofile"
)

var tenantLifecycleTransitions = map[string]map[string]struct{}{
	"trialing":  {"active": {}, "suspended": {}, "closed": {}},
	"active":    {"suspended": {}, "closed": {}},
	"suspended": {"active": {}, "closed": {}},
	"closed":    {"active": {}},
}

var activeTenantExecutionStatuses = []string{
	"queued", "recovering", "leased", "running", "waiting-for-approval", "suspended",
}

func (s *Service) TransitionTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input TransitionTenantInput,
	requestID string,
	ipAddress string,
) (Tenant, error) {
	role, err := s.requireTenantPermission(ctx, principal, tenantID, authorization.TenantUpdate)
	if err != nil {
		return Tenant{}, err
	}
	if role != "owner" {
		return Tenant{}, problem.New(403, "tenant_status_forbidden", "Only a tenant owner can change tenant status.")
	}
	toStatus := productprofile.InternalLifecycleStatus(strings.ToLower(strings.TrimSpace(input.ToStatus)))
	if _, known := tenantLifecycleTransitions[toStatus]; !known && toStatus != "active" {
		return Tenant{}, problem.New(400, "invalid_tenant_status", "Tenant status must be evaluation, active, suspended, or closed.")
	}
	if input.ExpectedVersion < 1 {
		return Tenant{}, problem.New(400, "invalid_tenant_lifecycle_version", "expectedVersion must be a positive integer.")
	}
	reason, err := normalizeLifecycleReason(input.Reason)
	if err != nil {
		return Tenant{}, err
	}
	now := time.Now().UTC()
	if toStatus == "trialing" {
		if input.TrialExpiresAt == nil || !input.TrialExpiresAt.After(now) {
			return Tenant{}, problem.New(400, "invalid_tenant_evaluation_expiry", "An evaluation Tenant requires a future evaluationExpiresAt.")
		}
	} else if input.TrialExpiresAt != nil {
		return Tenant{}, problem.New(400, "unexpected_tenant_evaluation_expiry", "evaluationExpiresAt is only valid for an evaluation Tenant.")
	}

	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var current persistence.Tenant
		lockErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND deleted_at IS NULL", tenantID).Take(&current).Error
		if errors.Is(lockErr, gorm.ErrRecordNotFound) {
			return problem.New(404, "tenant_not_found", "Tenant not found.")
		}
		if lockErr != nil {
			return problem.Wrap(500, "tenant_lock_failed", "Failed to lock the Tenant lifecycle.", lockErr)
		}
		if current.LifecycleVersion != input.ExpectedVersion {
			return problem.New(409, "tenant_lifecycle_version_conflict", "Tenant lifecycle changed; reload it before retrying.")
		}
		allowed := tenantLifecycleTransitions[current.Status]
		if _, ok := allowed[toStatus]; !ok {
			return problem.New(409, "invalid_tenant_status_transition", "The requested Tenant status transition is not allowed.")
		}
		if toStatus == "suspended" || toStatus == "closed" {
			var activeExecutions int64
			if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
				Where("tenant_id = ? AND status IN ?", tenantID, activeTenantExecutionStatuses).
				Count(&activeExecutions).Error; err != nil {
				return problem.Wrap(500, "tenant_execution_check_failed", "Failed to inspect active Tenant Executions.", err)
			}
			if activeExecutions != 0 {
				return problem.New(409, "tenant_has_active_executions", "Stop or drain active Tenant Executions before changing to this status.")
			}
		}

		updates := map[string]any{
			"status": toStatus, "lifecycle_version": current.LifecycleVersion + 1,
			"trial_expires_at": nil, "suspended_at": nil, "closed_at": nil,
		}
		switch toStatus {
		case "trialing":
			updates["trial_expires_at"] = input.TrialExpiresAt.UTC()
		case "suspended":
			updates["suspended_at"] = now
		case "closed":
			updates["closed_at"] = now
		}
		result := tx.Model(&persistence.Tenant{}).
			Where("id = ? AND deleted_at IS NULL AND lifecycle_version = ?", tenantID, current.LifecycleVersion).
			Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "tenant_lifecycle_update_failed", "Failed to update the Tenant lifecycle.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "tenant_lifecycle_version_conflict", "Tenant lifecycle changed; reload it before retrying.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.lifecycle_transitioned", ResourceType: "tenant", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"fromStatus": productprofile.PublicLifecycleStatus(current.Status),
				"toStatus":   productprofile.PublicLifecycleStatus(toStatus), "reason": reason,
				"fromVersion": current.LifecycleVersion, "toVersion": current.LifecycleVersion + 1,
			},
		})
	})
	if err != nil {
		return Tenant{}, err
	}
	return s.GetTenant(ctx, principal, tenantID)
}

func (s *Service) RestoreTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input RestoreTenantInput,
	requestID string,
	ipAddress string,
) (Tenant, error) {
	if input.ExpectedVersion < 1 {
		return Tenant{}, problem.New(400, "invalid_tenant_lifecycle_version", "expectedVersion must be a positive integer.")
	}
	reason, err := normalizeLifecycleReason(input.Reason)
	if err != nil {
		return Tenant{}, err
	}
	var membership persistence.TenantMembership
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, principal.UserID, "active").
		Take(&membership).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Tenant{}, problem.New(404, "tenant_not_found", "Tenant not found.")
	} else if err != nil {
		return Tenant{}, problem.Wrap(500, "tenant_authorization_failed", "Failed to authorize Tenant recovery.", err)
	}
	if membership.Role != "owner" {
		return Tenant{}, problem.New(403, "tenant_restore_forbidden", "Only a tenant owner can restore a Tenant.")
	}

	now := time.Now().UTC()
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var current persistence.Tenant
		lockErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ?", tenantID).Take(&current).Error
		if errors.Is(lockErr, gorm.ErrRecordNotFound) {
			return problem.New(404, "tenant_not_found", "Tenant not found.")
		}
		if lockErr != nil {
			return problem.Wrap(500, "tenant_lock_failed", "Failed to lock the Tenant lifecycle.", lockErr)
		}
		if current.LifecycleVersion != input.ExpectedVersion {
			return problem.New(409, "tenant_lifecycle_version_conflict", "Tenant lifecycle changed; reload it before retrying.")
		}
		if current.Status != "deleting" || current.DeletedAt == nil {
			return problem.New(409, "tenant_not_deleting", "Only a deleting Tenant can be restored.")
		}
		var activeExecutions int64
		if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND status IN ?", tenantID, activeTenantExecutionStatuses).
			Count(&activeExecutions).Error; err != nil {
			return problem.Wrap(500, "tenant_execution_check_failed", "Failed to inspect Tenant Executions for recovery.", err)
		}
		if activeExecutions != 0 {
			return problem.New(409, "tenant_restore_cleanup_pending", "Tenant recovery is blocked until active Executions are terminal.")
		}
		var unclearedWorkspaces int64
		if err := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND state <> ?", tenantID, "cleaned").Count(&unclearedWorkspaces).Error; err != nil {
			return problem.Wrap(500, "tenant_workspace_cleanup_check_failed", "Failed to inspect Tenant Workspace cleanup.", err)
		}
		if unclearedWorkspaces != 0 {
			return problem.New(409, "tenant_restore_cleanup_pending", "Tenant recovery is blocked until Workspace cleanup completes.")
		}
		result := tx.Model(&persistence.Tenant{}).
			Where("id = ? AND status = ? AND deleted_at IS NOT NULL AND lifecycle_version = ?", tenantID, "deleting", current.LifecycleVersion).
			Updates(map[string]any{
				"status": "closed", "deleted_at": nil, "closed_at": now,
				"trial_expires_at": nil, "suspended_at": nil,
				"lifecycle_version": current.LifecycleVersion + 1,
			})
		if result.Error != nil {
			return problem.Wrap(409, "tenant_restore_conflict", "Tenant could not be restored; its slug may have been reused.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "tenant_lifecycle_version_conflict", "Tenant lifecycle changed; reload it before retrying.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.restored", ResourceType: "tenant", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"fromStatus": "deleting", "toStatus": "closed", "reason": reason,
				"fromVersion": current.LifecycleVersion, "toVersion": current.LifecycleVersion + 1,
			},
		})
	})
	if err != nil {
		return Tenant{}, err
	}
	return s.loadTenantForRole(ctx, tenantID, membership.Role)
}
