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
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

const (
	defaultControlCommandPullLimit = 10
	maximumControlCommandPullLimit = 100
)

func (s *Service) RequestInterrupt(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[ControlCommand], error) {
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return OperationResult[ControlCommand]{}, err
	}
	session, err := s.sessions.Get(ctx, principal, tenantID, sessionID)
	if err != nil {
		return OperationResult[ControlCommand]{}, err
	}
	if _, err := s.authorizer.RequireOrganization(
		ctx, principal.UserID, tenantID, session.OrganizationID, authorization.ExecutionCancel,
	); err != nil {
		return OperationResult[ControlCommand]{}, err
	}

	var appended persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "session.turn.interrupt", SuccessStatus: 202,
		Request: map[string]any{"sessionId": sessionID},
	}, func(tx *gorm.DB) (ControlCommand, error) {
		var execution persistence.AgentExecution
		executionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND session_id = ? AND status IN ?", tenantID, sessionID,
				[]string{"queued", "recovering", "leased", "running", "waiting-for-approval"}).
			Order("queued_at DESC, id DESC").Take(&execution).Error
		if errors.Is(executionErr, gorm.ErrRecordNotFound) {
			return ControlCommand{}, problem.New(409, "active_execution_not_found", "The Session does not have an active Execution to interrupt.")
		}
		if executionErr != nil {
			return ControlCommand{}, problem.Wrap(500, "execution_lock_failed", "The active Execution could not be locked.", executionErr)
		}

		var existing persistence.ExecutionControlCommand
		existingErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ? AND command_type = ? AND status IN ?",
				tenantID, execution.ID, "InterruptTurn", []string{"pending", "delivered"}).
			Take(&existing).Error
		if existingErr == nil {
			return toControlCommand(existing), nil
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return ControlCommand{}, problem.Wrap(500, "control_command_lookup_failed", "The active Interrupt command could not be loaded.", existingErr)
		}

		provider := strings.TrimSpace(session.Provider)
		if execution.Provider != nil && strings.TrimSpace(*execution.Provider) != "" {
			provider = strings.TrimSpace(*execution.Provider)
		}
		if provider == "" {
			return ControlCommand{}, problem.New(409, "execution_provider_missing", "The active Execution does not have a Provider binding.")
		}
		now := s.now()
		commandID := uuid.New()
		model := persistence.ExecutionControlCommand{
			ID: commandID, TenantID: tenantID, ExecutionID: execution.ID, SessionID: execution.SessionID,
			TurnID: execution.TurnID, Provider: provider, CommandType: "InterruptTurn",
			CommandID: "interrupt:" + commandID.String(), Payload: map[string]any{"turnId": execution.TurnID.String()},
			Status: "pending", RequestedBy: principal.UserID, RequestedAt: now,
			DeliveryAvailableAt: now,
		}
		if execution.WorkerID != nil && execution.Generation > 0 {
			generation := execution.Generation
			model.DeliveryWorkerID = execution.WorkerID
			model.DeliveryGeneration = &generation
		}
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return ControlCommand{}, problem.Wrap(409, "control_command_conflict", "The Interrupt command conflicts with another active command.", err)
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, tenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "turn.interrupt-requested", ActorType: "user", ActorID: &principal.UserID,
			ExecutionID: &execution.ID, WorkerID: execution.WorkerID,
			Generation: model.DeliveryGeneration,
			Payload: map[string]any{
				"turnId": execution.TurnID, "controlCommandId": model.ID, "commandId": model.CommandID,
			},
		})
		if err != nil {
			return ControlCommand{}, err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "turn.interrupt-requested", MessageKey: model.ID.String(),
			Payload: map[string]any{
				"tenantId": tenantID, "sessionId": execution.SessionID, "turnId": execution.TurnID,
				"executionId": execution.ID, "controlCommandId": model.ID, "commandId": model.CommandID,
			},
		}); err != nil {
			return ControlCommand{}, problem.Wrap(500, "control_command_outbox_failed", "The Interrupt command event could not be queued.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "turn.interrupt_requested", ResourceType: "agent_execution", ResourceID: &execution.ID,
			OrganizationID: &session.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"sessionId": execution.SessionID, "turnId": execution.TurnID, "controlCommandId": model.ID,
			},
		}); err != nil {
			return ControlCommand{}, err
		}
		return toControlCommand(model), nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return OperationResult[ControlCommand]{
		Value: result.Value, Replayed: result.Replayed, StatusCode: result.StatusCode,
	}, err
}

