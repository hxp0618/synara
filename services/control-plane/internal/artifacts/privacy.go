package artifacts

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type PrivacyDeletionResult struct {
	Deleted  int `json:"deleted"`
	Retained int `json:"retained"`
}

// DeleteByPrivacyRequest deletes user-created Artifact payloads while keeping
// immutable Memory and active Checkpoint evidence explicit in the result.
func (s *Service) DeleteByPrivacyRequest(
	ctx context.Context,
	tenantID, subjectUserID, processorUserID, privacyRequestID uuid.UUID,
	limit int,
) (PrivacyDeletionResult, error) {
	if limit <= 0 {
		limit = 1000
	}
	var candidates []persistence.Artifact
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND created_by_type = ? AND created_by_id = ? AND deleted_at IS NULL",
			tenantID, "user", subjectUserID).
		Order("created_at, id").Limit(limit + 1).Find(&candidates).Error; err != nil {
		return PrivacyDeletionResult{}, problem.Wrap(500, "privacy_artifacts_load_failed", "Privacy erasure could not load user Artifacts.", err)
	}
	if len(candidates) > limit {
		return PrivacyDeletionResult{}, problem.New(409, "privacy_artifact_batch_too_large", "Privacy erasure has more Artifacts than the bounded execution limit.")
	}
	result := PrivacyDeletionResult{}
	for _, candidate := range candidates {
		changed, err := s.deleteModel(ctx, candidate, artifactDeleteActor{
			ActorType: "user", ActorID: &processorUserID,
			RequestID: "privacy-erasure:" + privacyRequestID.String(),
			Metadata: map[string]any{
				"reason": "privacy_erasure", "privacyRequestId": privacyRequestID,
				"subjectUserId": subjectUserID,
			},
		})
		if err != nil {
			var apiError *problem.Error
			if errors.As(err, &apiError) && (apiError.Code == "artifact_checkpoint_referenced" ||
				apiError.Code == "artifact_memory_referenced") {
				result.Retained++
				continue
			}
			return result, fmt.Errorf("delete privacy Artifact %s: %w", candidate.ID, err)
		}
		if changed {
			result.Deleted++
		}
	}
	return result, nil
}
