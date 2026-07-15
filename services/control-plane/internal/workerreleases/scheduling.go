package workerreleases

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func SelectExecution(
	ctx context.Context,
	db *gorm.DB,
	executionTargetID, executionID uuid.UUID,
) (*Selection, error) {
	if err := LockTargetForRelease(ctx, db, executionTargetID); err != nil {
		return nil, err
	}
	var policy persistence.WorkerReleasePolicy
	err := db.WithContext(ctx).Where("execution_target_id = ?", executionTargetID).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, problem.Wrap(500, "worker_release_policy_lookup_failed", "Failed to load the Worker release policy.", err)
	}
	selection := &Selection{RevisionID: policy.PromotedRevisionID, Channel: ChannelPromoted}
	if policy.CanaryRevisionID != nil && policy.CanaryPercent > 0 && releaseBucket(executionID) < policy.CanaryPercent {
		selection.RevisionID = *policy.CanaryRevisionID
		selection.Channel = ChannelCanary
	}
	return selection, nil
}

// LockTargetForRelease serializes release-policy reads and Worker/Execution
// mutations with policy transitions. Callers must acquire this target lock
// before locking Worker or Execution rows and keep it until their transaction
// commits.
func LockTargetForRelease(ctx context.Context, tx *gorm.DB, executionTargetID uuid.UUID) error {
	var target struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
		Table("execution_targets").Select("id").Where("id = ?", executionTargetID).Take(&target).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(404, "execution_target_not_found", "Execution Target not found.")
	}
	if err != nil {
		return problem.Wrap(500, "worker_release_target_lock_failed", "Failed to lock the Execution Target release policy scope.", err)
	}
	return nil
}

func SynchronizeWorker(
	ctx context.Context,
	tx *gorm.DB,
	worker *persistence.WorkerInstance,
	now time.Time,
) error {
	var policy persistence.WorkerReleasePolicy
	err := tx.WithContext(ctx).Where("execution_target_id = ?", worker.ExecutionTargetID).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return updateWorkerReleaseState(ctx, tx, worker, nil, nil, "unmanaged", nil, nil)
	}
	if err != nil {
		return problem.Wrap(500, "worker_release_policy_lookup_failed", "Failed to load the Worker release policy.", err)
	}

	var revision *persistence.WorkerReleaseRevision
	if worker.CurrentManifestID != nil {
		var matched persistence.WorkerReleaseRevision
		matchErr := tx.WithContext(ctx).
			Where("execution_target_id = ? AND worker_manifest_id = ?", worker.ExecutionTargetID, *worker.CurrentManifestID).
			Take(&matched).Error
		if matchErr == nil {
			revision = &matched
		} else if !errors.Is(matchErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "worker_release_revision_lookup_failed", "Failed to resolve the Worker's release revision.", matchErr)
		}
	}

	checkedAt := now.UTC()
	if revision != nil && revision.ID == policy.PromotedRevisionID {
		channel := ChannelPromoted
		return updateWorkerReleaseState(ctx, tx, worker, &revision.ID, &channel, "active", nil, &checkedAt)
	}
	if revision != nil && policy.CanaryRevisionID != nil && revision.ID == *policy.CanaryRevisionID && policy.CanaryPercent > 0 {
		channel := ChannelCanary
		return updateWorkerReleaseState(ctx, tx, worker, &revision.ID, &channel, "active", nil, &checkedAt)
	}
	reason := "Worker manifest is not selected by the active promoted or canary release policy."
	var revisionID *uuid.UUID
	if revision != nil {
		revisionID = &revision.ID
	}
	return updateWorkerReleaseState(ctx, tx, worker, revisionID, nil, "inactive", &reason, &checkedAt)
}

