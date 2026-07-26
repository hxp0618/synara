package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSQLiteSchedulingDecisionBackfillImmutabilityAndParentCascade(t *testing.T) {
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.AgentExecution{},
		&persistence.ExecutionSchedulingDecision{},
		&persistence.ExecutionSchedulingCandidate{},
	); err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		Attempt: 1, Status: "queued", ExecutionTargetID: uuid.New(), TargetKind: "local",
		Provider: &provider, Generation: 0, RequestedBy: uuid.New(), QueuedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := db.Create(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateExecutionSchedulingDecisionsSQLiteSafety(ctx, db); err != nil {
		t.Fatal(err)
	}
	var decision persistence.ExecutionSchedulingDecision
	if err := db.Where("tenant_id = ? AND execution_id = ?", execution.TenantID, execution.ID).Take(&decision).Error; err != nil {
		t.Fatal(err)
	}
	if decision.EvidenceCompleteness != "legacy-selected-only" || decision.AlgorithmVersion != "legacy-selected-only" {
		t.Fatalf("backfilled Decision = %#v", decision)
	}
	if err := db.Model(&decision).Update("provider", "claude").Error; err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("direct Decision update = %v, want immutable rejection", err)
	}
	if err := db.Delete(&decision).Error; err == nil || !strings.Contains(err.Error(), "retained by its Execution") {
		t.Fatalf("direct Decision delete = %v, want parent retention rejection", err)
	}
	mismatchedExecution := execution
	mismatchedExecution.ID = uuid.New()
	mismatchedExecution.SchedulingDecisionID = nil
	if err := db.Create(&mismatchedExecution).Error; err != nil {
		t.Fatal(err)
	}
	mismatchedDecision := decision
	mismatchedDecision.ID = uuid.New()
	mismatchedDecision.ExecutionID = mismatchedExecution.ID
	mismatchedDecision.AlgorithmVersion = "queue-pressure-v1"
	mismatchedDecision.EvidenceCompleteness = "selected-only"
	mismatchedDecision.DecisionKind = "target-group"
	mismatchedDecision.CandidateSetSHA256 = strings.Repeat("a", 64)
	if err := db.Create(&mismatchedDecision).Error; err == nil || !strings.Contains(err.Error(), "does not match its Execution") {
		t.Fatalf("mismatched Decision scope = %v, want DB rejection", err)
	}
	if err := db.Delete(&persistence.AgentExecution{}, "tenant_id = ? AND id = ?", execution.TenantID, execution.ID).Error; err != nil {
		t.Fatalf("delete parent Execution: %v", err)
	}
	for _, model := range []any{&persistence.ExecutionSchedulingDecision{}, &persistence.ExecutionSchedulingCandidate{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("parent delete retained %T count=%d", model, count)
		}
	}
}
