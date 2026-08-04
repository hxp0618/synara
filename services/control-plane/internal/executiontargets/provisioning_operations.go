package executiontargets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type ProvisioningOperation struct {
	ID                uuid.UUID       `json:"id"`
	TargetID          uuid.UUID       `json:"targetId"`
	Action            string          `json:"action"`
	State             string          `json:"state"`
	AttemptGeneration int64           `json:"attempt"`
	Result            map[string]any  `json:"result,omitempty"`
	Error             *OperationError `json:"error,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
	StartedAt         *time.Time      `json:"startedAt,omitempty"`
	CompletedAt       *time.Time      `json:"completedAt,omitempty"`
}

type OperationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ProvisioningClaim struct {
	Operation      ProvisioningOperation
	TenantID       uuid.UUID
	OrganizationID *uuid.UUID
	ActorType      string
	ActorID        uuid.UUID
	RequestID      string
	IPAddress      string
	ClaimHolder    string
	ClaimExpiresAt time.Time
}

func (s *Service) CreateProvisioningOperation(
	ctx context.Context, principal identity.Principal, tenantID, targetID uuid.UUID,
	action, idempotencyKey, requestID, ipAddress string,
) (ProvisioningOperation, bool, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return ProvisioningOperation{}, false, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return ProvisioningOperation{}, false, err
	}
	target, err := s.loadAccessible(ctx, tenantID, targetID, false)
	if err != nil {
		return ProvisioningOperation{}, false, err
	}
	if target.TenantID == nil || target.Kind != "ssh" {
		return ProvisioningOperation{}, false, problem.New(409, "execution_target_provisioning_unsupported", "Provisioning v1 requires a tenant-owned SSH execution target.")
	}
	if machine, ok := authorization.MachinePrincipalFromContext(ctx); ok && machine.OrganizationID != nil &&
		(target.OrganizationID == nil || *target.OrganizationID != *machine.OrganizationID) {
		return ProvisioningOperation{}, false, problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	action = strings.TrimSpace(action)
	if action != "install" && action != "upgrade" && action != "revoke" {
		return ProvisioningOperation{}, false, problem.New(400, "invalid_provisioning_action", "Provisioning action must be install, upgrade, or revoke.")
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if err := validateProvisioningIdempotencyKey(idempotencyKey); err != nil {
		return ProvisioningOperation{}, false, err
	}
	actorType, actorID := identity.ActorType(principal), identity.ActorID(principal)
	requestHash := provisioningRequestHash(targetID, action)
	now := time.Now().UTC()
	model := persistence.ExecutionTargetProvisioningOperation{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: target.OrganizationID,
		ExecutionTargetID: targetID, ActorType: actorType, ActorID: actorID, Action: action,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash, State: "accepted", Result: map[string]any{},
		RequestID: requestID, IPAddress: ipAddress, CreatedAt: now, UpdatedAt: now,
	}
	replayed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing persistence.ExecutionTargetProvisioningOperation
		lookup := tx.Where("tenant_id = ? AND actor_type = ? AND actor_id = ? AND idempotency_key = ?", tenantID, actorType, actorID, idempotencyKey).Take(&existing).Error
		if lookup == nil {
			if existing.RequestHash != requestHash {
				return problem.New(409, "idempotency_conflict", "Idempotency-Key was already used for a different request.")
			}
			model, replayed = existing, true
			return nil
		}
		if !errors.Is(lookup, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "provisioning_operation_lookup_failed", "Provisioning operation could not be loaded.", lookup)
		}
		var active persistence.ExecutionTargetProvisioningOperation
		if activeErr := tx.Where("execution_target_id = ? AND state IN ?", targetID, []string{"accepted", "running"}).Take(&active).Error; activeErr == nil {
			conflict := problem.New(409, "provisioning_in_progress", "Another provisioning operation is already active for this execution target.")
			conflict.Details = map[string]any{"provisioningOperationId": active.ID}
			return conflict
		} else if !errors.Is(activeErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "provisioning_operation_lookup_failed", "Provisioning operation could not be loaded.", activeErr)
		}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model)
		if created.Error != nil {
			return problem.Wrap(500, "provisioning_operation_create_failed", "Provisioning operation could not be created.", created.Error)
		}
		if created.RowsAffected == 0 {
			if err := tx.Where(
				"tenant_id = ? AND actor_type = ? AND actor_id = ? AND idempotency_key = ?",
				tenantID, actorType, actorID, idempotencyKey,
			).Take(&existing).Error; err == nil {
				if existing.RequestHash != requestHash {
					return problem.New(409, "idempotency_conflict", "Idempotency-Key was already used for a different request.")
				}
				model, replayed = existing, true
				return nil
			}
			if err := tx.Where("execution_target_id = ? AND state IN ?", targetID, []string{"accepted", "running"}).Take(&active).Error; err == nil {
				conflict := problem.New(409, "provisioning_in_progress", "Another provisioning operation is already active for this execution target.")
				conflict.Details = map[string]any{"provisioningOperationId": active.ID}
				return conflict
			}
			return problem.New(409, "provisioning_operation_conflict", "Provisioning operation changed concurrently; retry the request.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: actorType, ActorID: &actorID,
			Action: "execution_target.provisioning_accepted", ResourceType: "execution_target_provisioning_operation", ResourceID: &model.ID,
			OrganizationID: target.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"targetId": targetID, "action": action, "state": "accepted"},
		})
	})
	if err != nil {
		return ProvisioningOperation{}, false, err
	}
	return projectProvisioningOperation(model), replayed, nil
}

func (s *Service) ClaimNextProvisioningOperation(ctx context.Context, holder string, lease time.Duration) (ProvisioningClaim, bool, error) {
	holder = strings.TrimSpace(holder)
	if holder == "" || lease <= 0 {
		return ProvisioningClaim{}, false, errors.New("provisioning claim holder and positive lease are required")
	}
	var claimed persistence.ExecutionTargetProvisioningOperation
	now := time.Now().UTC()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "SKIP LOCKED").Where(
			"state = ? OR (state = ? AND claim_expires_at <= ?)", "accepted", "running", now,
		).Order("created_at, id").Limit(1)
		if err := query.Take(&claimed).Error; err != nil {
			return err
		}
		generation := claimed.AttemptGeneration + 1
		expires := now.Add(lease)
		result := tx.Model(&persistence.ExecutionTargetProvisioningOperation{}).Where(
			"id = ? AND attempt_generation = ? AND (state = ? OR (state = ? AND claim_expires_at <= ?))",
			claimed.ID, claimed.AttemptGeneration, "accepted", "running", now,
		).Updates(map[string]any{"state": "running", "attempt_generation": generation, "claim_holder": holder, "claim_expires_at": expires, "started_at": clause.Expr{SQL: "COALESCE(started_at, ?)", Vars: []any{now}}, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		claimed.State, claimed.AttemptGeneration, claimed.ClaimHolder, claimed.ClaimExpiresAt = "running", generation, &holder, &expires
		if claimed.StartedAt == nil {
			claimed.StartedAt = &now
		}
		claimed.UpdatedAt = now
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: claimed.TenantID, ActorType: claimed.ActorType, ActorID: &claimed.ActorID,
			Action: "execution_target.provisioning_claimed", ResourceType: "execution_target_provisioning_operation", ResourceID: &claimed.ID,
			OrganizationID: claimed.OrganizationID, RequestID: claimed.RequestID, IPAddress: claimed.IPAddress,
			Metadata: map[string]any{"targetId": claimed.ExecutionTargetID, "action": claimed.Action, "attempt": generation},
		})
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ProvisioningClaim{}, false, nil
	}
	if err != nil {
		return ProvisioningClaim{}, false, problem.Wrap(500, "provisioning_claim_failed", "Provisioning operation could not be claimed.", err)
	}
	return ProvisioningClaim{Operation: projectProvisioningOperation(claimed), TenantID: claimed.TenantID, OrganizationID: claimed.OrganizationID, ActorType: claimed.ActorType, ActorID: claimed.ActorID, RequestID: claimed.RequestID, IPAddress: claimed.IPAddress, ClaimHolder: holder, ClaimExpiresAt: *claimed.ClaimExpiresAt}, true, nil
}

func (s *Service) GetProvisioningOperation(
	ctx context.Context, principal identity.Principal, tenantID, targetID, operationID uuid.UUID,
) (ProvisioningOperation, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return ProvisioningOperation{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return ProvisioningOperation{}, err
	}
	target, err := s.loadAccessible(ctx, tenantID, targetID, false)
	if err != nil {
		return ProvisioningOperation{}, err
	}
	if target.TenantID == nil {
		return ProvisioningOperation{}, problem.New(404, "provisioning_operation_not_found", "Provisioning operation not found.")
	}
	if machine, ok := authorization.MachinePrincipalFromContext(ctx); ok && machine.OrganizationID != nil &&
		(target.OrganizationID == nil || *target.OrganizationID != *machine.OrganizationID) {
		return ProvisioningOperation{}, problem.New(404, "provisioning_operation_not_found", "Provisioning operation not found.")
	}
	var model persistence.ExecutionTargetProvisioningOperation
	if err := s.db.WithContext(ctx).Where(
		"id = ? AND tenant_id = ? AND execution_target_id = ?", operationID, tenantID, targetID,
	).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return ProvisioningOperation{}, problem.New(404, "provisioning_operation_not_found", "Provisioning operation not found.")
	} else if err != nil {
		return ProvisioningOperation{}, problem.Wrap(500, "provisioning_operation_lookup_failed", "Provisioning operation could not be loaded.", err)
	}
	return projectProvisioningOperation(model), nil
}

func (s *Service) RenewProvisioningClaim(ctx context.Context, claim ProvisioningClaim, lease time.Duration) (time.Time, error) {
	if lease <= 0 {
		return time.Time{}, errors.New("positive provisioning claim lease is required")
	}
	now, expires := time.Now().UTC(), time.Now().UTC().Add(lease)
	result := s.db.WithContext(ctx).Model(&persistence.ExecutionTargetProvisioningOperation{}).Where(
		"id = ? AND state = ? AND attempt_generation = ? AND claim_holder = ? AND claim_expires_at > ?",
		claim.Operation.ID, "running", claim.Operation.AttemptGeneration, claim.ClaimHolder, now,
	).Updates(map[string]any{"claim_expires_at": expires, "updated_at": now})
	if result.Error != nil {
		return time.Time{}, problem.Wrap(500, "provisioning_claim_renew_failed", "Provisioning claim could not be renewed.", result.Error)
	}
	if result.RowsAffected != 1 {
		return time.Time{}, problem.New(409, "provisioning_claim_lost", "Provisioning claim is no longer authoritative.")
	}
	return expires, nil
}

func (s *Service) CompleteProvisioningSuccess(
	ctx context.Context, claim ProvisioningClaim, result SSHProvisionResult,
) (ProvisioningOperation, error) {
	if result.TargetID != claim.Operation.TargetID || result.Operation != claim.Operation.Action ||
		(result.Status != "active" && result.Status != "disabled") {
		return ProvisioningOperation{}, errors.New("unsafe provisioning success result")
	}
	projected := map[string]any{"targetId": result.TargetID, "action": result.Operation, "status": result.Status}
	if result.BinarySHA256 != "" {
		decoded, err := hex.DecodeString(result.BinarySHA256)
		if err != nil || len(decoded) != sha256.Size || result.BinarySHA256 != strings.ToLower(result.BinarySHA256) {
			return ProvisioningOperation{}, errors.New("unsafe provisioning binary digest")
		}
		projected["binarySha256"] = result.BinarySHA256
	}
	return s.completeProvisioningOperation(ctx, claim, "succeeded", projected, "", "")
}

func (s *Service) CompleteProvisioningFailure(
	ctx context.Context, claim ProvisioningClaim, code, message string,
) (ProvisioningOperation, error) {
	code, message = strings.TrimSpace(code), strings.TrimSpace(message)
	if code == "" || len(code) > 120 || message == "" || len(message) > 500 || strings.ContainsAny(message, "\r\n") {
		return ProvisioningOperation{}, errors.New("unsafe provisioning failure projection")
	}
	return s.completeProvisioningOperation(ctx, claim, "failed", map[string]any{}, code, message)
}

func (s *Service) completeProvisioningOperation(
	ctx context.Context, claim ProvisioningClaim, state string, projected map[string]any, code, message string,
) (ProvisioningOperation, error) {
	var completed persistence.ExecutionTargetProvisioningOperation
	now := time.Now().UTC()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where(
			"id = ? AND state = ? AND attempt_generation = ? AND claim_holder = ? AND claim_expires_at > ?",
			claim.Operation.ID, "running", claim.Operation.AttemptGeneration, claim.ClaimHolder, now,
		).Take(&completed).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(409, "provisioning_claim_lost", "Provisioning claim is no longer authoritative.")
		} else if err != nil {
			return problem.Wrap(500, "provisioning_operation_lookup_failed", "Provisioning operation could not be loaded.", err)
		}
		claimHolder := claim.ClaimHolder
		completed.State, completed.Result, completed.ClaimHolder, completed.ClaimExpiresAt = state, projected, nil, nil
		completed.CompletedAt, completed.UpdatedAt = &now, now
		if state == "failed" {
			completed.ErrorCode, completed.ErrorMessage = &code, &message
		} else {
			completed.ErrorCode, completed.ErrorMessage = nil, nil
		}
		updated := tx.Model(&persistence.ExecutionTargetProvisioningOperation{}).Where(
			"id = ? AND attempt_generation = ? AND claim_holder = ?", completed.ID, claim.Operation.AttemptGeneration, claimHolder,
		).Select(
			"state", "result", "claim_holder", "claim_expires_at", "error_code", "error_message", "completed_at", "updated_at",
		).Updates(&completed)
		if updated.Error != nil {
			return problem.Wrap(500, "provisioning_operation_complete_failed", "Provisioning operation could not be completed.", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return problem.New(409, "provisioning_claim_lost", "Provisioning claim is no longer authoritative.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: completed.TenantID, ActorType: completed.ActorType, ActorID: &completed.ActorID,
			Action: "execution_target.provisioning_" + state, ResourceType: "execution_target_provisioning_operation", ResourceID: &completed.ID,
			OrganizationID: completed.OrganizationID, RequestID: completed.RequestID, IPAddress: completed.IPAddress,
			Metadata: map[string]any{"targetId": completed.ExecutionTargetID, "action": completed.Action, "state": state, "attempt": completed.AttemptGeneration},
		})
	})
	if err != nil {
		return ProvisioningOperation{}, err
	}
	return projectProvisioningOperation(completed), nil
}

func validateProvisioningIdempotencyKey(value string) error {
	if value == "" {
		return problem.New(400, "idempotency_key_required", "Idempotency-Key is required for provisioning operations.")
	}
	if len(value) > 200 {
		return problem.New(400, "invalid_idempotency_key", "Idempotency-Key must contain at most 200 characters.")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return problem.New(400, "invalid_idempotency_key", "Idempotency-Key must not contain whitespace or control characters.")
		}
	}
	return nil
}

func provisioningRequestHash(targetID uuid.UUID, action string) string {
	value := sha256.Sum256([]byte(targetID.String() + "\x00" + action))
	return hex.EncodeToString(value[:])
}

func projectProvisioningOperation(model persistence.ExecutionTargetProvisioningOperation) ProvisioningOperation {
	item := ProvisioningOperation{ID: model.ID, TargetID: model.ExecutionTargetID, Action: model.Action, State: model.State, AttemptGeneration: model.AttemptGeneration, Result: model.Result, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt, StartedAt: model.StartedAt, CompletedAt: model.CompletedAt}
	if model.ErrorCode != nil && model.ErrorMessage != nil {
		item.Error = &OperationError{Code: *model.ErrorCode, Message: *model.ErrorMessage}
	}
	return item
}
