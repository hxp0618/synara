package executiontargets

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The foundation cache is reachable from the exported ReconcileOnce, so it must
// be safe under concurrent callers. An unguarded map here does not corrupt
// quietly — Go aborts the process with a fatal concurrent map write — so this
// runs under -race with interleaved readers and writers.
func TestFoundationCacheIsSafeForConcurrentReconcilers(t *testing.T) {
	reconciler := &KubernetesReconciler{
		foundation: map[uuid.UUID]kubernetesFoundationState{},
		now:        func() time.Time { return time.Unix(0, 0).UTC() },
	}
	targets := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}

	var waitGroup sync.WaitGroup
	for round := 0; round < 200; round++ {
		for _, targetID := range targets {
			waitGroup.Add(2)
			go func(id uuid.UUID) {
				defer waitGroup.Done()
				reconciler.recordFoundationState(id, "hash", reconciler.now())
			}(targetID)
			go func(id uuid.UUID) {
				defer waitGroup.Done()
				_ = reconciler.foundationState(id)
			}(targetID)
		}
	}
	waitGroup.Wait()

	for _, targetID := range targets {
		if state := reconciler.foundationState(targetID); state.hash != "hash" {
			t.Fatalf("target %s foundation state = %#v", targetID, state)
		}
	}
}
