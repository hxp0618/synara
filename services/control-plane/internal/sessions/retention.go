package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Service) ArchiveByRetention(
	ctx context.Context,
	tenantID uuid.UUID,
	cutoff, archivedAt time.Time,
	workspaceCleanupAfterDays *int,
	limit int,
) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	var candidates []uuid.UUID
	err := s.db.WithContext(ctx).Model(&persistence.AgentSession{}).
		Select("id").
		Where("tenant_id = ? AND status = ? AND updated_at <= ?", tenantID, "active", cutoff).
		Where("NOT EXISTS (?)", s.db.Model(&persistence.AgentExecution{}).
			Select("1").Where("agent_executions.tenant_id = agent_sessions.tenant_id AND agent_executions.session_id = agent_sessions.id AND agent_executions.status IN ?",
			[]string{"queued", "leased", "running", "waiting-for-approval", "recovering"})).
		Order("updated_at, id").Limit(limit).Scan(&candidates).Error
	if err != nil {
		return 0, problem.Wrap(500, "retention_sessions_load_failed", "Retention could not load eligible Agent Sessions.", err)
	}
	archived := 0
	var failures []error
	for _, sessionID := range candidates {
		var published *persistence.SessionEvent
		err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
			var model persistence.AgentSession
			err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
				Where("tenant_id = ? AND id = ? AND status = ? AND updated_at <= ?", tenantID, sessionID, "active", cutoff).
				Take(&model).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			var activeExecutions int64
			if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
				Where("tenant_id = ? AND session_id = ? AND status IN ?", tenantID, sessionID,
					[]string{"queued", "leased", "running", "waiting-for-approval", "recovering"}).
				Count(&activeExecutions).Error; err != nil {
				return err
			}
			if activeExecutions > 0 {
				return nil
			}
			if err := tx.Model(&model).Updates(map[string]any{
				"status": "archived", "archived_at": archivedAt,
			}).Error; err != nil {
				return err
			}
			if err := scheduleArchivedWorkspaceCleanup(
				ctx, tx, tenantID, sessionID, archivedAt, workspaceCleanupAfterDays, "retention-session-archive",
			); err != nil {
				return err
			}
			event, err := appendEvent(ctx, tx, &model, eventInput{
				EventType: "session.archived", ActorType: "system",
				Payload: map[string]any{"reason": "retention_policy", "cutoff": cutoff},
			})
			if err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: "system",
				Action: "session.archived", ResourceType: "agent_session", ResourceID: &sessionID,
				OrganizationID: &model.OrganizationID,
				RequestID:      "retention-session:" + uuid.NewString(),
				Metadata:       map[string]any{"reason": "retention_policy", "cutoff": cutoff},
			}); err != nil {
				return err
			}
			published = &event
			return nil
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("archive session %s: %w", sessionID, err))
			continue
		}
		if published != nil {
			archived++
			s.events.publish(toEvent(*published))
		}
	}
	return archived, errors.Join(failures...)
}
