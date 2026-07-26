package sessions

import (
	"sync"
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

// Sharding must not weaken the close/send invariant: a cancel that closes a
// subscriber channel and a concurrent publish to the same Session have to
// serialize, or publish panics on a closed channel.
func TestEventBrokerPublishRacesCancelSafely(t *testing.T) {
	broker := newEventBroker()
	var waitGroup sync.WaitGroup
	for round := 0; round < 200; round++ {
		tenantID, sessionID := uuid.New(), uuid.New()
		_, cancel := broker.subscribe(tenantID, sessionID)
		waitGroup.Add(2)
		go func() {
			defer waitGroup.Done()
			broker.publish(Event{TenantID: tenantID, SessionID: sessionID, Sequence: 1})
		}()
		go func() {
			defer waitGroup.Done()
			cancel()
		}()
	}
	waitGroup.Wait()
}

// Concurrent traffic on many Sessions must stay correctly routed once those
// Sessions are spread across shards.
func TestEventBrokerRoutesConcurrentSessionsIndependently(t *testing.T) {
	broker := newEventBroker()
	const sessions = 64
	tenantID := uuid.New()
	type stream struct {
		sessionID uuid.UUID
		events    <-chan Event
		cancel    func()
	}
	streams := make([]stream, 0, sessions)
	for index := 0; index < sessions; index++ {
		sessionID := uuid.New()
		events, cancel := broker.subscribe(tenantID, sessionID)
		streams = append(streams, stream{sessionID: sessionID, events: events, cancel: cancel})
	}
	defer func() {
		for _, item := range streams {
			item.cancel()
		}
	}()

	var waitGroup sync.WaitGroup
	for index, item := range streams {
		waitGroup.Add(1)
		go func(sequence int64, sessionID uuid.UUID) {
			defer waitGroup.Done()
			broker.publish(Event{TenantID: tenantID, SessionID: sessionID, Sequence: sequence})
		}(int64(index+1), item.sessionID)
	}
	waitGroup.Wait()

	for index, item := range streams {
		select {
		case event := <-item.events:
			if event.Sequence != int64(index+1) || event.SessionID != item.sessionID {
				t.Fatalf("session %d received the wrong event: %#v", index, event)
			}
		default:
			t.Fatalf("session %d received no event", index)
		}
	}
}

// A degenerate hash would silently reintroduce the single-lock behavior.
func TestEventBrokerSpreadsSessionsAcrossShards(t *testing.T) {
	broker := newEventBroker()
	tenantID := uuid.New()
	occupied := make(map[*eventBrokerShard]struct{})
	for index := 0; index < 512; index++ {
		occupied[broker.shardFor(eventStreamKey{tenantID: tenantID, sessionID: uuid.New()})] = struct{}{}
	}
	if len(occupied) < eventBrokerShardCount/2 {
		t.Fatalf("512 Sessions occupied only %d of %d shards", len(occupied), eventBrokerShardCount)
	}
}
