package executions

import (
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestResolvedInteractionRuntimeEventInheritsRequestVersion(t *testing.T) {
	legacy := persistence.ExecutionInteraction{
		RequestID: "approval-v1", EventVersion: RuntimeEventVersionV1, Kind: "approval",
	}
	version, eventType, payload, err := resolvedInteractionRuntimeEvent(legacy, map[string]any{"decision": "accept"})
	if err != nil {
		t.Fatal(err)
	}
	if version != RuntimeEventVersionV1 || eventType != "approval.resolved" || payload["requestId"] != "approval-v1" {
		t.Fatalf("unexpected legacy resolved event: version=%d type=%q payload=%#v", version, eventType, payload)
	}

	canonicalApproval := persistence.ExecutionInteraction{
		RequestID: "approval-v2", EventVersion: RuntimeEventVersionV2, Kind: "approval",
		Payload: map[string]any{"requestType": "command_execution_approval"},
	}
	version, eventType, payload, err = resolvedInteractionRuntimeEvent(
		canonicalApproval, map[string]any{"decision": "decline"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if version != RuntimeEventVersionV2 || eventType != "request.resolved" ||
		payload["requestId"] != "approval-v2" || payload["requestType"] != "command_execution_approval" ||
		payload["decision"] != "decline" {
		t.Fatalf("unexpected canonical approval event: version=%d type=%q payload=%#v", version, eventType, payload)
	}
	if !IsCanonicalRuntimeEventV2Payload(eventType, payload) {
		t.Fatalf("generated approval resolution is not canonical: %#v", payload)
	}

	canonicalUserInput := persistence.ExecutionInteraction{
		RequestID: "input-v2", EventVersion: RuntimeEventVersionV2, Kind: "user-input",
	}
	version, eventType, payload, err = resolvedInteractionRuntimeEvent(
		canonicalUserInput, map[string]any{"answers": map[string]any{"q1": "A"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if version != RuntimeEventVersionV2 || eventType != "user-input.resolved" || payload["requestId"] != "input-v2" {
		t.Fatalf("unexpected canonical user-input event: version=%d type=%q payload=%#v", version, eventType, payload)
	}
	if !IsCanonicalRuntimeEventV2Payload(eventType, payload) {
		t.Fatalf("generated user-input resolution is not canonical: %#v", payload)
	}
}

func TestResolvedInteractionRuntimeEventRejectsCorruptVersionedState(t *testing.T) {
	for _, interaction := range []persistence.ExecutionInteraction{
		{RequestID: "future", EventVersion: 3, Kind: "approval"},
		{RequestID: "approval", EventVersion: RuntimeEventVersionV2, Kind: "approval", Payload: map[string]any{}},
		{RequestID: "input", EventVersion: RuntimeEventVersionV2, Kind: "user-input"},
	} {
		if _, _, _, err := resolvedInteractionRuntimeEvent(interaction, map[string]any{}); err == nil {
			t.Fatalf("corrupt interaction state was accepted: %#v", interaction)
		}
	}
}
