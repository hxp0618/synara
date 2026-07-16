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
		{name: "inline turn diff", eventType: "turn.diff.updated", payload: map[string]any{"unifiedDiff": "diff --git a/a b/a"}, valid: true},
		{name: "artifact turn diff", eventType: "turn.diff.updated", payload: map[string]any{"artifact": map[string]any{
			"artifactId": "artifact-diff-1", "contentType": "text/x-diff; charset=utf-8", "sizeBytes": 131072,
			"sha256": strings.Repeat("a", 64), "fileCount": 2, "additions": 120, "deletions": 40,
		}}, valid: true},
		{name: "turn diff cannot mix inline and artifact", eventType: "turn.diff.updated", payload: map[string]any{
			"unifiedDiff": "patch", "artifact": map[string]any{"artifactId": "artifact-diff-1"},
		}},
		{name: "artifact turn diff requires lowercase sha", eventType: "turn.diff.updated", payload: map[string]any{"artifact": map[string]any{
			"artifactId": "artifact-diff-1", "contentType": "text/x-diff", "sizeBytes": 1,
			"sha256": strings.Repeat("A", 64), "fileCount": 1, "additions": 1, "deletions": 0,
		}}},
		{name: "artifact turn diff requires canonical content type", eventType: "turn.diff.updated", payload: map[string]any{"artifact": map[string]any{
			"artifactId": "artifact-diff-1", "contentType": "text/plain", "sizeBytes": 1,
			"sha256": strings.Repeat("a", 64), "fileCount": 1, "additions": 1, "deletions": 0,
		}}},
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

func TestCanonicalRuntimeEventV2TerminalContentDelta(t *testing.T) {
	validUTF8 := map[string]any{
		"streamKind": "command_output", "delta": "A🙂", "terminalId": "terminal-1",
		"encoding": "utf-8", "byteOffset": 0, "byteLength": 5,
	}
	if !IsCanonicalRuntimeEventV2Payload("content.delta", validUTF8) {
		t.Fatal("valid UTF-8 command output was rejected")
	}

	for _, field := range []string{"terminalId", "encoding", "byteOffset", "byteLength"} {
		payload := cloneRuntimeEventPayload(validUTF8)
		delete(payload, field)
		if IsCanonicalRuntimeEventV2Payload("content.delta", payload) {
			t.Fatalf("command output missing %s was accepted", field)
		}
	}

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong UTF-8 length", mutate: func(payload map[string]any) { payload["byteLength"] = 4 }},
		{name: "invalid UTF-8", mutate: func(payload map[string]any) {
			payload["delta"] = string([]byte{0xff})
			payload["byteLength"] = 1
		}},
		{name: "negative offset", mutate: func(payload map[string]any) { payload["byteOffset"] = -1 }},
		{name: "negative length", mutate: func(payload map[string]any) { payload["byteLength"] = -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := cloneRuntimeEventPayload(validUTF8)
			test.mutate(payload)
			if IsCanonicalRuntimeEventV2Payload("content.delta", payload) {
				t.Fatal("invalid UTF-8 command output was accepted")
			}
		})
	}

	validBinary := map[string]any{
		"streamKind": "command_output", "delta": "AAEC/w==", "terminalId": "terminal-1",
		"encoding": "binary", "byteOffset": 8, "byteLength": 4,
	}
	if !IsCanonicalRuntimeEventV2Payload("content.delta", validBinary) {
		t.Fatal("valid binary command output was rejected")
	}
	for _, test := range []struct {
		name       string
		delta      string
		byteLength int
	}{
		{name: "noncanonical padding bits", delta: "AB==", byteLength: 1},
		{name: "embedded newline", delta: "AAEC/w==\n", byteLength: 4},
		{name: "invalid alphabet", delta: "AAEC_w==", byteLength: 4},
		{name: "decoded length mismatch", delta: "AAEC/w==", byteLength: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := cloneRuntimeEventPayload(validBinary)
			payload["delta"] = test.delta
			payload["byteLength"] = test.byteLength
			if IsCanonicalRuntimeEventV2Payload("content.delta", payload) {
				t.Fatal("invalid binary command output was accepted")
			}
		})
	}

	if !IsCanonicalRuntimeEventV2Payload("content.delta", map[string]any{
		"streamKind": "assistant_text", "delta": "legacy-compatible",
	}) {
		t.Fatal("non-command content delta lost backward compatibility")
	}
}

