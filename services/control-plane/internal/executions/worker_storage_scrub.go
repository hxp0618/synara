package executions

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	workerStorageScrubScopeExecution        = "execution"
	workerStorageScrubScopeWorkspaceCleanup = "workspace-cleanup"

	workerStorageScrubStatusPending      = "pending"
	workerStorageScrubStatusAcknowledged = "acknowledged"
	workerStorageScrubStatusFailed       = "failed"
)

type WorkerStorageScrub struct {
	ID                uuid.UUID  `json:"id"`
	ExecutionTargetID uuid.UUID  `json:"executionTargetId"`
	TenantID          uuid.UUID  `json:"tenantId"`
	ScopeKind         string     `json:"scopeKind"`
	ScopeID           uuid.UUID  `json:"scopeId"`
	ScopeGeneration   int64      `json:"scopeGeneration"`
	ScrubGeneration   int64      `json:"scrubGeneration"`
	Status            string     `json:"status"`
	CreatedAt         time.Time  `json:"createdAt"`
	AcknowledgedAt    *time.Time `json:"acknowledgedAt,omitempty"`
	FailedAt          *time.Time `json:"failedAt,omitempty"`
	FailureCode       *string    `json:"failureCode,omitempty"`
	FailureMessage    *string    `json:"failureMessage,omitempty"`
}

type WorkerStorageScrubReceiptInput struct {
	ScrubGeneration int64 `json:"scrubGeneration"`
}

type WorkerStorageScrubClaimResult struct {
	Scrub *WorkerStorageScrub `json:"scrub"`
}

type WorkerStorageScrubFailureInput struct {
	ScrubGeneration int64  `json:"scrubGeneration"`
	FailureCode     string `json:"failureCode"`
	FailureMessage  string `json:"failureMessage"`
}

func workerStorageScrubsAvailable(tx *gorm.DB) bool {
	return tx != nil && (tx.Dialector.Name() != "sqlite" || tx.Migrator().HasTable(&persistence.WorkerStorageScrub{}))
}

func ensureExecutionStorageScrubLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	lease persistence.WorkerLease,
	observedAt time.Time,
) error {
	if !workerStorageScrubsAvailable(tx) || worker.WorkerMode != WorkerModeGeneralPool {
		return nil
	}
	var execution persistence.AgentExecution
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "execution_target_id", "worker_pool_id", "worker_pool_version").
		Where("id = ? AND tenant_id = ?", lease.ExecutionID, lease.TenantID).
		Take(&execution).Error; err != nil {
		return problem.Wrap(500, "worker_storage_scrub_execution_load_failed", "The released Execution storage scope could not be loaded.", err)
	}
	isolation, err := loadExecutionPoolTenantIsolation(ctx, tx, execution)
	if err != nil {
		return err
	}
	if isolation != placement.TenantIsolationShared {
		return nil
	}
	return ensureWorkerStorageScrubLocked(
		ctx, tx, worker, execution.ExecutionTargetID, execution.TenantID,
		workerStorageScrubScopeExecution, execution.ID, lease.Generation, observedAt,
	)
}

func ensureWorkspaceCleanupStorageScrubLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	command persistence.WorkspaceCleanupCommand,
	observedAt time.Time,
) error {
	if !workerStorageScrubsAvailable(tx) || worker.WorkerMode != WorkerModeGeneralPool {
		return nil
	}
	isolation, err := loadDefaultPoolTenantIsolation(ctx, tx, command.ExecutionTargetID)
	if err != nil {
		return err
	}
	if isolation != placement.TenantIsolationShared {
		return nil
	}
	return ensureWorkerStorageScrubLocked(
		ctx, tx, worker, command.ExecutionTargetID, command.TenantID,
		workerStorageScrubScopeWorkspaceCleanup, command.ID, command.DispatchGeneration, observedAt,
	)
}

func ensureWorkerStorageScrubLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	targetID, tenantID uuid.UUID,
	scopeKind string,
	scopeID uuid.UUID,
	scopeGeneration int64,
	observedAt time.Time,
) error {
	var existing persistence.WorkerStorageScrub
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("scope_kind = ? AND scope_id = ? AND scope_generation = ?", scopeKind, scopeID, scopeGeneration).
		Take(&existing).Error
	if err == nil {
		if existing.WorkerID != worker.ID || existing.WorkerIncarnation != worker.Incarnation ||
			existing.WorkerInstanceUID != worker.InstanceUID || existing.ExecutionTargetID != targetID ||
			existing.TenantID != tenantID {
			return problem.New(409, "worker_storage_scrub_scope_conflict", "The storage scrub scope is already fenced to another physical Worker.")
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.Wrap(500, "worker_storage_scrub_lookup_failed", "The storage scrub fence could not be inspected.", err)
	}

	var active persistence.WorkerStorageScrub
	activeErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("worker_id = ? AND worker_incarnation = ? AND status IN ?",
			worker.ID, worker.Incarnation,
			[]string{workerStorageScrubStatusPending, workerStorageScrubStatusFailed}).
		Take(&active).Error
	if activeErr == nil {
		return problem.New(409, "worker_storage_scrub_already_required", "The physical Worker already has an unresolved storage scrub fence.")
	}
	if !errors.Is(activeErr, gorm.ErrRecordNotFound) {
		return problem.Wrap(500, "worker_storage_scrub_lookup_failed", "The active storage scrub fence could not be inspected.", activeErr)
	}

	var lastGeneration int64
	if err := tx.WithContext(ctx).Model(&persistence.WorkerStorageScrub{}).
		Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
		Select("COALESCE(MAX(scrub_generation), 0)").
		Scan(&lastGeneration).Error; err != nil {
		return problem.Wrap(500, "worker_storage_scrub_generation_failed", "The next storage scrub generation could not be allocated.", err)
	}
	scrub := persistence.WorkerStorageScrub{
		ID: uuid.New(), WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
		WorkerInstanceUID: worker.InstanceUID, ExecutionTargetID: targetID, TenantID: tenantID,
		ScopeKind: scopeKind, ScopeID: scopeID, ScopeGeneration: scopeGeneration,
		ScrubGeneration: lastGeneration + 1, Status: workerStorageScrubStatusPending,
		CreatedAt: observedAt.UTC(),
	}
	if err := tx.WithContext(ctx).Create(&scrub).Error; err != nil {
		return problem.Wrap(409, "worker_storage_scrub_create_conflict", "The storage scrub fence could not be created.", err)
	}
	return nil
}

func requireWorkerStorageScrubClearLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
) error {
	if !workerStorageScrubsAvailable(tx) {
		return nil
	}
	var count int64
	if err := tx.WithContext(ctx).Model(&persistence.WorkerStorageScrub{}).
		Where("worker_id = ? AND worker_incarnation = ? AND worker_instance_uid = ? AND status IN ?",
			worker.ID, worker.Incarnation, worker.InstanceUID,
			[]string{workerStorageScrubStatusPending, workerStorageScrubStatusFailed}).
		Count(&count).Error; err != nil {
		return problem.Wrap(500, "worker_storage_scrub_lookup_failed", "The Worker storage scrub fence could not be inspected.", err)
	}
	if count > 0 {
		return problem.New(409, "worker_storage_scrub_required", "The Worker must acknowledge its pending storage scrub before claiming more Tenant work.")
	}
	return nil
}

