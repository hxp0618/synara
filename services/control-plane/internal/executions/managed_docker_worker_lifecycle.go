package executions

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Service) RecoverMissingManagedDockerDrains(
	ctx context.Context,
	targetID uuid.UUID,
	presentContainerNames []string,
	observedAt time.Time,
) (bool, error) {
	if targetID == uuid.Nil || observedAt.IsZero() {
		return false, problem.New(400, "invalid_docker_worker_observation", "The managed Docker Worker observation is incomplete.")
	}
	present := make(map[string]struct{}, len(presentContainerNames))
	for _, value := range presentContainerNames {
		name := strings.TrimSpace(value)
		if name == "" {
			return false, problem.New(400, "invalid_docker_worker_observation", "The managed Docker container name is required.")
		}
		present[name] = struct{}{}
	}
	var draining []persistence.WorkerInstance
	if err := s.db.WithContext(ctx).
		Where(
			"execution_target_id = ? AND target_kind = ? AND reconciliation_drain_requested_at IS NOT NULL AND status <> ?",
			targetID, "docker", "terminated",
		).
		Order("reconciliation_drain_requested_at, id").
		Find(&draining).Error; err != nil {
		return false, problem.Wrap(500, "docker_worker_drain_load_failed", "Managed Docker Worker drains could not be loaded.", err)
	}
	advanced := false
	for _, worker := range draining {
		if _, found := present[worker.PodName]; found {
			continue
		}
		request := executiontargets.ManagedDockerWorkerDrainRequest{
			ExecutionTargetID: targetID,
			ContainerName:     worker.PodName,
			Reason:            managedDockerDrainReason(worker.ReconciliationDrainReason),
			ObservedAt:        observedAt.UTC(),
		}
		finalized, err := s.finalizeManagedDockerDrain(ctx, request, true)
		if err != nil {
			return false, err
		}
		advanced = advanced || finalized
	}
	return advanced, nil
}

func (s *Service) ActiveManagedDockerDrain(
	ctx context.Context,
	targetID uuid.UUID,
) (*executiontargets.ManagedDockerWorkerDrainState, error) {
	var workers []persistence.WorkerInstance
	if err := s.db.WithContext(ctx).
		Where(
			"execution_target_id = ? AND target_kind = ? AND reconciliation_drain_requested_at IS NOT NULL AND status <> ?",
			targetID, "docker", "terminated",
		).
		Order("reconciliation_drain_requested_at, id").
		Limit(2).
		Find(&workers).Error; err != nil {
		return nil, problem.Wrap(500, "docker_worker_drain_load_failed", "Managed Docker Worker drains could not be loaded.", err)
	}
	if len(workers) == 0 {
		return nil, nil
	}
	if len(workers) > 1 {
		return nil, problem.New(500, "docker_worker_drain_ambiguous", "More than one managed Docker Worker drain is active for the Target.")
	}
	worker := workers[0]
	if worker.ReconciliationDrainInstanceUID == nil {
		return nil, problem.New(500, "docker_worker_drain_corrupt", "The managed Docker Worker drain identity is incomplete.")
	}
	return &executiontargets.ManagedDockerWorkerDrainState{
		ContainerName:      worker.PodName,
		DrainInstanceUID:   *worker.ReconciliationDrainInstanceUID,
		CurrentInstanceUID: worker.InstanceUID,
	}, nil
}

func (s *Service) ManagedDockerDesiredWorkersReady(
	ctx context.Context,
	targetID uuid.UUID,
	containerNames []string,
	observedAt time.Time,
) (bool, error) {
	if len(containerNames) == 0 {
		return true, nil
	}
	names := append([]string(nil), containerNames...)
	sort.Strings(names)
	for index, name := range names {
		names[index] = strings.TrimSpace(name)
		if names[index] == "" || (index > 0 && names[index] == names[index-1]) {
			return false, problem.New(400, "invalid_docker_worker_observation", "Managed Docker desired container names must be non-empty and unique.")
		}
	}
	cutoff := observedAt.UTC().Add(-s.heartbeatTimeout)
	var ready int64
	if err := s.db.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where(
			"execution_target_id = ? AND target_kind = ? AND pod_name IN ? AND status = ? AND administrative_status = ?",
			targetID, "docker", names, "online", "active",
		).
		Where("reconciliation_drain_requested_at IS NULL").
		Where("last_heartbeat_at >= ?", cutoff).
		Where("protocol_version = ? AND compatibility_status NOT IN ?", WorkerProtocolVersion, []string{"incompatible", "revoked"}).
		Where("worker_release_status IN ?", []string{"", "unmanaged", "active"}).
		Distinct("pod_name").
		Count(&ready).Error; err != nil {
		return false, problem.Wrap(500, "docker_worker_readiness_load_failed", "Managed Docker Worker readiness could not be loaded.", err)
	}
	return ready == int64(len(names)), nil
}

