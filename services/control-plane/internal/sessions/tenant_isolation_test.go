package sessions

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
)

func TestSessionOperationsRejectCrossTenantNestedIDSubstitution(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	otherTenant, err := tenancy.NewService(fixture.db).CreateTenant(
		ctx,
		fixture.principal,
		tenancy.CreateTenantInput{
			Slug: "session-other-" + uuid.NewString()[:8], Name: "Session Other Tenant",
			PlanCode: "free", Status: "active",
		},
		"session-other-tenant",
		"127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	otherPrincipal := fixture.principal
	otherPrincipal.ActiveTenantID = &otherTenant.ID

	_, err = fixture.service.Get(ctx, otherPrincipal, otherTenant.ID, fixture.sessionID)
	assertSessionProblemCode(t, err, "session_not_found")
	_, err = fixture.service.ListByProject(ctx, otherPrincipal, fixture.projectID)
	assertSessionProblemCode(t, err, "project_not_found")
	_, err = fixture.service.CreateTurn(
		ctx, otherPrincipal, fixture.sessionID, CreateTurnInput{InputText: "cross-Tenant turn"},
		"session-cross-tenant-turn", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	_, err = fixture.service.Create(
		ctx, otherPrincipal, fixture.projectID, CreateSessionInput{},
		"session-cross-tenant-create", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "project_not_found")
	_, _, err = fixture.service.CreateWithIdempotency(
		ctx, otherPrincipal, fixture.projectID, CreateSessionInput{},
		"session-cross-tenant-create-idempotent", "session-cross-tenant-create-idempotent", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "project_not_found")
	_, _, err = fixture.service.CreateTurnWithIdempotency(
		ctx, otherPrincipal, fixture.sessionID, CreateTurnInput{InputText: "cross-Tenant idempotent turn"},
		"session-cross-tenant-turn-idempotent", "session-cross-tenant-turn-idempotent", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	_, err = fixture.service.ListEvents(ctx, otherPrincipal, fixture.sessionID, 0, 100)
	assertSessionProblemCode(t, err, "session_not_found")
	_, err = fixture.service.Archive(
		ctx, otherPrincipal, fixture.sessionID,
		"session-cross-tenant-archive", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	_, _, err = fixture.service.ArchiveWithIdempotency(
		ctx, otherPrincipal, fixture.sessionID,
		"session-cross-tenant-archive-idempotent", "session-cross-tenant-archive-idempotent", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	_, _, err = fixture.service.Suspend(
		ctx, otherPrincipal, fixture.sessionID,
		"session-cross-tenant-suspend", "session-cross-tenant-suspend", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	_, _, err = fixture.service.Resume(
		ctx, otherPrincipal, fixture.sessionID,
		"session-cross-tenant-resume", "session-cross-tenant-resume", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	_, err = fixture.service.SwitchModel(
		ctx, otherPrincipal, fixture.sessionID, SwitchModelInput{Model: "gpt-5.4"},
		"session-cross-tenant-switch-model", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	_, _, err = fixture.service.SwitchModelWithIdempotency(
		ctx, otherPrincipal, fixture.sessionID, SwitchModelInput{Model: "gpt-5.4"},
		"session-cross-tenant-switch-model-idempotent", "session-cross-tenant-switch-model-idempotent", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	expectedSequence := int64(0)
	_, _, err = fixture.service.Fork(
		ctx, otherPrincipal, fixture.sessionID,
		ForkSessionInput{ExpectedLastEventSequence: &expectedSequence},
		"session-cross-tenant-fork", "session-cross-tenant-fork", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	_, _, err = fixture.service.Rollback(
		ctx, otherPrincipal, fixture.sessionID,
		RollbackSessionInput{ExpectedLastEventSequence: &expectedSequence, FromTurnID: uuid.New()},
		"session-cross-tenant-rollback", "session-cross-tenant-rollback", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_not_found")
	_, _, _, err = fixture.service.SubscribeEvents(ctx, otherPrincipal, fixture.sessionID)
	assertSessionProblemCode(t, err, "session_not_found")
	_, err = fixture.service.SanitizeSubscribedEvent(ctx, otherPrincipal, Event{
		TenantID: fixture.tenantID, SessionID: fixture.sessionID, EventType: "turn.created",
	})
	assertSessionProblemCode(t, err, "tenant_not_found")

	var session persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.ArchivedAt != nil || session.LastEventSequence != 0 {
		t.Fatalf("cross-Tenant operations mutated original Session: %#v", session)
	}
	var turnCount int64
	if err := fixture.db.Model(&persistence.AgentTurn{}).
		Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Count(&turnCount).Error; err != nil {
		t.Fatal(err)
	}
	if turnCount != 0 {
		t.Fatalf("cross-Tenant turn count = %d", turnCount)
	}
}
