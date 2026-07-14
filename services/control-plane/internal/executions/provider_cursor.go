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

const (
	providerCursorMaximumBytes      = 1_000_000
	providerCursorMaximumFutureSkew = 5 * time.Minute
)

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

type providerCursorLoadResult struct {
	Cursor                       *string
	SelectedStrategy             string
	ReasonCode                   string
	CursorState                  string
	CursorIssuedAt               *time.Time
	CursorExpiresAt              *time.Time
	CursorSourceExecutionID      *uuid.UUID
	CursorSourceGeneration       *int64
	CursorHistorySequence        *int64
	AuthoritativeHistorySequence int64
	MaximumAge                   time.Duration
}

type providerCursorClaimDecision struct {
	RequestedStrategy            string     `json:"requestedStrategy"`
	SelectedStrategy             string     `json:"selectedStrategy"`
	ReasonCode                   string     `json:"reasonCode"`
	CursorState                  string     `json:"cursorState"`
	CursorIssuedAt               *time.Time `json:"cursorIssuedAt"`
	CursorSourceExecutionID      *uuid.UUID `json:"cursorSourceExecutionId"`
	CursorSourceGeneration       *int64     `json:"cursorSourceGeneration"`
	CursorHistorySequence        *int64     `json:"cursorHistorySequence"`
	AuthoritativeHistorySequence int64      `json:"authoritativeHistorySequence"`
}

