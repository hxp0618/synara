package executions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	apiidempotency "github.com/synara-ai/synara/services/control-plane/internal/idempotency"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

const (
	defaultInteractionResolutionPullLimit = 10
	maximumInteractionResolutionPullLimit = 100
)

type expiredInteractionCandidate struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	ExecutionID uuid.UUID
}

func (s *Service) ListInteractions(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
) ([]Interaction, error) {
	tenantID, _, err := s.authorizeInteraction(ctx, principal, executionID)
	if err != nil {
		return nil, err
	}
	models := make([]persistence.ExecutionInteraction, 0)
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ?", tenantID, executionID).
		Order("requested_at, id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "interactions_load_failed", "Execution interactions could not be loaded.", err)
	}
	items := make([]Interaction, 0, len(models))
	for _, model := range models {
		items = append(items, toInteraction(model))
	}
	return items, nil
}

func (s *Service) ListPendingInteractions(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
) (PendingInteractionSnapshot, error) {
	tenantID, session, err := s.authorizeSessionInteraction(ctx, principal, sessionID)
	if err != nil {
		return PendingInteractionSnapshot{}, err
	}
	models := make([]persistence.ExecutionInteraction, 0)
	if err := s.db.WithContext(ctx).
		Where(
			"tenant_id = ? AND session_id = ? AND status = 'pending' AND expires_at > ?",
			tenantID, sessionID, s.now(),
		).
		Order("requested_at, id").Find(&models).Error; err != nil {
		return PendingInteractionSnapshot{}, problem.Wrap(
			500, "interactions_load_failed", "Pending Session interactions could not be loaded.", err,
		)
	}
	items := make([]PendingInteraction, 0, len(models))
	for _, model := range models {
		items = append(items, toPendingInteraction(model))
	}
	return PendingInteractionSnapshot{Items: items, SnapshotSequence: session.LastEventSequence}, nil
}

func (s *Service) ExpirePendingInteractions(
	ctx context.Context,
	expiredAt time.Time,
	limit int,
) (int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	candidates := make([]expiredInteractionCandidate, 0, limit)
	if err := s.db.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Select("id", "tenant_id", "execution_id").
		Where("status = 'pending' AND expires_at <= ?", expiredAt).
		Order("expires_at, tenant_id, execution_id, id").Limit(limit).
		Find(&candidates).Error; err != nil {
		return 0, problem.Wrap(500, "expired_interaction_scan_failed", "Expired interactions could not be scanned.", err)
	}
	const reason = "The interaction exceeded its maximum waiting time before a user response was received."
	expired := 0
	for _, candidate := range candidates {
		changed, _, err := s.expirePendingInteractionCandidate(ctx, candidate, expiredAt, reason)
		if err != nil {
			return expired, err
		}
		if changed {
			expired++
		}
	}
	return expired, nil
}

func (s *Service) expireValidatedLeasePendingInteraction(
	ctx context.Context,
	tx *gorm.DB,
	lease persistence.WorkerLease,
	execution persistence.AgentExecution,
	expiredAt time.Time,
) (bool, persistence.SessionEvent, error) {
	if execution.Status != "waiting-for-approval" {
		return false, persistence.SessionEvent{}, nil
	}
	var interaction persistence.ExecutionInteraction
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where(
			"tenant_id = ? AND execution_id = ? AND worker_id = ? AND generation = ? AND status = 'pending' AND expires_at <= ?",
			execution.TenantID, execution.ID, lease.WorkerID, lease.Generation, expiredAt,
		).
		Order("expires_at, id").Take(&interaction).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, persistence.SessionEvent{}, nil
	}
	if err != nil {
		return false, persistence.SessionEvent{}, problem.Wrap(
			500, "interaction_expiry_lock_failed", "The expired interaction could not be locked.", err,
		)
	}
	leaseDelete := tx.WithContext(ctx).
		Where(
			"tenant_id = ? AND execution_id = ? AND worker_id = ? AND generation = ?",
			lease.TenantID, lease.ExecutionID, lease.WorkerID, lease.Generation,
		).
		Delete(&persistence.WorkerLease{})
	if err := expectOne(
		leaseDelete, 409, "interaction_expiry_lease_release_conflict", "The interaction lease changed during expiry.",
	); err != nil {
		return false, persistence.SessionEvent{}, err
	}
	const reason = "The interaction exceeded its maximum waiting time before a user response was received."
	appended, err := s.recoverExecutionGenerationLocked(
		ctx, tx, execution, lease, "interaction_expired", reason, "",
	)
	if err != nil {
		return false, persistence.SessionEvent{}, err
	}
	return true, appended, nil
}

