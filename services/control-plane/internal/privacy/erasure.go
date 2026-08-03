package privacy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/retentiongate"
)

type ErasureResult struct {
	Request Request        `json:"request"`
	Summary ErasureSummary `json:"summary"`
}

type ErasureSummary struct {
	LoginSessionsRevoked      int64 `json:"loginSessionsRevoked"`
	OrganizationMemberships   int64 `json:"organizationMembershipsRemoved"`
	CredentialsRevoked        int64 `json:"credentialsRevoked"`
	SessionsRedacted          int64 `json:"sessionsRedacted"`
	TurnsRedacted             int64 `json:"turnsRedacted"`
	EventsRedacted            int64 `json:"eventsRedacted"`
	ArtifactsDeleted          int   `json:"artifactsDeleted"`
	ArtifactsRetainedEvidence int   `json:"artifactsRetainedEvidence"`
	MemoryHeadsDisabled       int64 `json:"memoryHeadsDisabled"`
	GlobalUserPseudonymized   bool  `json:"globalUserPseudonymized"`
}

func (s *Service) ExecuteErasure(
	ctx context.Context,
	principal identity.Principal,
	tenantID, privacyRequestID uuid.UUID,
	expectedVersion int64,
	httpRequestID, ipAddress string,
) (ErasureResult, error) {
	role, err := s.requireTenant(ctx, principal, tenantID)
	if err != nil {
		return ErasureResult{}, err
	}
	if !authorization.TenantAllows(role, authorization.RetentionManage) {
		return ErasureResult{}, problem.New(403, "privacy_erasure_forbidden", "Tenant privacy administrator access is required.")
	}
	releaseLocal := retentiongate.AcquireHoldMutation(s.db, tenantID)
	var processing persistence.PrivacyRequest
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := retentiongate.LockHoldMutation(ctx, tx, tenantID); err != nil {
			return problem.Wrap(409, "privacy_erasure_scope_conflict", "Privacy erasure could not lock the Tenant governance scope.", err)
		}
		var current persistence.PrivacyRequest
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, privacyRequestID).Take(&current).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return problem.New(404, "privacy_request_not_found", "Privacy Request not found.")
		}
		if loadErr != nil {
			return problem.Wrap(500, "privacy_request_load_failed", "Privacy Request could not be loaded.", loadErr)
		}
		if current.RequestType != "erasure" {
			return problem.New(409, "privacy_request_type_mismatch", "Only an erasure Privacy Request can execute erasure.")
		}
		if current.Status != "approved" || current.Version != expectedVersion {
			return problem.New(409, "privacy_request_version_conflict", "Privacy Request must be approved at the expected version.")
		}
		if err := validateErasurePreconditions(ctx, tx, current); err != nil {
			return err
		}
		processing, err = transitionLocked(ctx, tx, tenantID, privacyRequestID, expectedVersion,
			"approved", "processing", principal.UserID,
			"Execute the approved Tenant-scoped user erasure after governance preflight.",
			map[string]any{}, nil)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "privacy_request.transitioned", ResourceType: "privacy_request", ResourceID: &privacyRequestID,
			RequestID: httpRequestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"subjectUserId": current.SubjectUserID, "requestType": "erasure",
				"fromStatus": "approved", "toStatus": "processing", "version": processing.Version,
			},
		})
	})
	releaseLocal()
	if err != nil {
		return ErasureResult{}, err
	}

	artifactResult := artifacts.PrivacyDeletionResult{}
	if s.artifacts != nil {
		artifactResult, err = s.artifacts.DeleteByPrivacyRequest(
			ctx, tenantID, processing.SubjectUserID, principal.UserID, processing.ID, 1000,
		)
		if err != nil {
			s.failProcessing(ctx, processing, principal.UserID,
				"Privacy erasure could not complete Artifact deletion and is safe to retry.", httpRequestID, ipAddress)
			return ErasureResult{}, problem.Wrap(503, "privacy_erasure_artifacts_failed", "Privacy erasure could not delete user Artifacts.", err)
		}
	}
	summary, err := s.redactTenantUser(ctx, processing, principal.UserID, artifactResult, httpRequestID, ipAddress)
	if err != nil {
		s.failProcessing(ctx, processing, principal.UserID,
			"Privacy erasure database redaction failed and requires an administrator retry.", httpRequestID, ipAddress)
		return ErasureResult{}, err
	}
	summaryMap, encoded, err := erasureSummaryMap(summary)
	if err != nil {
		s.failProcessing(ctx, processing, principal.UserID,
			"Privacy erasure result serialization failed and requires an administrator retry.", httpRequestID, ipAddress)
		return ErasureResult{}, err
	}
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	completed, err := s.transition(ctx, tenantID, privacyRequestID, processing.Version, "processing", "completed",
		principal.UserID, "The approved Tenant-scoped user erasure completed with retained evidence recorded.",
		summaryMap, &digest, httpRequestID, ipAddress)
	if err != nil {
		return ErasureResult{}, err
	}
	return ErasureResult{Request: toRequest(completed), Summary: summary}, nil
}