func (s *Service) PullControlCommands(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input PullControlCommandsInput,
) ([]ControlCommandDelivery, error) {
	limit := input.Limit
	if limit == 0 {
		limit = defaultControlCommandPullLimit
	}
	if limit < 1 || limit > maximumControlCommandPullLimit {
		return nil, problem.New(400, "invalid_control_command_limit", "limit must be between 1 and 100.")
	}
	items := make([]ControlCommandDelivery, 0)
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		_, execution, err := s.lockLease(ctx, tx, worker.ID, executionID, input.LeaseInput, true)
		if err != nil {
			return err
		}
		models := make([]persistence.ExecutionControlCommand, 0)
		if err := tx.WithContext(ctx).
			Where("tenant_id = ? AND execution_id = ? AND delivery_worker_id = ? AND delivery_generation = ? AND status IN ? AND delivery_available_at <= ?",
				execution.TenantID, execution.ID, worker.ID, input.Generation,
				[]string{"pending", "delivered"}, s.now()).
			Order("delivery_available_at, id").Limit(limit).Find(&models).Error; err != nil {
			return problem.Wrap(500, "control_commands_load_failed", "Control commands could not be loaded.", err)
		}
		for _, model := range models {
			items = append(items, toControlCommandDelivery(model))
		}
		return nil
	})
	return items, err
}

func (s *Service) MarkControlCommandDelivered(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID, controlCommandID uuid.UUID,
	input ControlCommandDeliveryInput,
	requestID string,
) (OperationResult[ControlCommand], error) {
	return s.updateControlCommandDelivery(
		ctx, worker, executionID, controlCommandID, input, requestID, false,
	)
}

func (s *Service) AcknowledgeControlCommand(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID, controlCommandID uuid.UUID,
	input ControlCommandDeliveryInput,
	requestID string,
) (OperationResult[ControlCommand], error) {
	return s.updateControlCommandDelivery(
		ctx, worker, executionID, controlCommandID, input, requestID, true,
	)
}

