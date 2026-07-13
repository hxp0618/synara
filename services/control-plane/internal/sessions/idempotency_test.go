package sessions

import (
	"context"
	"errors"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestTurnCreateIdempotencyDoesNotDuplicateExecutionEventOrOutbox(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	input := CreateTurnInput{
		InputText: "one durable Turn", RuntimeMode: "approval-required", InteractionMode: "plan",
	}

	first, replayed, err := fixture.service.CreateTurnWithIdempotency(
		ctx, fixture.principal, fixture.sessionID, input, "turn-key", "turn-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("first Turn creation was marked as replayed")
	}
	if first.RuntimeMode != "approval-required" || first.InteractionMode != "plan" {
		t.Fatalf("Turn modes were not persisted in the API result: %#v", first)
	}
	second, replayed, err := fixture.service.CreateTurnWithIdempotency(
		ctx, fixture.principal, fixture.sessionID, input, "turn-key", "turn-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || second.ID != first.ID {
		t.Fatalf("Turn replay mismatch: first=%#v second=%#v replayed=%t", first, second, replayed)
	}

	_, _, err = fixture.service.CreateTurnWithIdempotency(
		ctx, fixture.principal, fixture.sessionID,
		CreateTurnInput{InputText: "one durable Turn", RuntimeMode: "full-access", InteractionMode: "default"},
		"turn-key", "turn-conflict", "127.0.0.1",
	)
	assertIdempotencyConflict(t, err)

	assertCount(t, fixture, &persistence.AgentTurn{}, "session_id = ?", 1, fixture.sessionID)
	assertCount(t, fixture, &persistence.AgentExecution{}, "session_id = ?", 1, fixture.sessionID)
	assertCount(t, fixture, &persistence.ProviderRuntimeBinding{}, "session_id = ?", 1, fixture.sessionID)
	assertCount(t, fixture, &persistence.RemoteWorkspace{}, "session_id = ?", 1, fixture.sessionID)
	assertCount(t, fixture, &persistence.WorkspaceMaterialization{}, "session_id = ?", 1, fixture.sessionID)
	assertCount(t, fixture, &persistence.SessionEvent{}, "session_id = ? AND event_type = ?", 1, fixture.sessionID, "turn.created")
	assertCount(t, fixture, &persistence.OutboxMessage{}, "tenant_id = ? AND topic = ?", 1, fixture.tenantID, "execution.queued")
	var execution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Provider == nil || *execution.Provider != "codex" ||
		execution.ProviderRuntimeBindingID == nil || execution.RemoteWorkspaceID == nil ||
		execution.WorkspaceMaterializationID == nil {
		t.Fatalf("Turn Execution omitted Stage 3 runtime resources: %#v", execution)
	}
	var workspace persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.CurrentMaterializationID == nil || *workspace.CurrentMaterializationID != *execution.WorkspaceMaterializationID {
		t.Fatalf("logical Workspace did not point at the Execution materialization: workspace=%#v execution=%#v", workspace, execution)
	}
	var materialization persistence.WorkspaceMaterialization
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, *execution.WorkspaceMaterializationID).
		Take(&materialization).Error; err != nil {
		t.Fatal(err)
	}
	if materialization.State != "active" || materialization.LayoutVersion != currentWorkspaceLayoutVersion ||
		materialization.ExecutionTargetID != execution.ExecutionTargetID {
		t.Fatalf("Turn materialization is invalid: %#v", materialization)
	}
	var turn persistence.AgentTurn
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, first.ID).Take(&turn).Error; err != nil {
		t.Fatal(err)
	}
	if turn.RuntimeMode != "approval-required" || turn.InteractionMode != "plan" {
		t.Fatalf("Turn modes were not persisted in PostgreSQL: %#v", turn)
	}
	var event persistence.SessionEvent
	if err := fixture.db.Where("tenant_id = ? AND session_id = ? AND event_type = ?",
		fixture.tenantID, fixture.sessionID, "turn.created").Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.Payload["runtimeMode"] != "approval-required" || event.Payload["interactionMode"] != "plan" {
		t.Fatalf("Turn modes were not captured in the authoritative Event: %#v", event.Payload)
	}
}