func (s *Service) expirePendingInteractionCandidate(
	ctx context.Context,
	candidate expiredInteractionCandidate,
	expiredAt time.Time,
	reason string,
) (bool, persistence.SessionEvent, error) {
	var appended persistence.SessionEvent
	changed := false
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var lease persistence.WorkerLease
		leaseErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ?", candidate.TenantID, candidate.ExecutionID).
			Take(&lease).Error
		if leaseErr != nil && !errors.Is(leaseErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "interaction_expiry_lease_lock_failed", "The interaction lease could not be locked for expiry.", leaseErr)
		}

		var execution persistence.AgentExecution
		executionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", candidate.TenantID, candidate.ExecutionID).
			Take(&execution).Error
		if errors.Is(executionErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if executionErr != nil {
			return problem.Wrap(500, "interaction_expiry_execution_lock_failed", "The interaction Execution could not be locked for expiry.", executionErr)
		}

		var interaction persistence.ExecutionInteraction
		interactionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where(
				"id = ? AND tenant_id = ? AND execution_id = ? AND status = 'pending'",
				candidate.ID, candidate.TenantID, candidate.ExecutionID,
			).
			Take(&interaction).Error
		if errors.Is(interactionErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if interactionErr != nil {
			return problem.Wrap(500, "interaction_expiry_lock_failed", "The expired interaction could not be locked.", interactionErr)
		}
		if interaction.ExpiresAt.After(expiredAt) {
			return nil
		}

		activeGeneration := execution.Status == "waiting-for-approval" &&
			execution.WorkerID != nil && *execution.WorkerID == interaction.WorkerID &&
			execution.Generation == interaction.Generation
		if !activeGeneration {
			updated, err := expirePendingInteractionRow(ctx, tx, interaction.ID, expiredAt, reason)
			if err != nil {
				return err
			}
			changed = updated
			return nil
		}

		if leaseErr == nil {
			if lease.WorkerID != interaction.WorkerID || lease.Generation != interaction.Generation {
				updated, err := expirePendingInteractionRow(ctx, tx, interaction.ID, expiredAt, reason)
				if err != nil {
					return err
				}
				changed = updated
				return nil
			}
			leaseDelete := tx.WithContext(ctx).
				Where(
					"tenant_id = ? AND execution_id = ? AND worker_id = ? AND generation = ?",
					lease.TenantID, lease.ExecutionID, lease.WorkerID, lease.Generation,
				).
				Delete(&persistence.WorkerLease{})
			if err := expectOne(leaseDelete, 409, "interaction_expiry_lease_release_conflict", "The interaction lease changed during expiry."); err != nil {
				return err
			}
		} else {
			lease = persistence.WorkerLease{
				ExecutionID: execution.ID, TenantID: execution.TenantID,
				WorkerID: interaction.WorkerID, Generation: interaction.Generation,
			}
		}

		var err error
		appended, err = s.recoverExecutionGenerationLocked(
			ctx, tx, execution, lease, "interaction_expired", reason, "",
		)
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, persistence.SessionEvent{}, err
	}
	if appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return changed, appended, nil
}

