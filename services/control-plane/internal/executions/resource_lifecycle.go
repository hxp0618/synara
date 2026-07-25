package executions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var sessionAbsoluteExpiryExecutionStatuses = []string{
	"queued",
	"recovering",
	"leased",
	"running",
	"waiting-for-approval",
	"suspended",
}

var sessionAbsoluteExpirySessionStatuses = []string{"active", "suspended"}

const (
	sessionAbsoluteExpiryReason = "The Session reached its absolute lifetime before the Execution lifecycle completed."
	sessionAbsoluteExpiryAction = "session-absolute-expired"
)

type absoluteExpiryCandidate struct {
	TenantID    uuid.UUID `gorm:"column:tenant_id"`
	SessionID   uuid.UUID `gorm:"column:session_id"`
	ExecutionID uuid.UUID `gorm:"column:execution_id"`
}

func (s *Service) EnforceResourceLifecycle(
	ctx context.Context,
	now time.Time,
	limit int,
) (int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	release, acquired, err := persistence.TryAdvisoryLock(ctx, s.db, "synara:session-resource-lifecycle")
	if err != nil {
		return 0, problem.Wrap(500, "resource_lifecycle_lock_failed", "Resource lifecycle coordination failed.", err)
	}
	if !acquired {
		return 0, nil
	}
	defer release()

	candidates := make([]absoluteExpiryCandidate, 0, limit)
	if err := s.db.WithContext(ctx).
		Table("agent_executions AS execution").
		Select("execution.tenant_id", "execution.session_id", "execution.id AS execution_id").
		Joins("JOIN agent_sessions AS session ON session.tenant_id = execution.tenant_id AND session.id = execution.session_id").
		Where(
			"session.status IN ? AND session.archived_at IS NULL AND session.absolute_expires_at IS NOT NULL AND session.absolute_expires_at <= ? AND execution.status IN ?",
			sessionAbsoluteExpirySessionStatuses, now, sessionAbsoluteExpiryExecutionStatuses,
		).
		Order("session.absolute_expires_at, execution.id").
		Limit(limit).
		Scan(&candidates).Error; err != nil {
		return 0, problem.Wrap(500, "resource_lifecycle_scan_failed", "Absolute-expiry Sessions could not be scanned.", err)
	}

	expired := 0
	failures := make([]error, 0)
	for _, candidate := range candidates {
		changed, appended, err := s.expireAbsoluteSessionCandidate(ctx, candidate, now)
		if err != nil {
			failures = append(failures, fmt.Errorf("execution %s: %w", candidate.ExecutionID, err))
			continue
		}
		if !changed {
			continue
		}
		expired++
		if appended.EventID != uuid.Nil {
			s.sessions.PublishInternalEvent(appended)
		}
	}
	return expired, errors.Join(failures...)
}

func (s *Service) expireAbsoluteSessionCandidate(
	ctx context.Context,
	candidate absoluteExpiryCandidate,
	now time.Time,
) (bool, persistence.SessionEvent, error) {
	var appended persistence.SessionEvent
	changed := false
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		// Match every other terminal transition's Lease -> Execution lock order.
		// Interaction rows and the Session event sequence are locked later by the
		// shared cancellation path; pre-locking Session here would invert Resolve's
		// Interaction -> Session order. The absolute deadline itself is immutable.
		var lease persistence.WorkerLease
		leaseErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ?", candidate.TenantID, candidate.ExecutionID).
			Take(&lease).Error
		if leaseErr != nil && !errors.Is(leaseErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "resource_lifecycle_lease_lock_failed", "The absolute-expiry Execution lease could not be locked.", leaseErr)
		}

		var execution persistence.AgentExecution
		executionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND session_id = ? AND id = ?", candidate.TenantID, candidate.SessionID, candidate.ExecutionID).
			Take(&execution).Error
		if errors.Is(executionErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if executionErr != nil {
			return problem.Wrap(500, "resource_lifecycle_execution_lock_failed", "The absolute-expiry Execution could not be locked.", executionErr)
		}
		if !containsExecutionStatus(sessionAbsoluteExpiryExecutionStatuses, execution.Status) {
			return nil
		}

		var session persistence.AgentSession
		sessionErr := tx.WithContext(ctx).
			Where(
				"tenant_id = ? AND id = ? AND status IN ? AND archived_at IS NULL",
				candidate.TenantID,
				candidate.SessionID,
				sessionAbsoluteExpirySessionStatuses,
			).
			Take(&session).Error
		if errors.Is(sessionErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if sessionErr != nil {
			return problem.Wrap(500, "resource_lifecycle_session_load_failed", "An absolute-expiry Session could not be loaded.", sessionErr)
		}
		if session.AbsoluteExpiresAt == nil || session.AbsoluteExpiresAt.After(now) {
			return nil
		}

		var lockedLease *persistence.WorkerLease
		if execution.WorkerID != nil {
			if leaseErr == nil && lease.WorkerID == *execution.WorkerID && lease.Generation == execution.Generation {
				lockedLease = &lease
			} else if leaseErr == nil {
				return problem.New(
					409,
					"resource_lifecycle_lease_mismatch",
					"The absolute-expiry Execution and Worker lease generations do not match.",
				)
			} else {
				synthetic := persistence.WorkerLease{
					TenantID: execution.TenantID, ExecutionID: execution.ID,
					WorkerID: *execution.WorkerID, Generation: execution.Generation,
				}
				lockedLease = &synthetic
			}
		} else if leaseErr == nil {
			return problem.New(
				409,
				"resource_lifecycle_lease_mismatch",
				"The absolute-expiry Execution has a Worker lease without a matching Worker owner.",
			)
		}
		if execution.Status == "suspended" && (execution.WorkerID != nil || leaseErr == nil) {
			return problem.New(
				409,
				"resource_lifecycle_suspended_lease_conflict",
				"The absolute-expiry suspended Execution unexpectedly retained Worker ownership.",
			)
		}

		var err error
		appended, err = s.cancelExecutionLocked(
			ctx,
			tx,
			&execution,
			lockedLease,
			"system",
			nil,
			now,
			sessionAbsoluteExpiryAction,
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
	return changed, appended, nil
}
