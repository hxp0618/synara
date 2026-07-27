package executions

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/workerfacts"
)

const (
	workerFactStateIdle       = workerfacts.StateIdle
	workerFactStateActive     = workerfacts.StateActive
	workerFactStateDraining   = workerfacts.StateDraining
	workerFactStateOffline    = workerfacts.StateOffline
	workerFactStateTerminated = workerfacts.StateTerminated
)

func ensureWorkerIncarnationFactWithResourcesLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	observedAt time.Time,
	initialState string,
	requestedCPUMillicores, requestedMemoryBytes, requestedEphemeralStorageBytes *int64,
) error {
	input, available, err := workerFactEnsureInput(ctx, tx, worker, observedAt)
	if err != nil || !available {
		return err
	}
	input.InitialState = initialState
	input.RequestedResources = workerfacts.RequestedResources{
		CPUMillicores:         requestedCPUMillicores,
		MemoryBytes:           requestedMemoryBytes,
		EphemeralStorageBytes: requestedEphemeralStorageBytes,
	}
	_, err = workerfacts.EnsureCurrentInTransaction(ctx, tx, input)
	return err
}

func transitionWorkerIncarnationFactLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	observedAt time.Time,
	nextState string,
	incrementClaimCount bool,
	terminalReason string,
) error {
	input, available, err := workerFactEnsureInput(ctx, tx, worker, observedAt)
	if err != nil || !available {
		return err
	}
	var reason *string
	if trimmed := strings.TrimSpace(terminalReason); trimmed != "" {
		reason = &trimmed
	}
	_, err = workerfacts.TransitionCurrentInTransaction(ctx, tx, workerfacts.TransitionInput{
		EnsureInput:         input,
		NextState:           nextState,
		IncrementClaimCount: incrementClaimCount,
		TerminalReason:      reason,
	})
	return err
}

func workerFactEnsureInput(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	observedAt time.Time,
) (workerfacts.EnsureInput, bool, error) {
	if tx == nil {
		return workerfacts.EnsureInput{}, false, problem.New(500, "worker_fact_database_missing", "The Worker lifecycle fact database is unavailable.")
	}
	// Focused SQLite tests may intentionally construct a historical subset of
	// the schema. PostgreSQL production startup applies migrations before the
	// service starts, so absence there is an invariant violation rather than a
	// compatibility mode.
	if !workerIncarnationFactsAvailable(tx) {
		return workerfacts.EnsureInput{}, false, nil
	}

	input := workerfacts.EnsureInput{
		Worker:     worker,
		ObservedAt: observedAt.UTC(),
	}
	var existing persistence.WorkerIncarnationFact
	err := tx.WithContext(ctx).
		Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
		Take(&existing).Error
	if err == nil {
		input.Target = workerfacts.TargetSnapshot{TenantID: existing.TenantID}
		input.RequestedResources = workerfacts.RequestedResources{
			CPUMillicores:         existing.RequestedCPUMillicores,
			MemoryBytes:           existing.RequestedMemoryBytes,
			EphemeralStorageBytes: existing.RequestedEphemeralStorageBytes,
		}
		if existing.WorkerPoolID != nil && existing.WorkerPoolVersion != nil &&
			existing.PoolMode != nil && existing.CapacityClass != nil {
			input.Pool = &workerfacts.PoolSnapshot{
				ID: *existing.WorkerPoolID, Version: *existing.WorkerPoolVersion,
				Mode: *existing.PoolMode, CapacityClass: *existing.CapacityClass,
				Region: existing.Region,
			}
		}
		return input, true, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return workerfacts.EnsureInput{}, false, problem.Wrap(
			500, "worker_fact_load_failed", "The Worker incarnation fact could not be loaded.", err,
		)
	}

	var target persistence.ExecutionTarget
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id").
		Where("id = ?", worker.ExecutionTargetID).
		Take(&target).Error; err != nil {
		return workerfacts.EnsureInput{}, false, problem.Wrap(
			500, "worker_fact_target_load_failed", "The Worker fact Execution Target could not be loaded.", err,
		)
	}
	input.Target = workerfacts.TargetSnapshot{TenantID: target.TenantID}
	if worker.WorkerPoolID != nil {
		var pool persistence.WorkerPool
		if err := tx.WithContext(ctx).
			Select("id", "version", "mode", "capacity_class", "region").
			Where("id = ? AND execution_target_id = ?", *worker.WorkerPoolID, worker.ExecutionTargetID).
			Take(&pool).Error; err != nil {
			return workerfacts.EnsureInput{}, false, problem.Wrap(
				500, "worker_fact_pool_load_failed", "The Worker fact Pool snapshot could not be loaded.", err,
			)
		}
		input.Pool = &workerfacts.PoolSnapshot{
			ID: pool.ID, Version: pool.Version, Mode: pool.Mode,
			CapacityClass: pool.CapacityClass, Region: pool.Region,
		}
	}
	return input, true, nil
}

