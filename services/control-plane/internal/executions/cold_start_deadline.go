package executions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type ColdStartDeadlineScope struct {
	TenantID          uuid.UUID
	ExecutionTargetID uuid.UUID
	WorkerPoolID      uuid.UUID
	WorkerPoolVersion int64
	Cutoff            time.Time
	Limit             int
}

// ExpireInteractiveColdStarts enforces the configured hard upper bound. It
// terminalizes only still-unclaimed interactive work under an exact Worker
// Pool snapshot; a concurrent Claim wins the row/Lease race and is never
// labelled as a cold-start timeout after Provider ownership began.
func (s *Service) ExpireInteractiveColdStarts(
	ctx context.Context,
	scope ColdStartDeadlineScope,
) (int, error) {
	if scope.TenantID == uuid.Nil || scope.ExecutionTargetID == uuid.Nil ||
		scope.WorkerPoolID == uuid.Nil || scope.WorkerPoolVersion <= 0 || scope.Cutoff.IsZero() ||
		scope.Limit < 1 || scope.Limit > 10_000 {
		return 0, problem.New(400, "invalid_cold_start_deadline_scope", "Cold-start deadline scope is invalid.")
	}
	var ids []uuid.UUID
	if err := s.db.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where(
			`tenant_id = ? AND execution_target_id = ? AND worker_pool_id = ? AND worker_pool_version = ?
			 AND queue_class = ? AND status IN ? AND worker_id IS NULL AND queued_at <= ?`,
			scope.TenantID, scope.ExecutionTargetID, scope.WorkerPoolID, scope.WorkerPoolVersion,
			"interactive", []string{"queued", "recovering"}, scope.Cutoff,
		).
		Order("queued_at, id").Limit(scope.Limit).Pluck("id", &ids).Error; err != nil {
		return 0, problem.Wrap(500, "cold_start_deadline_candidates_load_failed", "Cold-start deadline candidates could not be loaded.", err)
	}
	changed := 0
	var failures []error
	for _, executionID := range ids {
		var appended persistence.SessionEvent
		err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
			var execution persistence.AgentExecution
			err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where(
					`tenant_id = ? AND id = ? AND execution_target_id = ? AND worker_pool_id = ?
					 AND worker_pool_version = ? AND queue_class = ? AND status IN ?
					 AND worker_id IS NULL AND queued_at <= ?`,
					scope.TenantID, executionID, scope.ExecutionTargetID, scope.WorkerPoolID,
					scope.WorkerPoolVersion, "interactive", []string{"queued", "recovering"}, scope.Cutoff,
				).
				Take(&execution).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return problem.Wrap(500, "cold_start_deadline_execution_lock_failed", "Cold-start deadline candidate could not be locked.", err)
			}
			var leases int64
			if err := tx.WithContext(ctx).Model(&persistence.WorkerLease{}).
				Where("tenant_id = ? AND execution_id = ?", execution.TenantID, execution.ID).
				Count(&leases).Error; err != nil {
				return problem.Wrap(500, "cold_start_deadline_lease_check_failed", "Cold-start deadline lease state could not be checked.", err)
			}
			if leases != 0 {
				return nil
			}
			appended, err = s.cancelExecutionLocked(
				ctx, tx, &execution, nil, "system", nil, s.now(), "interactive-cold-start-deadline",
			)
			if err != nil {
				return err
			}
			changed++
			return nil
		})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if appended.EventID != uuid.Nil {
			s.sessions.PublishInternalEvent(appended)
		}
	}
	return changed, errors.Join(failures...)
}