func (s *Service) PrepareManagedDockerDrain(
	ctx context.Context,
	request executiontargets.ManagedDockerWorkerDrainRequest,
) (executiontargets.ManagedDockerWorkerDrainDecision, error) {
	request, err := normalizeManagedDockerDrainRequest(request)
	if err != nil {
		return executiontargets.ManagedDockerWorkerDrainDecision{}, err
	}
	decision := executiontargets.ManagedDockerWorkerDrainDecision{DeletionAllowed: true}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		worker, found, err := lockManagedDockerWorker(ctx, tx, request.ExecutionTargetID, request.ContainerName)
		if err != nil || !found {
			return err
		}
		decision.WorkerFound = true
		if worker.ReconciliationDrainRequestedAt == nil ||
			worker.ReconciliationDrainInstanceUID == nil ||
			*worker.ReconciliationDrainInstanceUID != worker.InstanceUID {
			incarnation := worker.Incarnation
			instanceUID := worker.InstanceUID
			reason := request.Reason
			updates := map[string]any{
				"reconciliation_drain_incarnation":  incarnation,
				"reconciliation_drain_instance_uid": instanceUID,
				"reconciliation_drain_requested_at": request.ObservedAt,
				"reconciliation_drain_reason":       reason,
			}
			if worker.Status != "draining" {
				updates["status"] = "draining"
				updates["draining_at"] = request.ObservedAt
			}
			updated := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
				Where("id = ? AND incarnation = ? AND instance_uid = ? AND status <> ?", worker.ID, worker.Incarnation, worker.InstanceUID, "terminated").
				Updates(updates)
			if updated.Error != nil {
				return problem.Wrap(409, "docker_worker_drain_conflict", "The managed Docker Worker drain could not be persisted.", updated.Error)
			}
			if updated.RowsAffected != 1 {
				return problem.New(409, "docker_worker_drain_conflict", "The managed Docker Worker changed before its drain could be persisted.")
			}
			worker.ReconciliationDrainIncarnation = &incarnation
			worker.ReconciliationDrainInstanceUID = &instanceUID
			worker.ReconciliationDrainRequestedAt = &request.ObservedAt
			worker.ReconciliationDrainReason = &reason
			worker.Status = "draining"
			worker.DrainingAt = &request.ObservedAt
		}
		if err := transitionWorkerIncarnationFactLocked(
			ctx, tx, worker, request.ObservedAt, workerFactStateDraining, false, "",
		); err != nil {
			return err
		}
		decision.ExecutionLeaseCount, decision.WorkspaceCleanupCount, err = managedDockerWorkerLeaseCounts(ctx, tx, worker)
		if err != nil {
			return err
		}
		decision.DeletionAllowed = decision.ExecutionLeaseCount == 0 && decision.WorkspaceCleanupCount == 0
		return nil
	})
	if err != nil {
		return executiontargets.ManagedDockerWorkerDrainDecision{}, err
	}
	return decision, nil
}

func (s *Service) FinalizeManagedDockerDrain(
	ctx context.Context,
	request executiontargets.ManagedDockerWorkerDrainRequest,
) error {
	request, err := normalizeManagedDockerDrainRequest(request)
	if err != nil {
		return err
	}
	_, err = s.finalizeManagedDockerDrain(ctx, request, false)
	return err
}

func (s *Service) finalizeManagedDockerDrain(
	ctx context.Context,
	request executiontargets.ManagedDockerWorkerDrainRequest,
	allowBusy bool,
) (bool, error) {
	finalized := false
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		worker, found, err := lockManagedDockerWorker(ctx, tx, request.ExecutionTargetID, request.ContainerName)
		if err != nil || !found {
			return err
		}
		if worker.ReconciliationDrainRequestedAt == nil {
			return nil
		}
		executionLeases, cleanupLeases, err := managedDockerWorkerLeaseCounts(ctx, tx, worker)
		if err != nil {
			return err
		}
		if executionLeases > 0 || cleanupLeases > 0 {
			if allowBusy {
				return nil
			}
			return problem.New(409, "docker_worker_drain_busy", "The removed managed Docker Worker still owns an active lease.")
		}
		if err := terminalizeWorkerIncarnationFromLeaseLocked(
			ctx,
			tx,
			persistence.WorkerLease{
				WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
				WorkerInstanceUID: worker.InstanceUID,
			},
			request.ObservedAt,
			request.Reason,
		); err != nil {
			return err
		}
		finalized = true
		return nil
	})
	return finalized, err
}