func workerIncarnationFactsAvailable(tx *gorm.DB) bool {
	return tx != nil && (tx.Dialector.Name() != "sqlite" || tx.Migrator().HasTable(&persistence.WorkerIncarnationFact{}))
}

func currentWorkerFactStateLocked(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
) (string, error) {
	switch {
	case worker.Status == "terminated":
		return workerFactStateTerminated, nil
	case worker.ReconciliationDrainRequestedAt != nil:
		return workerFactStateDraining, nil
	case worker.AdministrativeStatus == "revoked", worker.AdministrativeStatus == "draining", worker.Status == "draining":
		return workerFactStateDraining, nil
	case worker.Status == "offline":
		return workerFactStateOffline, nil
	}
	var leases int64
	if err := tx.WithContext(ctx).Model(&persistence.WorkerLease{}).
		Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
		Count(&leases).Error; err != nil {
		return "", problem.Wrap(500, "worker_fact_lease_probe_failed", "The Worker lifecycle lease state could not be loaded.", err)
	}
	if leases > 0 {
		return workerFactStateActive, nil
	}
	if tx.Dialector.Name() != "sqlite" || tx.Migrator().HasTable(&persistence.WorkspaceCleanupCommand{}) {
		referenceTime := worker.LastHeartbeatAt.UTC()
		if referenceTime.IsZero() {
			referenceTime = worker.RegisteredAt.UTC()
		}
		if referenceTime.IsZero() {
			referenceTime = time.Now().UTC()
		}
		var cleanupLeases int64
		if err := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
			Where("delivery_worker_id = ? AND delivery_worker_incarnation = ? AND status IN ?",
				worker.ID, worker.Incarnation, []string{"leased", "running"}).
			Where("(lease_expires_at IS NULL OR lease_expires_at > ?)", referenceTime).
			Count(&cleanupLeases).Error; err != nil {
			return "", problem.Wrap(500, "worker_fact_cleanup_probe_failed", "The Worker cleanup lease state could not be loaded.", err)
		}
		if cleanupLeases > 0 {
			return workerFactStateActive, nil
		}
	}
	return workerFactStateIdle, nil
}