func expirePendingInteractionRow(
	ctx context.Context,
	tx *gorm.DB,
	interactionID uuid.UUID,
	expiredAt time.Time,
	reason string,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where("id = ? AND status = 'pending' AND expires_at <= ?", interactionID, expiredAt).
		Updates(map[string]any{
			"status": "expired", "delivery_status": "superseded", "delivery_error": reason,
		})
	if result.Error != nil {
		return false, problem.Wrap(500, "interaction_expiry_failed", "The expired interaction could not be persisted.", result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (s *Service) ResolveApproval(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
	requestID string,
	input ResolveApprovalInput,
	idempotencyKey, auditRequestID, ipAddress string,
) (OperationResult[Interaction], error) {
	decision := strings.ToLower(strings.TrimSpace(input.Decision))
	if decision != "accept" && decision != "decline" {
		return OperationResult[Interaction]{}, problem.New(400, "invalid_approval_decision", "decision must be accept or decline.")
	}
	return s.resolveInteraction(
		ctx, principal, executionID, requestID, "approval", map[string]any{"decision": decision},
		idempotencyKey, auditRequestID, ipAddress,
	)
}

func (s *Service) ResolveUserInput(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
	requestID string,
	input ResolveUserInputInput,
	idempotencyKey, auditRequestID, ipAddress string,
) (OperationResult[Interaction], error) {
	if input.Answers == nil {
		return OperationResult[Interaction]{}, problem.New(400, "invalid_user_input_answers", "answers must be a JSON object.")
	}
	return s.resolveInteraction(
		ctx, principal, executionID, requestID, "user-input", map[string]any{"answers": input.Answers},
		idempotencyKey, auditRequestID, ipAddress,
	)
}

func (s *Service) PullInteractionResolutions(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input PullInteractionResolutionsInput,
) ([]InteractionResolutionDelivery, error) {
	limit := input.Limit
	if limit == 0 {
		limit = defaultInteractionResolutionPullLimit
	}
	if limit < 1 || limit > maximumInteractionResolutionPullLimit {
		return nil, problem.New(400, "invalid_interaction_resolution_limit", "limit must be between 1 and 100.")
	}

	items := make([]InteractionResolutionDelivery, 0)
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return err
		}
		models := make([]persistence.ExecutionInteraction, 0)
		if err := tx.WithContext(ctx).
			Where(
				"tenant_id = ? AND execution_id = ? AND status = ? AND delivery_worker_id = ? AND delivery_generation = ? AND delivery_status IN ? AND delivery_available_at <= ?",
				execution.TenantID, execution.ID, "resolved", worker.ID, input.Generation,
				[]string{"pending", "delivered", "failed"}, s.now(),
			).
			Order("delivery_available_at, id").Limit(limit).Find(&models).Error; err != nil {
			return problem.Wrap(500, "interaction_resolutions_load_failed", "Interaction resolutions could not be loaded.", err)
		}
		for _, model := range models {
			item, err := toInteractionResolutionDelivery(model)
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		return nil
	})
	return items, err
}

func (s *Service) MarkInteractionResolutionDelivered(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID, interactionID uuid.UUID,
	input InteractionResolutionDeliveryInput,
	requestID string,
) (OperationResult[Interaction], error) {
	return s.updateInteractionResolutionDelivery(
		ctx, worker, executionID, interactionID, input, requestID, "delivered",
	)
}

func (s *Service) AcknowledgeInteractionResolution(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID, interactionID uuid.UUID,
	input InteractionResolutionDeliveryInput,
	requestID string,
) (OperationResult[Interaction], error) {
	return s.updateInteractionResolutionDelivery(
		ctx, worker, executionID, interactionID, input, requestID, "acknowledged",
	)
}

func (s *Service) updateInteractionResolutionDelivery(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID, interactionID uuid.UUID,
	input InteractionResolutionDeliveryInput,
	requestID, targetStatus string,
) (OperationResult[Interaction], error) {
	input.ResolutionCommandID = strings.TrimSpace(input.ResolutionCommandID)
	if input.ResolutionCommandID == "" || len(input.ResolutionCommandID) > 240 ||
		strings.ContainsAny(input.ResolutionCommandID, "\r\n\t") {
		return OperationResult[Interaction]{}, problem.New(
			400, "invalid_interaction_resolution_command_id", "resolutionCommandId is invalid.",
		)
	}
	return runIdempotent(ctx, s, worker, requestID, "interaction.resolution."+targetStatus, struct {
		ExecutionID   uuid.UUID                          `json:"executionId"`
		InteractionID uuid.UUID                          `json:"interactionId"`
		Input         InteractionResolutionDeliveryInput `json:"input"`
	}{executionID, interactionID, input}, 200, func(tx *gorm.DB) (Interaction, error) {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return Interaction{}, err
		}
		var interaction persistence.ExecutionInteraction
		err = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ? AND id = ?", execution.TenantID, execution.ID, interactionID).
			Take(&interaction).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Interaction{}, problem.New(404, "interaction_not_found", "Execution interaction not found.")
		}
		if err != nil {
			return Interaction{}, problem.Wrap(500, "interaction_lock_failed", "The execution interaction could not be locked.", err)
		}
		if interaction.Status != "resolved" || interaction.ResolutionCommandID == nil {
			return Interaction{}, problem.New(409, "interaction_resolution_not_ready", "The interaction does not have a deliverable resolution.")
		}
		if interaction.DeliveryWorkerID == nil || *interaction.DeliveryWorkerID != worker.ID ||
			interaction.DeliveryGeneration == nil || *interaction.DeliveryGeneration != input.Generation {
			return Interaction{}, problem.New(409, "interaction_generation_fenced", "The interaction resolution belongs to an obsolete Worker generation.")
		}
		if *interaction.ResolutionCommandID != input.ResolutionCommandID {
			return Interaction{}, problem.New(409, "interaction_resolution_command_mismatch", "The resolution command does not match the persisted interaction.")
		}

		now := s.now()
		switch targetStatus {
		case "delivered":
			if interaction.DeliveryStatus == "acknowledged" || interaction.DeliveryStatus == "delivered" {
				return toInteraction(interaction), nil
			}
			if interaction.DeliveryStatus != "pending" && interaction.DeliveryStatus != "failed" {
				return Interaction{}, problem.New(409, "interaction_resolution_not_deliverable", "The interaction resolution cannot be delivered from its current state.")
			}
			updated := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
				Where("tenant_id = ? AND execution_id = ? AND id = ? AND delivery_worker_id = ? AND delivery_generation = ? AND delivery_status IN ?",
					execution.TenantID, execution.ID, interaction.ID, worker.ID, input.Generation, []string{"pending", "failed"}).
				Updates(map[string]any{
					"delivery_status": "delivered", "delivery_attempts": gorm.Expr("delivery_attempts + 1"),
					"delivered_at": now, "delivery_error": nil,
				})
			if err := expectOne(updated, 409, "interaction_delivery_conflict", "The interaction resolution delivery changed concurrently."); err != nil {
				return Interaction{}, err
			}
			interaction.DeliveryStatus = "delivered"
			interaction.DeliveryAttempts++
			interaction.DeliveredAt = &now
			interaction.DeliveryError = nil
		case "acknowledged":
			if interaction.DeliveryStatus == "acknowledged" {
				return toInteraction(interaction), nil
			}
			if interaction.DeliveryStatus != "delivered" || interaction.DeliveredAt == nil {
				return Interaction{}, problem.New(409, "interaction_resolution_not_delivered", "The interaction resolution must be delivered before it can be acknowledged.")
			}
			updated := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
				Where("tenant_id = ? AND execution_id = ? AND id = ? AND delivery_worker_id = ? AND delivery_generation = ? AND delivery_status = ?",
					execution.TenantID, execution.ID, interaction.ID, worker.ID, input.Generation, "delivered").
				Updates(map[string]any{"delivery_status": "acknowledged", "acknowledged_at": now, "delivery_error": nil})
			if err := expectOne(updated, 409, "interaction_acknowledgement_conflict", "The interaction resolution acknowledgement changed concurrently."); err != nil {
				return Interaction{}, err
			}
			interaction.DeliveryStatus = "acknowledged"
			interaction.AcknowledgedAt = &now
			interaction.DeliveryError = nil
		default:
			return Interaction{}, problem.New(500, "invalid_interaction_delivery_transition", "The interaction resolution delivery transition is invalid.")
		}
		return toInteraction(interaction), nil
	})
}

