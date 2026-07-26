package sessions

import (
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

type eventStreamKey struct {
	tenantID  uuid.UUID
	sessionID uuid.UUID
}

// eventBrokerShardCount must stay a power of two so the shard index is a mask
// rather than a division. Runtime events fan out on the hot path of every
// active Execution, so one broker-wide mutex would serialize unrelated Tenants
// and Sessions against each other.
const eventBrokerShardCount = 64

type eventBrokerShard struct {
	mu          sync.Mutex
	subscribers map[eventStreamKey]map[uint64]chan Event
}

type eventBroker struct {
	nextID atomic.Uint64
	shards [eventBrokerShardCount]eventBrokerShard
}

func newEventBroker() *eventBroker {
	broker := &eventBroker{}
	for index := range broker.shards {
		broker.shards[index].subscribers = make(map[eventStreamKey]map[uint64]chan Event)
	}
	return broker
}

const (
	fnvOffsetBasis64 = 14695981039346656037
	fnvPrime64       = 1099511628211
)

// shardFor is deterministic per key, so every subscribe, cancel and publish for
// one Session serializes on the same shard. That is what keeps a publish from
// racing the cancel that closes the same channel.
func (b *eventBroker) shardFor(key eventStreamKey) *eventBrokerShard {
	hash := uint64(fnvOffsetBasis64)
	for _, value := range key.tenantID {
		hash ^= uint64(value)
		hash *= fnvPrime64
	}
	for _, value := range key.sessionID {
		hash ^= uint64(value)
		hash *= fnvPrime64
	}
	return &b.shards[hash&(eventBrokerShardCount-1)]
}

func (b *eventBroker) subscribe(tenantID, sessionID uuid.UUID) (<-chan Event, func()) {
	key := eventStreamKey{tenantID: tenantID, sessionID: sessionID}
	shard := b.shardFor(key)
	subscriberID := b.nextID.Add(1)
	channel := make(chan Event, 64)

	shard.mu.Lock()
	if shard.subscribers[key] == nil {
		shard.subscribers[key] = make(map[uint64]chan Event)
	}
	shard.subscribers[key][subscriberID] = channel
	shard.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			shard.mu.Lock()
			defer shard.mu.Unlock()
			subscribers := shard.subscribers[key]
			delete(subscribers, subscriberID)
			if len(subscribers) == 0 {
				delete(shard.subscribers, key)
			}
			close(channel)
		})
	}
	return channel, cancel
}

func (b *eventBroker) publish(event Event) {
	key := eventStreamKey{tenantID: event.TenantID, sessionID: event.SessionID}
	shard := b.shardFor(key)

	shard.mu.Lock()
	defer shard.mu.Unlock()

	for _, subscriber := range shard.subscribers[key] {
		select {
		case subscriber <- event:
		default:
			// PostgreSQL remains authoritative. A slow subscriber catches up from its
			// last durable sequence during the next periodic ORM query.
		}
	}
}