func validateErasurePreconditions(
	ctx context.Context,
	tx *gorm.DB,
	request persistence.PrivacyRequest,
) error {
	var holdCount int64
	if err := tx.WithContext(ctx).Table("legal_holds AS legal_hold").
		Where("legal_hold.tenant_id = ? AND legal_hold.status = ?", request.TenantID, "active").
		Where(`legal_hold.scope_type = 'tenant'
			OR (legal_hold.scope_type = 'user' AND legal_hold.scope_id = ?)
			OR (legal_hold.scope_type IN ('organization', 'project', 'session') AND EXISTS (
				SELECT 1 FROM agent_sessions AS held_session
				WHERE held_session.tenant_id = legal_hold.tenant_id
				  AND (held_session.created_by = ? OR EXISTS (
					SELECT 1 FROM agent_turns AS held_turn
					WHERE held_turn.tenant_id = held_session.tenant_id
					  AND held_turn.session_id = held_session.id
					  AND held_turn.created_by = ?
				  ))
				  AND (
					(legal_hold.scope_type = 'organization' AND legal_hold.scope_id = held_session.organization_id)
					OR (legal_hold.scope_type = 'project' AND legal_hold.scope_id = held_session.project_id)
					OR (legal_hold.scope_type = 'session' AND legal_hold.scope_id = held_session.id)
				  )
			))`, request.SubjectUserID, request.SubjectUserID, request.SubjectUserID).
		Count(&holdCount).Error; err != nil {
		return problem.Wrap(500, "privacy_erasure_legal_hold_check_failed", "Privacy erasure could not inspect Legal Holds.", err)
	}
	if holdCount != 0 {
		return problem.New(409, "privacy_erasure_legal_hold_active", "Privacy erasure is blocked by an active Legal Hold.")
	}
	var activeExecutions int64
	if err := tx.WithContext(ctx).Table("agent_executions AS execution").
		Joins("JOIN agent_sessions AS session ON session.tenant_id = execution.tenant_id AND session.id = execution.session_id").
		Joins("JOIN agent_turns AS turn ON turn.tenant_id = execution.tenant_id AND turn.id = execution.turn_id").
		Where("execution.tenant_id = ? AND execution.status IN ?", request.TenantID,
			[]string{"queued", "recovering", "leased", "running", "waiting-for-approval", "suspended"}).
		Where("execution.requested_by = ? OR session.created_by = ? OR turn.created_by = ?",
			request.SubjectUserID, request.SubjectUserID, request.SubjectUserID).
		Count(&activeExecutions).Error; err != nil {
		return problem.Wrap(500, "privacy_erasure_execution_check_failed", "Privacy erasure could not inspect active Executions.", err)
	}
	if activeExecutions != 0 {
		return problem.New(409, "privacy_erasure_active_executions", "Privacy erasure requires all subject Executions to be terminal.")
	}
	var membership persistence.TenantMembership
	if err := tx.WithContext(ctx).Where("tenant_id = ? AND user_id = ?", request.TenantID, request.SubjectUserID).
		Take(&membership).Error; err != nil {
		return problem.Wrap(409, "privacy_erasure_membership_missing", "Privacy erasure requires an existing Tenant membership.", err)
	}
	if membership.Role == "owner" && membership.Status == "active" {
		var ownerCount int64
		if err := tx.WithContext(ctx).Model(&persistence.TenantMembership{}).
			Where("tenant_id = ? AND role = ? AND status = ?", request.TenantID, "owner", "active").
			Count(&ownerCount).Error; err != nil {
			return problem.Wrap(500, "privacy_erasure_owner_check_failed", "Privacy erasure could not inspect Tenant owners.", err)
		}
		if ownerCount <= 1 {
			return problem.New(409, "privacy_erasure_last_owner", "Transfer Tenant ownership before erasing the last active owner.")
		}
	}
	return nil
}

