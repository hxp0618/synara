package sessions

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEventBrokerScopesNotificationsByTenantAndSession(t *testing.T) {
	broker := newEventBroker()
	tenantID := uuid.New()
	sessionID := uuid.New()
	events, cancel := broker.subscribe(tenantID, sessionID)
	defer cancel()

	broker.publish(Event{TenantID: uuid.New(), SessionID: sessionID, Sequence: 1})
	broker.publish(Event{TenantID: tenantID, SessionID: uuid.New(), Sequence: 2})
	broker.publish(Event{TenantID: tenantID, SessionID: sessionID, Sequence: 3})

	select {
	case event := <-events:
		if event.Sequence != 3 {
			t.Fatalf("received sequence %d, want 3", event.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped event")
	}
}