func (result providerCursorLoadResult) eventPayload(requestedStrategy string) map[string]any {
	payload := map[string]any{
		"requestedStrategy":            requestedStrategy,
		"selectedStrategy":             result.SelectedStrategy,
		"reasonCode":                   result.ReasonCode,
		"cursorState":                  result.CursorState,
		"authoritativeHistorySequence": result.AuthoritativeHistorySequence,
		"maximumAgeSeconds":            int64(result.MaximumAge / time.Second),
	}
	if result.CursorIssuedAt != nil {
		payload["cursorIssuedAt"] = result.CursorIssuedAt.UTC().Format(time.RFC3339Nano)
	}
	if result.CursorExpiresAt != nil {
		payload["cursorExpiresAt"] = result.CursorExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if result.CursorSourceExecutionID != nil {
		payload["cursorSourceExecutionId"] = result.CursorSourceExecutionID.String()
	}
	if result.CursorSourceGeneration != nil {
		payload["cursorSourceGeneration"] = *result.CursorSourceGeneration
	}
	if result.CursorHistorySequence != nil {
		payload["cursorHistorySequence"] = *result.CursorHistorySequence
	}
	return payload
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
	enforceMaximumAge bool,
	authoritativeHistorySequence int64,
) (providerCursorLoadResult, error) {
	session, err := lockProviderCursorSession(ctx, tx, execution)
	if err != nil {
		return providerCursorLoadResult{}, err
	}
	result := providerCursorLoadResult{
		SelectedStrategy:             "authoritative-history",
		ReasonCode:                   "cursor_absent",
		CursorState:                  session.ProviderResumeCursorState,
		CursorSourceExecutionID:      session.ProviderResumeCursorSourceExecutionID,
		CursorSourceGeneration:       session.ProviderResumeCursorSourceGeneration,
		CursorHistorySequence:        session.ProviderResumeCursorHistorySequence,
		AuthoritativeHistorySequence: authoritativeHistorySequence,
		MaximumAge:                   s.providerCursorMaximumAge,
	}
	if len(session.ProviderResumeCursorEncrypted) == 0 || session.ProviderResumeCursorState == providerCursorStateAbsent {
		result.CursorState = providerCursorStateAbsent
		return result, nil
	}
	if session.ProviderResumeCursorState != providerCursorStateUsable {
		result.ReasonCode = "cursor_quarantined"
		return result, nil
	}
	if execution.ProviderResumeStrategySnapshot != "native-cursor" {
		result.ReasonCode = "resume_strategy_authoritative_history"
		if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
			return providerCursorLoadResult{}, err
		}
		result.CursorState = providerCursorStateQuarantined
		return result, nil
	}
	binding, available := providerCursorBindingFromExecution(execution)
	if !available {
		result.ReasonCode = "cursor_binding_unavailable"
		if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
			return providerCursorLoadResult{}, err
		}
		result.CursorState = providerCursorStateQuarantined
		return result, nil
	}
	if s.cursorCipher == nil {
		result.ReasonCode = "cursor_cipher_unavailable"
		if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
			return providerCursorLoadResult{}, err
		}
		result.CursorState = providerCursorStateQuarantined
		return result, nil
	}
	plain, status, err := s.cursorCipher.OpenV2(
		session.ProviderResumeCursorEncrypted,
		binding.Version,
		binding.Digest,
	)
	if err != nil {
		result.ReasonCode = "cursor_open_failed"
		if quarantineErr := s.quarantineProviderCursor(ctx, tx, execution, session); quarantineErr != nil {
			return providerCursorLoadResult{}, quarantineErr
		}
		result.CursorState = providerCursorStateQuarantined
		return result, nil
	}
	switch status {
	case secret.CursorOpenBindingMismatch:
		result.ReasonCode = "cursor_binding_mismatch"
		if err := s.clearProviderCursor(ctx, tx, execution, session); err != nil {
			return providerCursorLoadResult{}, err
		}
		result.CursorState = providerCursorStateAbsent
		return result, nil
	case secret.CursorOpenValid:
		var payload providerCursorPayloadV1
		if json.Unmarshal(plain, &payload) != nil || strings.TrimSpace(payload.Cursor) == "" ||
			len(payload.Cursor) > providerCursorMaximumBytes || payload.SourceExecutionID == uuid.Nil ||
			payload.SourceGeneration <= 0 || payload.AuthoritativeHistorySequence < 0 ||
			payload.AuthoritativeHistorySequence > session.LastEventSequence || payload.IssuedAt.IsZero() {
			result.ReasonCode = "cursor_payload_invalid"
			if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
				return providerCursorLoadResult{}, err
			}
			result.CursorState = providerCursorStateQuarantined
			return result, nil
		}
		issuedAt := payload.IssuedAt.UTC()
		expiresAt := issuedAt.Add(s.providerCursorMaximumAge)
		sourceExecutionID := payload.SourceExecutionID
		sourceGeneration := payload.SourceGeneration
		historySequence := payload.AuthoritativeHistorySequence
		result.CursorIssuedAt = &issuedAt
		result.CursorExpiresAt = &expiresAt
		result.CursorSourceExecutionID = &sourceExecutionID
		result.CursorSourceGeneration = &sourceGeneration
		result.CursorHistorySequence = &historySequence
		if !providerCursorPayloadMatchesSession(payload, session) {
			result.ReasonCode = "cursor_lineage_mismatch"
			if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
				return providerCursorLoadResult{}, err
			}
			result.CursorState = providerCursorStateQuarantined
			return result, nil
		}
		now := s.now()
		if issuedAt.After(now.Add(providerCursorMaximumFutureSkew)) {
			result.ReasonCode = "cursor_issued_in_future"
			if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
				return providerCursorLoadResult{}, err
			}
			result.CursorState = providerCursorStateQuarantined
			return result, nil
		}
		if enforceMaximumAge && !now.Before(expiresAt) {
			result.ReasonCode = "cursor_expired"
			if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
				return providerCursorLoadResult{}, err
			}
			result.CursorState = providerCursorStateQuarantined
			return result, nil
		}
		cursor := payload.Cursor
		result.Cursor = &cursor
		result.SelectedStrategy = "native-cursor"
		result.ReasonCode = "cursor_usable"
		result.CursorState = providerCursorStateUsable
		return result, nil
	default:
		result.ReasonCode = providerCursorOpenFallbackReason(status)
		if err := s.quarantineProviderCursor(ctx, tx, execution, session); err != nil {
			return providerCursorLoadResult{}, err
		}
		result.CursorState = providerCursorStateQuarantined
		return result, nil
	}
}

