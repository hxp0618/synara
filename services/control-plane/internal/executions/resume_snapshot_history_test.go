package executions

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestResumeSnapshotLoadsOnlyNewestFiveHundredFromFiveHundredOneEventTail(t *testing.T) {
	db := newResumeSnapshotHistoryTestDB(t)
	tenantID := uuid.New()
	sessionID := uuid.New()
	executionID := uuid.New()
	turnID := uuid.New()
	createResumeSnapshotHistorySession(t, db, tenantID, sessionID, 502)
	events := make([]persistence.SessionEvent, 0, 502)
	for sequence := int64(1); sequence <= 501; sequence++ {
		events = append(events, resumeSnapshotHistoryEvent(
			tenantID, sessionID, sequence, "content.delta",
			map[string]any{"streamKind": "assistant_text", "delta": "x"}, nil,
		))
	}
	events = append(events, resumeSnapshotHistoryEvent(
		tenantID, sessionID, 502, "turn.created",
		map[string]any{"inputText": "current"}, &executionID,
	))
	if err := db.CreateInBatches(events, 100).Error; err != nil {
		t.Fatal(err)
	}

	service := &Service{now: func() time.Time { return time.Now().UTC() }}
	execution := persistence.AgentExecution{
		ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
	}
	snapshot, err := service.loadResumeSnapshot(context.Background(), db, execution, resumeSnapshotContext{
		Provider: "codex", RuntimeMode: "full-access", InteractionMode: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceSequenceRange != (ResumeSequenceRange{From: 2, Through: 501}) ||
		snapshot.AuthoritativeHistorySequence != 501 {
		t.Fatalf("snapshot source range did not reflect the actual loaded tail: %#v", snapshot)
	}
	if snapshot.Truncation == nil || !containsString(snapshot.Truncation.Reasons, "event_limit") ||
		snapshot.Truncation.DroppedBeforeSequence == nil || *snapshot.Truncation.DroppedBeforeSequence != 1 {
		t.Fatalf("snapshot did not record the 501-to-500 event truncation: %#v", snapshot.Truncation)
	}
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].SequenceFrom != 2 ||
		snapshot.Messages[0].SequenceThrough != 501 || len(snapshot.Messages[0].Text) != 500 {
		t.Fatalf("snapshot did not retain exactly the newest 500 events: %#v", snapshot.Messages)
	}
}

func TestResumeSnapshotOrdersNewestRollbackChainSegmentsAndDropsRolledBackSpans(t *testing.T) {
	db := newResumeSnapshotHistoryTestDB(t)
	tenantID := uuid.New()
	sessionID := uuid.New()
	executionID := uuid.New()
	turnID := uuid.New()
	createResumeSnapshotHistorySession(t, db, tenantID, sessionID, 13)
	events := []persistence.SessionEvent{
		resumeSnapshotHistoryEvent(tenantID, sessionID, 1, "turn.created", map[string]any{"inputText": "A"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 2, "content.delta", map[string]any{"streamKind": "assistant_text", "delta": "a"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 3, "turn.created", map[string]any{"inputText": "B"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 4, "content.delta", map[string]any{"streamKind": "assistant_text", "delta": "b"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 5, "session.history.rolled-back", map[string]any{"fromSequence": int64(3)}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 6, "turn.created", map[string]any{"inputText": "C"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 7, "content.delta", map[string]any{"streamKind": "assistant_text", "delta": "c"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 8, "turn.created", map[string]any{"inputText": "D"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 9, "content.delta", map[string]any{"streamKind": "assistant_text", "delta": "d"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 10, "session.history.rolled-back", map[string]any{"fromSequence": int64(8)}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 11, "turn.created", map[string]any{"inputText": "E"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 12, "content.delta", map[string]any{"streamKind": "assistant_text", "delta": "e"}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 13, "turn.created", map[string]any{"inputText": "current"}, &executionID),
	}
	if err := db.Create(&events).Error; err != nil {
		t.Fatal(err)
	}
	execution := persistence.AgentExecution{
		ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
	}
	loaded, truncated, err := loadEffectiveResumeSnapshotEvents(context.Background(), db, execution, 12)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("short rollback chain was unexpectedly truncated")
	}
	assertResumeSnapshotHistorySequences(t, loaded, []int64{1, 2, 6, 7, 11, 12})

	service := &Service{now: func() time.Time { return time.Now().UTC() }}
	snapshot, err := service.loadResumeSnapshot(context.Background(), db, execution, resumeSnapshotContext{
		Provider: "codex", RuntimeMode: "full-access", InteractionMode: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceSequenceRange != (ResumeSequenceRange{From: 1, Through: 12}) || snapshot.Truncation != nil {
		t.Fatalf("rollback-chain source range/truncation = %#v/%#v", snapshot.SourceSequenceRange, snapshot.Truncation)
	}
	if len(snapshot.Messages) != 6 {
		t.Fatalf("rollback-chain message count = %d: %#v", len(snapshot.Messages), snapshot.Messages)
	}
	texts := make([]string, 0, len(snapshot.Messages))
	for _, message := range snapshot.Messages {
		texts = append(texts, message.Text)
	}
	if strings.Join(texts, "") != "AaCcEe" {
		t.Fatalf("rollback-chain message order = %#v, want A a C c E e", texts)
	}
}

func TestResumeSnapshotStateMarkerRecoverySkipsRolledBackMarkers(t *testing.T) {
	db := newResumeSnapshotHistoryTestDB(t)
	tenantID := uuid.New()
	sessionID := uuid.New()
	executionID := uuid.New()
	turnID := uuid.New()
	createResumeSnapshotHistorySession(t, db, tenantID, sessionID, 508)
	events := []persistence.SessionEvent{
		resumeSnapshotHistoryEvent(tenantID, sessionID, 1, "item.completed", map[string]any{
			"itemType": "review_exited", "status": "completed",
		}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 2, "item.completed", map[string]any{
			"itemType": "context_compaction", "status": "completed", "detail": "Effective compact summary",
		}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 3, "turn.created", map[string]any{
			"inputText": "Rolled back Turn",
		}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 4, "item.completed", map[string]any{
			"itemType": "review_entered", "status": "completed",
		}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 5, "item.completed", map[string]any{
			"itemType": "context_compaction", "status": "completed", "detail": "Rolled back summary",
		}, nil),
		resumeSnapshotHistoryEvent(tenantID, sessionID, 6, "session.history.rolled-back", map[string]any{
			"fromSequence": int64(3),
		}, nil),
	}
	for sequence := int64(7); sequence <= 507; sequence++ {
		events = append(events, resumeSnapshotHistoryEvent(
			tenantID, sessionID, sequence, "content.delta",
			map[string]any{"streamKind": "assistant_text", "delta": "x"}, nil,
		))
	}
	events = append(events, resumeSnapshotHistoryEvent(
		tenantID, sessionID, 508, "turn.created",
		map[string]any{"inputText": "current"}, &executionID,
	))
	if err := db.CreateInBatches(events, 100).Error; err != nil {
		t.Fatal(err)
	}

	service := &Service{now: func() time.Time { return time.Now().UTC() }}
	snapshot, err := service.loadResumeSnapshot(context.Background(), db, persistence.AgentExecution{
		ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
	}, resumeSnapshotContext{
		Provider: "codex", RuntimeMode: "full-access", InteractionMode: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Mode.Review || snapshot.Mode.ReviewSequence == nil || *snapshot.Mode.ReviewSequence != 1 {
		t.Fatalf("rolled-back Review marker replaced the effective state: %#v", snapshot.Mode)
	}
	if snapshot.CompactBoundary == nil || snapshot.CompactBoundary.Sequence != 2 ||
		snapshot.CompactBoundary.Summary != "Effective compact summary" {
		t.Fatalf("rolled-back compact marker replaced the effective boundary: %#v", snapshot.CompactBoundary)
	}
}

func newResumeSnapshotHistoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.AgentSession{},
		&persistence.SessionEvent{},
		&persistence.ExecutionInteraction{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func createResumeSnapshotHistorySession(
	t *testing.T,
	db *gorm.DB,
	tenantID, sessionID uuid.UUID,
	lastSequence int64,
) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), CreatedBy: uuid.New(),
		Title: "Resume Snapshot history", Status: "active", Visibility: "private", Provider: "codex",
		ExecutionTargetID: uuid.New(), LastEventSequence: lastSequence, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func resumeSnapshotHistoryEvent(
	tenantID, sessionID uuid.UUID,
	sequence int64,
	eventType string,
	payload map[string]any,
	executionID *uuid.UUID,
) persistence.SessionEvent {
	return persistence.SessionEvent{
		TenantID: tenantID, SessionID: sessionID, Sequence: sequence, EventID: uuid.New(),
		EventVersion: 2, EventType: eventType, ActorType: "worker", ExecutionID: executionID,
		Payload: payload, OccurredAt: time.Now().UTC(),
	}
}

func assertResumeSnapshotHistorySequences(
	t *testing.T,
	events []persistence.SessionEvent,
	want []int64,
) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("resume history count = %d, want %d: %#v", len(events), len(want), events)
	}
	for index, sequence := range want {
		if events[index].Sequence != sequence {
			t.Fatalf("resume history %d sequence = %d, want %d: %#v", index, events[index].Sequence, sequence, events)
		}
	}
}
