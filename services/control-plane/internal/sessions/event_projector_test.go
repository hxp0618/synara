package sessions

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

type recordingEventProjector struct {
	events []persistence.SessionEvent
	err    error
}

func (p *recordingEventProjector) ProjectSessionEvent(
	_ context.Context,
	_ *gorm.DB,
	event persistence.SessionEvent,
) error {
	p.events = append(p.events, event)
	return p.err
}

func TestSessionEventProjectionSharesBusinessTransaction(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	projector := &recordingEventProjector{}
	fixture.service.eventProjector = projector

	turn, err := fixture.service.CreateTurn(
		context.Background(), fixture.principal, fixture.sessionID,
		CreateTurnInput{InputText: "project this event", RuntimeMode: "full-access", InteractionMode: "default"},
		"event-projector", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projector.events) != 1 || projector.events[0].EventType != "turn.created" || projector.events[0].ExecutionID == nil {
		t.Fatalf("projected events = %#v", projector.events)
	}
	assertCount(t, fixture, &persistence.AgentTurn{}, "id = ?", 1, turn.ID)

	failingFixture := newTenantExecutionPolicyFixture(t)
	failingFixture.service.eventProjector = &recordingEventProjector{err: errors.New("projection unavailable")}
	_, err = failingFixture.service.CreateTurn(
		context.Background(), failingFixture.principal, failingFixture.sessionID,
		CreateTurnInput{InputText: "must roll back", RuntimeMode: "full-access", InteractionMode: "default"},
		"event-projector-failure", "127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_event_projection_failed")
	assertCount(t, failingFixture, &persistence.AgentTurn{}, "session_id = ?", 0, failingFixture.sessionID)
	assertCount(t, failingFixture, &persistence.SessionEvent{}, "session_id = ?", 0, failingFixture.sessionID)
}
