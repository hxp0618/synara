package executions

import (
	"context"
	"errors"
	"math"
	"strings"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Service) projectRuntimeUsageLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	eventType string,
	payload map[string]any,
	sequence int64,
) error {
	if eventType != "turn.started" && eventType != "thread.token-usage.updated" && eventType != "turn.completed" {
		return nil
	}
	var current persistence.ExecutionUsageSummary
	loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", execution.TenantID, execution.ID, execution.Generation).
		Take(&current).Error
	if errors.Is(loadErr, gorm.ErrRecordNotFound) {
		provider := "unknown"
		if execution.Provider != nil && strings.TrimSpace(*execution.Provider) != "" {
			provider = strings.TrimSpace(*execution.Provider)
		}
		current = persistence.ExecutionUsageSummary{
			TenantID: execution.TenantID, ExecutionID: execution.ID, Generation: execution.Generation,
			SessionID: execution.SessionID, TurnID: execution.TurnID, Provider: provider,
			CurrencyCode: "USD",
		}
	} else if loadErr != nil {
		return problem.Wrap(500, "execution_usage_load_failed", "Execution Usage could not be loaded.", loadErr)
	}
	if current.LatestEventSequence >= sequence {
		return nil
	}

	switch eventType {
	case "turn.started":
		if model, ok := payload["model"].(string); ok && strings.TrimSpace(model) != "" {
			value := strings.TrimSpace(model)
			current.Model = &value
		}
	case "thread.token-usage.updated":
		usage, ok := payload["usage"].(map[string]any)
		if !ok {
			return nil
		}
		current.InputTokens = maxInt64(current.InputTokens, firstUsageInteger(usage, "lastInputTokens", "inputTokens"))
		current.CachedInputTokens = maxInt64(current.CachedInputTokens, firstUsageInteger(usage, "lastCachedInputTokens", "cachedInputTokens"))
		current.OutputTokens = maxInt64(current.OutputTokens, firstUsageInteger(usage, "lastOutputTokens", "outputTokens"))
		current.ReasoningTokens = maxInt64(current.ReasoningTokens, firstUsageInteger(usage, "lastReasoningOutputTokens", "reasoningOutputTokens"))
		current.DurationMillis = maxInt64(current.DurationMillis, firstUsageInteger(usage, "durationMs"))
		reportedTotal := firstUsageInteger(usage, "lastUsedTokens", "usedTokens")
		minimumTotal := current.InputTokens + current.OutputTokens
		current.TotalTokens = maxInt64(current.TotalTokens, maxInt64(reportedTotal, minimumTotal))
	case "turn.completed":
		current.Final = true
		if costUSD, ok := finiteNonNegativeNumber(payload["totalCostUsd"]); ok {
			costMicros := costUSD * 1_000_000
			if costMicros <= float64(math.MaxInt64) {
				current.ProviderCostMicros = maxInt64(current.ProviderCostMicros, int64(math.Round(costMicros)))
				current.ProviderCostReported = true
			}
		}
	}
	current.LatestEventSequence = sequence
	if errors.Is(loadErr, gorm.ErrRecordNotFound) {
		if err := tx.WithContext(ctx).Create(&current).Error; err != nil {
			return problem.Wrap(409, "execution_usage_create_failed", "Execution Usage could not be created.", err)
		}
		return nil
	}
	result := tx.WithContext(ctx).Model(&persistence.ExecutionUsageSummary{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ? AND latest_event_sequence < ?", current.TenantID, current.ExecutionID, current.Generation, sequence).
		Updates(map[string]any{
			"model": current.Model, "input_tokens": current.InputTokens,
			"cached_input_tokens": current.CachedInputTokens, "output_tokens": current.OutputTokens,
			"reasoning_tokens": current.ReasoningTokens, "total_tokens": current.TotalTokens,
			"duration_millis": current.DurationMillis, "provider_cost_micros": current.ProviderCostMicros,
			"provider_cost_reported": current.ProviderCostReported,
			"currency_code":          current.CurrencyCode, "final": current.Final, "latest_event_sequence": sequence,
		})
	if result.Error != nil {
		return problem.Wrap(409, "execution_usage_update_failed", "Execution Usage could not be updated.", result.Error)
	}
	if result.RowsAffected != 1 {
		return problem.New(409, "execution_usage_sequence_conflict", "Execution Usage changed while the Runtime Event was appended.")
	}
	return nil
}

func firstUsageInteger(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if value, ok := nonNegativeInt64(values[key]); ok {
			return value
		}
	}
	return 0
}

func nonNegativeInt64(value any) (int64, bool) {
	if !isJSONInteger(value) {
		return 0, false
	}
	number := numberValue(value)
	if number < 0 || number > float64(math.MaxInt64) {
		return 0, false
	}
	return int64(number), true
}

func finiteNonNegativeNumber(value any) (float64, bool) {
	if !isJSONNumber(value) {
		return 0, false
	}
	number := numberValue(value)
	return number, !math.IsNaN(number) && !math.IsInf(number, 0) && number >= 0
}

func maxInt64(left, right int64) int64 {
	if right > left {
		return right
	}
	return left
}
