package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func TestSessionEventCursorPrefersExplicitSequence(t *testing.T) {
	request := httptest.NewRequest("GET", "/events?afterSequence=7", nil)
	request.Header.Set("Last-Event-ID", "4")
	value, err := sessionEventCursor(request)
	if err != nil {
		t.Fatal(err)
	}
	if value != 7 {
		t.Fatalf("cursor = %d, want 7", value)
	}
}

func TestSessionEventCursorRejectsInvalidLastEventID(t *testing.T) {
	request := httptest.NewRequest("GET", "/events", nil)
	request.Header.Set("Last-Event-ID", "invalid")
	if _, err := sessionEventCursor(request); err == nil {
		t.Fatal("invalid Last-Event-ID was accepted")
	}
}

func TestWriteSessionEventUsesSequenceAsSSEID(t *testing.T) {
	recorder := httptest.NewRecorder()
	event := sessions.Event{
		EventID: uuid.New(), EventVersion: 1, TenantID: uuid.New(), OrganizationID: uuid.New(),
		ProjectID: uuid.New(), SessionID: uuid.New(), Sequence: 9, EventType: "turn.created",
		ActorType: "user", Payload: map[string]any{"status": "queued"},
	}
	if err := writeSessionEvent(recorder, event); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "id: 9\nevent: session-event\ndata: {") {
		t.Fatalf("unexpected SSE body: %s", body)
	}
}
