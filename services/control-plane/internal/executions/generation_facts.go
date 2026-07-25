package executions

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	generationWarmPoolResultPending      = "pending"
	generationWarmPoolResultNotRequested = "not-requested"
	generationWarmPoolResultHit          = "hit"
	generationWarmPoolResultFallback     = "fallback"

	generationTerminalOutcomeCompleted   = "completed"
	generationTerminalOutcomeFailed      = "failed"
	generationTerminalOutcomeCancelled   = "cancelled"
	generationTerminalOutcomeInterrupted = "interrupted"
	generationTerminalOutcomeRecovering  = "recovering"
)

type generationFactSeed struct {
	Provider     string `gorm:"column:provider"`
	WarmPoolMode string
}

func generationFactsTableAvailable(tx *gorm.DB) bool {
	if tx == nil {
		return false
	}
	if tx.Dialector.Name() != "sqlite" {
		return true
	}
	return tx.Migrator().HasTable(&persistence.ExecutionGenerationFact{})
}

func loadGenerationFactSeed(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (generationFactSeed, error) {
	var seed generationFactSeed
	if err := tx.WithContext(ctx).
		Table("agent_sessions").
		Select("provider").
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
		Take(&seed).Error; err != nil {
		return generationFactSeed{}, problem.Wrap(
			500,
			"execution_generation_fact_session_load_failed",
			"The generation observability Session snapshot could not be loaded.",
			err,
		)
	}
	if execution.Provider != nil && strings.TrimSpace(*execution.Provider) != "" {
		seed.Provider = strings.TrimSpace(*execution.Provider)
	}
	seed.Provider = strings.TrimSpace(seed.Provider)
	seed.WarmPoolMode = normalizeGenerationWarmPoolMode(execution.WarmPoolModeSnapshot)
	return seed, nil
}

func normalizeGenerationWarmPoolMode(value string) string {
	switch strings.TrimSpace(value) {
	case "balanced", "low-latency":
		return strings.TrimSpace(value)
	default:
		return "disabled"
	}
}

func deriveWarmPoolResult(requestedMode, workerMode string) string {
	if normalizeGenerationWarmPoolMode(requestedMode) == "disabled" {
		return generationWarmPoolResultNotRequested
	}
	if strings.TrimSpace(workerMode) == WorkerModeWarmPool {
		return generationWarmPoolResultHit
	}
	return generationWarmPoolResultFallback
}

func (s *Service) recordExecutionGenerationDispatchRequested(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	generation int64,
	dispatchRequestedAt time.Time,
	recoveryReason string,
) error {
	if !generationFactsTableAvailable(tx) || generation <= 0 {
		return nil
	}
	seed, err := loadGenerationFactSeed(ctx, tx, execution)
	if err != nil {
		return err
	}
	if strings.TrimSpace(recoveryReason) == "" {
		prospective := execution
		prospective.Generation = generation
		recoveryReason, _, err = resolveRecoveryBundleLineage(ctx, tx, prospective)
		if err != nil {
			return err
		}
	}
	dispatchCopy := dispatchRequestedAt
	row := persistence.ExecutionGenerationFact{
		TenantID: execution.TenantID, ExecutionID: execution.ID, Generation: generation,
		SessionID: execution.SessionID, TurnID: execution.TurnID,
		ExecutionTargetID: execution.ExecutionTargetID, TargetKind: execution.TargetKind,
		Provider: seed.Provider, RecoveryReason: recoveryReason,
		WarmPoolMode: seed.WarmPoolMode, DispatchRequestedAt: &dispatchCopy,
		WarmPoolResult: generationWarmPoolResultPending,
		CreatedAt:      dispatchRequestedAt, UpdatedAt: dispatchRequestedAt,
	}
	if seed.WarmPoolMode == "disabled" {
		row.WarmPoolResult = generationWarmPoolResultNotRequested
	}
	if err := tx.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{
			{Name: "tenant_id"}, {Name: "execution_id"}, {Name: "generation"},
		}, DoNothing: true}).
		Create(&row).Error; err != nil {
		return problem.Wrap(
			500,
			"execution_generation_fact_dispatch_create_failed",
			"The generation dispatch fact could not be created.",
			err,
		)
	}
	updates := map[string]any{
		"session_id":          execution.SessionID,
		"turn_id":             execution.TurnID,
		"execution_target_id": execution.ExecutionTargetID,
		"target_kind":         execution.TargetKind,
		"provider":            seed.Provider,
		"recovery_reason":     recoveryReason,
		"warm_pool_mode":      seed.WarmPoolMode,
		"updated_at":          dispatchRequestedAt,
		"dispatch_requested_at": gorm.Expr(
			"COALESCE(dispatch_requested_at, ?)",
			dispatchRequestedAt,
		),
	}
	if seed.WarmPoolMode == "disabled" {
		updates["warm_pool_result"] = generationWarmPoolResultNotRequested
	}
	return updateGenerationFactRow(
		ctx, tx, execution.TenantID, execution.ID, generation, updates,
		"execution_generation_fact_dispatch_update_failed",
		"The generation dispatch fact could not be updated.",
	)
}

func (s *Service) recordExecutionGenerationClaimLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	worker persistence.WorkerInstance,
	bundle RecoveryBundle,
	leasedAt time.Time,
) error {
	if !generationFactsTableAvailable(tx) || execution.Generation <= 0 {
		return nil
	}
	dispatchRequestedAt := execution.QueuedAt
	if execution.Generation > 1 {
		dispatchRequestedAt = bundle.CreatedAt
		if dispatchRequestedAt.IsZero() {
			dispatchRequestedAt = leasedAt
		}
	}
	// A bounded clock correction must not turn the durable cold-start interval
	// negative. Recovery enqueue normally supplies this timestamp first; this
	// clamp only handles a control-plane clock moving backwards before Claim.
	if dispatchRequestedAt.After(leasedAt) {
		dispatchRequestedAt = leasedAt
	}
	if err := s.recordExecutionGenerationDispatchRequested(
		ctx, tx, execution, execution.Generation, dispatchRequestedAt, bundle.RecoveryReason,
	); err != nil {
		return err
	}
	seed, err := loadGenerationFactSeed(ctx, tx, execution)
	if err != nil {
		return err
	}
	return updateGenerationFactRow(
		ctx,
		tx,
		execution.TenantID,
		execution.ID,
		execution.Generation,
		map[string]any{
			"session_id":               execution.SessionID,
			"turn_id":                  execution.TurnID,
			"execution_target_id":      execution.ExecutionTargetID,
			"target_kind":              execution.TargetKind,
			"provider":                 seed.Provider,
			"recovery_reason":          bundle.RecoveryReason,
			"warm_pool_mode":           seed.WarmPoolMode,
			"warm_pool_result":         deriveWarmPoolResult(seed.WarmPoolMode, worker.WorkerMode),
			"provider_resume_strategy": execution.ProviderResumeStrategySnapshot,
			"updated_at":               leasedAt,
			"dispatch_requested_at": gorm.Expr(
				"CASE WHEN dispatch_requested_at IS NULL OR dispatch_requested_at > ? THEN ? ELSE dispatch_requested_at END",
				leasedAt,
				leasedAt,
			),
			"bundle_created_at": gorm.Expr("COALESCE(bundle_created_at, ?)", bundle.CreatedAt),
			"leased_at": gorm.Expr(
				"CASE WHEN leased_at IS NULL OR leased_at > ? THEN ? ELSE leased_at END",
				leasedAt,
				leasedAt,
			),
		},
		"execution_generation_fact_claim_update_failed",
		"The generation claim fact could not be updated.",
	)
}

func (s *Service) markExecutionGenerationStartedLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	startedAt time.Time,
) error {
	if !generationFactsTableAvailable(tx) || execution.Generation <= 0 {
		return nil
	}
	return updateGenerationFactRow(
		ctx,
		tx,
		execution.TenantID,
		execution.ID,
		execution.Generation,
		map[string]any{
			"updated_at": startedAt,
			"execution_started_at": gorm.Expr(
				"CASE WHEN execution_started_at IS NULL OR execution_started_at > ? THEN ? ELSE execution_started_at END",
				startedAt,
				startedAt,
			),
		},
		"execution_generation_fact_start_update_failed",
		"The generation start fact could not be updated.",
	)
}

