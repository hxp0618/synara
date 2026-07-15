package executions

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	apiidempotency "github.com/synara-ai/synara/services/control-plane/internal/idempotency"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Service) ListWorkers(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
) ([]ManagedWorker, error) {
	if err := requireWorkerTenant(principal, tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return nil, err
	}
	workers := make([]persistence.WorkerInstance, 0)
	err := s.db.WithContext(ctx).
		Table("worker_instances AS worker").
		Select("worker.*").
		Joins("JOIN execution_targets AS target ON target.id = worker.execution_target_id").
		Where("target.tenant_id = ?", tenantID).
		Order("worker.execution_target_id, worker.cluster_id, worker.namespace, worker.pod_name, worker.incarnation DESC, worker.id").
		Find(&workers).Error
	if err != nil {
		return nil, problem.Wrap(500, "workers_load_failed", "Failed to load Workers.", err)
	}
	result := make([]ManagedWorker, 0, len(workers))
	for _, worker := range workers {
		result = append(result, toManagedWorker(worker))
	}
	return result, nil
}

func (s *Service) RevokeWorker(
	ctx context.Context,
	principal identity.Principal,
	tenantID, workerID uuid.UUID,
	input RevokeWorkerInput,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[WorkerRevocation], error) {
	if err := requireWorkerTenant(principal, tenantID); err != nil {
		return OperationResult[WorkerRevocation]{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return OperationResult[WorkerRevocation]{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return OperationResult[WorkerRevocation]{}, problem.New(400, "idempotency_key_required", "Idempotency-Key is required for Worker revocation.")
	}
	if workerID == uuid.Nil {
		return OperationResult[WorkerRevocation]{}, problem.New(400, "invalid_worker", "workerId is required.")
	}
	if input.ExpectedIncarnation <= 0 {
		return OperationResult[WorkerRevocation]{}, problem.New(400, "invalid_worker_incarnation", "expectedIncarnation must be greater than zero.")
	}
	reason := strings.TrimSpace(input.Reason)
	if len(reason) == 0 || len(reason) > 2000 {
		return OperationResult[WorkerRevocation]{}, problem.New(400, "invalid_worker_revocation_reason", "reason must contain between 1 and 2000 characters.")
	}
	input.Reason = reason

	appended := make([]persistence.SessionEvent, 0)
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "worker.revoke", SuccessStatus: 200,
		Request: map[string]any{
			"workerId": workerID, "expectedIncarnation": input.ExpectedIncarnation, "reason": reason,
		},
	}, func(tx *gorm.DB) (WorkerRevocation, error) {
		worker, target, err := lockTenantWorker(ctx, tx, tenantID, workerID)
		if err != nil {
			return WorkerRevocation{}, err
		}
		if worker.Incarnation != input.ExpectedIncarnation {
			conflict := problem.New(409, "worker_incarnation_conflict", "The Worker incarnation changed before it could be revoked.")
			conflict.Details = map[string]any{
				"expectedIncarnation": input.ExpectedIncarnation,
				"currentIncarnation":  worker.Incarnation,
			}
			return WorkerRevocation{}, conflict
		}
		if worker.AdministrativeStatus == "revoked" {
			if worker.RevocationReason == nil || *worker.RevocationReason != reason {
				return WorkerRevocation{}, problem.New(409, "worker_revocation_conflict", "The Worker was already revoked with a different reason.")
			}
			return WorkerRevocation{Worker: toManagedWorker(worker)}, nil
		}
		if worker.AdministrativeStatus != "active" && worker.AdministrativeStatus != "draining" {
			return WorkerRevocation{}, problem.New(409, "worker_administrative_state_invalid", "The Worker cannot be revoked from its current administrative state.")
		}

		revocation := WorkerRevocation{}
		executionEvents, executionCounts, err := s.revokeWorkerExecutionLeasesLocked(ctx, tx, worker, reason)
		if err != nil {
			return WorkerRevocation{}, err
		}
		appended = append(appended, executionEvents...)
		revocation.ReleasedExecutionLeases = executionCounts.releasedLeases
		revocation.RecoveringExecutions = executionCounts.recovering
		revocation.OutcomeUnknownExecutions = executionCounts.outcomeUnknown
		revocation.CheckpointUnconfirmedExecutions = executionCounts.checkpointUnconfirmed

		requeuedCleanups, err := s.requeueWorkerWorkspaceCleanupLeasesLocked(ctx, tx, worker, reason)
		if err != nil {
			return WorkerRevocation{}, err
		}
		revocation.RequeuedWorkspaceCleanups = requeuedCleanups

		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("id = ? AND incarnation = ? AND administrative_status IN ?",
				worker.ID, worker.Incarnation, []string{"active", "draining"}).
			Updates(map[string]any{
				"administrative_status": "revoked",
				"revoked_at":            now,
				"revoked_by":            principal.UserID,
				"revocation_reason":     reason,
			})
		if err := expectOne(updated, 409, "worker_revocation_conflict", "The Worker could not be revoked from its current incarnation."); err != nil {
			return WorkerRevocation{}, err
		}
		worker.AdministrativeStatus = "revoked"
		worker.RevokedAt = &now
		worker.RevokedBy = &principal.UserID
		worker.RevocationReason = &reason
		revocation.Worker = toManagedWorker(worker)

		metadata := map[string]any{
			"reason": reason, "incarnation": worker.Incarnation,
			"executionTargetId": worker.ExecutionTargetID,
			"clusterId":         worker.ClusterID, "namespace": worker.Namespace, "podName": worker.PodName,
			"releasedExecutionLeases":         revocation.ReleasedExecutionLeases,
			"recoveringExecutions":            revocation.RecoveringExecutions,
			"outcomeUnknownExecutions":        revocation.OutcomeUnknownExecutions,
			"checkpointUnconfirmedExecutions": revocation.CheckpointUnconfirmedExecutions,
			"requeuedWorkspaceCleanups":       revocation.RequeuedWorkspaceCleanups,
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "worker.revoked", ResourceType: "worker", ResourceID: &worker.ID,
			OrganizationID: target.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: metadata,
		}); err != nil {
			return WorkerRevocation{}, problem.Wrap(500, "worker_revocation_audit_failed", "The Worker revocation audit record could not be persisted.", err)
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "worker.revoked",
			MessageKey: worker.ID.String() + ":" + formatGeneration(worker.Incarnation),
			Payload: map[string]any{
				"tenantId": tenantID, "organizationId": target.OrganizationID,
				"workerId": worker.ID, "incarnation": worker.Incarnation,
				"executionTargetId":    worker.ExecutionTargetID,
				"administrativeStatus": worker.AdministrativeStatus,
				"revokedAt":            now, "revokedBy": principal.UserID, "reason": reason,
				"releasedExecutionLeases":         revocation.ReleasedExecutionLeases,
				"recoveringExecutions":            revocation.RecoveringExecutions,
				"outcomeUnknownExecutions":        revocation.OutcomeUnknownExecutions,
				"checkpointUnconfirmedExecutions": revocation.CheckpointUnconfirmedExecutions,
				"requeuedWorkspaceCleanups":       revocation.RequeuedWorkspaceCleanups,
			},
		}); err != nil {
			return WorkerRevocation{}, problem.Wrap(500, "worker_revocation_outbox_failed", "The Worker revocation event could not be queued.", err)
		}
		return revocation, nil
	})
	if err != nil {
		return OperationResult[WorkerRevocation]{}, err
	}
	if !result.Replayed {
		for _, event := range appended {
			s.sessions.PublishInternalEvent(event)
		}
	}
	return OperationResult[WorkerRevocation]{
		Value: result.Value, Replayed: result.Replayed, StatusCode: result.StatusCode,
	}, nil
}

