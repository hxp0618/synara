package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func TestDeveloperExecutionAndControlCommandProjectionsExcludeRuntimeAuthority(t *testing.T) {
	now := time.Now().UTC()
	workerID, manifestID, bindingID := uuid.New(), uuid.New(), uuid.New()
	execution := projectDeveloperExecution(executions.Execution{
		ID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(), Attempt: 1, Status: "running",
		ExecutionTargetID: uuid.New(), TargetKind: "kubernetes", Provider: stringPointerHTTPAPI("codex"),
		WorkerID: &workerID, WorkerManifestID: &manifestID, ProviderRuntimeBindingID: &bindingID,
		Generation: 7, QueuedAt: now,
	})
	command := projectDeveloperControlCommand(executions.ControlCommand{
		ID: uuid.New(), ExecutionID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		Provider: "codex", CommandType: "SteerTurn", Status: "delivered", RequestedAt: now,
		Payload: map[string]any{"inputText": "sensitive steer text"}, DeliveryWorkerID: &workerID,
		DeliveryGeneration: int64PointerHTTPAPI(7), DeliveryAttempts: 3,
	})
	encodedExecution, err := json.Marshal(execution)
	if err != nil {
		t.Fatal(err)
	}
	encodedCommand, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"workerId", "workerManifestId", "providerRuntimeBindingId", "generation"} {
		if containsJSONField(string(encodedExecution), forbidden) {
			t.Fatalf("Execution projection leaked %q: %s", forbidden, encodedExecution)
		}
	}
	for _, forbidden := range []string{"payload", "inputText", "deliveryWorkerId", "deliveryGeneration", "deliveryAttempts"} {
		if containsJSONField(string(encodedCommand), forbidden) {
			t.Fatalf("Control command projection leaked %q: %s", forbidden, encodedCommand)
		}
	}
	queued := projectDeveloperQueuedSessionOperation(executions.QueuedSessionOperation{
		Type: "review", Turn: sessions.Turn{ID: uuid.New(), SessionID: uuid.New(), TurnKind: "review"},
		ExecutionID: uuid.New(),
		ControlCommand: executions.ControlCommand{
			ID: uuid.New(), ExecutionID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
			Provider: "codex", CommandType: "StartReview", Status: "pending", RequestedAt: now,
			Payload:          map[string]any{"target": map[string]any{"type": "baseBranch", "branch": "sensitive"}},
			DeliveryWorkerID: &workerID, DeliveryGeneration: int64PointerHTTPAPI(7),
		},
	})
	encodedQueued, err := json.Marshal(queued)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"payload", "deliveryWorkerId", "deliveryGeneration"} {
		if containsJSONField(string(encodedQueued), forbidden) {
			t.Fatalf("Queued operation projection leaked %q: %s", forbidden, encodedQueued)
		}
	}
	if strings.Contains(string(encodedQueued), "sensitive") {
		t.Fatalf("Queued operation projection leaked command payload content: %s", encodedQueued)
	}
}

func stringPointerHTTPAPI(value string) *string { return &value }

func int64PointerHTTPAPI(value int64) *int64 { return &value }
