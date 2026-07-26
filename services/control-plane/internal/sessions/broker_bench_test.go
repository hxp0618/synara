package sessions

import (
	"testing"

	"github.com/google/uuid"
)

// Publishes to distinct Sessions from many goroutines: the case a single
// broker-wide mutex serialized.
func BenchmarkEventBrokerPublishDistinctSessions(b *testing.B) {
	broker := newEventBroker()
	tenantID := uuid.New()
	const sessions = 256
	ids := make([]uuid.UUID, sessions)
	for index := range ids {
		ids[index] = uuid.New()
		_, cancel := broker.subscribe(tenantID, ids[index])
		b.Cleanup(cancel)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		index := 0
		for pb.Next() {
			broker.publish(Event{TenantID: tenantID, SessionID: ids[index%sessions], Sequence: 1})
			index++
		}
	})
}
