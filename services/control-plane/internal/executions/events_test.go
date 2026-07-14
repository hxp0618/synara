package executions

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestValidateRuntimeEventContract(t *testing.T) {
	valid := RuntimeEventInput{
		EventID: uuid.New(), EventVersion: RuntimeEventVersionV1,
		EventType: "runtime.output.delta", Payload: map[string]any{"text": "hello"},
	}
	if err := validateRuntimeEventContract(valid); err != nil {
		t.Fatalf("valid Runtime Event was rejected: %v", err)
	}

	tests := []struct {
		name       string
		mutate     func(*RuntimeEventInput)
		wantStatus int
		wantCode   string
	}{
		{
			name: "unsupported version",
			mutate: func(input *RuntimeEventInput) {
				input.EventVersion = RuntimeEventVersionV2 + 1
			},
			wantStatus: 422,
			wantCode:   "runtime_event_version_unsupported",
		},
		{
			name: "missing payload",
			mutate: func(input *RuntimeEventInput) {
				input.Payload = nil
			},
			wantStatus: 400,
			wantCode:   "invalid_runtime_event_payload",
		},
		{
			name: "non JSON payload",
			mutate: func(input *RuntimeEventInput) {
				input.Payload = map[string]any{"value": math.NaN()}
			},
			wantStatus: 400,
			wantCode:   "invalid_runtime_event_payload",
		},
		{
			name: "oversized payload",
			mutate: func(input *RuntimeEventInput) {
				input.Payload = map[string]any{"text": strings.Repeat("x", RuntimeEventMaxPayloadBytes)}
			},
			wantStatus: 413,
			wantCode:   "runtime_event_payload_too_large",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.mutate(&input)
			err := validateRuntimeEventContract(input)
			var apiError *problem.Error
			if !errors.As(err, &apiError) {
				t.Fatalf("expected problem error, got %v", err)
			}
			if apiError.Status != test.wantStatus || apiError.Code != test.wantCode {
				t.Fatalf("error = status %d code %q, want status %d code %q", apiError.Status, apiError.Code, test.wantStatus, test.wantCode)
			}
		})
	}
}

func TestValidateRuntimeEventContractSeparatesLegacyAndCanonicalTypes(t *testing.T) {
	legacy := RuntimeEventInput{
		EventID: uuid.New(), EventVersion: RuntimeEventVersionV1,
		EventType: "provider.extension.future", Payload: map[string]any{"value": true},
	}
	if err := validateRuntimeEventContract(legacy); err != nil {
		t.Fatalf("legacy v1 extension event was rejected: %v", err)
	}

	canonical := RuntimeEventInput{
		EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
		EventType: "content.delta", Payload: map[string]any{"streamKind": "assistant_text", "delta": "hello"},
	}
	if err := validateRuntimeEventContract(canonical); err != nil {
		t.Fatalf("canonical v2 event was rejected: %v", err)
	}

	canonical.EventType = "runtime.output.delta"
	err := validateRuntimeEventContract(canonical)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != 422 || apiError.Code != "runtime_event_type_unsupported" {
		t.Fatalf("unknown canonical type error = %#v, %v", apiError, err)
	}
}

