package executions

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestUserExecutionOperationsRejectCrossTenantIDsWithoutMutation(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	ctx := context.Background()
	expectedSequence := fixture.lastSequence(t, fixture.sessionID)
	created, err := fixture.service.RequestReview(
		ctx,
		fixture.principal,
		fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expectedSequence,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
			RuntimeMode:               "approval-required",
		},
		"tenant-operation-isolation-setup",
		"tenant-operation-isolation-setup",
		"127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}

	var before persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, created.Value.ExecutionID).
		Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	countsBefore := tenantOperationIsolationCounts(t, fixture, created.Value.ExecutionID)

	otherTenantID := uuid.New()
	otherPrincipal := fixture.principal
	otherPrincipal.ActiveTenantID = &otherTenantID

	_, err = fixture.service.ListPendingInteractions(ctx, otherPrincipal, fixture.sessionID)
	assertAdvancedOperationProblem(t, err, 404, "session_not_found")
	_, err = fixture.service.ProjectProviderCapabilitiesForProject(ctx, otherPrincipal, fixture.projectID, nil)
	assertAdvancedOperationProblem(t, err, 404, "project_not_found")
	_, err = fixture.service.ProjectProviderCapabilitiesForSession(ctx, otherPrincipal, fixture.sessionID)
	assertAdvancedOperationProblem(t, err, 404, "session_not_found")
	_, err = fixture.service.RequestCompact(
		ctx, otherPrincipal, fixture.sessionID,
		CompactSessionInput{ExpectedLastEventSequence: &expectedSequence},
		"tenant-operation-isolation-compact", "tenant-operation-isolation-compact", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "session_not_found")
	_, err = fixture.service.RequestReview(
		ctx, otherPrincipal, fixture.sessionID,
		StartReviewInput{
			ExpectedLastEventSequence: &expectedSequence,
			Target:                    ReviewTarget{Type: "uncommittedChanges"},
		},
		"tenant-operation-isolation-review", "tenant-operation-isolation-review", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "session_not_found")
	_, err = fixture.service.ResumeActiveTurnForSession(
		ctx, otherPrincipal, fixture.sessionID,
		"tenant-operation-isolation-resume-session", "tenant-operation-isolation-resume-session", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 409, "active_suspend_execution_not_found")
	_, err = fixture.service.ListWorkers(ctx, otherPrincipal, fixture.tenantID)
	assertAdvancedOperationProblem(t, err, 404, "tenant_not_found")
	_, err = fixture.service.ListWorkerManifests(ctx, otherPrincipal, fixture.tenantID)
	assertAdvancedOperationProblem(t, err, 404, "tenant_not_found")
	_, err = fixture.service.RevokeWorker(
		ctx, otherPrincipal, fixture.tenantID, uuid.New(), RevokeWorkerInput{},
		"tenant-operation-isolation-revoke-worker", "tenant-operation-isolation-revoke-worker", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "tenant_not_found")
	err = fixture.service.RevokeExecutionTargetWorkers(
		ctx, otherPrincipal, fixture.tenantID, fixture.targetID, 0,
		"cross-Tenant revoke must fail", "tenant-operation-isolation-revoke-target", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "tenant_not_found")

	_, err = fixture.service.Cancel(
		ctx, otherPrincipal, created.Value.ExecutionID,
		"tenant-operation-isolation-cancel", "tenant-operation-isolation-cancel", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "execution_not_found")
	_, err = fixture.service.ResumeActiveTurn(
		ctx, otherPrincipal, created.Value.ExecutionID,
		"tenant-operation-isolation-resume", "tenant-operation-isolation-resume", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "execution_not_found")
	_, err = fixture.service.RequestInterrupt(
		ctx, otherPrincipal, fixture.sessionID,
		"tenant-operation-isolation-interrupt", "tenant-operation-isolation-interrupt", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "session_not_found")
	_, err = fixture.service.RequestSteer(
		ctx, otherPrincipal, fixture.sessionID, SteerActiveTurnInput{InputText: "cross-Tenant steer"},
		"tenant-operation-isolation-steer", "tenant-operation-isolation-steer", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "session_not_found")
	_, err = fixture.service.ListInteractions(ctx, otherPrincipal, created.Value.ExecutionID)
	assertAdvancedOperationProblem(t, err, 404, "execution_not_found")
	_, err = fixture.service.ListRuntimeIsolationDecisions(
		ctx, otherPrincipal, created.Value.ExecutionID,
	)
	assertAdvancedOperationProblem(t, err, 404, "execution_not_found")
	_, err = fixture.service.ResolveApproval(
		ctx, otherPrincipal, created.Value.ExecutionID, "approval-request",
		ResolveApprovalInput{Decision: "accept"},
		"tenant-operation-isolation-approval", "tenant-operation-isolation-approval", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "execution_not_found")
	_, err = fixture.service.ResolveUserInput(
		ctx, otherPrincipal, created.Value.ExecutionID, "user-input-request",
		ResolveUserInputInput{Answers: map[string]any{"answer": "cross-Tenant"}},
		"tenant-operation-isolation-user-input", "tenant-operation-isolation-user-input", "127.0.0.1",
	)
	assertAdvancedOperationProblem(t, err, 404, "execution_not_found")

	var after persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, created.Value.ExecutionID).
		Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != before.Status || after.Generation != before.Generation ||
		after.WorkerID != before.WorkerID || after.StartedAt != before.StartedAt ||
		after.FinishedAt != before.FinishedAt {
		t.Fatalf("cross-Tenant operations mutated Execution: before=%#v after=%#v", before, after)
	}
	if countsAfter := tenantOperationIsolationCounts(t, fixture, created.Value.ExecutionID); countsAfter != countsBefore {
		t.Fatalf("cross-Tenant operations changed durable row counts: before=%#v after=%#v", countsBefore, countsAfter)
	}
}

type tenantOperationCounts struct {
	events       int64
	commands     int64
	interactions int64
	audits       int64
}

func tenantOperationIsolationCounts(
	t *testing.T,
	fixture advancedOperationFixture,
	executionID uuid.UUID,
) tenantOperationCounts {
	t.Helper()
	var counts tenantOperationCounts
	queries := []struct {
		model any
		query string
		value *int64
	}{
		{&persistence.SessionEvent{}, "tenant_id = ? AND execution_id = ?", &counts.events},
		{&persistence.ExecutionControlCommand{}, "tenant_id = ? AND execution_id = ?", &counts.commands},
		{&persistence.ExecutionInteraction{}, "tenant_id = ? AND execution_id = ?", &counts.interactions},
		{&persistence.AuditLog{}, "tenant_id = ? AND resource_id = ?", &counts.audits},
	}
	for _, query := range queries {
		if err := fixture.db.Model(query.model).Where(query.query, fixture.tenantID, executionID).
			Count(query.value).Error; err != nil {
			t.Fatal(err)
		}
	}
	return counts
}
