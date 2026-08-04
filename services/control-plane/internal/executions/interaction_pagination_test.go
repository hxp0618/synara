package executions

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInteractionListCursorIsOpaqueAndResourceBound(t *testing.T) {
	tenantID, executionID := uuid.New(), uuid.New()
	cursor := interactionListCursor{
		Version: 1, TenantID: tenantID, ResourceID: executionID, Scope: "execution",
		RequestedAt: time.Now().UTC(), ID: uuid.New(),
	}
	encoded := encodeInteractionListCursor(cursor)
	limit, decoded, err := normalizeInteractionListQuery(
		InteractionListQuery{Limit: 25, Cursor: encoded}, tenantID, executionID, "execution",
	)
	if err != nil || limit != 25 || decoded == nil || decoded.ID != cursor.ID {
		t.Fatalf("decoded Interaction cursor = %#v limit=%d err=%v", decoded, limit, err)
	}
	if _, _, err := normalizeInteractionListQuery(
		InteractionListQuery{Limit: 25, Cursor: encoded}, tenantID, uuid.New(), "execution",
	); err == nil {
		t.Fatal("Interaction cursor was accepted for another resource")
	}
	if _, _, err := normalizeInteractionListQuery(
		InteractionListQuery{Limit: 201}, tenantID, executionID, "execution",
	); err == nil {
		t.Fatal("Interaction list accepted an unbounded limit")
	}
}