func requireLogicalWorkerStorageScrubClearForRegistrationLocked(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	clusterID, namespace, podName string,
) error {
	if !workerStorageScrubsAvailable(tx) {
		return nil
	}
	var count int64
	if err := tx.WithContext(ctx).Table("worker_storage_scrubs AS scrub").
		Joins("JOIN worker_instances AS worker ON worker.id = scrub.worker_id").
		Where("worker.execution_target_id = ? AND worker.cluster_id = ? AND worker.namespace = ? AND worker.pod_name = ?",
			targetID, clusterID, namespace, podName).
		Where("scrub.status IN ?", []string{workerStorageScrubStatusPending, workerStorageScrubStatusFailed}).
		Count(&count).Error; err != nil {
		return problem.Wrap(500, "worker_storage_scrub_lookup_failed", "The logical Worker storage scrub history could not be inspected.", err)
	}
	if count > 0 {
		return problem.New(409, "worker_storage_scrub_replacement_blocked", "The logical Worker identity has an unresolved storage scrub; replacement requires explicit physical-storage reconciliation.")
	}
	return nil
}

func pendingWorkerStorageScrubForReregistrationLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	nextInstanceUID string,
) (uuid.UUID, error) {
	if !workerStorageScrubsAvailable(tx) {
		return uuid.Nil, nil
	}
	var scrub persistence.WorkerStorageScrub
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("worker_id = ? AND worker_incarnation = ? AND status IN ?",
			worker.ID, worker.Incarnation,
			[]string{workerStorageScrubStatusPending, workerStorageScrubStatusFailed}).
		Take(&scrub).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, problem.Wrap(500, "worker_storage_scrub_lookup_failed", "The Worker storage scrub recovery fence could not be inspected.", err)
	}
	if scrub.Status == workerStorageScrubStatusFailed {
		return uuid.Nil, problem.New(409, "worker_storage_scrub_failed", "A Worker with a failed storage scrub cannot re-register; replace or revoke the physical Worker.")
	}
	if scrub.WorkerInstanceUID != worker.InstanceUID || nextInstanceUID != worker.InstanceUID {
		return uuid.Nil, problem.New(409, "worker_storage_scrub_physical_instance_mismatch", "A pending storage scrub can only resume on the exact same physical Worker instance.")
	}
	return scrub.ID, nil
}

func transferWorkerStorageScrubForReregistrationLocked(
	ctx context.Context,
	tx *gorm.DB,
	scrubID, workerID uuid.UUID,
	previousIncarnation, nextIncarnation int64,
	instanceUID string,
) error {
	updated := tx.WithContext(ctx).Model(&persistence.WorkerStorageScrub{}).
		Where("id = ? AND worker_id = ? AND worker_incarnation = ? AND worker_instance_uid = ? AND status = ?",
			scrubID, workerID, previousIncarnation, instanceUID, workerStorageScrubStatusPending).
		Update("worker_incarnation", nextIncarnation)
	return expectOne(updated, 409, "worker_storage_scrub_reregistration_fenced", "The pending storage scrub changed while Worker re-registration was fenced.")
}

func toWorkerStorageScrub(model persistence.WorkerStorageScrub) WorkerStorageScrub {
	return WorkerStorageScrub{
		ID: model.ID, ExecutionTargetID: model.ExecutionTargetID, TenantID: model.TenantID,
		ScopeKind: model.ScopeKind, ScopeID: model.ScopeID, ScopeGeneration: model.ScopeGeneration,
		ScrubGeneration: model.ScrubGeneration, Status: model.Status, CreatedAt: model.CreatedAt,
		AcknowledgedAt: model.AcknowledgedAt, FailedAt: model.FailedAt,
		FailureCode: model.FailureCode, FailureMessage: model.FailureMessage,
	}
}

func validateWorkerStorageScrubFailure(input WorkerStorageScrubFailureInput) (WorkerStorageScrubFailureInput, error) {
	input.FailureCode = strings.TrimSpace(input.FailureCode)
	input.FailureMessage = strings.TrimSpace(input.FailureMessage)
	if input.ScrubGeneration <= 0 || input.FailureCode == "" || len(input.FailureCode) > 160 ||
		strings.ContainsAny(input.FailureCode, "\r\n\t") || len(input.FailureMessage) > 10_000 {
		return WorkerStorageScrubFailureInput{}, problem.New(400, "invalid_worker_storage_scrub_failure", "scrubGeneration, failureCode, and bounded failureMessage are required.")
	}
	return input, nil
}

