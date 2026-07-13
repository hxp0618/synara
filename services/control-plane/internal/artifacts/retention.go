package artifacts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

var checkpointArtifactReferenceStatuses = []string{"pending", "uploading", "ready"}

const expiredUploadRecheckInterval = 24 * time.Hour

func (s *Service) DeleteByRetention(
	ctx context.Context,
	tenantID uuid.UUID,
	cutoff, deletedAt time.Time,
	limit int,
) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	var candidates []persistence.Artifact
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND deleted_at IS NULL AND status IN ?", tenantID,
			[]string{"pending", "ready", "failed", "deleting"}).
		Where("(expires_at IS NOT NULL AND expires_at <= ?) OR COALESCE(ready_at, created_at) <= ?", deletedAt, cutoff).
		Where(`NOT EXISTS (
			SELECT 1
			FROM workspace_checkpoints checkpoint
			WHERE checkpoint.tenant_id = artifacts.tenant_id
			  AND checkpoint.artifact_id = artifacts.id
			  AND checkpoint.status IN ?
		)`, checkpointArtifactReferenceStatuses).
		Order("COALESCE(ready_at, created_at), id").Limit(limit).Find(&candidates).Error
	if err != nil {
		return 0, problem.Wrap(500, "retention_artifacts_load_failed", "Retention could not load eligible Artifacts.", err)
	}
	deleted := 0
	var failures []error
	for _, candidate := range candidates {
		changed, err := s.deleteModel(ctx, candidate, artifactDeleteActor{
			ActorType: "system", RequestID: "retention-artifact:" + uuid.NewString(),
			Metadata: map[string]any{"reason": "retention_policy", "cutoff": cutoff},
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("delete artifact %s: %w", candidate.ID, err))
			continue
		}
		if changed {
			deleted++
		}
	}
	return deleted, errors.Join(failures...)
}

// CleanupExpiredUploads seals expired pending uploads before deleting their objects. A Checkpoint-
// bound upload fails the Checkpoint and releases the logical Workspace from checkpointing in the
// same transaction. Object-store rows retain the temporary key for one bounded grace interval so a
// late presigned PUT is deleted by a final pass; that pass clears the cleanup handle so terminal
// Artifacts do not remain permanent sweep candidates. Ready rows never delete their verified final
// object.
func (s *Service) CleanupExpiredUploads(ctx context.Context, expiredAt time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	var candidates []persistence.Artifact
	if err := s.db.WithContext(ctx).
		Where("status IN ? AND upload_expires_at IS NOT NULL AND upload_expires_at <= ?", []string{"pending", "ready", "failed", "deleted"}, expiredAt).
		Order("upload_expires_at, id").Limit(limit).Find(&candidates).Error; err != nil {
		return 0, problem.Wrap(500, "expired_artifact_uploads_load_failed", "Expired Artifact uploads could not be loaded.", err)
	}
	cleaned := 0
	var failures []error
	for _, candidate := range candidates {
		current, appended, err := s.sealExpiredUpload(ctx, candidate, expiredAt)
		if err != nil {
			failures = append(failures, fmt.Errorf("seal expired Artifact upload %s: %w", candidate.ID, err))
			s.observe("cleanup", 0, err)
			continue
		}
		if appended.EventID != uuid.Nil {
			s.sessions.PublishInternalEvent(appended)
		}
		if current.ID == uuid.Nil || current.UploadExpiresAt == nil {
			continue
		}
		if err := s.deleteExpiredUploadObjects(ctx, current); err != nil {
			failures = append(failures, err)
			s.observe("cleanup", 0, err)
			continue
		}
		updates := map[string]any{"upload_token_hash": nil}
		if current.UploadObjectKey == nil || current.UploadCleanupAt != nil {
			updates["upload_object_key"] = nil
			updates["upload_expires_at"] = nil
			updates["upload_cleanup_at"] = nil
		} else {
			cleanupAt := s.now()
			nextCleanupAt := cleanupAt.Add(expiredUploadRecheckInterval)
			if !nextCleanupAt.After(expiredAt) {
				nextCleanupAt = expiredAt.Add(expiredUploadRecheckInterval)
			}
			updates["upload_cleanup_at"] = cleanupAt
			updates["upload_expires_at"] = nextCleanupAt
		}
		cleanupUpdate := s.db.WithContext(ctx).Model(&persistence.Artifact{}).
			Where("id = ? AND tenant_id = ? AND status = ? AND upload_expires_at IS NOT NULL AND upload_expires_at <= ?",
				current.ID, current.TenantID, current.Status, expiredAt)
		if current.UploadCleanupAt == nil {
			cleanupUpdate = cleanupUpdate.Where("upload_cleanup_at IS NULL")
		} else {
			cleanupUpdate = cleanupUpdate.Where("upload_cleanup_at = ?", *current.UploadCleanupAt)
		}
		updateColumns := make([]string, 0, len(updates))
		for column := range updates {
			updateColumns = append(updateColumns, column)
		}
		result := cleanupUpdate.Select(updateColumns).Updates(updates)
		if result.Error != nil {
			failures = append(failures, fmt.Errorf("seal expired Artifact upload %s metadata: %w", current.ID, result.Error))
			s.observe("cleanup", 0, result.Error)
			continue
		}
		if result.RowsAffected > 0 {
			cleaned++
			s.observe("cleanup", 0, nil)
		}
	}
	return cleaned, errors.Join(failures...)
}

