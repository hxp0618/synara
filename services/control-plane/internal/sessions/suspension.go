package sessions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	apiidempotency "github.com/synara-ai/synara/services/control-plane/internal/idempotency"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Service) Suspend(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	idempotencyKey, requestID, ipAddress string,
) (Session, bool, error) {
	return s.transitionOperationalStatus(
		ctx, principal, sessionID, "active", "suspended", "suspend", "session.suspended",
		idempotencyKey, requestID, ipAddress,
	)
}

func (s *Service) Resume(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	idempotencyKey, requestID, ipAddress string,
) (Session, bool, error) {
	return s.transitionOperationalStatus(
		ctx, principal, sessionID, "suspended", "active", "resume", "session.resumed",
		idempotencyKey, requestID, ipAddress,
	)
}

func (s *Service) transitionOperationalStatus(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	fromStatus, toStatus string,
	actionName, eventType string,
	idempotencyKey, requestID, ipAddress string,
) (Session, bool, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return Session{}, false, err
	}
	current, _, err := s.authorizedModel(ctx, principal, tenantID, sessionID, authorization.SessionArchive)
	if err != nil {
		return Session{}, false, err
	}
	if current.Visibility == "private" && current.CreatedBy != principal.UserID {
		return Session{}, false, problem.New(404, "session_not_found", "Session not found.")
	}

	var appended persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "session." + actionName, SuccessStatus: 200,
		Request: map[string]any{"sessionId": sessionID, "targetStatus": toStatus},
	}, func(tx *gorm.DB) (Session, error) {
		var locked persistence.AgentSession
		lookupErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, sessionID).Take(&locked).Error
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return Session{}, problem.New(404, "session_not_found", "Session not found.")
		}
		if lookupErr != nil {
			return Session{}, problem.Wrap(500, "session_lock_failed", "Failed to lock the session.", lookupErr)
		}
		if locked.Status == toStatus {
			return toSession(locked), nil
		}
		if locked.Status != fromStatus {
			return Session{}, problem.New(409, "session_state_conflict", "The Session cannot transition from its current state.")
		}
		now := time.Now().UTC()
		updated := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ? AND status = ?", tenantID, sessionID, fromStatus).
			Update("status", toStatus)
		if updated.Error != nil {
			return Session{}, problem.Wrap(500, "session_status_update_failed", "The Session status could not be updated.", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return Session{}, problem.New(409, "session_state_conflict", "The Session state changed concurrently.")
		}
		locked.Status = toStatus
		locked.UpdatedAt = now
		appended, err = appendEvent(ctx, tx, &locked, eventInput{
			EventType: eventType, ActorType: "user", ActorID: &principal.UserID,
			Payload: map[string]any{"status": toStatus}, OccurredAt: now,
		})
		if err != nil {
			return Session{}, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: eventType, ResourceType: "agent_session", ResourceID: &sessionID,
			OrganizationID: &locked.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		}); err != nil {
			return Session{}, err
		}
		return toSession(locked), nil
	})
	if err != nil {
		return Session{}, false, err
	}
	if !result.Replayed && appended.EventID != uuid.Nil {
		s.events.publish(toEvent(appended))
	}
	return result.Value, result.Replayed, nil
}