func toInteractionResolutionDelivery(model persistence.ExecutionInteraction) (InteractionResolutionDelivery, error) {
	if model.ResolutionCommandID == nil || model.ResolutionKind == nil || model.DeliveryAvailableAt == nil || model.Resolution == nil {
		return InteractionResolutionDelivery{}, problem.New(500, "interaction_resolution_corrupt", "The persisted interaction resolution is incomplete.")
	}
	commandType := ""
	switch model.Kind {
	case "approval":
		commandType = "ResolveApproval"
	case "user-input":
		commandType = "ResolveUserInput"
	default:
		return InteractionResolutionDelivery{}, problem.New(500, "interaction_resolution_corrupt", fmt.Sprintf("Unsupported persisted interaction kind %q.", model.Kind))
	}
	return InteractionResolutionDelivery{
		InteractionID: model.ID, RequestID: model.RequestID, Provider: model.Provider,
		CommandType: commandType, CommandID: *model.ResolutionCommandID,
		ResolutionKind: *model.ResolutionKind, Resolution: model.Resolution,
		DeliveryStatus: model.DeliveryStatus, DeliveryAttempts: model.DeliveryAttempts,
		DeliveryAvailableAt: *model.DeliveryAvailableAt,
	}, nil
}

func (s *Service) supersedeInteractionGeneration(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	lease persistence.WorkerLease,
) error {
	return s.supersedeInteractionGenerationWithReason(
		ctx, tx, execution, lease, "The Worker lease expired before the interaction lifecycle completed.",
	)
}