func (s *Service) loadReplayedProviderCursor(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	workload Workload,
) (*string, error) {
	if workload.ResumeSnapshot == nil {
		return nil, problem.New(500, "claim_resume_decision_invalid", "The replayed Claim omitted its authoritative Resume Snapshot.")
	}
	decision, err := loadProviderCursorClaimDecision(ctx, tx, execution)
	if err != nil {
		return nil, err
	}
	if decision.RequestedStrategy != execution.ProviderResumeStrategySnapshot ||
		decision.AuthoritativeHistorySequence != workload.ResumeSnapshot.AuthoritativeHistorySequence {
		return nil, problem.New(500, "claim_resume_decision_invalid", "The replayed Claim Resume decision did not match its committed workload.")
	}
	if decision.SelectedStrategy == "authoritative-history" {
		return nil, nil
	}
	if decision.SelectedStrategy != "native-cursor" ||
		decision.RequestedStrategy != "native-cursor" ||
		decision.ReasonCode != "cursor_usable" ||
		decision.CursorState != providerCursorStateUsable ||
		decision.CursorIssuedAt == nil || decision.CursorIssuedAt.IsZero() ||
		decision.CursorSourceExecutionID == nil || *decision.CursorSourceExecutionID == uuid.Nil ||
		decision.CursorSourceGeneration == nil || *decision.CursorSourceGeneration <= 0 ||
		decision.CursorHistorySequence == nil || *decision.CursorHistorySequence < 0 {
		return nil, problem.New(500, "claim_resume_decision_invalid", "The replayed Claim native Cursor decision was invalid.")
	}

	session, err := lockProviderCursorSession(ctx, tx, execution)
	if err != nil {
		return nil, err
	}
	if session.ProviderResumeCursorState != providerCursorStateUsable ||
		len(session.ProviderResumeCursorEncrypted) == 0 ||
		session.ProviderResumeCursorSourceExecutionID == nil ||
		*session.ProviderResumeCursorSourceExecutionID != *decision.CursorSourceExecutionID ||
		session.ProviderResumeCursorSourceGeneration == nil ||
		*session.ProviderResumeCursorSourceGeneration != *decision.CursorSourceGeneration ||
		session.ProviderResumeCursorHistorySequence == nil ||
		*session.ProviderResumeCursorHistorySequence != *decision.CursorHistorySequence {
		return nil, providerCursorReplayUnavailable()
	}
	binding, available := providerCursorBindingFromExecution(execution)
	if !available || s.cursorCipher == nil {
		return nil, providerCursorReplayUnavailable()
	}
	plain, status, openErr := s.cursorCipher.OpenV2(
		session.ProviderResumeCursorEncrypted,
		binding.Version,
		binding.Digest,
	)
	if openErr != nil || status != secret.CursorOpenValid {
		return nil, providerCursorReplayUnavailable()
	}
	var payload providerCursorPayloadV1
	if json.Unmarshal(plain, &payload) != nil ||
		strings.TrimSpace(payload.Cursor) == "" || len(payload.Cursor) > providerCursorMaximumBytes || payload.IssuedAt.IsZero() ||
		payload.SourceExecutionID != *decision.CursorSourceExecutionID ||
		payload.SourceGeneration != *decision.CursorSourceGeneration ||
		payload.AuthoritativeHistorySequence != *decision.CursorHistorySequence ||
		!payload.IssuedAt.UTC().Equal(decision.CursorIssuedAt.UTC()) ||
		!providerCursorPayloadMatchesSession(payload, session) {
		return nil, providerCursorReplayUnavailable()
	}
	cursor := payload.Cursor
	return &cursor, nil
}

func providerCursorReplayUnavailable() error {
	return problem.New(
		409,
		"claim_replay_resume_cursor_unavailable",
		"The Provider Cursor selected by the original Claim is no longer available; start a new Execution instead of replaying this Claim.",
	)
}

func loadProviderCursorClaimDecision(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (providerCursorClaimDecision, error) {
	events := make([]persistence.SessionEvent, 0, 2)
	if err := tx.WithContext(ctx).
		Where(
			"tenant_id = ? AND session_id = ? AND execution_id = ? AND generation = ? AND event_type = ?",
			execution.TenantID,
			execution.SessionID,
			execution.ID,
			execution.Generation,
			"execution.leased",
		).
		Order("sequence").
		Limit(2).
		Find(&events).Error; err != nil {
		return providerCursorClaimDecision{}, problem.Wrap(500, "claim_resume_decision_load_failed", "Failed to load the committed Claim Resume decision.", err)
	}
	if len(events) != 1 {
		return providerCursorClaimDecision{}, problem.New(500, "claim_resume_decision_invalid", "The replayed Claim did not have exactly one committed Resume decision.")
	}
	payload, ok := anyMap(events[0].Payload["providerResume"])
	if !ok {
		return providerCursorClaimDecision{}, problem.New(500, "claim_resume_decision_invalid", "The replayed Claim Resume decision was missing.")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return providerCursorClaimDecision{}, problem.Wrap(500, "claim_resume_decision_invalid", "The replayed Claim Resume decision was invalid.", err)
	}
	var decision providerCursorClaimDecision
	if err := json.Unmarshal(encoded, &decision); err != nil {
		return providerCursorClaimDecision{}, problem.Wrap(500, "claim_resume_decision_invalid", "The replayed Claim Resume decision was invalid.", err)
	}
	return decision, nil
}

func providerCursorOpenFallbackReason(status secret.CursorOpenStatus) string {
	switch status {
	case secret.CursorOpenLegacyUnbound:
		return "cursor_legacy_unbound"
	case secret.CursorOpenAuthenticationFailed:
		return "cursor_authentication_failed"
	case secret.CursorOpenUnsupportedEnvelope:
		return "cursor_envelope_unsupported"
	case secret.CursorOpenCipherUnavailable:
		return "cursor_cipher_unavailable"
	default:
		return "cursor_unusable"
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
