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