type workerRevocationExecutionCounts struct {
	releasedLeases        int
	recovering            int
	outcomeUnknown        int
	checkpointUnconfirmed int
}

func (s *Service) revokeWorkerExecutionLeasesLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	reason string,
) ([]persistence.SessionEvent, workerRevocationExecutionCounts, error) {
	leases := make([]persistence.WorkerLease, 0)
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("worker_id = ?", worker.ID).
		Order("execution_id").
		Find(&leases).Error; err != nil {
		return nil, workerRevocationExecutionCounts{}, problem.Wrap(500, "worker_revocation_lease_lock_failed", "The Worker's execution leases could not be locked.", err)
	}
	events := make([]persistence.SessionEvent, 0, len(leases))
	counts := workerRevocationExecutionCounts{}
	for _, lease := range leases {
		var execution persistence.AgentExecution
		executionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND tenant_id = ?", lease.ExecutionID, lease.TenantID).
			Take(&execution).Error
		if executionErr != nil && !errors.Is(executionErr, gorm.ErrRecordNotFound) {
			return nil, counts, problem.Wrap(500, "worker_revocation_execution_lock_failed", "A leased Execution could not be locked for Worker revocation.", executionErr)
		}
		deleted := tx.WithContext(ctx).Where(
			"execution_id = ? AND tenant_id = ? AND worker_id = ? AND generation = ?",
			lease.ExecutionID, lease.TenantID, lease.WorkerID, lease.Generation,
		).Delete(&persistence.WorkerLease{})
		if deleted.Error != nil {
			return nil, counts, problem.Wrap(500, "worker_revocation_lease_release_failed", "A Worker lease could not be released.", deleted.Error)
		}
		if deleted.RowsAffected == 0 {
			continue
		}
		counts.releasedLeases++
		if errors.Is(executionErr, gorm.ErrRecordNotFound) || execution.WorkerID == nil ||
			*execution.WorkerID != worker.ID || execution.Generation != lease.Generation ||
			!containsExecutionStatus(nonterminalExecutionStatuses, execution.Status) {
			continue
		}
		recoveryRisk, err := s.executionGenerationCheckpointRisk(ctx, tx, execution, lease.Generation)
		if err != nil {
			return nil, counts, err
		}
		if recoveryRisk != "" {
			counts.checkpointUnconfirmed++
		}
		reasonMessage := "The Worker was administratively revoked before the execution lifecycle completed. Revocation reason: " + reason
		event, err := s.recoverExecutionGenerationLocked(
			ctx, tx, execution, lease, "worker_revoked", reasonMessage, recoveryRisk,
		)
		if err != nil {
			return nil, counts, err
		}
		if event.EventType == "execution.failed" {
			counts.outcomeUnknown++
		} else {
			counts.recovering++
		}
		events = append(events, event)
	}
	return events, counts, nil
}

