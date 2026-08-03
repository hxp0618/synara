package executions

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestRuntimeUsageProjectionPersistsTurnTokensAndProviderCost(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.ExecutionUsageSummary{}); err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		Generation: 1, Provider: &provider,
	}
	service := &Service{}
	project := func(eventType string, payload map[string]any, sequence int64) {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error {
			return service.projectRuntimeUsageLocked(context.Background(), tx, execution, eventType, payload, sequence)
		}); err != nil {
			t.Fatal(err)
		}
	}
	project("turn.started", map[string]any{"model": "gpt-5.6-sol"}, 1)
	project("thread.token-usage.updated", map[string]any{"usage": map[string]any{
		"usedTokens": 180, "lastUsedTokens": 180, "lastInputTokens": 120,
		"lastCachedInputTokens": 40, "lastOutputTokens": 60, "lastReasoningOutputTokens": 20,
		"durationMs": 2500,
	}}, 2)
	project("turn.completed", map[string]any{"state": "completed", "totalCostUsd": 0.012345}, 3)
	project("thread.token-usage.updated", map[string]any{"usage": map[string]any{
		"usedTokens": 999, "lastInputTokens": 999,
	}}, 2)

	var summary persistence.ExecutionUsageSummary
	if err := db.Where("tenant_id = ? AND execution_id = ? AND generation = ?", execution.TenantID, execution.ID, 1).Take(&summary).Error; err != nil {
		t.Fatal(err)
	}
	if summary.Model == nil || *summary.Model != "gpt-5.6-sol" || summary.InputTokens != 120 ||
		summary.CachedInputTokens != 40 || summary.OutputTokens != 60 || summary.ReasoningTokens != 20 ||
		summary.TotalTokens != 180 || summary.DurationMillis != 2500 || summary.ProviderCostMicros != 12345 ||
		!summary.ProviderCostReported || !summary.Final || summary.LatestEventSequence != 3 {
		t.Fatalf("unexpected Execution Usage summary: %#v", summary)
	}
}

func TestRuntimeUsageProjectionDistinguishesMissingAndExplicitZeroProviderCost(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.ExecutionUsageSummary{}); err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	service := &Service{}
	missing := persistence.AgentExecution{
		ID: uuid.New(), TenantID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(), Generation: 1, Provider: &provider,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return service.projectRuntimeUsageLocked(context.Background(), tx, missing, "turn.completed", map[string]any{"state": "completed"}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	explicitZero := persistence.AgentExecution{
		ID: uuid.New(), TenantID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(), Generation: 1, Provider: &provider,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return service.projectRuntimeUsageLocked(context.Background(), tx, explicitZero, "turn.completed", map[string]any{"state": "completed", "totalCostUsd": 0.0}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	var missingSummary, zeroSummary persistence.ExecutionUsageSummary
	if err := db.Where("tenant_id = ? AND execution_id = ?", missing.TenantID, missing.ID).Take(&missingSummary).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("tenant_id = ? AND execution_id = ?", explicitZero.TenantID, explicitZero.ID).Take(&zeroSummary).Error; err != nil {
		t.Fatal(err)
	}
	if missingSummary.ProviderCostReported || missingSummary.ProviderCostMicros != 0 {
		t.Fatalf("missing Provider cost = %#v", missingSummary)
	}
	if !zeroSummary.ProviderCostReported || zeroSummary.ProviderCostMicros != 0 {
		t.Fatalf("explicit zero Provider cost = %#v", zeroSummary)
	}
}
