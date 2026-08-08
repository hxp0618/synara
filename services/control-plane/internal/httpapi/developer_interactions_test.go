package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestDeveloperInteractionProjectionOmitsWorkerAndDeliveryInternals(t *testing.T) {
	deliveryError := "ssh://operator:secret@internal.example failed"
	workerID := uuid.New()
	item := projectDeveloperInteraction(executions.Interaction{
		ID: uuid.New(), ExecutionID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		WorkerID: workerID, Generation: 7, Provider: "codex", RequestID: "approval-1",
		Kind: "approval", Status: "resolved", Payload: map[string]any{"detail": "Run tests"},
		DeliveryStatus: "failed", DeliveryWorkerID: &workerID, DeliveryAttempts: 3, DeliveryError: &deliveryError,
	})
	payload, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"workerId", "generation", "resolvedBy", "resolutionKind", "resolutionCommandId",
		"deliveryStatus", "deliveryWorkerId", "deliveryGeneration", "deliveryAttempts",
		"deliveryAvailableAt", "deliveredAt", "acknowledgedAt", "deliveryError",
	} {
		if _, present := decoded[forbidden]; present {
			t.Fatalf("developer Interaction leaked %q: %s", forbidden, payload)
		}
	}
	if string(payload) == "" || strings.Contains(string(payload), "secret") {
		t.Fatalf("developer Interaction leaked delivery error: %s", payload)
	}
}
