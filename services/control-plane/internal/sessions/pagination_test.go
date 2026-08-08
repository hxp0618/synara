package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestListByProjectPageUsesStableBoundedCursor(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	base := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Update("updated_at", base.Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, 0, 4)
	for index := 0; index < 4; index++ {
		id := uuid.New()
		ids = append(ids, id)
		at := base.Add(time.Duration(index) * time.Minute)
		if err := fixture.db.Create(&persistence.AgentSession{
			ID: id, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
			ProjectID: fixture.projectID, CreatedBy: fixture.principal.UserID,
			Title: "Page Session", Status: "active", Visibility: "project", Provider: "codex",
			ExecutionTargetID: fixture.executionTargetID, CreatedAt: at, UpdatedAt: at,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	first, err := fixture.service.ListByProjectPage(
		context.Background(), fixture.principal, fixture.projectID, SessionListQuery{Limit: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == nil || first.Items[0].ID != ids[3] || first.Items[1].ID != ids[2] {
		t.Fatalf("first Session page = %#v", first)
	}
	second, err := fixture.service.ListByProjectPage(
		context.Background(), fixture.principal, fixture.projectID,
		SessionListQuery{Limit: 2, Cursor: *first.NextCursor},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 2 || second.NextCursor == nil || second.Items[0].ID != ids[1] || second.Items[1].ID != ids[0] {
		t.Fatalf("second Session page = %#v", second)
	}
	third, err := fixture.service.ListByProjectPage(
		context.Background(), fixture.principal, fixture.projectID,
		SessionListQuery{Limit: 2, Cursor: *second.NextCursor},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Items) != 1 || third.Items[0].ID != fixture.sessionID || third.NextCursor != nil {
		t.Fatalf("third Session page = %#v", third)
	}

	_, err = fixture.service.ListByProjectPage(
		context.Background(), fixture.principal, fixture.projectID,
		SessionListQuery{Limit: 2, Cursor: "not-a-cursor"},
	)
	assertSessionProblemCode(t, err, "invalid_session_cursor")
	_, err = fixture.service.ListByProjectPage(
		context.Background(), fixture.principal, fixture.projectID, SessionListQuery{Limit: 201},
	)
	assertSessionProblemCode(t, err, "invalid_session_limit")
}