func (s *Service) updateControlCommandDelivery(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID, controlCommandID uuid.UUID,
	input ControlCommandDeliveryInput,
	requestID string,
	acknowledge bool,
) (OperationResult[ControlCommand], error) {
	input.CommandID = strings.TrimSpace(input.CommandID)
	if input.CommandID == "" || len(input.CommandID) > 240 || strings.ContainsAny(input.CommandID, "\r\n\t") {
		return OperationResult[ControlCommand]{}, problem.New(400, "invalid_control_command_id", "commandId is invalid.")
	}
	operation := "control-command.delivered"
	if acknowledge {
		operation = "control-command.acknowledged"
	}
	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker.ID, requestID, operation, struct {
		ExecutionID      uuid.UUID                   `json:"executionId"`
		ControlCommandID uuid.UUID                   `json:"controlCommandId"`
		Input            ControlCommandDeliveryInput `json:"input"`
	}{executionID, controlCommandID, input}, 200, func(tx *gorm.DB) (ControlCommand, error) {
		lease, execution, err := s.lockLease(ctx, tx, worker.ID, executionID, input.LeaseInput, true)
		if err != nil {
			return ControlCommand{}, err
		}
		var command persistence.ExecutionControlCommand
		err = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ? AND id = ?", execution.TenantID, execution.ID, controlCommandID).
			Take(&command).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ControlCommand{}, problem.New(404, "control_command_not_found", "Control command not found.")
		}
		if err != nil {
			return ControlCommand{}, problem.Wrap(500, "control_command_lock_failed", "The Control command could not be locked.", err)
		}
		if command.DeliveryWorkerID == nil || *command.DeliveryWorkerID != worker.ID ||
			command.DeliveryGeneration == nil || *command.DeliveryGeneration != input.Generation {
			return ControlCommand{}, problem.New(409, "control_command_generation_fenced", "The Control command belongs to an obsolete Worker generation.")
		}
		if command.CommandID != input.CommandID {
			return ControlCommand{}, problem.New(409, "control_command_mismatch", "The delivered command does not match the persisted Control command.")
		}
		now := s.now()
		if !acknowledge {
			if command.Status == "acknowledged" || command.Status == "delivered" {
				return toControlCommand(command), nil
			}
			if command.Status != "pending" {
				return ControlCommand{}, problem.New(409, "control_command_not_deliverable", "The Control command cannot be delivered from its current state.")
			}
			updated := tx.WithContext(ctx).Model(&persistence.ExecutionControlCommand{}).
				Where("tenant_id = ? AND execution_id = ? AND id = ? AND delivery_worker_id = ? AND delivery_generation = ? AND status = ?",
					execution.TenantID, execution.ID, command.ID, worker.ID, input.Generation, "pending").
				Updates(map[string]any{
					"status": "delivered", "delivery_attempts": gorm.Expr("delivery_attempts + 1"),
					"delivered_at": now, "delivery_error": nil,
				})
			if err := expectOne(updated, 409, "control_command_delivery_conflict", "The Control command delivery changed concurrently."); err != nil {
				return ControlCommand{}, err
			}
			command.Status = "delivered"
			command.DeliveryAttempts++
			command.DeliveredAt = &now
			command.DeliveryError = nil
			return toControlCommand(command), nil
		}

		if command.Status == "acknowledged" {
			return toControlCommand(command), nil
		}
		if command.Status != "delivered" || command.DeliveredAt == nil {
			return ControlCommand{}, problem.New(409, "control_command_not_delivered", "The Control command must be delivered before it can be acknowledged.")
		}
		if command.CommandType != "InterruptTurn" {
			return ControlCommand{}, problem.New(409, "control_command_not_implemented", fmt.Sprintf("Control command %q is not implemented.", command.CommandType))
		}
		if err := s.storeProviderCursor(ctx, tx, execution, input.ProviderResumeCursor); err != nil {
			return ControlCommand{}, err
		}
		if err := s.supersedeInteractionGeneration(ctx, tx, execution, lease); err != nil {
			return ControlCommand{}, err
		}
		if err := tx.WithContext(ctx).Delete(&lease).Error; err != nil {
			return ControlCommand{}, problem.Wrap(500, "lease_release_failed", "Failed to release the interrupted Execution lease.", err)
		}
		updatedCommand := tx.WithContext(ctx).Model(&persistence.ExecutionControlCommand{}).
			Where("tenant_id = ? AND execution_id = ? AND id = ? AND status = ?", execution.TenantID, execution.ID, command.ID, "delivered").
			Updates(map[string]any{"status": "acknowledged", "acknowledged_at": now, "delivery_error": nil})
		if err := expectOne(updatedCommand, 409, "control_command_acknowledgement_conflict", "The Control command acknowledgement changed concurrently."); err != nil {
			return ControlCommand{}, err
		}
		execution.Status = "interrupted"
		execution.FinishedAt = &now
		executionUpdate := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ? AND worker_id = ? AND generation = ? AND status IN ?",
				execution.TenantID, execution.ID, worker.ID, input.Generation,
				[]string{"leased", "running", "waiting-for-approval"}).
			Updates(map[string]any{"status": "interrupted", "finished_at": now})
		if err := expectOne(executionUpdate, 409, "execution_interrupt_conflict", "The Execution could not be interrupted from its current state."); err != nil {
			return ControlCommand{}, err
		}
		turnUpdate := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND session_id = ? AND id = ?", execution.TenantID, execution.SessionID, execution.TurnID).
			Updates(map[string]any{"status": "interrupted", "completed_at": now})
		if err := expectOne(turnUpdate, 500, "turn_interrupt_failed", "Failed to mark the Turn interrupted."); err != nil {
			return ControlCommand{}, err
		}
		if err := supersedeControlCommands(ctx, tx, execution, command.ID, "The Execution reached the interrupted terminal state."); err != nil {
			return ControlCommand{}, err
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventType: "execution.interrupted", ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: map[string]any{
				"turnId": execution.TurnID, "controlCommandId": command.ID,
				"commandId": command.CommandID, "finishedAt": now,
			},
		})
		if err != nil {
			return ControlCommand{}, err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &execution.TenantID, Topic: "execution.interrupted", MessageKey: execution.ID.String(),
			Payload: map[string]any{
				"tenantId": execution.TenantID, "sessionId": execution.SessionID, "turnId": execution.TurnID,
				"executionId": execution.ID, "controlCommandId": command.ID, "finishedAt": now,
			},
		}); err != nil {
			return ControlCommand{}, problem.Wrap(500, "execution_interrupt_outbox_failed", "The interrupted Execution event could not be queued.", err)
		}
		command.Status = "acknowledged"
		command.AcknowledgedAt = &now
		command.DeliveryError = nil
		return toControlCommand(command), nil
	})
	if err == nil && acknowledge && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

