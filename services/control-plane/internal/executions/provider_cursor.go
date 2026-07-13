package executions

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

const providerCursorMaximumBytes = 1_000_000

const (
	providerCursorStateAbsent      = "absent"
	providerCursorStateUsable      = "usable"
	providerCursorStateQuarantined = "quarantined"
)

type providerCursorPayloadV1 struct {
	Cursor                       string    `json:"cursor"`
	SourceExecutionID            uuid.UUID `json:"sourceExecutionId"`
	SourceGeneration             int64     `json:"sourceGeneration"`
	AuthoritativeHistorySequence int64     `json:"authoritativeHistorySequence"`
	IssuedAt                     time.Time `json:"issuedAt"`
}

type providerCursorExecutionBinding struct {
	Version byte
	Digest  [32]byte
}

func (s *Service) storeProviderCursor(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	cursor *string,
	freshRequired bool,
) error {
	if cursor == nil || strings.TrimSpace(*cursor) == "" {
		if !freshRequired {
			return nil
		}
		session, err := lockProviderCursorSession(ctx, tx, execution)
		if err != nil {
			return err
		}
		return s.quarantineProviderCursor(ctx, tx, execution, session)
	}
	if len(*cursor) > providerCursorMaximumBytes {
		return problem.New(400, "provider_cursor_too_large", "providerResumeCursor must not exceed 1000000 characters.")
	}
	session, err := lockProviderCursorSession(ctx, tx, execution)
	if err != nil {
		return err
	}
	binding, available := providerCursorBindingFromExecution(execution)
	if !available || s.cursorCipher == nil {
		return s.quarantineProviderCursor(ctx, tx, execution, session)
	}
	if len(session.ProviderResumeCursorEncrypted) > 0 && session.ProviderResumeCursorState == providerCursorStateUsable {
		plain, status, openErr := s.cursorCipher.OpenV2(
			session.ProviderResumeCursorEncrypted,
			binding.Version,
			binding.Digest,
		)
		if openErr == nil && status == secret.CursorOpenValid {
			var previous providerCursorPayloadV1
			if json.Unmarshal(plain, &previous) == nil &&
				previous.AuthoritativeHistorySequence > session.LastEventSequence {
				return nil
			}
		}
	}
	payloadModel := providerCursorPayloadV1{
		Cursor: *cursor, SourceExecutionID: execution.ID, SourceGeneration: execution.Generation,
		AuthoritativeHistorySequence: session.LastEventSequence, IssuedAt: s.now(),
	}
	payload, err := json.Marshal(payloadModel)
	if err != nil {
		return nil
	}
	encrypted, err := s.cursorCipher.SealV2(payload, binding.Version, binding.Digest)
	if err != nil {
		return nil
	}
	replaced, err := replaceProviderCursorCAS(ctx, tx, execution, session, encrypted, payloadModel)
	if err != nil {
		return err
	}
	if !replaced {
		return nil
	}
	if execution.ProviderRuntimeBindingID != nil {
		now := s.now()
		updates := map[string]any{
			"cursor_updated_at": now, "cursor_compatibility_key": hex.EncodeToString(binding.Digest[:]),
			"last_execution_id": execution.ID, "last_generation": execution.Generation,
			"resume_strategy": "native-cursor", "updated_at": now,
		}
		if err := tx.WithContext(ctx).Model(&persistence.ProviderRuntimeBinding{}).
			Where("tenant_id = ? AND id = ? AND session_id = ?", execution.TenantID, *execution.ProviderRuntimeBindingID, execution.SessionID).
			Updates(updates).Error; err != nil {
			return problem.Wrap(500, "runtime_binding_cursor_store_failed", "Failed to update Provider Cursor compatibility metadata.", err)
		}
	}
	return nil
}

