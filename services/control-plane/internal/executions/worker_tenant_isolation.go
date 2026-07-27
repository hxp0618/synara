package executions

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func filterClaimQueryByWorkerTenantBinding(
	query *gorm.DB,
	worker persistence.WorkerInstance,
	column string,
) *gorm.DB {
	if worker.TenantBindingID == nil {
		return query
	}
	return query.Where(column+" = ?", *worker.TenantBindingID)
}

func bindGeneralWorkerTenantForExecution(
	ctx context.Context,
	tx *gorm.DB,
	worker *persistence.WorkerInstance,
	execution persistence.AgentExecution,
) error {
	if worker == nil || worker.WorkerMode != WorkerModeGeneralPool {
		return nil
	}
	isolation, err := loadExecutionPoolTenantIsolation(ctx, tx, execution)
	if err != nil {
		return err
	}
	return bindGeneralWorkerTenant(ctx, tx, worker, execution.TenantID, isolation)
}

func validateWorkerTenantBinding(
	worker persistence.WorkerInstance,
	tenantID uuid.UUID,
) error {
	if worker.TenantBindingID != nil && *worker.TenantBindingID != tenantID {
		return problem.New(409, "worker_tenant_binding_mismatch", "The Worker is permanently bound to another Tenant.")
	}
	return nil
}

func bindGeneralWorkerTenantForWorkspaceCleanup(
	ctx context.Context,
	tx *gorm.DB,
	worker *persistence.WorkerInstance,
	targetID, tenantID uuid.UUID,
) error {
	if worker == nil || worker.WorkerMode != WorkerModeGeneralPool {
		return nil
	}
	isolation, err := loadDefaultPoolTenantIsolation(ctx, tx, targetID)
	if err != nil {
		return err
	}
	return bindGeneralWorkerTenant(ctx, tx, worker, tenantID, isolation)
}

func loadExecutionPoolTenantIsolation(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (string, error) {
	if execution.WorkerPoolID == nil {
		return loadDefaultPoolTenantIsolation(ctx, tx, execution.ExecutionTargetID)
	}
	if execution.WorkerPoolVersion == nil {
		return "", problem.New(409, "worker_pool_assignment_mismatch", "The Execution Worker pool snapshot is incomplete.")
	}
	var pool persistence.WorkerPool
	err := tx.WithContext(ctx).
		Select("id, execution_target_id, version, mode, tenant_isolation").
		Where("id = ? AND execution_target_id = ? AND version = ?", *execution.WorkerPoolID, execution.ExecutionTargetID, *execution.WorkerPoolVersion).
		Take(&pool).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", problem.New(409, "worker_pool_assignment_mismatch", "The Execution Worker pool snapshot is unavailable.")
	}
	if err != nil {
		return "", problem.Wrap(500, "worker_pool_tenant_isolation_lookup_failed", "The Worker pool Tenant isolation policy could not be loaded.", err)
	}
	return normalizePersistedTenantIsolation(pool.TenantIsolation)
}

func loadDefaultPoolTenantIsolation(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
) (string, error) {
	var row struct {
		TenantIsolation string `gorm:"column:tenant_isolation"`
	}
	err := tx.WithContext(ctx).Table("execution_placement_policies AS placement_policy").
		Select("default_pool.tenant_isolation").
		Joins("JOIN worker_pools AS default_pool ON default_pool.id = placement_policy.default_pool_id AND default_pool.execution_target_id = placement_policy.execution_target_id").
		Where("placement_policy.execution_target_id = ?", targetID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Direct-import and pre-placement Target rows use the same safe default as
		// placement.EnsureDefault. Missing metadata must never imply shared reuse.
		return placement.TenantIsolationPinned, nil
	}
	if err != nil {
		return "", problem.Wrap(500, "worker_pool_tenant_isolation_lookup_failed", "The default Worker pool Tenant isolation policy could not be loaded.", err)
	}
	return normalizePersistedTenantIsolation(row.TenantIsolation)
}

func normalizePersistedTenantIsolation(value string) (string, error) {
	switch value {
	case placement.TenantIsolationPinned, placement.TenantIsolationShared:
		return value, nil
	default:
		return "", problem.New(500, "worker_pool_tenant_isolation_invalid", "The persisted Worker pool Tenant isolation policy is invalid.")
	}
}

func bindGeneralWorkerTenant(
	ctx context.Context,
	tx *gorm.DB,
	worker *persistence.WorkerInstance,
	tenantID uuid.UUID,
	isolation string,
) error {
	if worker.TenantBindingID != nil {
		if *worker.TenantBindingID != tenantID {
			return problem.New(409, "worker_tenant_binding_mismatch", "The Worker is permanently bound to another Tenant.")
		}
		return nil
	}
	if isolation == placement.TenantIsolationShared {
		return nil
	}
	if isolation != placement.TenantIsolationPinned {
		return problem.New(500, "worker_pool_tenant_isolation_invalid", "The persisted Worker pool Tenant isolation policy is invalid.")
	}
	result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where("id = ? AND incarnation = ? AND instance_uid = ? AND tenant_binding_id IS NULL", worker.ID, worker.Incarnation, worker.InstanceUID).
		Update("tenant_binding_id", tenantID)
	if result.Error != nil {
		return problem.Wrap(500, "worker_tenant_binding_failed", "The Worker could not be bound to the selected Tenant.", result.Error)
	}
	if result.RowsAffected != 1 {
		return problem.New(409, "worker_tenant_binding_conflict", "The Worker Tenant binding changed concurrently.")
	}
	bound := tenantID
	worker.TenantBindingID = &bound
	return nil
}
