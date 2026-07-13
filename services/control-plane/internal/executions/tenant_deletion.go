package executions

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

var nonterminalExecutionStatuses = []string{"queued", "recovering", "leased", "running", "waiting-for-approval"}

func (s *Service) PrepareTenantDeletion(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, actorID uuid.UUID,
	now time.Time,
) ([]persistence.SessionEvent, error) {
	var executionIDs []uuid.UUID
	if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Select("id").Where("tenant_id = ? AND status IN ?", tenantID, nonterminalExecutionStatuses).
		Order("queued_at, id").Scan(&executionIDs).Error; err != nil {
		return nil, problem.Wrap(500, "tenant_executions_load_failed", "Failed to load active Tenant Executions for deletion.", err)
	}

	events := make([]persistence.SessionEvent, 0, len(executionIDs))
	for _, executionID := range executionIDs {
		var lease persistence.WorkerLease
		leaseErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ?", tenantID, executionID).Take(&lease).Error
		if leaseErr != nil && !errors.Is(leaseErr, gorm.ErrRecordNotFound) {
			return nil, problem.Wrap(500, "tenant_execution_lease_lock_failed", "Failed to lock a Tenant Execution lease for deletion.", leaseErr)
		}

		var execution persistence.AgentExecution
		executionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&execution).Error
		if errors.Is(executionErr, gorm.ErrRecordNotFound) {
			continue
		}
		if executionErr != nil {
			return nil, problem.Wrap(500, "tenant_execution_lock_failed", "Failed to lock a Tenant Execution for deletion.", executionErr)
		}
		if !containsExecutionStatus(nonterminalExecutionStatuses, execution.Status) {
			continue
		}

		leaseCurrent := leaseErr == nil && lease.ExpiresAt.After(now) && execution.WorkerID != nil &&
			*execution.WorkerID == lease.WorkerID && execution.Generation == lease.Generation && execution.Generation > 0
		if leaseCurrent {
			event, err := s.ensureTenantDeletionInterrupt(ctx, tx, execution, lease, actorID, now)
			if err != nil {
				return nil, err
			}
			if event.EventID != uuid.Nil {
				events = append(events, event)
			}
			continue
		}

		var lockedLease *persistence.WorkerLease
		if leaseErr == nil {
			lockedLease = &lease
		}
		event, err := s.cancelExecutionLocked(
			ctx, tx, &execution, lockedLease, "user", &actorID, now, "tenant-delete",
		)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func (s *Service) PublishTenantDeletionEvents(events []persistence.SessionEvent) {
	for _, event := range events {
		if event.EventID != uuid.Nil {
			s.sessions.PublishInternalEvent(event)
		}
	}
}

func (s *Service) ensureTenantDeletionInterrupt(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	lease persistence.WorkerLease,
	actorID uuid.UUID,
	now time.Time,
) (persistence.SessionEvent, error) {
	var existing persistence.ExecutionControlCommand
	existingErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND execution_id = ? AND command_type = ? AND status IN ?",
			execution.TenantID, execution.ID, "InterruptTurn", outstandingControlCommandStatuses).
		Take(&existing).Error
	if existingErr == nil {
		return persistence.SessionEvent{}, nil
	}
	if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
		return persistence.SessionEvent{}, problem.Wrap(500, "tenant_interrupt_lookup_failed", "Failed to inspect an existing Tenant deletion interrupt.", existingErr)
	}

	provider := ""
	if execution.Provider != nil {
		provider = strings.TrimSpace(*execution.Provider)
	}
	if provider == "" || execution.WorkerManifestID == nil {
		return persistence.SessionEvent{}, nil
	}
	var providerManifest persistence.WorkerProviderManifest
	manifestErr := tx.WithContext(ctx).
		Where("worker_manifest_id = ? AND provider = ? AND compatibility_status = ?",
			*execution.WorkerManifestID, provider, "compatible").Take(&providerManifest).Error
	if errors.Is(manifestErr, gorm.ErrRecordNotFound) {
		return persistence.SessionEvent{}, nil
	}
	if manifestErr != nil {
		return persistence.SessionEvent{}, problem.Wrap(500, "tenant_interrupt_capability_load_failed", "Failed to inspect Tenant deletion interrupt support.", manifestErr)
	}
	if !isSupportedProviderCapability(providerManifest.Capabilities["interrupt-turn"]) {
		return persistence.SessionEvent{}, nil
	}

	commandID := uuid.New()
	generation := lease.Generation
	command := persistence.ExecutionControlCommand{
		ID: commandID, TenantID: execution.TenantID, ExecutionID: execution.ID,
		SessionID: execution.SessionID, TurnID: execution.TurnID, Provider: provider,
		CommandType: "InterruptTurn", CommandID: "tenant-delete-interrupt:" + commandID.String(),
		Payload: map[string]any{"turnId": execution.TurnID.String(), "reason": "tenant-delete"},
		Status:  "pending", RequestedBy: actorID, RequestedAt: now,
		DeliveryWorkerID: &lease.WorkerID, DeliveryGeneration: &generation, DeliveryAvailableAt: now,
	}
	if err := tx.WithContext(ctx).Create(&command).Error; err != nil {
		return persistence.SessionEvent{}, problem.Wrap(409, "tenant_interrupt_create_failed", "Failed to create the Tenant deletion interrupt.", err)
	}
	eventPayload := controlCommandEventPayload(command, map[string]any{"reason": "tenant-delete"})
	event, err := s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
		EventType: "turn.interrupt-requested", ActorType: "user", ActorID: &actorID,
		ExecutionID: &execution.ID, WorkerID: &lease.WorkerID, Generation: &generation,
		Payload: eventPayload,
	})
	if err != nil {
		return persistence.SessionEvent{}, err
	}
	if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
		TenantID: &execution.TenantID, Topic: "turn.interrupt-requested", MessageKey: command.ID.String(),
		Payload: controlCommandOutboxPayload(command, map[string]any{"reason": "tenant-delete"}),
	}); err != nil {
		return persistence.SessionEvent{}, problem.Wrap(500, "tenant_interrupt_outbox_failed", "Failed to queue the Tenant deletion interrupt.", err)
	}
	return event, nil
}

func executionTenantDeleting(ctx context.Context, tx *gorm.DB, tenantID uuid.UUID) (bool, error) {
	var tenant struct {
		Status    string
		DeletedAt *time.Time
	}
	err := tx.WithContext(ctx).Table("tenants").Select("status", "deleted_at").Where("id = ?", tenantID).Take(&tenant).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, problem.Wrap(500, "tenant_execution_state_load_failed", "Failed to inspect the Execution Tenant state.", err)
	}
	return tenant.DeletedAt != nil || tenant.Status == "deleting", nil
}

func requireExecutionTenantActive(ctx context.Context, tx *gorm.DB, tenantID uuid.UUID) error {
	deleting, err := executionTenantDeleting(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	if deleting {
		return problem.New(409, "tenant_deleting", "The Tenant is being deleted and cannot continue the Execution.")
	}
	return nil
}

func containsExecutionStatus(statuses []string, status string) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}