func TestCanonicalRuntimeEventV2TerminalLifecycleData(t *testing.T) {
	artifactID := uuid.New().String()
	basePayload := func(eventType string, terminal map[string]any) map[string]any {
		terminal["terminalId"] = "terminal-1"
		terminal["eventType"] = eventType
		return map[string]any{
			"itemType": "command_execution", "status": "inProgress",
			"data": map[string]any{"provider": "codex", "terminal": terminal},
		}
	}

	validPayloads := []map[string]any{
		basePayload("terminal.started", map[string]any{
			"commandSummary": "bun run test", "cwdLabel": "apps/provider-host",
		}),
		basePayload("terminal.output.reference", map[string]any{
			"artifactId": artifactID, "offset": 0, "length": 1_048_576,
			"segmentIndex": 0, "encoding": "binary",
		}),
		basePayload("terminal.exited", map[string]any{
			"totalBytes": 64, "previewBytes": 64, "segmentCount": 0, "truncated": false,
			"exitCode": 0,
		}),
		basePayload("terminal.failed", map[string]any{
			"totalBytes": 65_536, "previewBytes": 32_768, "segmentCount": 1, "truncated": true,
			"exitCode": 137, "signal": "SIGKILL", "failureKind": "oom",
		}),
	}
	for index, payload := range validPayloads {
		if !IsCanonicalRuntimeEventV2Payload("item.updated", payload) {
			t.Fatalf("valid terminal lifecycle payload %d was rejected", index)
		}
	}

	referenceTerminal := func() map[string]any {
		return map[string]any{
			"terminalId": "terminal-1", "eventType": "terminal.output.reference",
			"artifactId": artifactID, "offset": 0, "length": 1, "segmentIndex": 0, "encoding": "utf-8",
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "invalid artifact UUID", mutate: func(terminal map[string]any) { terminal["artifactId"] = "not-a-uuid" }},
		{name: "noncanonical artifact UUID", mutate: func(terminal map[string]any) { terminal["artifactId"] = strings.ReplaceAll(artifactID, "-", "") }},
		{name: "negative offset", mutate: func(terminal map[string]any) { terminal["offset"] = -1 }},
		{name: "negative length", mutate: func(terminal map[string]any) { terminal["length"] = -1 }},
		{name: "negative segment", mutate: func(terminal map[string]any) { terminal["segmentIndex"] = -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			terminal := referenceTerminal()
			test.mutate(terminal)
			if IsCanonicalRuntimeEventV2Payload("item.updated", basePayload("terminal.output.reference", terminal)) {
				t.Fatal("invalid terminal reference was accepted")
			}
		})
	}

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "preview exceeds total", mutate: func(terminal map[string]any) { terminal["previewBytes"] = 65 }},
		{name: "negative segment count", mutate: func(terminal map[string]any) { terminal["segmentCount"] = -1 }},
		{name: "invalid failure kind", mutate: func(terminal map[string]any) { terminal["failureKind"] = "crash" }},
		{name: "missing truncation marker", mutate: func(terminal map[string]any) { delete(terminal, "truncated") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			terminal := map[string]any{
				"totalBytes": 64, "previewBytes": 64, "segmentCount": 1, "truncated": false,
				"failureKind": "exit",
			}
			test.mutate(terminal)
			if IsCanonicalRuntimeEventV2Payload("item.completed", basePayload("terminal.failed", terminal)) {
				t.Fatal("invalid terminal completion was accepted")
			}
		})
	}

	if IsCanonicalRuntimeEventV2Payload("item.started", map[string]any{
		"itemType": "mcp_tool_call",
		"data": map[string]any{"terminal": map[string]any{
			"terminalId": "terminal-1", "eventType": "terminal.started",
		}},
	}) {
		t.Fatal("terminal lifecycle data was accepted on a non-command item")
	}
}

func cloneRuntimeEventPayload(payload map[string]any) map[string]any {
	cloned := make(map[string]any, len(payload))
	for key, value := range payload {
		cloned[key] = value
	}
	return cloned
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