func RequireActiveWorker(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
) error {
	var policy persistence.WorkerReleasePolicy
	err := tx.WithContext(ctx).Where("execution_target_id = ?", worker.ExecutionTargetID).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if worker.WorkerReleaseStatus == "" || worker.WorkerReleaseStatus == "unmanaged" {
			return nil
		}
		return problem.New(409, "worker_release_state_stale", "Worker release state is stale; re-register the Worker.")
	}
	if err != nil {
		return problem.Wrap(500, "worker_release_policy_lookup_failed", "Failed to load the Worker release policy.", err)
	}
	if worker.WorkerReleaseStatus != "active" || worker.WorkerReleaseRevisionID == nil || worker.WorkerReleaseChannel == nil {
		return inactiveWorkerRelease()
	}
	switch *worker.WorkerReleaseChannel {
	case ChannelPromoted:
		if *worker.WorkerReleaseRevisionID != policy.PromotedRevisionID {
			return inactiveWorkerRelease()
		}
	case ChannelCanary:
		if policy.CanaryRevisionID == nil || policy.CanaryPercent <= 0 ||
			*worker.WorkerReleaseRevisionID != *policy.CanaryRevisionID {
			return inactiveWorkerRelease()
		}
	default:
		return inactiveWorkerRelease()
	}
	return nil
}

func FilterClaimQuery(query *gorm.DB, worker persistence.WorkerInstance) *gorm.DB {
	if worker.WorkerReleaseStatus == "active" && worker.WorkerReleaseRevisionID != nil && worker.WorkerReleaseChannel != nil {
		return query.Where(
			"agent_executions.worker_release_revision_id = ? AND agent_executions.worker_release_channel = ?",
			*worker.WorkerReleaseRevisionID, *worker.WorkerReleaseChannel,
		)
	}
	return query.Where(
		"agent_executions.worker_release_revision_id IS NULL AND agent_executions.worker_release_channel IS NULL",
	)
}

func WorkerMatchesExecution(worker persistence.WorkerInstance, execution persistence.AgentExecution) bool {
	if execution.WorkerReleaseRevisionID == nil || execution.WorkerReleaseChannel == nil {
		return worker.WorkerReleaseStatus == "" || worker.WorkerReleaseStatus == "unmanaged"
	}
	return worker.WorkerReleaseStatus == "active" && worker.WorkerReleaseRevisionID != nil &&
		worker.WorkerReleaseChannel != nil && *worker.WorkerReleaseRevisionID == *execution.WorkerReleaseRevisionID &&
		*worker.WorkerReleaseChannel == *execution.WorkerReleaseChannel
}

func updateWorkerReleaseState(
	ctx context.Context,
	tx *gorm.DB,
	worker *persistence.WorkerInstance,
	revisionID *uuid.UUID,
	channel *string,
	status string,
	reason *string,
	checkedAt *time.Time,
) error {
	updates := map[string]any{
		"worker_release_revision_id": revisionID,
		"worker_release_channel":     channel,
		"worker_release_status":      status,
		"worker_release_reason":      reason,
		"worker_release_checked_at":  checkedAt,
	}
	result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where("id = ?", worker.ID).Updates(updates)
	if result.Error != nil {
		return problem.Wrap(500, "worker_release_state_update_failed", "Failed to update the Worker release state.", result.Error)
	}
	if result.RowsAffected != 1 {
		return problem.New(409, "worker_release_state_conflict", "The Worker release state changed concurrently.")
	}
	worker.WorkerReleaseRevisionID = revisionID
	worker.WorkerReleaseChannel = channel
	worker.WorkerReleaseStatus = status
	worker.WorkerReleaseReason = reason
	worker.WorkerReleaseCheckedAt = checkedAt
	return nil
}

func releaseBucket(executionID uuid.UUID) int {
	digest := sha256.Sum256([]byte(strings.ToLower(executionID.String())))
	return int(binary.BigEndian.Uint16(digest[:2]) % 100)
}

func inactiveWorkerRelease() error {
	return problem.New(
		409,
		"worker_release_inactive",
		"Worker manifest is not in the active promoted or canary release pool.",
	)
}