func (s *Service) ClaimWorkerStorageScrub(
	ctx context.Context,
	worker persistence.WorkerInstance,
) (WorkerStorageScrubClaimResult, error) {
	var result *WorkerStorageScrub
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		current, err := lockCurrentWorker(ctx, tx, worker)
		if err != nil {
			return err
		}
		if !workerStorageScrubsAvailable(tx) {
			return nil
		}
		var model persistence.WorkerStorageScrub
		err = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("worker_id = ? AND worker_incarnation = ? AND worker_instance_uid = ? AND status IN ?",
				current.ID, current.Incarnation, current.InstanceUID,
				[]string{workerStorageScrubStatusPending, workerStorageScrubStatusFailed}).
			Order("scrub_generation, id").Take(&model).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return problem.Wrap(500, "worker_storage_scrub_claim_failed", "The pending Worker storage scrub could not be loaded.", err)
		}
		converted := toWorkerStorageScrub(model)
		result = &converted
		return nil
	})
	return WorkerStorageScrubClaimResult{Scrub: result}, err
}

func (s *Service) AcknowledgeWorkerStorageScrub(
	ctx context.Context,
	worker persistence.WorkerInstance,
	scrubID uuid.UUID,
	input WorkerStorageScrubReceiptInput,
	requestID string,
) (OperationResult[WorkerStorageScrub], error) {
	if scrubID == uuid.Nil || input.ScrubGeneration <= 0 {
		return OperationResult[WorkerStorageScrub]{}, problem.New(400, "invalid_worker_storage_scrub_receipt", "scrubId and scrubGeneration are required.")
	}
	return runIdempotent(ctx, s, worker, requestID, "worker_storage_scrub.acknowledged", struct {
		ScrubID uuid.UUID                      `json:"scrubId"`
		Input   WorkerStorageScrubReceiptInput `json:"input"`
	}{scrubID, input}, 200, func(tx *gorm.DB) (WorkerStorageScrub, error) {
		current, err := lockCurrentWorker(ctx, tx, worker)
		if err != nil {
			return WorkerStorageScrub{}, err
		}
		model, err := lockWorkerStorageScrub(ctx, tx, current, scrubID, input.ScrubGeneration)
		if err != nil {
			return WorkerStorageScrub{}, err
		}
		if model.Status == workerStorageScrubStatusFailed {
			return WorkerStorageScrub{}, problem.New(409, "worker_storage_scrub_failed", "A failed storage scrub cannot be acknowledged.")
		}
		if model.Status == workerStorageScrubStatusAcknowledged {
			return toWorkerStorageScrub(model), nil
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.WorkerStorageScrub{}).
			Where("id = ? AND status = ? AND scrub_generation = ?", model.ID, workerStorageScrubStatusPending, model.ScrubGeneration).
			Updates(map[string]any{"status": workerStorageScrubStatusAcknowledged, "acknowledged_at": now})
		if err := expectOne(updated, 409, "worker_storage_scrub_receipt_fenced", "The storage scrub fence changed before acknowledgement."); err != nil {
			return WorkerStorageScrub{}, err
		}
		model.Status = workerStorageScrubStatusAcknowledged
		model.AcknowledgedAt = &now
		state, err := currentWorkerFactStateLocked(ctx, tx, current)
		if err != nil {
			return WorkerStorageScrub{}, err
		}
		if err := transitionWorkerIncarnationFactLocked(ctx, tx, current, now, state, false, ""); err != nil {
			return WorkerStorageScrub{}, err
		}
		return toWorkerStorageScrub(model), nil
	})
}