func (s *Service) markExecutionGenerationProviderReadyLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	readyAt time.Time,
) error {
	if !generationFactsTableAvailable(tx) || execution.Generation <= 0 {
		return nil
	}
	return updateGenerationFactRow(
		ctx,
		tx,
		execution.TenantID,
		execution.ID,
		execution.Generation,
		map[string]any{
			"updated_at": readyAt,
			"provider_ready_at": gorm.Expr(
				"CASE WHEN provider_ready_at IS NULL OR provider_ready_at > ? THEN ? ELSE provider_ready_at END",
				readyAt,
				readyAt,
			),
		},
		"execution_generation_fact_provider_ready_update_failed",
		"The generation provider-ready fact could not be updated.",
	)
}

func (s *Service) recordExecutionGenerationFallbackLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	payload map[string]any,
	recordedAt time.Time,
) error {
	if !generationFactsTableAvailable(tx) || execution.Generation <= 0 || !IsProviderResumeFallbackRuntimeWarningPayload(payload) {
		return nil
	}
	detail, _ := payload["detail"].(map[string]any)
	updates := map[string]any{"updated_at": recordedAt}
	for column, key := range map[string]string{
		"resume_attempted_strategy":   "attemptedStrategy",
		"resume_selected_strategy":    "selectedStrategy",
		"resume_fallback_outcome":     "outcome",
		"resume_fallback_reason_code": "reasonCode",
		"resume_fallback_safety":      "fallbackSafety",
		"resume_fallback_provider":    "provider",
	} {
		if value, ok := detail[key].(string); ok && strings.TrimSpace(value) != "" {
			updates[column] = strings.TrimSpace(value)
		}
	}
	if value, ok := detail["authoritativeHistorySequence"]; ok {
		if sequence, ok := asFallbackSequence(value); ok {
			updates["resume_authoritative_history_sequence"] = sequence
		}
	}
	return updateGenerationFactRow(
		ctx,
		tx,
		execution.TenantID,
		execution.ID,
		execution.Generation,
		updates,
		"execution_generation_fact_fallback_update_failed",
		"The generation resume-fallback fact could not be updated.",
	)
}

func asFallbackSequence(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		if typed >= 0 {
			return int64(typed), true
		}
	}
	return 0, false
}

func (s *Service) markExecutionGenerationTerminalOutcomeLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	terminalAt time.Time,
	outcome string,
) error {
	return s.markExecutionGenerationTerminalOutcomeAtGenerationLocked(
		ctx, tx, execution, execution.Generation, terminalAt, outcome,
	)
}

func (s *Service) markExecutionGenerationTerminalOutcomeAtGenerationLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	generation int64,
	terminalAt time.Time,
	outcome string,
) error {
	if !generationFactsTableAvailable(tx) || generation <= 0 {
		return nil
	}
	return updateGenerationFactRow(
		ctx,
		tx,
		execution.TenantID,
		execution.ID,
		generation,
		map[string]any{
			"updated_at": terminalAt,
			"terminal_at": gorm.Expr(
				"CASE WHEN terminal_at IS NULL OR terminal_at > ? THEN ? ELSE terminal_at END",
				terminalAt,
				terminalAt,
			),
			"terminal_outcome": gorm.Expr("COALESCE(terminal_outcome, ?)", outcome),
		},
		"execution_generation_fact_terminal_update_failed",
		"The generation terminal fact could not be updated.",
	)
}

func updateGenerationFactRow(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	executionID uuid.UUID,
	generation int64,
	updates map[string]any,
	code, message string,
) error {
	if len(updates) == 0 {
		return nil
	}
	if generation <= 0 {
		return problem.New(
			500,
			"execution_generation_fact_generation_invalid",
			"The generation fact update did not identify a valid Execution generation.",
		)
	}
	result := tx.WithContext(ctx).
		Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", tenantID, executionID, generation).
		Updates(updates)
	if result.Error != nil {
		return problem.Wrap(500, code, message, result.Error)
	}
	if result.RowsAffected != 1 {
		return problem.New(
			500,
			"execution_generation_fact_missing",
			"The authoritative generation fact was not available for the Execution generation.",
		)
	}
	return nil
}