func transitionWorkerAfterLeaseReleasedLocked(
	ctx context.Context,
	tx *gorm.DB,
	lease persistence.WorkerLease,
	observedAt time.Time,
) error {
	var worker persistence.WorkerInstance
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ?", lease.WorkerID).
		Take(&worker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return problem.Wrap(500, "worker_fact_worker_load_failed", "The released lease Worker lifecycle could not be loaded.", err)
	}
	if worker.Incarnation != lease.WorkerIncarnation || worker.InstanceUID != lease.WorkerInstanceUID {
		// Re-registration already terminalizes the prior physical incarnation.
		return nil
	}

	now := observedAt.UTC()
	if worker.WorkerMode != WorkerModeGeneralPool && worker.Status == "online" && worker.AdministrativeStatus != "revoked" {
		updated := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("id = ? AND incarnation = ? AND instance_uid = ? AND status = ?",
				worker.ID, worker.Incarnation, worker.InstanceUID, "online").
			Updates(map[string]any{"status": "draining", "draining_at": now})
		if updated.Error != nil {
			return problem.Wrap(500, "worker_drain_update_failed", "The one-shot Worker could not enter Drain after its Execution attempt.", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return problem.New(409, "worker_drain_update_conflict", "The one-shot Worker lifecycle changed while its Execution lease was released.")
		}
		worker.Status = "draining"
		worker.DrainingAt = &now
	}
	state, err := currentWorkerFactStateLocked(ctx, tx, worker)
	if err != nil {
		return err
	}
	return transitionWorkerIncarnationFactLocked(ctx, tx, worker, now, state, false, "")
}

func transitionWorkerAfterWorkspaceCleanupLeaseReleasedLocked(
	ctx context.Context,
	tx *gorm.DB,
	command persistence.WorkspaceCleanupCommand,
	observedAt time.Time,
) error {
	if command.DeliveryWorkerID == nil || command.DeliveryWorkerIncarnation == nil {
		return nil
	}

	var worker persistence.WorkerInstance
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ?", *command.DeliveryWorkerID).
		Take(&worker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return problem.Wrap(500, "worker_fact_worker_load_failed", "The released cleanup Worker lifecycle could not be loaded.", err)
	}
	if worker.Incarnation != *command.DeliveryWorkerIncarnation {
		// Re-registration already terminalizes the prior physical incarnation.
		return nil
	}

	now := observedAt.UTC()
	if worker.WorkerMode != WorkerModeGeneralPool && worker.Status == "online" && worker.AdministrativeStatus != "revoked" {
		updated := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("id = ? AND incarnation = ? AND status = ?",
				worker.ID, worker.Incarnation, "online").
			Updates(map[string]any{"status": "draining", "draining_at": now})
		if updated.Error != nil {
			return problem.Wrap(500, "worker_drain_update_failed", "The cleanup Worker could not enter Drain after its attempt.", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return problem.New(409, "worker_drain_update_conflict", "The cleanup Worker lifecycle changed while its lease was released.")
		}
		worker.Status = "draining"
		worker.DrainingAt = &now
	}
	state, err := currentWorkerFactStateLocked(ctx, tx, worker)
	if err != nil {
		return err
	}
	return transitionWorkerIncarnationFactLocked(ctx, tx, worker, now, state, false, "")
}

func terminalizeWorkerIncarnationFromLeaseLocked(
	ctx context.Context,
	tx *gorm.DB,
	lease persistence.WorkerLease,
	observedAt time.Time,
	reason string,
) error {
	if !workerIncarnationFactsAvailable(tx) {
		return nil
	}
	now := observedAt.UTC()
	var fact persistence.WorkerIncarnationFact
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("worker_id = ? AND worker_incarnation = ?", lease.WorkerID, lease.WorkerIncarnation).
		Take(&fact).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var current persistence.WorkerInstance
		if loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND incarnation = ? AND instance_uid = ?",
				lease.WorkerID, lease.WorkerIncarnation, lease.WorkerInstanceUID).
			Take(&current).Error; loadErr != nil {
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				return problem.New(500, "worker_fact_missing", "The terminal Worker incarnation fact was not available for the observed physical Worker.")
			}
			return problem.Wrap(500, "worker_fact_worker_load_failed", "The terminal Worker incarnation could not be loaded.", loadErr)
		}
		if err := transitionWorkerIncarnationFactLocked(
			ctx, tx, current, observedAt, workerFactStateTerminated, false, reason,
		); err != nil {
			return err
		}
	} else if err != nil {
		return problem.Wrap(500, "worker_fact_load_failed", "The terminal Worker incarnation fact could not be loaded.", err)
	} else {
		if fact.CurrentState != workerFactStateTerminated {
			worker := workerInstanceFromIncarnationFact(fact)
			if err := transitionWorkerIncarnationFactLocked(
				ctx, tx, worker, observedAt, workerFactStateTerminated, false, reason,
			); err != nil {
				return err
			}
		} else if fact.TerminatedAt != nil {
			now = fact.TerminatedAt.UTC()
		}
	}

	updated := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where("id = ? AND incarnation = ? AND instance_uid = ? AND status <> ?",
			lease.WorkerID, lease.WorkerIncarnation, lease.WorkerInstanceUID, "terminated").
		Updates(map[string]any{"status": "terminated", "terminated_at": now})
	if updated.Error != nil {
		return problem.Wrap(500, "worker_terminal_update_failed", "The observed terminal Worker could not be persisted.", updated.Error)
	}
	return nil
}

func workerInstanceFromIncarnationFact(fact persistence.WorkerIncarnationFact) persistence.WorkerInstance {
	return persistence.WorkerInstance{
		ID: fact.WorkerID, Incarnation: fact.WorkerIncarnation, InstanceUID: fact.InstanceUID,
		ExecutionTargetID: fact.ExecutionTargetID, TargetKind: fact.TargetKind,
		WorkerMode: fact.WorkerMode, WorkerPoolID: fact.WorkerPoolID,
		WorkerPoolVersion: fact.WorkerPoolVersion, CapacityClass: fact.CapacityClass,
		ClusterID: fact.ClusterID, Namespace: fact.Namespace, PodName: fact.PodName,
		Status: fact.CurrentState, AdministrativeStatus: "active",
		RegisteredAt: fact.RegisteredAt, LastHeartbeatAt: fact.UpdatedAt,
	}
}
