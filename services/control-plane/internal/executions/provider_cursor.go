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
) error {
	if cursor == nil || strings.TrimSpace(*cursor) == "" {
		return nil
	}
	if len(*cursor) > providerCursorMaximumBytes {
		return problem.New(400, "provider_cursor_too_large", "providerResumeCursor must not exceed 1000000 characters.")
	}
	binding, available := providerCursorBindingFromExecution(execution)
	if !available || s.cursorCipher == nil {
		return nil
	}
	session, err := lockProviderCursorSession(ctx, tx, execution)
	if err != nil {
		return err
	}
	if len(session.ProviderResumeCursorEncrypted) > 0 {
		plain, status, openErr := s.cursorCipher.OpenV2(
			session.ProviderResumeCursorEncrypted,
			binding.Version,
			binding.Digest,
		)
		if openErr != nil {
			return nil
		}
		switch status {
		case secret.CursorOpenAuthenticationFailed, secret.CursorOpenUnsupportedEnvelope, secret.CursorOpenCipherUnavailable:
			return nil
		case secret.CursorOpenValid:
			var previous providerCursorPayloadV1
			if json.Unmarshal(plain, &previous) != nil ||
				previous.AuthoritativeHistorySequence > session.LastEventSequence {
				return nil
			}
		}
	}
	payload, err := json.Marshal(providerCursorPayloadV1{
		Cursor: *cursor, SourceExecutionID: execution.ID, SourceGeneration: execution.Generation,
		AuthoritativeHistorySequence: session.LastEventSequence, IssuedAt: s.now(),
	})
	if err != nil {
		return nil
	}
	encrypted, err := s.cursorCipher.SealV2(payload, binding.Version, binding.Digest)
	if err != nil {
		return nil
	}
	if err := replaceProviderCursorCAS(ctx, tx, execution, session.ProviderResumeCursorEncrypted, encrypted); err != nil {
		return err
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
	binding, available := providerCursorBindingFromExecution(execution)
	if !available {
		return nil, nil
	}
	session, err := lockProviderCursorSession(ctx, tx, execution)
	if err != nil {
		return nil, err
	}
	if len(session.ProviderResumeCursorEncrypted) == 0 || s.cursorCipher == nil {
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
		if err := clearProviderCursorCAS(ctx, tx, execution, session.ProviderResumeCursorEncrypted); err != nil {
			return nil, err
		}
		return nil, nil
	case secret.CursorOpenValid:
		var payload providerCursorPayloadV1
		if json.Unmarshal(plain, &payload) != nil || strings.TrimSpace(payload.Cursor) == "" ||
			len(payload.Cursor) > providerCursorMaximumBytes || payload.SourceExecutionID == uuid.Nil ||
			payload.SourceGeneration <= 0 || payload.AuthoritativeHistorySequence < 0 ||
			payload.AuthoritativeHistorySequence > session.LastEventSequence || payload.IssuedAt.IsZero() {
			return nil, nil
		}
		return &payload.Cursor, nil
	default:
		return nil, nil
	}
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
		Select("id", "tenant_id", "provider_resume_cursor_encrypted", "last_event_sequence").
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
	previous, replacement []byte,
) error {
	query := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID)
	query = providerCursorCiphertextCAS(query, previous)
	result := query.Update("provider_resume_cursor_encrypted", replacement)
	if result.Error != nil {
		return problem.Wrap(500, "provider_cursor_store_failed", "Failed to store the encrypted provider resume cursor.", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil
	}
	return nil
}

func clearProviderCursorCAS(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	previous []byte,
) error {
	query := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID)
	result := providerCursorCiphertextCAS(query, previous).Update("provider_resume_cursor_encrypted", nil)
	if result.Error != nil {
		return problem.Wrap(500, "provider_cursor_discard_failed", "Failed to discard an incompatible provider resume cursor.", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil
	}
	return nil
}

func providerCursorCiphertextCAS(query *gorm.DB, previous []byte) *gorm.DB {
	if len(previous) == 0 {
		return query.Where("provider_resume_cursor_encrypted IS NULL OR length(provider_resume_cursor_encrypted) = 0")
	}
	return query.Where("provider_resume_cursor_encrypted = ?", previous)
}