func TestSessionCreateAndArchiveIdempotency(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	input := CreateSessionInput{Title: "Idempotent Session", Visibility: "project", Provider: "codex"}

	first, replayed, err := fixture.service.CreateWithIdempotency(
		ctx, fixture.principal, fixture.projectID, input, "session-key", "session-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("first Session creation was marked as replayed")
	}
	second, replayed, err := fixture.service.CreateWithIdempotency(
		ctx, fixture.principal, fixture.projectID, input, "session-key", "session-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || second.ID != first.ID {
		t.Fatalf("Session replay mismatch: first=%#v second=%#v replayed=%t", first, second, replayed)
	}

	archived, replayed, err := fixture.service.ArchiveWithIdempotency(
		ctx, fixture.principal, first.ID, "archive-key", "archive-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || archived.Status != "archived" {
		t.Fatalf("unexpected first archive result: %#v replayed=%t", archived, replayed)
	}
	replayedArchive, replayed, err := fixture.service.ArchiveWithIdempotency(
		ctx, fixture.principal, first.ID, "archive-key", "archive-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || replayedArchive.ID != archived.ID {
		t.Fatalf("Session archive replay mismatch: first=%#v second=%#v replayed=%t", archived, replayedArchive, replayed)
	}

	assertCount(t, fixture, &persistence.AgentSession{}, "id = ?", 1, first.ID)
	assertCount(t, fixture, &persistence.ProviderRuntimeBinding{}, "session_id = ?", 1, first.ID)
	assertCount(t, fixture, &persistence.RemoteWorkspace{}, "session_id = ?", 1, first.ID)
	assertCount(t, fixture, &persistence.SessionEvent{}, "session_id = ? AND event_type = ?", 1, first.ID, "session.created")
	assertCount(t, fixture, &persistence.SessionEvent{}, "session_id = ? AND event_type = ?", 1, first.ID, "session.archived")
	assertCount(t, fixture, &persistence.OutboxMessage{}, "tenant_id = ? AND topic = ?", 1, fixture.tenantID, "session.archived")
	var workspace persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, first.ID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.RetentionUntil != nil || workspace.CurrentMaterializationID == nil {
		t.Fatalf("archive without a Workspace cleanup policy scheduled automatic cleanup: %#v", workspace)
	}
	var materialization persistence.WorkspaceMaterialization
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, *workspace.CurrentMaterializationID).
		Take(&materialization).Error; err != nil {
		t.Fatal(err)
	}
	if materialization.CleanupRequestedAt != nil {
		t.Fatalf("archive without a Workspace cleanup policy created cleanup intent: %#v", materialization)
	}
}

func TestSessionSuspendResumeTransitionsAreIdempotent(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	suspended, replayed, err := fixture.service.Suspend(
		ctx, fixture.principal, fixture.sessionID, "suspend-key", "suspend-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || suspended.Status != "suspended" {
		t.Fatalf("unexpected suspend result: %#v replayed=%t", suspended, replayed)
	}
	replayedSuspend, replayed, err := fixture.service.Suspend(
		ctx, fixture.principal, fixture.sessionID, "suspend-key", "suspend-second", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || replayedSuspend.Status != "suspended" {
		t.Fatalf("unexpected suspend replay: %#v replayed=%t", replayedSuspend, replayed)
	}
	if _, err := fixture.service.CreateTurn(
		ctx, fixture.principal, fixture.sessionID, CreateTurnInput{InputText: "blocked while suspended"},
		"suspended-turn", "127.0.0.1",
	); err == nil {
		t.Fatal("suspended Session accepted a Turn")
	}
	if _, _, err := fixture.service.ArchiveWithIdempotency(
		ctx, fixture.principal, fixture.sessionID, "archive-suspended", "archive-suspended", "127.0.0.1",
	); err == nil {
		t.Fatal("suspended Session was archived without resuming")
	}

	resumed, replayed, err := fixture.service.Resume(
		ctx, fixture.principal, fixture.sessionID, "resume-key", "resume-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || resumed.Status != "active" {
		t.Fatalf("unexpected resume result: %#v replayed=%t", resumed, replayed)
	}
	if _, err := fixture.service.CreateTurn(
		ctx, fixture.principal, fixture.sessionID, CreateTurnInput{InputText: "allowed after resume"},
		"resumed-turn", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	assertCount(t, fixture, &persistence.SessionEvent{}, "session_id = ? AND event_type = ?", 1, fixture.sessionID, "session.suspended")
	assertCount(t, fixture, &persistence.SessionEvent{}, "session_id = ? AND event_type = ?", 1, fixture.sessionID, "session.resumed")
}

func assertIdempotencyConflict(t *testing.T, err error) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "idempotency_conflict" {
		t.Fatalf("expected idempotency_conflict, got %v", err)
	}
}

func assertCount(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	model any,
	query string,
	want int64,
	args ...any,
) {
	t.Helper()
	var count int64
	if err := fixture.db.Model(model).Where(query, args...).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%T count = %d, want %d", model, count, want)
	}
}