func (s *Service) redactTenantUser(
	ctx context.Context,
	request persistence.PrivacyRequest,
	processorUserID uuid.UUID,
	artifactResult artifacts.PrivacyDeletionResult,
	httpRequestID, ipAddress string,
) (ErasureSummary, error) {
	summary := ErasureSummary{
		ArtifactsDeleted: artifactResult.Deleted, ArtifactsRetainedEvidence: artifactResult.Retained,
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := validateErasurePreconditions(ctx, tx, request); err != nil {
			return err
		}
		now := s.now()
		var privateSessionIDs []uuid.UUID
		if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).Select("id").
			Where("tenant_id = ? AND created_by = ? AND visibility = ?", request.TenantID, request.SubjectUserID, "private").
			Scan(&privateSessionIDs).Error; err != nil {
			return err
		}
		var events []persistence.SessionEvent
		eventQuery := tx.WithContext(ctx).Where("tenant_id = ? AND actor_id = ?", request.TenantID, request.SubjectUserID)
		if len(privateSessionIDs) > 0 {
			eventQuery = tx.WithContext(ctx).Where("tenant_id = ? AND (actor_id = ? OR session_id IN ?)",
				request.TenantID, request.SubjectUserID, privateSessionIDs)
		}
		if err := eventQuery.Order("session_id, sequence").Find(&events).Error; err != nil {
			return err
		}
		for _, event := range events {
			payload := map[string]any{
				"redacted": true, "privacyRequestId": request.ID.String(), "eventType": event.EventType,
			}
			updated := tx.WithContext(ctx).Model(&persistence.SessionEvent{}).
				Where("tenant_id = ? AND session_id = ? AND sequence = ?", event.TenantID, event.SessionID, event.Sequence).
				Select("payload").Updates(&persistence.SessionEvent{Payload: payload})
			if updated.Error != nil {
				return updated.Error
			}
			summary.EventsRedacted += updated.RowsAffected
		}
		redaction := "[redacted by Privacy Request " + request.ID.String() + "]"
		turns := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND created_by = ?", request.TenantID, request.SubjectUserID).
			Update("input_text", redaction)
		if turns.Error != nil {
			return turns.Error
		}
		summary.TurnsRedacted = turns.RowsAffected
		sessions := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND created_by = ?", request.TenantID, request.SubjectUserID).
			Updates(map[string]any{
				"title": "Deleted user Session", "provider_resume_cursor_encrypted": nil,
				"provider_resume_cursor_key_id": nil,
				"provider_resume_cursor_state":  "absent", "provider_resume_cursor_source_execution_id": nil,
				"provider_resume_cursor_source_generation": nil, "provider_resume_cursor_history_sequence": nil,
				"updated_at": now,
			})
		if sessions.Error != nil {
			return sessions.Error
		}
		summary.SessionsRedacted = sessions.RowsAffected
		if len(privateSessionIDs) > 0 {
			if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).Where("tenant_id = ? AND id IN ?", request.TenantID, privateSessionIDs).
				Updates(map[string]any{"status": "archived", "archived_at": now, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		memories := tx.WithContext(ctx).Model(&persistence.AgentMemoryHead{}).
			Where("tenant_id = ? AND scope_type = ? AND scope_user_id = ? AND enabled = ?",
				request.TenantID, "user", request.SubjectUserID, true).
			Updates(map[string]any{"enabled": false, "updated_by": processorUserID, "updated_at": now})
		if memories.Error != nil {
			return memories.Error
		}
		summary.MemoryHeadsDisabled = memories.RowsAffected
		credentials := tx.WithContext(ctx).Model(&persistence.ProviderCredential{}).
			Where("tenant_id = ? AND scope = ? AND scope_user_id = ? AND revoked_at IS NULL",
				request.TenantID, "user", request.SubjectUserID).
			Updates(map[string]any{
				"revoked_at": now, "revoked_by": processorUserID, "updated_by": processorUserID,
				"auto_select_enabled": false,
			})
		if credentials.Error != nil {
			return credentials.Error
		}
		summary.CredentialsRevoked = credentials.RowsAffected
		sessionsRevoked := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
			Where("user_id = ? AND active_tenant_id = ? AND revoked_at IS NULL", request.SubjectUserID, request.TenantID).
			Update("revoked_at", now)
		if sessionsRevoked.Error != nil {
			return sessionsRevoked.Error
		}
		summary.LoginSessionsRevoked = sessionsRevoked.RowsAffected
		memberships := tx.WithContext(ctx).Where("tenant_id = ? AND user_id = ?", request.TenantID, request.SubjectUserID).
			Delete(&persistence.OrganizationMembership{})
		if memberships.Error != nil {
			return memberships.Error
		}
		summary.OrganizationMemberships = memberships.RowsAffected
		if err := tx.WithContext(ctx).Model(&persistence.TenantMembership{}).
			Where("tenant_id = ? AND user_id = ?", request.TenantID, request.SubjectUserID).
			Updates(map[string]any{"status": "suspended", "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.WithContext(ctx).Where("tenant_id = ? AND actor_id = ?", request.TenantID, request.SubjectUserID).
			Delete(&persistence.APIIdempotencyKey{}).Error; err != nil {
			return err
		}
		var otherActiveMemberships int64
		if err := tx.WithContext(ctx).Model(&persistence.TenantMembership{}).
			Where("user_id = ? AND tenant_id <> ? AND status = ?", request.SubjectUserID, request.TenantID, "active").
			Count(&otherActiveMemberships).Error; err != nil {
			return err
		}
		if otherActiveMemberships == 0 {
			pseudonymSeed := sha256.Sum256([]byte(request.SubjectUserID.String() + ":" + request.ID.String()))
			pseudonymEmail := "deleted+" + hex.EncodeToString(pseudonymSeed[:6]) + "@redacted.invalid"
			if err := tx.WithContext(ctx).Model(&persistence.User{}).Where("id = ?", request.SubjectUserID).
				Updates(map[string]any{
					"email": pseudonymEmail, "display_name": "Deleted user", "avatar_url": nil,
					"status": "suspended", "deleted_at": now, "updated_at": now,
				}).Error; err != nil {
				return err
			}
			if err := tx.WithContext(ctx).Where("user_id = ?", request.SubjectUserID).
				Delete(&persistence.UserIdentity{}).Error; err != nil {
				return err
			}
			if err := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
				Where("user_id = ? AND revoked_at IS NULL", request.SubjectUserID).
				Update("revoked_at", now).Error; err != nil {
				return err
			}
			summary.GlobalUserPseudonymized = true
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: request.TenantID, ActorType: "user", ActorID: &processorUserID,
			Action: "privacy_request.erasure_applied", ResourceType: "privacy_request", ResourceID: &request.ID,
			RequestID: httpRequestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"subjectUserId": request.SubjectUserID, "retainedImmutableEvidence": artifactResult.Retained,
				"globalUserPseudonymized": summary.GlobalUserPseudonymized,
			},
		})
	})
	if err != nil {
		return ErasureSummary{}, problem.Wrap(500, "privacy_erasure_apply_failed", "Privacy erasure could not apply Tenant data redaction.", err)
	}
	return summary, nil
}

func erasureSummaryMap(summary ErasureSummary) (map[string]any, []byte, error) {
	encoded, err := json.Marshal(summary)
	if err != nil {
		return nil, nil, problem.Wrap(500, "privacy_erasure_summary_failed", "Privacy erasure result could not be encoded.", err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, nil, fmt.Errorf("decode Privacy erasure result: %w", err)
	}
	return result, encoded, nil
}