func (s *Service) FailWorkerStorageScrub(
	ctx context.Context,
	worker persistence.WorkerInstance,
	scrubID uuid.UUID,
	input WorkerStorageScrubFailureInput,
	requestID string,
) (OperationResult[WorkerStorageScrub], error) {
	if scrubID == uuid.Nil {
		return OperationResult[WorkerStorageScrub]{}, problem.New(400, "invalid_worker_storage_scrub_failure", "scrubId is required.")
	}
	normalized, err := validateWorkerStorageScrubFailure(input)
	if err != nil {
		return OperationResult[WorkerStorageScrub]{}, err
	}
	return runIdempotent(ctx, s, worker, requestID, "worker_storage_scrub.failed", struct {
		ScrubID uuid.UUID                      `json:"scrubId"`
		Input   WorkerStorageScrubFailureInput `json:"input"`
	}{scrubID, normalized}, 200, func(tx *gorm.DB) (WorkerStorageScrub, error) {
		current, err := lockCurrentWorker(ctx, tx, worker)
		if err != nil {
			return WorkerStorageScrub{}, err
		}
		model, err := lockWorkerStorageScrub(ctx, tx, current, scrubID, normalized.ScrubGeneration)
		if err != nil {
			return WorkerStorageScrub{}, err
		}
		if model.Status == workerStorageScrubStatusAcknowledged {
			return WorkerStorageScrub{}, problem.New(409, "worker_storage_scrub_already_acknowledged", "An acknowledged storage scrub cannot fail.")
		}
		if model.Status == workerStorageScrubStatusFailed {
			return toWorkerStorageScrub(model), nil
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.WorkerStorageScrub{}).
			Where("id = ? AND status = ? AND scrub_generation = ?", model.ID, workerStorageScrubStatusPending, model.ScrubGeneration).
			Updates(map[string]any{
				"status": workerStorageScrubStatusFailed, "failed_at": now,
				"failure_code": normalized.FailureCode, "failure_message": normalized.FailureMessage,
			})
		if err := expectOne(updated, 409, "worker_storage_scrub_failure_fenced", "The storage scrub fence changed before failure was recorded."); err != nil {
			return WorkerStorageScrub{}, err
		}
		if current.Status == "online" {
			drained := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
				Where("id = ? AND incarnation = ? AND instance_uid = ? AND status = ?", current.ID, current.Incarnation, current.InstanceUID, "online").
				Updates(map[string]any{"status": "draining", "draining_at": now})
			if err := expectOne(drained, 409, "worker_storage_scrub_drain_conflict", "The Worker changed while failed storage scrub containment was applied."); err != nil {
				return WorkerStorageScrub{}, err
			}
			current.Status = "draining"
			current.DrainingAt = &now
		}
		model.Status = workerStorageScrubStatusFailed
		model.FailedAt = &now
		model.FailureCode = &normalized.FailureCode
		model.FailureMessage = &normalized.FailureMessage
		if err := transitionWorkerIncarnationFactLocked(ctx, tx, current, now, workerFactStateDraining, false, ""); err != nil {
			return WorkerStorageScrub{}, err
		}
		return toWorkerStorageScrub(model), nil
	})
}

func lockWorkerStorageScrub(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	scrubID uuid.UUID,
	scrubGeneration int64,
) (persistence.WorkerStorageScrub, error) {
	var model persistence.WorkerStorageScrub
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND worker_id = ? AND worker_incarnation = ? AND worker_instance_uid = ? AND scrub_generation = ?",
			scrubID, worker.ID, worker.Incarnation, worker.InstanceUID, scrubGeneration).
		Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerStorageScrub{}, problem.New(409, "worker_storage_scrub_fenced", "The storage scrub does not belong to this physical Worker generation.")
	}
	if err != nil {
		return persistence.WorkerStorageScrub{}, problem.Wrap(500, "worker_storage_scrub_load_failed", "The storage scrub fence could not be loaded.", err)
	}
	return model, nil
}