func (s *Service) supersedeInteractionGenerationWithReason(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	lease persistence.WorkerLease,
	reason string,
) error {
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND worker_id = ? AND generation = ? AND status = ?",
			execution.TenantID, execution.ID, lease.WorkerID, lease.Generation, "pending").
		Updates(map[string]any{
			"status": "expired", "delivery_status": "superseded", "delivery_error": reason,
		}).Error; err != nil {
		return problem.Wrap(500, "interaction_expiry_failed", "Pending interactions could not be expired during Worker recovery.", err)
	}
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND delivery_worker_id = ? AND delivery_generation = ? AND status = ? AND delivery_status IN ?",
			execution.TenantID, execution.ID, lease.WorkerID, lease.Generation, "resolved",
			[]string{"pending", "delivered", "failed"}).
		Updates(map[string]any{"delivery_status": "superseded", "delivery_error": reason}).Error; err != nil {
		return problem.Wrap(500, "interaction_delivery_supersede_failed", "Interaction resolution delivery could not be superseded during Worker recovery.", err)
	}
	return nil
}

func (s *Service) resolveInteraction(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
	requestID, kind string,
	resolution map[string]any,
	idempotencyKey, auditRequestID, ipAddress string,
) (OperationResult[Interaction], error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 200 || strings.ContainsAny(requestID, "\r\n\t") {
		return OperationResult[Interaction]{}, problem.New(400, "invalid_interaction_request_id", "requestId is invalid.")
	}
	tenantID, session, err := s.authorizeInteraction(ctx, principal, executionID)
	if err != nil {
		return OperationResult[Interaction]{}, err
	}

	var appended persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "interaction." + kind + ".resolve", SuccessStatus: 200,
		Request: map[string]any{
			"executionId": executionID, "requestId": requestID, "kind": kind, "resolution": resolution,
		},
	}, func(tx *gorm.DB) (Interaction, error) {
		var lease persistence.WorkerLease
		leaseErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ?", tenantID, executionID).Take(&lease).Error
		if errors.Is(leaseErr, gorm.ErrRecordNotFound) {
			var interactionState persistence.ExecutionInteraction
			stateErr := tx.WithContext(ctx).
				Select("id", "status", "expires_at").
				Where(
					"tenant_id = ? AND execution_id = ? AND request_id = ? AND kind = ?",
					tenantID, executionID, requestID, kind,
				).
				Take(&interactionState).Error
			if stateErr == nil && (interactionState.Status == "expired" || !interactionState.ExpiresAt.After(s.now())) {
				return Interaction{}, problem.New(409, "interaction_expired", "The execution interaction expired before it was resolved.")
			}
			if stateErr != nil && !errors.Is(stateErr, gorm.ErrRecordNotFound) {
				return Interaction{}, problem.Wrap(500, "interaction_load_failed", "The execution interaction could not be loaded.", stateErr)
			}
			return Interaction{}, problem.New(409, "interaction_lease_expired", "The execution lease is no longer active.")
		}
		if leaseErr != nil {
			return Interaction{}, problem.Wrap(500, "lease_lock_failed", "Failed to lock the execution lease.", leaseErr)
		}
		if !lease.ExpiresAt.After(s.now()) {
			return Interaction{}, problem.New(409, "interaction_lease_expired", "The execution lease expired before the interaction was resolved.")
		}

		var execution persistence.AgentExecution
		executionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&execution).Error
		if executionErr != nil {
			return Interaction{}, problem.Wrap(409, "execution_state_conflict", "The execution is no longer available for interaction resolution.", executionErr)
		}
		var interaction persistence.ExecutionInteraction
		interactionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ? AND request_id = ? AND kind = ?", tenantID, executionID, requestID, kind).
			Take(&interaction).Error
		if errors.Is(interactionErr, gorm.ErrRecordNotFound) {
			return Interaction{}, problem.New(404, "interaction_not_found", "Execution interaction not found.")
		}
		if interactionErr != nil {
			return Interaction{}, problem.Wrap(500, "interaction_lock_failed", "The execution interaction could not be locked.", interactionErr)
		}
		if interaction.Status == "resolved" {
			if !sameJSON(interaction.Resolution, resolution) {
				return Interaction{}, problem.New(409, "interaction_resolution_conflict", "The execution interaction was already resolved differently.")
			}
			return toInteraction(interaction), nil
		}
		if interaction.Status != "pending" {
			return Interaction{}, problem.New(409, "interaction_not_pending", "The execution interaction is no longer pending.")
		}
		now := s.now()
		if !interaction.ExpiresAt.After(now) {
			return Interaction{}, problem.New(409, "interaction_expired", "The execution interaction expired before it was resolved.")
		}
		if kind == "user-input" {
			answers, _ := resolution["answers"].(map[string]any)
			if err := validateUserInputResolution(interaction.Payload, answers); err != nil {
				return Interaction{}, err
			}
		}
		if interaction.WorkerID != lease.WorkerID || interaction.Generation != lease.Generation ||
			execution.WorkerID == nil || *execution.WorkerID != lease.WorkerID || execution.Generation != lease.Generation {
			return Interaction{}, problem.New(409, "interaction_generation_fenced", "The interaction belongs to an obsolete Worker generation.")
		}

		resolutionKind := interactionResolutionKind(kind, resolution)
		resolutionCommandID := requestID + ":resolution"
		deliveryWorkerID := lease.WorkerID
		deliveryGeneration := lease.Generation
		updated := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
			Where("id = ? AND status = ?", interaction.ID, "pending").
			Select(
				"status", "resolution", "resolved_at", "resolved_by", "resolution_kind", "resolution_command_id",
				"delivery_status", "delivery_worker_id", "delivery_generation", "delivery_available_at",
			).
			Updates(&persistence.ExecutionInteraction{
				Status: "resolved", Resolution: resolution, ResolvedAt: &now, ResolvedBy: &principal.UserID,
				ResolutionKind: &resolutionKind, ResolutionCommandID: &resolutionCommandID,
				DeliveryStatus: "pending", DeliveryWorkerID: &deliveryWorkerID,
				DeliveryGeneration: &deliveryGeneration, DeliveryAvailableAt: &now,
			})
		if err := expectOne(updated, 409, "interaction_resolve_conflict", "The execution interaction was resolved concurrently."); err != nil {
			return Interaction{}, err
		}
		interaction.Status = "resolved"
		interaction.Resolution = resolution
		interaction.ResolvedAt = &now
		interaction.ResolvedBy = &principal.UserID
		interaction.ResolutionKind = &resolutionKind
		interaction.ResolutionCommandID = &resolutionCommandID
		interaction.DeliveryStatus = "pending"
		interaction.DeliveryWorkerID = &deliveryWorkerID
		interaction.DeliveryGeneration = &deliveryGeneration
		interaction.DeliveryAvailableAt = &now

		var pending int64
		if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
			Where("tenant_id = ? AND execution_id = ? AND status = ?", tenantID, executionID, "pending").
			Count(&pending).Error; err != nil {
			return Interaction{}, problem.Wrap(500, "interaction_pending_count_failed", "Pending interactions could not be checked.", err)
		}
		if pending == 0 && execution.Status == "waiting-for-approval" {
			resumed := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
				Where("tenant_id = ? AND id = ? AND status = ? AND worker_id = ? AND generation = ?",
					tenantID, executionID, "waiting-for-approval", lease.WorkerID, lease.Generation).
				Update("status", "running")
			if err := expectOne(resumed, 409, "execution_resume_conflict", "The execution could not resume after interaction resolution."); err != nil {
				return Interaction{}, err
			}
		}
		eventVersion, eventType, eventPayload, eventErr := resolvedInteractionRuntimeEvent(interaction, resolution)
		if eventErr != nil {
			return Interaction{}, eventErr
		}
		workerID := interaction.WorkerID
		generation := interaction.Generation
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, tenantID, execution.SessionID, sessions.InternalEventInput{
			EventVersion: eventVersion, EventType: eventType, ActorType: "user", ActorID: &principal.UserID,
			ExecutionID: &execution.ID, WorkerID: &workerID, Generation: &generation, Payload: eventPayload,
		})
		if err != nil {
			return Interaction{}, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "execution." + kind + "_resolved", ResourceType: "execution_interaction", ResourceID: &interaction.ID,
			OrganizationID: &session.OrganizationID, RequestID: auditRequestID, IPAddress: ipAddress,
			Metadata: map[string]any{"executionId": executionID, "requestId": requestID},
		}); err != nil {
			return Interaction{}, err
		}
		return toInteraction(interaction), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return OperationResult[Interaction]{
		Value: result.Value, Replayed: result.Replayed, StatusCode: result.StatusCode,
	}, err
}