func (s *Service) sealExpiredUpload(
	ctx context.Context,
	candidate persistence.Artifact,
	expiredAt time.Time,
) (persistence.Artifact, persistence.SessionEvent, error) {
	var checkpointScope *persistence.WorkspaceCheckpoint
	if candidate.WorkspaceCheckpointID != nil {
		var checkpoint persistence.WorkspaceCheckpoint
		if err := s.db.WithContext(ctx).
			Where("tenant_id = ? AND id = ?", candidate.TenantID, *candidate.WorkspaceCheckpointID).
			Take(&checkpoint).Error; err != nil {
			return persistence.Artifact{}, persistence.SessionEvent{}, err
		}
		checkpointScope = &checkpoint
	}
	var current persistence.Artifact
	var appended persistence.SessionEvent
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if checkpointScope != nil {
			var lease persistence.WorkerLease
			leaseErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("tenant_id = ? AND execution_id = ? AND generation = ?",
					checkpointScope.TenantID, checkpointScope.ExecutionID, checkpointScope.Generation).
				Take(&lease).Error
			if leaseErr != nil && !errors.Is(leaseErr, gorm.ErrRecordNotFound) {
				return leaseErr
			}
			var execution persistence.AgentExecution
			if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Select("id").Where("tenant_id = ? AND id = ?", checkpointScope.TenantID, checkpointScope.ExecutionID).
				Take(&execution).Error; err != nil {
				return err
			}
			var workspace persistence.RemoteWorkspace
			if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Select("id").Where("tenant_id = ? AND id = ?", checkpointScope.TenantID, checkpointScope.WorkspaceID).
				Take(&workspace).Error; err != nil {
				return err
			}
		}
		var checkpoint persistence.WorkspaceCheckpoint
		if checkpointScope != nil {
			if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("tenant_id = ? AND id = ?", checkpointScope.TenantID, checkpointScope.ID).
				Take(&checkpoint).Error; err != nil {
				return err
			}
		}
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND upload_expires_at IS NOT NULL AND upload_expires_at <= ?", candidate.ID, expiredAt).
			Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if current.Status == "ready" || current.Status == "failed" {
			return nil
		}
		if current.Status != "pending" {
			return nil
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.Artifact{}).
			Where("id = ? AND tenant_id = ? AND status = ? AND upload_expires_at <= ?",
				current.ID, current.TenantID, "pending", expiredAt).
			Updates(map[string]any{
				"status": "failed", "object_version": nil, "upload_token_hash": nil,
			})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected == 0 {
			return nil
		}
		current.Status = "failed"
		current.ObjectVersion = nil
		current.UploadTokenHash = nil
		if current.WorkspaceCheckpointID == nil {
			return nil
		}
		if checkpointScope == nil || checkpoint.ID != *current.WorkspaceCheckpointID || checkpoint.ArtifactID == nil || *checkpoint.ArtifactID != current.ID {
			return problem.New(409, "checkpoint_artifact_binding_changed", "The expired Artifact Checkpoint binding changed concurrently.")
		}
		if checkpoint.Status != "pending" && checkpoint.Status != "uploading" {
			return nil
		}
		const failureCode = "artifact_upload_expired"
		const failureMessage = "The Workspace Checkpoint Artifact upload expired before confirmation."
		checkpointUpdate := tx.WithContext(ctx).Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ? AND status IN ?", checkpoint.TenantID, checkpoint.ID, []string{"pending", "uploading"}).
			Updates(map[string]any{
				"status": "failed", "failure_code": failureCode, "failure_message": failureMessage,
				"failed_at": now, "ready_at": nil,
			})
		if checkpointUpdate.Error != nil || checkpointUpdate.RowsAffected != 1 {
			return problem.Wrap(409, "checkpoint_expiry_conflict", "The expired Workspace Checkpoint changed concurrently.", checkpointUpdate.Error)
		}
		workspaceState := "ready"
		if checkpoint.Strategy != "git-reference" {
			workspaceState = "dirty"
		}
		workspaceUpdate := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND state = ?", checkpoint.TenantID, checkpoint.WorkspaceID, "checkpointing").
			Updates(map[string]any{"state": workspaceState, "last_used_at": now, "updated_at": now})
		if workspaceUpdate.Error != nil {
			return workspaceUpdate.Error
		}
		workerID := current.CreatedByID
		event, appendErr := s.sessions.AppendInternalEvent(ctx, tx, checkpoint.TenantID, checkpoint.SessionID, sessions.InternalEventInput{
			EventType: "checkpoint.failed", ActorType: "system", ExecutionID: &checkpoint.ExecutionID,
			WorkerID: &workerID, Generation: &checkpoint.Generation,
			Payload: map[string]any{
				"turnId": checkpoint.TurnID, "workspaceId": checkpoint.WorkspaceID,
				"checkpointId": checkpoint.ID, "strategy": checkpoint.Strategy,
				"failureCode": failureCode, "failureMessage": failureMessage,
			},
		})
		appended = event
		return appendErr
	})
	return current, appended, err
}

func (s *Service) deleteExpiredUploadObjects(ctx context.Context, artifact persistence.Artifact) error {
	keys := make([]string, 0, 2)
	if artifact.UploadObjectKey != nil {
		keys = append(keys, *artifact.UploadObjectKey)
	}
	if artifact.Status != "ready" {
		keys = append(keys, artifact.ObjectKey)
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if err := s.store.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete expired Artifact upload %s object %q: %w", artifact.ID, key, err)
		}
	}
	return nil
}
