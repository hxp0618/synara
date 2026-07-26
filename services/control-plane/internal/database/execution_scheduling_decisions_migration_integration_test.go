package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingdecision"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresSchedulingDecisionImmutabilityAndParentCascade(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrationsThrough(t, "000075_worker_claim_release_facts.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "scheduling-decision-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID, sessionID, legacyTurnID := uuid.New(), uuid.New(), uuid.New()
	for _, model := range []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Scheduling evidence", DefaultBranch: "main", Visibility: "private", CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now},
		&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Scheduling evidence", Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID, ProviderResumeCursorState: "absent", ResourceState: "provisioning", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30, WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now},
		&persistence.AgentTurn{ID: legacyTurnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "completed", InputText: "legacy schedule", TurnKind: "message", RuntimeMode: "full-access", InteractionMode: "default", CompletedAt: &now, CreatedAt: now},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	provider := "codex"
	legacyExecution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: sessionID, TurnID: legacyTurnID,
		Attempt: 1, Status: "completed", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
		Provider: &provider, Generation: 1, RequestedBy: domain.UserID, QueuedAt: now, FinishedAt: &now,
	}
	expectedLegacyDecision, expectedLegacyCandidates, err := schedulingdecision.BuildLegacySelectedOnly(legacyExecution)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Omit("SchedulingDecisionID").Create(&legacyExecution).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var backfilledExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", domain.TenantID, legacyExecution.ID).
		Take(&backfilledExecution).Error; err != nil {
		t.Fatal(err)
	}
	var backfilledDecision persistence.ExecutionSchedulingDecision
	if err := db.Where("tenant_id = ? AND execution_id = ?", domain.TenantID, legacyExecution.ID).
		Take(&backfilledDecision).Error; err != nil {
		t.Fatal(err)
	}
	var backfilledCandidate persistence.ExecutionSchedulingCandidate
	if err := db.Where("tenant_id = ? AND decision_id = ? AND ordinal = 0", domain.TenantID, backfilledDecision.ID).
		Take(&backfilledCandidate).Error; err != nil {
		t.Fatal(err)
	}
	if backfilledExecution.SchedulingDecisionID == nil ||
		*backfilledExecution.SchedulingDecisionID != expectedLegacyDecision.ID ||
		backfilledDecision.ID != expectedLegacyDecision.ID ||
		backfilledDecision.EvidenceCompleteness != schedulingdecision.EvidenceLegacySelectedOnly ||
		backfilledDecision.CandidateSetSHA256 != expectedLegacyDecision.CandidateSetSHA256 ||
		len(expectedLegacyCandidates) != 1 ||
		backfilledCandidate.CandidateSHA256 != expectedLegacyCandidates[0].CandidateSHA256 {
		t.Fatalf(
			"PostgreSQL legacy backfill diverged: execution=%#v decision=%#v candidate=%#v expectedDecision=%#v expectedCandidates=%#v",
			backfilledExecution, backfilledDecision, backfilledCandidate, expectedLegacyDecision, expectedLegacyCandidates,
		)
	}

	turnID := uuid.New()
	if err := db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
		Status: "queued", InputText: "new schedule", TurnKind: "message", RuntimeMode: "full-access",
		InteractionMode: "default", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
		Provider: &provider, Generation: 0, RequestedBy: domain.UserID, QueuedAt: now,
	}
	input := schedulingdecision.NewSelectedOnlyInput(uuid.New(), schedulingdecision.AlgorithmFixedTargetV1, now, schedulingdecision.CandidateFromExecution(execution))
	decision, err := schedulingdecision.CreateExecution(ctx, db, &execution, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionSchedulingCandidate{}).
		Where("tenant_id = ? AND decision_id = ? AND ordinal = 0", execution.TenantID, decision.ID).
		Update("target_kind", "ssh").Error; err == nil {
		t.Fatal("direct Candidate update succeeded")
	}
	var retained persistence.ExecutionSchedulingCandidate
	if err := db.Where("tenant_id = ? AND decision_id = ? AND ordinal = 0", execution.TenantID, decision.ID).
		Take(&retained).Error; err != nil {
		t.Fatal(err)
	}
	if retained.TargetKind != execution.TargetKind {
		t.Fatalf("rejected Candidate update changed target kind to %q", retained.TargetKind)
	}
	if err := db.Delete(&persistence.ExecutionSchedulingDecision{}, "tenant_id = ? AND id = ?", execution.TenantID, decision.ID).Error; err == nil {
		t.Fatal("direct Decision delete succeeded while parent Execution survives")
	}
	if err := db.Delete(&persistence.AgentExecution{}, "tenant_id = ? AND id = ?", execution.TenantID, execution.ID).Error; err != nil {
		t.Fatalf("delete parent Execution: %v", err)
	}
	var decisionCount, candidateCount int64
	if err := db.Model(&persistence.ExecutionSchedulingDecision{}).
		Where("tenant_id = ? AND id = ?", execution.TenantID, decision.ID).
		Count(&decisionCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionSchedulingCandidate{}).
		Where("tenant_id = ? AND decision_id = ?", execution.TenantID, decision.ID).
		Count(&candidateCount).Error; err != nil {
		t.Fatal(err)
	}
	if decisionCount != 0 || candidateCount != 0 {
		t.Fatalf("parent delete retained decision=%d candidates=%d", decisionCount, candidateCount)
	}
	if err := db.Delete(&persistence.AgentExecution{}, "tenant_id = ? AND id = ?", domain.TenantID, legacyExecution.ID).Error; err != nil {
		t.Fatalf("delete legacy parent Execution: %v", err)
	}
	for _, model := range []any{&persistence.ExecutionSchedulingDecision{}, &persistence.ExecutionSchedulingCandidate{}} {
		var count int64
		if err := db.Model(model).Where("tenant_id = ?", domain.TenantID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("legacy parent delete retained %T count=%d", model, count)
		}
	}
}
