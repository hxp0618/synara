package executions

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestDeterministicWorkerClaimFactIDUsesBusinessClaimIdentity(t *testing.T) {
	worker := persistence.WorkerInstance{ID: uuid.New(), Incarnation: 3}
	executionID := uuid.New()
	generation := int64(7)
	base := workerClaimFactInput{
		Worker:              worker,
		ClaimKind:           workerClaimKindExecution,
		RequestID:           "request-a",
		ClaimedAt:           time.Now().UTC(),
		ExecutionID:         &executionID,
		ExecutionGeneration: &generation,
	}

	first := deterministicWorkerClaimFactID(base)
	replayed := base
	replayed.RequestID = "request-b"
	if got := deterministicWorkerClaimFactID(replayed); got != first {
		t.Fatalf("same execution generation produced claim fact id %s, want %s", got, first)
	}

	otherExecution := base
	otherExecutionID := uuid.New()
	otherExecution.ExecutionID = &otherExecutionID
	otherExecution.RequestID = base.RequestID
	if got := deterministicWorkerClaimFactID(otherExecution); got == first {
		t.Fatalf("different execution generation reused claim fact id %s", got)
	}

	cleanupGeneration := generation
	cleanup := base
	cleanup.ClaimKind = workerClaimKindWorkspaceCleanup
	cleanup.ExecutionID = nil
	cleanup.ExecutionGeneration = nil
	cleanup.CleanupCommandID = &executionID
	cleanup.CleanupDispatchGeneration = &cleanupGeneration
	if got := deterministicWorkerClaimFactID(cleanup); got == first {
		t.Fatalf("execution and cleanup business identities collided at %s", got)
	}
}
