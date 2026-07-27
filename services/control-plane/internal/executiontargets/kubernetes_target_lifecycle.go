package executiontargets

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
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

const kubernetesReconcilerAdvisoryLock = "synara:kubernetes-execution-reconciler"

var kubernetesTargetNonterminalExecutionStatuses = []string{
	"queued", "recovering", "leased", "running", "waiting-for-approval", "suspended",
}

// DisableManagedKubernetesTarget permanently removes a tenant-owned managed
// Kubernetes Target from placement and reconciliation. It deliberately shares
// the Reconciler's cycle lock: a Target row lock alone cannot fence an older
// reconciliation snapshot that is already about to write to the Kubernetes
// API.
func (s *Service) DisableManagedKubernetesTarget(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	requestID, ipAddress string,
) (Target, bool, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Target{}, false, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return Target{}, false, err
	}

	release, acquired, err := s.tryKubernetesReconcilerLock(ctx)
	if err != nil {
		return Target{}, false, problem.Wrap(
			500,
			"kubernetes_target_disable_coordination_failed",
			"Managed Kubernetes Target disable coordination failed.",
			err,
		)
	}
	if !acquired {
		return Target{}, false, problem.New(
			409,
			"kubernetes_reconciler_busy",
			"The managed Kubernetes Reconciler is active; retry Target disable after the current cycle finishes.",
		)
	}
	defer release()

	var target persistence.ExecutionTarget
	replayed := false
	now := time.Now().UTC()
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND tenant_id = ?", targetID, tenantID).
			Take(&target).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return problem.New(404, "execution_target_not_found", "Execution target not found.")
		}
		if loadErr != nil {
			return problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the execution target.", loadErr)
		}
		if target.Kind != "kubernetes" {
			return problem.New(
				409,
				"managed_kubernetes_target_required",
				"Only a tenant-owned managed Kubernetes Target can use this disable operation.",
			)
		}
		if target.Status == "disabled" {
			replayed = true
			return nil
		}
		if target.Status != "active" {
			return problem.New(
				409,
				"kubernetes_target_not_ready_for_disable",
				"The managed Kubernetes Target must complete a healthy reconciliation before it can be disabled.",
			)
		}

		if err := requireManagedKubernetesTargetCountZero(
			ctx, tx, &persistence.ExecutionTargetGroupMember{},
			"tenant_id = ? AND execution_target_id = ? AND status <> ?",
			[]any{tenantID, targetID, routing.MemberStatusDisabled},
			"kubernetes_target_routing_active",
			"Disable every Target Group member before disabling the managed Kubernetes Target.",
			"activeMemberCount",
		); err != nil {
			return err
		}
		if err := requireManagedKubernetesTargetCountZero(
			ctx, tx, &persistence.AgentSession{},
			"tenant_id = ? AND execution_target_id = ? AND execution_target_group_id IS NULL AND status IN ? AND archived_at IS NULL",
			[]any{tenantID, targetID, []string{"active", "suspended"}},
			"kubernetes_target_fixed_session_active",
			"Migrate or archive every fixed-Target Session before disabling the managed Kubernetes Target.",
			"activeSessionCount",
		); err != nil {
			return err
		}
		if err := requireManagedKubernetesTargetCountZero(
			ctx, tx, &persistence.AgentExecution{},
			"tenant_id = ? AND execution_target_id = ? AND status IN ?",
			[]any{tenantID, targetID, kubernetesTargetNonterminalExecutionStatuses},
			"kubernetes_target_execution_active",
			"The managed Kubernetes Target still owns nonterminal Executions.",
			"activeExecutionCount",
		); err != nil {
			return err
		}
		if err := requireManagedKubernetesTargetCountZero(
			ctx, tx, &persistence.WorkerPool{},
			"execution_target_id = ? AND status <> ?",
			[]any{targetID, "disabled"},
			"kubernetes_target_worker_pool_active",
			"Disable every Worker Pool before disabling the managed Kubernetes Target.",
			"activeWorkerPoolCount",
		); err != nil {
			return err
		}
		if err := requireManagedKubernetesTargetCountZero(
			ctx, tx, &persistence.WorkspaceMaterialization{},
			"tenant_id = ? AND execution_target_id = ? AND state <> ?",
			[]any{tenantID, targetID, "cleaned"},
			"kubernetes_target_workspace_active",
			"Clean every physical Workspace materialization before disabling the managed Kubernetes Target.",
			"activeWorkspaceCount",
		); err != nil {
			return err
		}
		if err := requireManagedKubernetesTargetCountZero(
			ctx, tx, &persistence.WorkspaceCleanupCommand{},
			"tenant_id = ? AND execution_target_id = ? AND status IN ?",
			[]any{tenantID, targetID, []string{"pending", "leased", "running"}},
			"kubernetes_target_workspace_cleanup_active",
			"Workspace cleanup delivery must finish before disabling the managed Kubernetes Target.",
			"activeWorkspaceCleanupCount",
		); err != nil {
			return err
		}
		if err := requireManagedKubernetesTargetCountZero(
			ctx, tx, &persistence.WorkerInstance{},
			"execution_target_id = ? AND (status <> ? OR terminated_at IS NULL)",
			[]any{targetID, "terminated"},
			"kubernetes_target_worker_active",
			"Every Worker incarnation must be authoritatively terminated before disabling the managed Kubernetes Target.",
			"activeWorkerCount",
		); err != nil {
			return err
		}
		if err := requireManagedKubernetesTargetLeaseCountZero(ctx, tx, targetID); err != nil {
			return err
		}

		var health persistence.ExecutionTargetHealth
		healthErr := tx.WithContext(ctx).
			Where("execution_target_id = ?", targetID).
			Take(&health).Error
		if errors.Is(healthErr, gorm.ErrRecordNotFound) {
			return problem.New(
				409,
				"kubernetes_target_health_unavailable",
				"A fresh authoritative managed Kubernetes health observation is required before Target disable.",
			)
		}
		if healthErr != nil {
			return problem.Wrap(
				500,
				"kubernetes_target_health_load_failed",
				"Managed Kubernetes Target health could not be loaded.",
				healthErr,
			)
		}
		if !managedKubernetesTargetHealthAuthoritativelyIdle(health, now) {
			return &problem.Error{
				Status:  409,
				Code:    "kubernetes_target_health_not_idle",
				Message: "A fresh successful managed Kubernetes reconciliation must prove zero allocated capacity before Target disable.",
				Details: map[string]any{
					"healthStatus":                 health.Status,
					"capacityStatus":               health.CapacityStatus,
					"allocatedCapacityUnits":       health.AllocatedCapacityUnits,
					"reservationAcknowledgedUnits": health.ReservationAcknowledgedUnits,
					"healthVersion":                health.Version,
				},
			}
		}

		updatedAt := time.Now().UTC()
		result := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
			Where("id = ? AND tenant_id = ? AND kind = ? AND status = ?", targetID, tenantID, "kubernetes", "active").
			Updates(map[string]any{"status": "disabled", "updated_at": updatedAt})
		if result.Error != nil {
			return problem.Wrap(
				409,
				"kubernetes_target_disable_rejected",
				"Managed Kubernetes Target disable was rejected.",
				result.Error,
			)
		}
		if result.RowsAffected != 1 {
			return problem.New(
				409,
				"kubernetes_target_disable_conflict",
				"Managed Kubernetes Target state changed while disable was being committed.",
			)
		}
		target.Status = "disabled"
		target.UpdatedAt = updatedAt
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "execution_target.kubernetes_disabled", ResourceType: "execution_target", ResourceID: &target.ID,
			OrganizationID: target.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"fromStatus": "active", "toStatus": "disabled",
				"healthVersion": health.Version, "healthObservedAt": health.ObservedAt,
			},
		})
	})
	if err != nil {
		return Target{}, false, err
	}
	return toTarget(target), replayed, nil
}

