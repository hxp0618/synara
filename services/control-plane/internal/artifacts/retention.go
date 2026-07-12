package artifacts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

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

// CleanupExpiredUploads removes expired temporary upload keys. Pending rows also lose any final
// key left by a process crash between object promotion and metadata commit; ready rows retain their
// verified final key. The caller coordinates the sweep across replicas, and every metadata update
// remains conditional so retries are idempotent.
func (s *Service) CleanupExpiredUploads(ctx context.Context, expiredAt time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	var candidates []persistence.Artifact
	if err := s.db.WithContext(ctx).
		Where("status IN ? AND upload_expires_at IS NOT NULL AND upload_expires_at <= ?", []string{"pending", "ready"}, expiredAt).
		Order("upload_expires_at, id").Limit(limit).Find(&candidates).Error; err != nil {
		return 0, problem.Wrap(500, "expired_artifact_uploads_load_failed", "Expired Artifact uploads could not be loaded.", err)
	}
	cleaned := 0
	var failures []error
	for _, candidate := range candidates {
		uploadKey := candidate.ObjectKey
		if candidate.UploadObjectKey != nil {
			uploadKey = *candidate.UploadObjectKey
		}
		keys := []string{uploadKey}
		if candidate.Status == "pending" && uploadKey != candidate.ObjectKey {
			keys = append(keys, candidate.ObjectKey)
		}
		failed := false
		for _, key := range keys {
			if err := s.store.Delete(ctx, key); err != nil {
				failures = append(failures, fmt.Errorf("delete expired Artifact upload %s object %q: %w", candidate.ID, key, err))
				s.observe("cleanup", 0, err)
				failed = true
				break
			}
		}
		if failed {
			continue
		}
		updates := map[string]any{
			"upload_token_hash": nil, "upload_expires_at": nil, "upload_object_key": nil,
		}
		if candidate.Status == "pending" {
			updates["status"] = "failed"
			updates["object_version"] = nil
		}
		result := s.db.WithContext(ctx).Model(&persistence.Artifact{}).
			Where("id = ? AND tenant_id = ? AND status = ? AND upload_expires_at <= ?", candidate.ID, candidate.TenantID, candidate.Status, expiredAt).
			Updates(updates)
		if result.Error != nil {
			failures = append(failures, fmt.Errorf("mark expired Artifact upload %s failed: %w", candidate.ID, result.Error))
			s.observe("cleanup", 0, result.Error)
			continue
		}
		if result.RowsAffected == 1 {
			cleaned++
			s.observe("cleanup", 0, nil)
		}
	}
	return cleaned, errors.Join(failures...)
}