func TestValidateRuntimeEventContractChecksCanonicalPayloadShape(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		payload   map[string]any
		valid     bool
	}{
		{name: "content delta", eventType: "content.delta", payload: map[string]any{"streamKind": "assistant_text", "delta": "hello"}, valid: true},
		{name: "content delta missing stream", eventType: "content.delta", payload: map[string]any{"delta": "hello"}},
		{name: "item lifecycle", eventType: "item.started", payload: map[string]any{"itemType": "command_execution", "status": "inProgress"}, valid: true},
		{name: "item lifecycle invalid item type", eventType: "item.started", payload: map[string]any{"itemType": "shell"}},
		{name: "usage", eventType: "thread.token-usage.updated", payload: map[string]any{"usage": map[string]any{"usedTokens": float64(42), "usedPercent": 10.5}}, valid: true},
		{name: "usage negative", eventType: "thread.token-usage.updated", payload: map[string]any{"usage": map[string]any{"usedTokens": -1}}},
		{name: "approval requested", eventType: "request.opened", payload: map[string]any{"requestId": "approval-1", "requestType": "command_execution_approval", "detail": "Run command"}, valid: true},
		{name: "approval missing request type", eventType: "request.opened", payload: map[string]any{"requestId": "approval-1"}},
		{name: "user input", eventType: "user-input.requested", payload: map[string]any{"requestId": "input-1", "questions": []any{map[string]any{"id": "q1", "header": "Choice", "question": "Pick one", "options": []any{map[string]any{"label": "A", "description": "Option A"}}}}}, valid: true},
		{name: "user input invalid options", eventType: "user-input.requested", payload: map[string]any{"requestId": "input-1", "questions": []any{map[string]any{"id": "q1", "header": "Choice", "question": "Pick one", "options": nil}}}},
		{name: "warning", eventType: "runtime.warning", payload: map[string]any{"message": "Provider emitted an unknown native event"}, valid: true},
		{name: "warning empty", eventType: "runtime.warning", payload: map[string]any{"message": "  "}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := RuntimeEventInput{
				EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
				EventType: test.eventType, Payload: test.payload,
			}
			err := validateRuntimeEventContract(input)
			if test.valid && err != nil {
				t.Fatalf("valid canonical payload was rejected: %v", err)
			}
			if !test.valid {
				var apiError *problem.Error
				if !errors.As(err, &apiError) || apiError.Status != 400 || apiError.Code != "invalid_runtime_event_payload" {
					t.Fatalf("invalid canonical payload error = %#v, %v", apiError, err)
				}
			}
		})
	}
}

func TestValidateProviderResumeFallbackRuntimeWarningPayload(t *testing.T) {
	validPayload := func() map[string]any {
		return map[string]any{
			"message": "Native Provider resume failed before turn activity; authoritative-history fallback selected.",
			"detail": map[string]any{
				"kind":                         "session_resume",
				"attemptedStrategy":            "native-cursor",
				"selectedStrategy":             "authoritative-history",
				"outcome":                      "fallback_selected",
				"reasonCode":                   "session_resume_invalid",
				"fallbackSafety":               "before_turn_activity",
				"authoritativeHistorySequence": float64(42),
				"provider":                     "codex",
			},
		}
	}
	tests := []struct {
		name   string
		mutate func(map[string]any, map[string]any)
		valid  bool
	}{
		{name: "codex invalid cursor", valid: true},
		{name: "claude expired cursor", mutate: func(_ map[string]any, detail map[string]any) {
			detail["provider"] = "claudeAgent"
			detail["reasonCode"] = "session_resume_expired"
		}, valid: true},
		{name: "maximum safe history sequence", mutate: func(_ map[string]any, detail map[string]any) {
			detail["authoritativeHistorySequence"] = float64(9_007_199_254_740_991)
		}, valid: true},
		{name: "missing required field", mutate: func(_ map[string]any, detail map[string]any) {
			delete(detail, "fallbackSafety")
		}},
		{name: "partial fallback marker", mutate: func(_ map[string]any, detail map[string]any) {
			for key := range detail {
				if key != "kind" && key != "provider" {
					delete(detail, key)
				}
			}
		}},
		{name: "wrong attempted strategy", mutate: func(_ map[string]any, detail map[string]any) {
			detail["attemptedStrategy"] = "authoritative-history"
		}},
		{name: "wrong selected strategy", mutate: func(_ map[string]any, detail map[string]any) {
			detail["selectedStrategy"] = "native-cursor"
		}},
		{name: "wrong outcome", mutate: func(_ map[string]any, detail map[string]any) {
			detail["outcome"] = "fallback_succeeded"
		}},
		{name: "wrong reason", mutate: func(_ map[string]any, detail map[string]any) {
			detail["reasonCode"] = "provider_unavailable"
		}},
		{name: "unsafe fallback point", mutate: func(_ map[string]any, detail map[string]any) {
			detail["fallbackSafety"] = "after_turn_activity"
		}},
		{name: "provider alias", mutate: func(_ map[string]any, detail map[string]any) {
			detail["provider"] = "claude"
		}},
		{name: "negative history sequence", mutate: func(_ map[string]any, detail map[string]any) {
			detail["authoritativeHistorySequence"] = float64(-1)
		}},
		{name: "fractional history sequence", mutate: func(_ map[string]any, detail map[string]any) {
			detail["authoritativeHistorySequence"] = 1.5
		}},
		{name: "unsafe history sequence", mutate: func(_ map[string]any, detail map[string]any) {
			detail["authoritativeHistorySequence"] = float64(9_007_199_254_740_992)
		}},
		{name: "raw provider error", mutate: func(_ map[string]any, detail map[string]any) {
			detail["rawError"] = "native provider error"
		}},
		{name: "cursor", mutate: func(_ map[string]any, detail map[string]any) {
			detail["cursor"] = "provider-cursor"
		}},
		{name: "secret", mutate: func(_ map[string]any, detail map[string]any) {
			detail["secret"] = "credential"
		}},
		{name: "unexpected detail field", mutate: func(_ map[string]any, detail map[string]any) {
			detail["extra"] = true
		}},
		{name: "unexpected outer field", mutate: func(payload map[string]any, _ map[string]any) {
			payload["rawError"] = "native provider error"
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := validPayload()
			detail := payload["detail"].(map[string]any)
			if test.mutate != nil {
				test.mutate(payload, detail)
			}
			input := RuntimeEventInput{
				EventID: uuid.New(), EventVersion: RuntimeEventVersionV2,
				EventType: "runtime.warning", Payload: payload,
			}
			err := validateRuntimeEventContract(input)
			if test.valid {
				if err != nil {
					t.Fatalf("valid provider resume fallback warning was rejected: %v", err)
				}
				if !IsProviderResumeFallbackRuntimeWarningPayload(payload) {
					t.Fatal("valid provider resume fallback warning was not identified")
				}
				return
			}
			var apiError *problem.Error
			if !errors.As(err, &apiError) || apiError.Status != 400 || apiError.Code != "invalid_runtime_event_payload" {
				t.Fatalf("invalid provider resume fallback warning error = %#v, %v", apiError, err)
			}
			if IsProviderResumeFallbackRuntimeWarningPayload(payload) {
				t.Fatal("invalid provider resume fallback warning was identified as canonical")
			}
		})
	}
}