func resolvedInteractionRuntimeEvent(
	interaction persistence.ExecutionInteraction,
	resolution map[string]any,
) (int, string, map[string]any, error) {
	eventVersion := interaction.EventVersion
	if eventVersion <= 0 {
		eventVersion = RuntimeEventVersionV1
	}
	if eventVersion == RuntimeEventVersionV1 {
		return eventVersion, interaction.Kind + ".resolved", map[string]any{
			"requestId": interaction.RequestID, "resolution": resolution,
		}, nil
	}
	if eventVersion != RuntimeEventVersionV2 {
		return 0, "", nil, problem.New(500, "interaction_event_version_corrupt", "The persisted interaction Runtime Event version is unsupported.")
	}

	switch interaction.Kind {
	case "approval":
		requestType, _ := interaction.Payload["requestType"].(string)
		if !requiredCanonicalRequestType(map[string]any{"requestType": requestType}) {
			return 0, "", nil, problem.New(500, "interaction_event_payload_corrupt", "The persisted approval requestType is invalid.")
		}
		payload := map[string]any{
			"requestId": interaction.RequestID, "requestType": requestType, "resolution": resolution,
		}
		if decision, ok := resolution["decision"].(string); ok && strings.TrimSpace(decision) != "" {
			payload["decision"] = decision
		}
		return eventVersion, "request.resolved", payload, nil
	case "user-input":
		answers, ok := resolution["answers"].(map[string]any)
		if !ok || answers == nil {
			return 0, "", nil, problem.New(500, "interaction_event_payload_corrupt", "The persisted user-input answers are invalid.")
		}
		return eventVersion, "user-input.resolved", map[string]any{
			"requestId": interaction.RequestID, "answers": answers,
		}, nil
	default:
		return 0, "", nil, problem.New(500, "interaction_kind_corrupt", "The persisted interaction kind is unsupported.")
	}
}