func (s *Service) loadProviderCursor(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (*string, error) {
	session, err := lockProviderCursorSession(ctx, tx, execution)
	if err != nil {
		return nil, err
	}
	if len(session.ProviderResumeCursorEncrypted) == 0 || session.ProviderResumeCursorState == providerCursorStateAbsent {
		return nil, nil
	}
	if session.ProviderResumeCursorState != providerCursorStateUsable {
		return nil, nil
	}
	binding, available := providerCursorBindingFromExecution(execution)
	if !available || s.cursorCipher == nil {
		if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
			return nil, err
		}
		return nil, nil
	}
	plain, status, err := s.cursorCipher.OpenV2(
		session.ProviderResumeCursorEncrypted,
		binding.Version,
		binding.Digest,
	)
	if err != nil {
		return nil, nil
	}
	switch status {
	case secret.CursorOpenBindingMismatch:
		if err := s.clearProviderCursor(ctx, tx, execution, session); err != nil {
			return nil, err
		}
		return nil, nil
	case secret.CursorOpenValid:
		var payload providerCursorPayloadV1
		if json.Unmarshal(plain, &payload) != nil || strings.TrimSpace(payload.Cursor) == "" ||
			len(payload.Cursor) > providerCursorMaximumBytes || payload.SourceExecutionID == uuid.Nil ||
			payload.SourceGeneration <= 0 || payload.AuthoritativeHistorySequence < 0 ||
			payload.AuthoritativeHistorySequence > session.LastEventSequence || payload.IssuedAt.IsZero() ||
			!providerCursorPayloadMatchesSession(payload, session) {
			if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return &payload.Cursor, nil
	default:
		if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

func providerCursorPayloadMatchesSession(payload providerCursorPayloadV1, session persistence.AgentSession) bool {
	return session.ProviderResumeCursorSourceExecutionID != nil &&
		*session.ProviderResumeCursorSourceExecutionID == payload.SourceExecutionID &&
		session.ProviderResumeCursorSourceGeneration != nil &&
		*session.ProviderResumeCursorSourceGeneration == payload.SourceGeneration &&
		session.ProviderResumeCursorHistorySequence != nil &&
		*session.ProviderResumeCursorHistorySequence == payload.AuthoritativeHistorySequence
}

func providerCursorBindingFromExecution(execution persistence.AgentExecution) (providerCursorExecutionBinding, bool) {
	if execution.ProviderResumeStrategySnapshot != "native-cursor" || execution.ProviderCursorBindingVersion == nil ||
		*execution.ProviderCursorBindingVersion != providerCursorBindingVersion || len(execution.ProviderCursorBindingDigest) != 32 {
		return providerCursorExecutionBinding{}, false
	}
	var digest [32]byte
	copy(digest[:], execution.ProviderCursorBindingDigest)
	return providerCursorExecutionBinding{Version: byte(*execution.ProviderCursorBindingVersion), Digest: digest}, true
}

func lockProviderCursorSession(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (persistence.AgentSession, error) {
	var session persistence.AgentSession
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Select(
			"id", "tenant_id", "provider_resume_cursor_encrypted", "provider_resume_cursor_state",
			"provider_resume_cursor_source_execution_id", "provider_resume_cursor_source_generation",
			"provider_resume_cursor_history_sequence", "last_event_sequence",
		).
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
		Take(&session).Error; err != nil {
		return persistence.AgentSession{}, problem.Wrap(500, "provider_cursor_load_failed", "Failed to lock the provider resume cursor.", err)
	}
	return session, nil
}

func replaceProviderCursorCAS(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	session persistence.AgentSession,
	replacement []byte,
	payload providerCursorPayloadV1,
) (bool, error) {
	query := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID)
	query = providerCursorSessionCAS(query, session)
	result := query.Updates(map[string]any{
		"provider_resume_cursor_encrypted":           replacement,
		"provider_resume_cursor_state":               providerCursorStateUsable,
		"provider_resume_cursor_source_execution_id": payload.SourceExecutionID,
		"provider_resume_cursor_source_generation":   payload.SourceGeneration,
		"provider_resume_cursor_history_sequence":    payload.AuthoritativeHistorySequence,
	})
	if result.Error != nil {
		return false, problem.Wrap(500, "provider_cursor_store_failed", "Failed to store the encrypted provider resume cursor.", result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (s *Service) quarantineProviderCursor(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	session persistence.AgentSession,
) error {
	if len(session.ProviderResumeCursorEncrypted) == 0 || session.ProviderResumeCursorState == providerCursorStateAbsent {
		return nil
	}
	if session.ProviderResumeCursorState != providerCursorStateQuarantined {
		query := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID)
		result := providerCursorSessionCAS(query, session).Update(
			"provider_resume_cursor_state", providerCursorStateQuarantined,
		)
		if result.Error != nil {
			return problem.Wrap(500, "provider_cursor_quarantine_failed", "Failed to quarantine an unusable provider resume cursor.", result.Error)
		}
		if result.RowsAffected == 0 {
			return nil
		}
	}
	return s.markProviderRuntimeBindingCursorUnavailable(ctx, tx, execution)
}

func (s *Service) clearProviderCursor(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	session persistence.AgentSession,
) error {
	query := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID)
	result := providerCursorSessionCAS(query, session).Updates(map[string]any{
		"provider_resume_cursor_encrypted":           nil,
		"provider_resume_cursor_state":               providerCursorStateAbsent,
		"provider_resume_cursor_source_execution_id": nil,
		"provider_resume_cursor_source_generation":   nil,
		"provider_resume_cursor_history_sequence":    nil,
	})
	if result.Error != nil {
		return problem.Wrap(500, "provider_cursor_discard_failed", "Failed to discard an incompatible provider resume cursor.", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil
	}
	return s.markProviderRuntimeBindingCursorUnavailable(ctx, tx, execution)
}

func providerCursorSessionCAS(query *gorm.DB, session persistence.AgentSession) *gorm.DB {
	query = query.Where("provider_resume_cursor_state = ?", session.ProviderResumeCursorState)
	if len(session.ProviderResumeCursorEncrypted) == 0 {
		return query.Where("provider_resume_cursor_encrypted IS NULL OR length(provider_resume_cursor_encrypted) = 0")
	}
	return query.Where("provider_resume_cursor_encrypted = ?", session.ProviderResumeCursorEncrypted)
}

func (s *Service) markProviderRuntimeBindingCursorUnavailable(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) error {
	if execution.ProviderRuntimeBindingID == nil {
		return nil
	}
	now := s.now()
	if err := tx.WithContext(ctx).Model(&persistence.ProviderRuntimeBinding{}).
		Where("tenant_id = ? AND id = ? AND session_id = ?", execution.TenantID, *execution.ProviderRuntimeBindingID, execution.SessionID).
		Updates(map[string]any{
			"resume_strategy": "authoritative-history", "cursor_compatibility_key": nil,
			"cursor_updated_at": nil, "updated_at": now,
		}).Error; err != nil {
		return problem.Wrap(500, "runtime_binding_cursor_quarantine_failed", "Failed to quarantine Provider Cursor compatibility metadata.", err)
	}
	return nil
}