func toControlCommandDelivery(model persistence.ExecutionControlCommand) ControlCommandDelivery {
	payload := model.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	return ControlCommandDelivery{
		ControlCommandID: model.ID, Provider: model.Provider, CommandType: model.CommandType,
		CommandID: model.CommandID, Payload: payload, DeliveryStatus: model.Status,
		DeliveryAttempts: model.DeliveryAttempts, DeliveryAvailableAt: model.DeliveryAvailableAt,
	}
}

func bindExecutionControlCommands(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	workerID uuid.UUID,
	now time.Time,
) error {
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionControlCommand{}).
		Where("tenant_id = ? AND execution_id = ? AND status = ? AND delivery_worker_id IS NULL AND delivery_generation IS NULL",
			execution.TenantID, execution.ID, "pending").
		Updates(map[string]any{
			"delivery_worker_id": workerID, "delivery_generation": execution.Generation,
			"delivery_available_at": now, "delivery_error": nil,
		}).Error; err != nil {
		return problem.Wrap(500, "control_command_bind_failed", "Pending Control commands could not be bound to the claimed Worker.", err)
	}
	return nil
}

func requeueExecutionControlCommands(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	lease persistence.WorkerLease,
	reason string,
) error {
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionControlCommand{}).
		Where("tenant_id = ? AND execution_id = ? AND delivery_worker_id = ? AND delivery_generation = ? AND status IN ?",
			execution.TenantID, execution.ID, lease.WorkerID, lease.Generation, []string{"pending", "delivered"}).
		Updates(map[string]any{
			"status": "pending", "delivery_worker_id": nil, "delivery_generation": nil,
			"delivered_at": nil, "delivery_error": reason,
		}).Error; err != nil {
		return problem.Wrap(500, "control_command_requeue_failed", "Control commands could not be requeued for Worker recovery.", err)
	}
	return nil
}

func supersedeControlCommands(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	exceptID uuid.UUID,
	reason string,
) error {
	query := tx.WithContext(ctx).Model(&persistence.ExecutionControlCommand{}).
		Where("tenant_id = ? AND execution_id = ? AND status IN ?", execution.TenantID, execution.ID, []string{"pending", "delivered"})
	if exceptID != uuid.Nil {
		query = query.Where("id <> ?", exceptID)
	}
	if err := query.Updates(map[string]any{"status": "superseded", "delivery_error": reason}).Error; err != nil {
		return problem.Wrap(500, "control_command_supersede_failed", "Outstanding Control commands could not be superseded.", err)
	}
	return nil
}