func (s *Service) tryKubernetesReconcilerLock(ctx context.Context) (func(), bool, error) {
	if s.db.Dialector.Name() == "postgres" {
		return persistence.TryAdvisoryLock(ctx, s.db, kubernetesReconcilerAdvisoryLock)
	}
	if !s.kubernetesReconcilerLocalMu.TryLock() {
		return func() {}, false, nil
	}
	return s.kubernetesReconcilerLocalMu.Unlock, true, nil
}

func requireManagedKubernetesTargetCountZero(
	ctx context.Context,
	tx *gorm.DB,
	model any,
	where string,
	args []any,
	code, message, countKey string,
) error {
	var count int64
	if err := tx.WithContext(ctx).Model(model).Where(where, args...).Count(&count).Error; err != nil {
		return problem.Wrap(
			500,
			"kubernetes_target_disable_probe_failed",
			"Managed Kubernetes Target disable safety could not be evaluated.",
			err,
		)
	}
	if count == 0 {
		return nil
	}
	return &problem.Error{
		Status: 409, Code: code, Message: message, Details: map[string]any{countKey: count},
	}
}

func requireManagedKubernetesTargetLeaseCountZero(ctx context.Context, tx *gorm.DB, targetID uuid.UUID) error {
	var count int64
	if err := tx.WithContext(ctx).Table("worker_leases AS lease").
		Joins("JOIN worker_instances AS worker ON worker.id = lease.worker_id AND worker.incarnation = lease.worker_incarnation").
		Where("worker.execution_target_id = ?", targetID).
		Count(&count).Error; err != nil {
		return problem.Wrap(
			500,
			"kubernetes_target_disable_probe_failed",
			"Managed Kubernetes Target disable safety could not be evaluated.",
			err,
		)
	}
	if count == 0 {
		return nil
	}
	return &problem.Error{
		Status:  409,
		Code:    "kubernetes_target_lease_active",
		Message: "Every Worker lease must be released before disabling the managed Kubernetes Target.",
		Details: map[string]any{"activeLeaseCount": count},
	}
}

func managedKubernetesTargetHealthAuthoritativelyIdle(
	health persistence.ExecutionTargetHealth,
	now time.Time,
) bool {
	return health.Status == routing.HealthHealthy &&
		health.CapacityStatus == routing.CapacityAvailable &&
		health.AvailableCapacityUnits != nil && *health.AvailableCapacityUnits > 0 &&
		health.AllocatedCapacityUnits == 0 &&
		health.ReservationAuthorityMode != nil &&
		*health.ReservationAuthorityMode == routing.ReservationAuthorityExactActiveV1 &&
		health.ReservationAcknowledgedUnits == 0 &&
		strings.HasPrefix(health.Source, managedKubernetesRoutingPublisherPrefix) &&
		!health.ObservedAt.After(now) &&
		health.ExpiresAt.After(now)
}
