package sessions

import (
	"sync"

	"github.com/google/uuid"
)

type eventStreamKey struct {
	tenantID  uuid.UUID
	sessionID uuid.UUID
}

type eventBroker struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[eventStreamKey]map[uint64]chan Event
}

func newEventBroker() *eventBroker {
	return &eventBroker{subscribers: make(map[eventStreamKey]map[uint64]chan Event)}
}

func (b *eventBroker) subscribe(tenantID, sessionID uuid.UUID) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	subscriberID := b.nextID
	key := eventStreamKey{tenantID: tenantID, sessionID: sessionID}
	if b.subscribers[key] == nil {
		b.subscribers[key] = make(map[uint64]chan Event)
	}
	channel := make(chan Event, 64)
	b.subscribers[key][subscriberID] = channel

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			subscribers := b.subscribers[key]
			delete(subscribers, subscriberID)
			if len(subscribers) == 0 {
				delete(b.subscribers, key)
			}
			close(channel)
		})
	}
	return channel, cancel
}

func (b *eventBroker) publish(event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := eventStreamKey{tenantID: event.TenantID, sessionID: event.SessionID}
	for _, subscriber := range b.subscribers[key] {
		select {
		case subscriber <- event:
		default:
			// PostgreSQL remains authoritative. A slow subscriber catches up from its
			// last durable sequence during the next periodic ORM query.
		}
	}
}
