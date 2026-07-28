package agentd

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestRegisterWorkerRetriesOnlyPendingSandboxAllocation(t *testing.T) {
	attempts := 0
	expected := executions.RegisteredWorker{Worker: executions.Worker{ID: uuid.New()}}
	registered, err := registerWorkerAfterSandboxAllocationBound(
		context.Background(), time.Millisecond, time.Second,
		func(context.Context) (executions.RegisteredWorker, error) {
			attempts++
			if attempts < 3 {
				return executions.RegisteredWorker{}, &controlPlaneProblem{
					Status: http.StatusConflict,
					Code:   "kubernetes_sandbox_allocation_not_bound",
				}
			}
			return expected, nil
		},
	)
	if err != nil || registered.Worker.ID != expected.Worker.ID || attempts != 3 {
		t.Fatalf("registration retry = %#v, %v, attempts=%d", registered, err, attempts)
	}
}

func TestRegisterWorkerDoesNotRetryIdentityRejection(t *testing.T) {
	attempts := 0
	expected := &controlPlaneProblem{Status: http.StatusConflict, Code: "kubernetes_sandbox_allocation_generation_stale"}
	_, err := registerWorkerAfterSandboxAllocationBound(
		context.Background(), time.Millisecond, time.Second,
		func(context.Context) (executions.RegisteredWorker, error) {
			attempts++
			return executions.RegisteredWorker{}, expected
		},
	)
	if !errors.Is(err, expected) || attempts != 1 {
		t.Fatalf("terminal registration rejection = %v, attempts=%d", err, attempts)
	}
}

func TestRegisterWorkerPendingAllocationRetryIsBounded(t *testing.T) {
	attempts := 0
	_, err := registerWorkerAfterSandboxAllocationBound(
		context.Background(), time.Millisecond, 5*time.Millisecond,
		func(context.Context) (executions.RegisteredWorker, error) {
			attempts++
			return executions.RegisteredWorker{}, &controlPlaneProblem{
				Status: http.StatusConflict,
				Code:   "kubernetes_sandbox_allocation_not_bound",
			}
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) || attempts < 2 {
		t.Fatalf("bounded registration retry = %v, attempts=%d", err, attempts)
	}
}
