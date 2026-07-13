package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSanitizeEventForAccessRedactsInteractionDetailsWithoutBreakingSequence(t *testing.T) {
	executionID := uuid.New()
	workerID := uuid.New()
	actorID := uuid.New()
	generation := int64(3)
	event := Event{
		EventID: uuid.New(), EventVersion: 2, ExecutionID: &executionID, WorkerID: &workerID,
		Generation: &generation, Sequence: 41, EventType: "request.opened", ActorType: "worker",
		ActorID: &actorID, Payload: map[string]any{
			"requestId": "approval-1", "requestType": "exec_command_approval", "detail": "rm -rf /tmp/build",
		},
	}

	redacted := SanitizeEventForAccess(event, EventAccess{})
	if redacted.Sequence != event.Sequence || redacted.EventID != event.EventID {
		t.Fatalf("redaction changed the durable event cursor: %#v", redacted)
	}
	if redacted.EventVersion != 1 || redacted.EventType != "session.event.redacted" ||
		redacted.ExecutionID != nil || redacted.WorkerID != nil || redacted.Generation != nil ||
		redacted.ActorID != nil || len(redacted.Payload) != 0 {
		t.Fatalf("interaction details were not fully redacted: %#v", redacted)
	}

	visible := SanitizeEventForAccess(event, EventAccess{CanReadInteractionDetails: true})
	if visible.EventType != event.EventType || visible.Payload["detail"] != event.Payload["detail"] {
		t.Fatalf("approver event was unexpectedly redacted: %#v", visible)
	}
}

func TestListEventsRedactsInteractionDetailsForViewer(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	now := time.Now().UTC()
	viewerID := uuid.New()
	if err := fixture.db.Create(&persistence.User{
		ID: viewerID, Email: uuid.NewString() + "@example.com", DisplayName: "Session viewer",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.TenantMembership{
		TenantID: fixture.tenantID, UserID: viewerID, Role: "member", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.OrganizationMembership{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, UserID: viewerID,
		Role: "viewer", Status: "active", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{"visibility": "organization", "last_event_sequence": 2}).Error; err != nil {
		t.Fatal(err)
	}
	events := []persistence.SessionEvent{
		{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
			SessionID: fixture.sessionID, EventID: uuid.New(), EventVersion: 2, Sequence: 1,
			EventType: "request.opened", ActorType: "worker", Payload: map[string]any{
				"requestId": "approval-1", "requestType": "exec_command_approval", "detail": "Deploy production",
			}, OccurredAt: now,
		},
		{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
			SessionID: fixture.sessionID, EventID: uuid.New(), EventVersion: 1, Sequence: 2,
			EventType: "execution.started", ActorType: "worker", Payload: map[string]any{}, OccurredAt: now,
		},
	}
	if err := fixture.db.Create(&events).Error; err != nil {
		t.Fatal(err)
	}

	viewer := identity.Principal{UserID: viewerID, ActiveTenantID: &fixture.tenantID}
	page, err := fixture.service.ListEvents(context.Background(), viewer, fixture.sessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.LastSequence != 2 {
		t.Fatalf("unexpected viewer event page: %#v", page)
	}
	if page.Items[0].EventType != "session.event.redacted" || len(page.Items[0].Payload) != 0 ||
		page.Items[1].EventType != "execution.started" {
		t.Fatalf("viewer event redaction was incomplete: %#v", page.Items)
	}
}