func (s *Service) CompleteManagedDockerReplacement(
	ctx context.Context,
	targetID uuid.UUID,
	containerName string,
	drainInstanceUID string,
	observedAt time.Time,
) (bool, error) {
	containerName = strings.TrimSpace(containerName)
	drainInstanceUID = strings.TrimSpace(drainInstanceUID)
	if targetID == uuid.Nil || containerName == "" || observedAt.IsZero() {
		return false, problem.New(400, "invalid_docker_worker_observation", "The managed Docker replacement observation is incomplete.")
	}
	if parsed, err := uuid.Parse(drainInstanceUID); err != nil || parsed.String() != drainInstanceUID {
		return false, problem.New(400, "invalid_docker_worker_observation", "The managed Docker drain instance UID is invalid.")
	}
	completed := false
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		worker, found, err := lockManagedDockerWorker(ctx, tx, targetID, containerName)
		if err != nil || !found {
			return err
		}
		if worker.ReconciliationDrainInstanceUID == nil || *worker.ReconciliationDrainInstanceUID != drainInstanceUID {
			return nil
		}
		if worker.InstanceUID == drainInstanceUID {
			return nil
		}
		updates := map[string]any{
			"reconciliation_drain_incarnation":  nil,
			"reconciliation_drain_instance_uid": nil,
			"reconciliation_drain_requested_at": nil,
			"reconciliation_drain_reason":       nil,
		}
		if worker.AdministrativeStatus == "active" && worker.Status == "draining" {
			updates["status"] = "online"
			updates["draining_at"] = nil
			worker.Status = "online"
			worker.DrainingAt = nil
		}
		updated := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where(
				"id = ? AND incarnation = ? AND instance_uid = ? AND reconciliation_drain_instance_uid = ?",
				worker.ID, worker.Incarnation, worker.InstanceUID, drainInstanceUID,
			).
			Updates(updates)
		if updated.Error != nil {
			return problem.Wrap(500, "docker_worker_replacement_complete_failed", "The managed Docker replacement could not be completed.", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return problem.New(409, "docker_worker_replacement_conflict", "The managed Docker replacement changed before completion.")
		}
		worker.ReconciliationDrainIncarnation = nil
		worker.ReconciliationDrainInstanceUID = nil
		worker.ReconciliationDrainRequestedAt = nil
		worker.ReconciliationDrainReason = nil
		state, err := currentWorkerFactStateLocked(ctx, tx, worker)
		if err != nil {
			return err
		}
		if err := transitionWorkerIncarnationFactLocked(ctx, tx, worker, observedAt.UTC(), state, false, ""); err != nil {
			return err
		}
		completed = true
		return nil
	})
	return completed, err
}

func lockManagedDockerWorker(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	containerName string,
) (persistence.WorkerInstance, bool, error) {
	var worker persistence.WorkerInstance
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where(
			"execution_target_id = ? AND target_kind = ? AND pod_name = ? AND status <> ?",
			targetID, "docker", containerName, "terminated",
		).
		Take(&worker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerInstance{}, false, nil
	}
	if err != nil {
		return persistence.WorkerInstance{}, false, problem.Wrap(500, "docker_worker_lifecycle_load_failed", "The managed Docker Worker lifecycle could not be loaded.", err)
	}
	return worker, true, nil
}

func managedDockerWorkerLeaseCounts(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
) (int64, int64, error) {
	var executionLeases int64
	if err := tx.WithContext(ctx).Model(&persistence.WorkerLease{}).
		Where(
			"worker_id = ? AND worker_incarnation = ? AND worker_instance_uid = ?",
			worker.ID, worker.Incarnation, worker.InstanceUID,
		).
		Count(&executionLeases).Error; err != nil {
		return 0, 0, problem.Wrap(500, "docker_worker_lease_load_failed", "Managed Docker Worker Execution leases could not be loaded.", err)
	}
	var cleanupLeases int64
	if tx.Dialector.Name() != "sqlite" || tx.Migrator().HasTable(&persistence.WorkspaceCleanupCommand{}) {
		if err := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
			Where(
				"delivery_worker_id = ? AND delivery_worker_incarnation = ? AND status IN ?",
				worker.ID, worker.Incarnation, []string{"leased", "running"},
			).
			Count(&cleanupLeases).Error; err != nil {
			return 0, 0, problem.Wrap(500, "docker_worker_cleanup_lease_load_failed", "Managed Docker Worker cleanup leases could not be loaded.", err)
		}
	}
	return executionLeases, cleanupLeases, nil
}

func normalizeManagedDockerDrainRequest(
	request executiontargets.ManagedDockerWorkerDrainRequest,
) (executiontargets.ManagedDockerWorkerDrainRequest, error) {
	request.ContainerName = strings.TrimSpace(request.ContainerName)
	request.Reason = strings.TrimSpace(request.Reason)
	request.ObservedAt = request.ObservedAt.UTC()
	if request.ExecutionTargetID == uuid.Nil || request.ContainerName == "" || len(request.ContainerName) > 253 || request.ObservedAt.IsZero() {
		return executiontargets.ManagedDockerWorkerDrainRequest{}, problem.New(400, "invalid_docker_worker_observation", "The managed Docker Worker drain request is incomplete.")
	}
	if request.Reason != executiontargets.ManagedDockerDrainReasonStaleSpec && request.Reason != executiontargets.ManagedDockerDrainReasonScaleDown {
		return executiontargets.ManagedDockerWorkerDrainRequest{}, problem.New(400, "invalid_docker_worker_observation", "The managed Docker Worker drain reason is unsupported.")
	}
	return request, nil
}

func managedDockerDrainReason(reason *string) string {
	if reason != nil && strings.TrimSpace(*reason) == executiontargets.ManagedDockerDrainReasonScaleDown {
		return executiontargets.ManagedDockerDrainReasonScaleDown
	}
	return executiontargets.ManagedDockerDrainReasonStaleSpec
}