func (s *Service) requeueWorkerWorkspaceCleanupLeasesLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	reason string,
) (int, error) {
	commands := make([]persistence.WorkspaceCleanupCommand, 0)
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("delivery_worker_id = ? AND delivery_worker_incarnation = ? AND status IN ?",
			worker.ID, worker.Incarnation, activeWorkspaceCleanupStatuses).
		Order("id").Find(&commands).Error; err != nil {
		return 0, problem.Wrap(500, "worker_revocation_cleanup_lock_failed", "The Worker's Workspace cleanup leases could not be locked.", err)
	}
	requeued := 0
	for _, command := range commands {
		recovered, err := s.recoverWorkspaceCleanupCommandLocked(
			ctx, tx, command, s.now(),
			"workspace_cleanup_worker_revoked",
			"The Workspace cleanup Worker was administratively revoked. Revocation reason: "+reason,
		)
		if err != nil {
			return 0, err
		}
		if recovered {
			requeued++
		}
	}
	return requeued, nil
}

func lockTenantWorker(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, workerID uuid.UUID,
) (persistence.WorkerInstance, persistence.ExecutionTarget, error) {
	var worker persistence.WorkerInstance
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND execution_target_id IN (SELECT id FROM execution_targets WHERE tenant_id = ?)", workerID, tenantID).
		Take(&worker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerInstance{}, persistence.ExecutionTarget{}, problem.New(404, "worker_not_found", "Worker not found.")
	}
	if err != nil {
		return persistence.WorkerInstance{}, persistence.ExecutionTarget{}, problem.Wrap(500, "worker_lock_failed", "The Worker could not be locked.", err)
	}
	var target persistence.ExecutionTarget
	if err := tx.WithContext(ctx).Select("id", "tenant_id", "organization_id").
		Where("id = ? AND tenant_id = ?", worker.ExecutionTargetID, tenantID).Take(&target).Error; err != nil {
		return persistence.WorkerInstance{}, persistence.ExecutionTarget{}, problem.Wrap(500, "worker_target_load_failed", "The Worker's Execution Target could not be loaded.", err)
	}
	return worker, target, nil
}

func requireWorkerTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}

func workerTokenRevoked() error {
	return problem.New(401, "worker_token_revoked", "The Worker token was administratively revoked.")
}

func equalTokenHashes(first, second []byte) bool {
	return len(first) == len(second) && len(first) > 0 && subtle.ConstantTimeCompare(first, second) == 1
}

func toManagedWorker(model persistence.WorkerInstance) ManagedWorker {
	return ManagedWorker{
		ID: model.ID, Incarnation: model.Incarnation, InstanceUID: model.InstanceUID,
		ExecutionTargetID: model.ExecutionTargetID, TargetKind: model.TargetKind,
		ClusterID: model.ClusterID, Namespace: model.Namespace, PodName: model.PodName,
		Version: model.Version, ProtocolVersion: model.ProtocolVersion,
		CurrentManifestID: model.CurrentManifestID, CompatibilityStatus: model.CompatibilityStatus,
		CompatibilityReason: model.CompatibilityReason, CompatibilityCheckedAt: model.CompatibilityCheckedAt,
		WorkerReleaseRevisionID: model.WorkerReleaseRevisionID, WorkerReleaseChannel: model.WorkerReleaseChannel,
		WorkerReleaseStatus: model.WorkerReleaseStatus, WorkerReleaseReason: model.WorkerReleaseReason,
		WorkerReleaseCheckedAt: model.WorkerReleaseCheckedAt,
		LeaseSupported:         model.LeaseSupported, FencingSupported: model.FencingSupported,
		Status: model.Status, AdministrativeStatus: model.AdministrativeStatus,
		RegisteredAt: model.RegisteredAt, LastHeartbeatAt: model.LastHeartbeatAt,
		DrainingAt: model.DrainingAt, TerminatedAt: model.TerminatedAt,
		RevokedAt: model.RevokedAt, RevokedBy: model.RevokedBy, RevocationReason: model.RevocationReason,
	}
}