func interactionResolutionKind(kind string, resolution map[string]any) string {
	if kind == "user-input" {
		return "answered"
	}
	decision, _ := resolution["decision"].(string)
	if strings.EqualFold(strings.TrimSpace(decision), "accept") {
		return "approved"
	}
	return "denied"
}

func (s *Service) authorizeInteraction(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
) (uuid.UUID, sessions.Session, error) {
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return uuid.Nil, sessions.Session{}, err
	}
	var execution persistence.AgentExecution
	err = s.db.WithContext(ctx).Select("id", "session_id").
		Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&execution).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return uuid.Nil, sessions.Session{}, problem.New(404, "execution_not_found", "Execution not found.")
	}
	if err != nil {
		return uuid.Nil, sessions.Session{}, problem.Wrap(500, "execution_load_failed", "Failed to load the execution.", err)
	}
	return s.authorizeSessionInteractionForTenant(ctx, principal, tenantID, execution.SessionID)
}

func (s *Service) authorizeSessionInteraction(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
) (uuid.UUID, sessions.Session, error) {
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return uuid.Nil, sessions.Session{}, err
	}
	return s.authorizeSessionInteractionForTenant(ctx, principal, tenantID, sessionID)
}

func (s *Service) authorizeSessionInteractionForTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID, sessionID uuid.UUID,
) (uuid.UUID, sessions.Session, error) {
	session, err := s.sessions.Get(ctx, principal, tenantID, sessionID)
	if err != nil {
		return uuid.Nil, sessions.Session{}, err
	}
	if _, err := s.authorizer.RequireOrganization(
		ctx, principal.UserID, tenantID, session.OrganizationID, authorization.ExecutionApprove,
	); err != nil {
		return uuid.Nil, sessions.Session{}, err
	}
	return tenantID, session, nil
}
