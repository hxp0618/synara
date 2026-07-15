package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestLogicalHistoryPageAndTailTraverseForkPrefixesInOrder(t *testing.T) {
	db := newLogicalHistoryTestDB(t)
	tenantID := uuid.New()
	rootID := uuid.New()
	childID := uuid.New()
	grandchildID := uuid.New()
	prefixThree := int64(3)
	prefixFive := int64(5)
	strategy := "emulated"
	createLogicalHistorySession(t, db, tenantID, rootID, nil, nil, nil, 3)
	createLogicalHistorySession(t, db, tenantID, childID, &rootID, &prefixThree, &strategy, 5)
	createLogicalHistorySession(t, db, tenantID, grandchildID, &childID, &prefixFive, &strategy, 7)
	createLogicalHistoryEvents(t, db, tenantID, rootID, 1, 3)
	createLogicalHistoryEvents(t, db, tenantID, childID, 4, 5)
	createLogicalHistoryEvents(t, db, tenantID, grandchildID, 6, 7)

	page, err := LoadLogicalEventsPage(context.Background(), db, tenantID, grandchildID, 0, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	assertLogicalHistorySequences(t, page, []int64{1, 2, 3, 4, 5, 6, 7})
	for index, item := range page {
		wantOrigin := rootID
		if index >= 3 && index < 5 {
			wantOrigin = childID
		} else if index >= 5 {
			wantOrigin = grandchildID
		}
		if item.OriginSessionID != wantOrigin {
			t.Fatalf("page sequence %d origin = %s, want %s", item.Event.Sequence, item.OriginSessionID, wantOrigin)
		}
	}

	middle, err := LoadLogicalEventsPage(context.Background(), db, tenantID, grandchildID, 3, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertLogicalHistorySequences(t, middle, []int64{4, 5, 6})

	tail, err := LoadLogicalEventsTail(context.Background(), db, tenantID, grandchildID, 0, 7, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertLogicalHistorySequences(t, tail, []int64{4, 5, 6, 7})
	if tail[0].OriginSessionID != childID || tail[1].OriginSessionID != childID ||
		tail[2].OriginSessionID != grandchildID || tail[3].OriginSessionID != grandchildID {
		t.Fatalf("tail lost fork-prefix origins: %#v", tail)
	}
}

func TestLogicalHistoryPageAndTailTerminateOnCycleAndExcessiveDepth(t *testing.T) {
	loaders := []struct {
		name string
		load func(context.Context, *gorm.DB, uuid.UUID, uuid.UUID) error
	}{
		{
			name: "page",
			load: func(ctx context.Context, db *gorm.DB, tenantID, sessionID uuid.UUID) error {
				_, err := LoadLogicalEventsPage(ctx, db, tenantID, sessionID, 0, 1, 1)
				return err
			},
		},
		{
			name: "tail",
			load: func(ctx context.Context, db *gorm.DB, tenantID, sessionID uuid.UUID) error {
				_, err := LoadLogicalEventsTail(ctx, db, tenantID, sessionID, 0, 1, 1, nil)
				return err
			},
		},
	}
	for _, loader := range loaders {
		t.Run(loader.name+" cycle", func(t *testing.T) {
			db := newLogicalHistoryTestDB(t)
			tenantID := uuid.New()
			firstID := uuid.New()
			secondID := uuid.New()
			prefix := int64(1)
			strategy := "emulated"
			createLogicalHistorySession(t, db, tenantID, firstID, &secondID, &prefix, &strategy, 1)
			createLogicalHistorySession(t, db, tenantID, secondID, &firstID, &prefix, &strategy, 1)
			assertLogicalHistoryProblem(t, loader.load(context.Background(), db, tenantID, firstID), "fork_lineage_cycle")
		})

		t.Run(loader.name+" depth", func(t *testing.T) {
			db := newLogicalHistoryTestDB(t)
			tenantID := uuid.New()
			prefix := int64(1)
			strategy := "emulated"
			ids := make([]uuid.UUID, maximumForkLineageDepth+1)
			for index := range ids {
				ids[index] = uuid.New()
				if index == 0 {
					createLogicalHistorySession(t, db, tenantID, ids[index], nil, nil, nil, 1)
					continue
				}
				createLogicalHistorySession(t, db, tenantID, ids[index], &ids[index-1], &prefix, &strategy, 1)
			}
			assertLogicalHistoryProblem(
				t,
				loader.load(context.Background(), db, tenantID, ids[len(ids)-1]),
				"fork_lineage_too_deep",
			)
		})
	}
}

func newLogicalHistoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.AgentSession{}, &persistence.SessionEvent{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func createLogicalHistorySession(
	t *testing.T,
	db *gorm.DB,
	tenantID, sessionID uuid.UUID,
	sourceSessionID *uuid.UUID,
	sourceSequence *int64,
	strategy *string,
	lastSequence int64,
) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), CreatedBy: uuid.New(),
		Title: "Logical history", Status: "active", Visibility: "private", Provider: "codex",
		ExecutionTargetID: uuid.New(), ForkSourceSessionID: sourceSessionID,
		ForkSourceEventSequence: sourceSequence, ForkStrategy: strategy,
		LastEventSequence: lastSequence, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func createLogicalHistoryEvents(
	t *testing.T,
	db *gorm.DB,
	tenantID, sessionID uuid.UUID,
	from, through int64,
) {
	t.Helper()
	events := make([]persistence.SessionEvent, 0, through-from+1)
	for sequence := from; sequence <= through; sequence++ {
		events = append(events, persistence.SessionEvent{
			TenantID: tenantID, SessionID: sessionID, Sequence: sequence, EventID: uuid.New(),
			EventVersion: 2, EventType: "content.delta", ActorType: "worker",
			Payload: map[string]any{"streamKind": "assistant_text", "delta": "x"}, OccurredAt: time.Now().UTC(),
		})
	}
	if err := db.Create(&events).Error; err != nil {
		t.Fatal(err)
	}
}

func assertLogicalHistorySequences(t *testing.T, events []LogicalEvent, want []int64) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("logical event count = %d, want %d: %#v", len(events), len(want), events)
	}
	for index, sequence := range want {
		if events[index].Event.Sequence != sequence {
			t.Fatalf("logical event %d sequence = %d, want %d: %#v", index, events[index].Event.Sequence, sequence, events)
		}
	}
}

func assertLogicalHistoryProblem(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("error = %#v, want problem code %q", err, code)
	}
}