func TestPendingInteractionKindIsVersioned(t *testing.T) {
	for _, test := range []struct {
		version   int
		eventType string
		wantKind  string
		want      bool
	}{
		{RuntimeEventVersionV1, "approval.requested", "approval", true},
		{RuntimeEventVersionV1, "user-input.requested", "user-input", true},
		{RuntimeEventVersionV2, "request.opened", "approval", true},
		{RuntimeEventVersionV2, "user-input.requested", "user-input", true},
		{RuntimeEventVersionV2, "approval.requested", "", false},
		{RuntimeEventVersionV1, "request.opened", "", false},
	} {
		kind, pending := pendingInteractionKind(test.version, test.eventType)
		if kind != test.wantKind || pending != test.want {
			t.Fatalf("pendingInteractionKind(%d, %q) = %q, %t; want %q, %t", test.version, test.eventType, kind, pending, test.wantKind, test.want)
		}
	}
}

func TestCanonicalRuntimeEventV2TypesMatchJSONContract(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Runtime Event contract test source")
	}
	contractPath := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "..", "docs", "contracts", "runtime-event-v2.schema.json")
	encoded, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Properties struct {
			EventType struct {
				Enum []string `json:"enum"`
			} `json:"eventType"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(encoded, &contract); err != nil {
		t.Fatalf("decode Runtime Event v2 JSON contract: %v", err)
	}
	if len(contract.Properties.EventType.Enum) == 0 {
		t.Fatal("Runtime Event v2 JSON contract omitted eventType enum")
	}

	contractTypes := make(map[string]struct{}, len(contract.Properties.EventType.Enum))
	for _, eventType := range contract.Properties.EventType.Enum {
		if _, duplicated := contractTypes[eventType]; duplicated {
			t.Fatalf("Runtime Event v2 JSON contract repeats eventType %q", eventType)
		}
		contractTypes[eventType] = struct{}{}
	}
	missingInGo := make([]string, 0)
	for eventType := range contractTypes {
		if !IsCanonicalRuntimeEventV2Type(eventType) {
			missingInGo = append(missingInGo, eventType)
		}
	}
	missingInContract := make([]string, 0)
	for eventType := range canonicalRuntimeEventV2Types {
		if _, found := contractTypes[eventType]; !found {
			missingInContract = append(missingInContract, eventType)
		}
	}
	sort.Strings(missingInGo)
	sort.Strings(missingInContract)
	if len(missingInGo) > 0 || len(missingInContract) > 0 {
		t.Fatalf("Runtime Event v2 type drift: missing in Go=%v missing in JSON contract=%v", missingInGo, missingInContract)
	}
}
