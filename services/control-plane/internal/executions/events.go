package executions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func (s *Service) AppendRuntimeEvent(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input RuntimeEventInput,
	requestID string,
) (OperationResult[RuntimeEventResult], error) {
	input.EventType = strings.TrimSpace(input.EventType)
	if input.EventID == uuid.Nil {
		return OperationResult[RuntimeEventResult]{}, problem.New(400, "invalid_event_id", "eventId is required.")
	}
	if err := validateRuntimeEventContract(input); err != nil {
		return OperationResult[RuntimeEventResult]{}, err
	}
	if input.OccurredAt.IsZero() {
		input.OccurredAt = s.now()
	}

	var appended persistence.SessionEvent
	result, err := runIdempotent(ctx, s, worker, requestID, "execution.runtime_event", struct {
		ExecutionID uuid.UUID         `json:"executionId"`
		Input       RuntimeEventInput `json:"input"`
	}{executionID, input}, 201, func(tx *gorm.DB) (RuntimeEventResult, error) {
		var existing persistence.SessionEvent
		existingErr := tx.WithContext(ctx).
			Where("tenant_id = ? AND event_id = ?", input.TenantID, input.EventID).Take(&existing).Error
		if existingErr == nil {
			if existing.ExecutionID == nil || *existing.ExecutionID != executionID ||
				existing.WorkerID == nil || *existing.WorkerID != worker.ID ||
				existing.Generation == nil || *existing.Generation != input.Generation ||
				existing.EventType != input.EventType || existing.EventVersion != input.EventVersion ||
				!sameJSON(existing.Payload, input.Payload) {
				return RuntimeEventResult{}, problem.New(409, "event_id_reused", "eventId was already used for different event content.")
			}
			return RuntimeEventResult{
				EventID: existing.EventID, SessionID: existing.SessionID,
				Sequence: existing.Sequence, EventVersion: existing.EventVersion,
			}, nil
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return RuntimeEventResult{}, problem.Wrap(500, "runtime_event_lookup_failed", "Failed to inspect the runtime event.", existingErr)
		}
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return RuntimeEventResult{}, err
		}
		if kind, pending := pendingInteractionKind(input.EventVersion, input.EventType); pending {
			if err := s.persistPendingInteraction(
				ctx, tx, worker, &execution, input.EventVersion, kind, input.Payload, input.OccurredAt,
			); err != nil {
				return RuntimeEventResult{}, err
			}
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, execution.TenantID, execution.SessionID, sessions.InternalEventInput{
			EventID: &input.EventID, EventVersion: input.EventVersion,
			EventType: input.EventType, ActorType: "worker", ActorID: &worker.ID,
			ExecutionID: &execution.ID, WorkerID: &worker.ID, Generation: &execution.Generation,
			Payload: input.Payload, OccurredAt: &input.OccurredAt,
		})
		if err != nil {
			return RuntimeEventResult{}, err
		}
		return RuntimeEventResult{
			EventID: appended.EventID, SessionID: appended.SessionID,
			Sequence: appended.Sequence, EventVersion: appended.EventVersion,
		}, nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return result, err
}

var canonicalRuntimeEventV2Types = map[string]struct{}{
	"session.started": {}, "session.configured": {}, "session.state.changed": {}, "session.exited": {},
	"thread.started": {}, "thread.state.changed": {}, "thread.metadata.updated": {}, "thread.token-usage.updated": {},
	"thread.realtime.started": {}, "thread.realtime.item-added": {}, "thread.realtime.audio.delta": {},
	"thread.realtime.error": {}, "thread.realtime.closed": {},
	"turn.started": {}, "turn.completed": {}, "turn.aborted": {}, "turn.tasks.updated": {},
	"turn.proposed.delta": {}, "turn.proposed.completed": {}, "turn.diff.updated": {}, "turn.steered": {},
	"item.started": {}, "item.updated": {}, "item.completed": {}, "content.delta": {},
	"request.opened": {}, "request.resolved": {}, "user-input.requested": {}, "user-input.resolved": {},
	"task.started": {}, "task.progress": {}, "task.updated": {}, "task.completed": {},
	"hook.started": {}, "hook.progress": {}, "hook.completed": {},
	"tool.progress": {}, "tool.summary": {}, "auth.status": {}, "account.updated": {},
	"account.rate-limits.updated": {}, "mcp.status.updated": {}, "mcp.oauth.completed": {},
	"model.rerouted": {}, "config.warning": {}, "deprecation.notice": {}, "files.persisted": {},
	"runtime.warning": {}, "runtime.error": {},
}

func IsCanonicalRuntimeEventV2Type(eventType string) bool {
	_, ok := canonicalRuntimeEventV2Types[eventType]
	return ok
}

func validateRuntimeEventContract(input RuntimeEventInput) error {
	if input.EventVersion != RuntimeEventVersionV1 && input.EventVersion != RuntimeEventVersionV2 {
		return &problem.Error{
			Status:  422,
			Code:    "runtime_event_version_unsupported",
			Message: "eventVersion is not supported by this Control Plane.",
			Details: map[string]any{
				"minimumSupported": RuntimeEventVersionV1,
				"maximumSupported": RuntimeEventVersionV2,
			},
		}
	}
	if input.EventType == "" || len(input.EventType) > 160 || strings.ContainsAny(input.EventType, "\r\n\t") {
		return problem.New(400, "invalid_event_type", "eventType must contain between 1 and 160 characters.")
	}
	if input.Payload == nil {
		return problem.New(400, "invalid_runtime_event_payload", "payload must be a JSON object.")
	}
	encoded, err := json.Marshal(input.Payload)
	if err != nil {
		return problem.New(400, "invalid_runtime_event_payload", "payload must be a valid JSON object.")
	}
	if len(encoded) > RuntimeEventMaxPayloadBytes {
		return &problem.Error{
			Status:  413,
			Code:    "runtime_event_payload_too_large",
			Message: "Runtime Event payload is too large; upload larger content as an Artifact and reference artifactId.",
			Details: map[string]any{"maximumBytes": RuntimeEventMaxPayloadBytes},
		}
	}
	if input.EventVersion == RuntimeEventVersionV2 {
		if !IsCanonicalRuntimeEventV2Type(input.EventType) {
			return &problem.Error{
				Status: 422, Code: "runtime_event_type_unsupported",
				Message: "eventType is not part of the canonical Runtime Event v2 contract.",
				Details: map[string]any{"eventVersion": RuntimeEventVersionV2, "eventType": input.EventType},
			}
		}
		if !IsCanonicalRuntimeEventV2Payload(input.EventType, input.Payload) {
			return &problem.Error{
				Status: 400, Code: "invalid_runtime_event_payload",
				Message: "payload does not match the canonical Runtime Event v2 contract.",
				Details: map[string]any{"eventVersion": RuntimeEventVersionV2, "eventType": input.EventType},
			}
		}
	}
	return nil
}

func IsCanonicalRuntimeEventV2Payload(eventType string, payload map[string]any) bool {
	switch eventType {
	case "session.started":
		return optionalTrimmedString(payload, "message")
	case "session.configured":
		return requiredObject(payload, "config")
	case "session.state.changed":
		return requiredEnum(payload, "state", "starting", "ready", "running", "waiting", "stopped", "error") &&
			optionalTrimmedString(payload, "reason")
	case "session.exited":
		return optionalTrimmedString(payload, "reason") && optionalBool(payload, "recoverable") &&
			optionalEnum(payload, "exitKind", "graceful", "error")
	case "thread.started":
		return optionalTrimmedString(payload, "providerThreadId")
	case "thread.state.changed":
		return requiredEnum(payload, "state", "active", "idle", "archived", "closed", "compacted", "error")
	case "thread.metadata.updated":
		return optionalTrimmedString(payload, "name") && optionalObject(payload, "metadata")
	case "thread.token-usage.updated":
		usage, ok := objectField(payload, "usage")
		return ok && validTokenUsage(usage)
	case "thread.realtime.started":
		return optionalTrimmedString(payload, "realtimeSessionId")
	case "thread.realtime.item-added":
		return hasField(payload, "item")
	case "thread.realtime.audio.delta":
		return hasField(payload, "audio")
	case "thread.realtime.error":
		return requiredTrimmedString(payload, "message")
	case "thread.realtime.closed":
		return optionalTrimmedString(payload, "reason")
	case "turn.started":
		return optionalTrimmedString(payload, "model") && optionalTrimmedString(payload, "effort")
	case "turn.completed":
		return requiredEnum(payload, "state", "completed", "failed", "interrupted", "cancelled") &&
			optionalNullableTrimmedString(payload, "stopReason") && optionalObject(payload, "modelUsage") &&
			optionalNumber(payload, "totalCostUsd") && optionalNumber(payload, "cumulativeCostUsd") &&
			optionalTrimmedString(payload, "errorMessage")
	case "turn.aborted":
		return requiredTrimmedString(payload, "reason")
	case "turn.tasks.updated":
		values, ok := arrayField(payload, "tasks")
		return ok && optionalNullableTrimmedString(payload, "explanation") && allObjects(values, func(task map[string]any) bool {
			return requiredTrimmedString(task, "task") && requiredEnum(task, "status", "pending", "inProgress", "completed")
		})
	case "turn.proposed.delta":
		return requiredString(payload, "delta")
	case "turn.proposed.completed":
		return requiredTrimmedString(payload, "planMarkdown")
	case "turn.diff.updated":
		if _, inline := payload["unifiedDiff"]; inline {
			_, artifact := payload["artifact"]
			return !artifact && requiredString(payload, "unifiedDiff")
		}
		artifact, ok := objectField(payload, "artifact")
		return ok && requiredTrimmedString(artifact, "artifactId") &&
			requiredEnum(artifact, "contentType", "text/x-diff; charset=utf-8") &&
			requiredNonNegativeInteger(artifact, "sizeBytes") &&
			requiredLowerSHA256(artifact, "sha256") && requiredNonNegativeInteger(artifact, "fileCount") &&
			requiredNonNegativeInteger(artifact, "additions") && requiredNonNegativeInteger(artifact, "deletions")
	case "turn.steered":
		return requiredTrimmedString(payload, "message")
	case "item.started", "item.updated", "item.completed":
		return validItemLifecycle(payload)
	case "content.delta":
		return validContentDelta(payload)
	case "request.opened":
		return requiredCanonicalRequestType(payload) && optionalTrimmedString(payload, "detail")
	case "request.resolved":
		return requiredCanonicalRequestType(payload) && optionalTrimmedString(payload, "decision")
	case "user-input.requested":
		questions, ok := arrayField(payload, "questions")
		return ok && allObjects(questions, validUserInputQuestion)
	case "user-input.resolved":
		return requiredObject(payload, "answers")
	case "task.started":
		return requiredTrimmedString(payload, "taskId") && optionalTrimmedString(payload, "description") &&
			optionalTrimmedString(payload, "taskType")
	case "task.progress":
		return requiredTrimmedString(payload, "taskId") && requiredTrimmedString(payload, "description") &&
			optionalTrimmedString(payload, "summary") && optionalTrimmedString(payload, "lastToolName")
	case "task.updated":
		return requiredTrimmedString(payload, "taskId") &&
			optionalEnum(payload, "status", "pending", "running", "completed", "failed", "killed", "paused") &&
			optionalTrimmedString(payload, "error") && optionalBool(payload, "isBackgrounded") &&
			optionalTrimmedString(payload, "toolUseId") && optionalTrimmedString(payload, "workflowTaskId") &&
			optionalTrimmedString(payload, "workflowRunId") && optionalTrimmedString(payload, "workflowScriptPath")
	case "task.completed":
		return requiredTrimmedString(payload, "taskId") && requiredEnum(payload, "status", "completed", "failed", "stopped") &&
			optionalTrimmedString(payload, "summary")
	case "hook.started":
		return requiredTrimmedString(payload, "hookId") && requiredTrimmedString(payload, "hookName") &&
			requiredTrimmedString(payload, "hookEvent")
	case "hook.progress":
		return requiredTrimmedString(payload, "hookId") && optionalString(payload, "output") &&
			optionalString(payload, "stdout") && optionalString(payload, "stderr")
	case "hook.completed":
		return requiredTrimmedString(payload, "hookId") && requiredEnum(payload, "outcome", "success", "error", "cancelled") &&
			optionalString(payload, "output") && optionalString(payload, "stdout") && optionalString(payload, "stderr") &&
			optionalInteger(payload, "exitCode")
	case "tool.progress":
		return optionalTrimmedString(payload, "toolUseId") && optionalTrimmedString(payload, "toolName") &&
			optionalTrimmedString(payload, "summary") && optionalNumber(payload, "elapsedSeconds")
	case "tool.summary":
		return requiredTrimmedString(payload, "summary") && optionalStringArray(payload, "precedingToolUseIds", true)
	case "auth.status":
		return optionalBool(payload, "isAuthenticating") && optionalStringArray(payload, "output", false) &&
			optionalTrimmedString(payload, "error")
	case "account.updated":
		return hasField(payload, "account")
	case "account.rate-limits.updated":
		return hasField(payload, "rateLimits")
	case "mcp.status.updated":
		return hasField(payload, "status")
	case "mcp.oauth.completed":
		return requiredBool(payload, "success") && optionalTrimmedString(payload, "name") && optionalTrimmedString(payload, "error")
	case "model.rerouted":
		return requiredTrimmedString(payload, "fromModel") && requiredTrimmedString(payload, "toModel") &&
			requiredTrimmedString(payload, "reason")
	case "config.warning":
		return requiredTrimmedString(payload, "summary") && optionalTrimmedString(payload, "details") && optionalTrimmedString(payload, "path")
	case "deprecation.notice":
		return requiredTrimmedString(payload, "summary") && optionalTrimmedString(payload, "details")
	case "files.persisted":
		files, ok := arrayField(payload, "files")
		if !ok || !allObjects(files, func(file map[string]any) bool {
			return requiredTrimmedString(file, "filename") && requiredTrimmedString(file, "fileId")
		}) {
			return false
		}
		failed, found := payload["failed"]
		if !found {
			return true
		}
		values, ok := failed.([]any)
		return ok && allObjects(values, func(file map[string]any) bool {
			return requiredTrimmedString(file, "filename") && requiredTrimmedString(file, "error")
		})
	case "runtime.warning":
		if !requiredTrimmedString(payload, "message") {
			return false
		}
		detail, ok := objectField(payload, "detail")
		if !ok || !providerResumeFallbackWarningDetailCandidate(detail) {
			return true
		}
		return IsProviderResumeFallbackRuntimeWarningPayload(payload)
	case "runtime.error":
		return requiredTrimmedString(payload, "message") && optionalEnum(payload, "class", "provider_error", "transport_error", "permission_error", "validation_error", "unknown")
	default:
		return false
	}
}

// IsProviderResumeFallbackRuntimeWarningPayload identifies the one warning semantic slot
// that agentd may replay with a deterministic Event ID. Keep this stricter than generic
// runtime.warning detail so resume fallback outcomes cannot carry Provider errors or secrets.
func IsProviderResumeFallbackRuntimeWarningPayload(payload map[string]any) bool {
	if !exactObjectFields(payload, "message", "detail") || !requiredTrimmedString(payload, "message") {
		return false
	}
	detail, ok := objectField(payload, "detail")
	return ok && validProviderResumeFallbackWarningDetail(detail)
}

func providerResumeFallbackWarningDetailCandidate(detail map[string]any) bool {
	for _, field := range []string{
		"kind", "attemptedStrategy", "selectedStrategy", "outcome", "reasonCode", "fallbackSafety",
		"authoritativeHistorySequence",
	} {
		if _, found := detail[field]; found {
			return true
		}
	}
	return false
}

func validProviderResumeFallbackWarningDetail(detail map[string]any) bool {
	return exactObjectFields(
		detail,
		"kind",
		"attemptedStrategy",
		"selectedStrategy",
		"outcome",
		"reasonCode",
		"fallbackSafety",
		"authoritativeHistorySequence",
		"provider",
	) &&
		requiredEnum(detail, "kind", "session_resume") &&
		requiredEnum(detail, "attemptedStrategy", "native-cursor") &&
		requiredEnum(detail, "selectedStrategy", "authoritative-history") &&
		requiredEnum(detail, "outcome", "fallback_selected") &&
		requiredEnum(detail, "reasonCode", "session_resume_invalid", "session_resume_expired") &&
		requiredEnum(detail, "fallbackSafety", "before_turn_activity") &&
		requiredNonNegativeSafeInteger(detail, "authoritativeHistorySequence") &&
		requiredEnum(detail, "provider", "codex", "claudeAgent")
}

func validTokenUsage(usage map[string]any) bool {
	if !requiredNonNegativeInteger(usage, "usedTokens") ||
		!optionalBoundedNumber(usage, "usedPercent", 0, 100) || !optionalBool(usage, "compactsAutomatically") {
		return false
	}
	for _, field := range []string{
		"totalProcessedTokens", "maxTokens", "inputTokens", "cachedInputTokens", "outputTokens",
		"reasoningOutputTokens", "lastUsedTokens", "lastInputTokens", "lastCachedInputTokens",
		"lastOutputTokens", "lastReasoningOutputTokens", "toolUses", "durationMs",
	} {
		if !optionalNonNegativeInteger(usage, field) {
			return false
		}
	}
	if value, found := usage["maxTokens"]; found && (!isJSONInteger(value) || numberValue(value) <= 0) {
		return false
	}
	return true
}

func validItemLifecycle(payload map[string]any) bool {
	if !requiredEnum(payload, "itemType", "user_message", "assistant_message", "reasoning", "plan",
		"command_execution", "file_change", "mcp_tool_call", "dynamic_tool_call", "collab_agent_tool_call",
		"web_search", "image_view", "image_generation", "review_entered", "review_exited",
		"context_compaction", "error", "unknown") &&
		optionalEnum(payload, "status", "inProgress", "completed", "failed", "declined") &&
		optionalTrimmedString(payload, "title") && optionalTrimmedString(payload, "detail") {
		return false
	}
	dataValue, found := payload["data"]
	if !found {
		return true
	}
	data, ok := dataValue.(map[string]any)
	if !ok || data == nil {
		return true
	}
	terminalValue, found := data["terminal"]
	if !found {
		return true
	}
	if payload["itemType"] != "command_execution" {
		return false
	}
	terminal, ok := terminalValue.(map[string]any)
	return ok && terminal != nil && validTerminalLifecycleData(terminal)
}

func validContentDelta(payload map[string]any) bool {
	if !requiredEnum(payload, "streamKind", "assistant_text", "reasoning_text", "reasoning_summary_text", "plan_text", "command_output", "file_change_output", "unknown") ||
		!requiredString(payload, "delta") || !optionalInteger(payload, "contentIndex") || !optionalInteger(payload, "summaryIndex") ||
		!optionalTrimmedString(payload, "terminalId") || !optionalEnum(payload, "encoding", "utf-8", "binary") ||
		!optionalNonNegativeInteger(payload, "byteOffset") || !optionalNonNegativeInteger(payload, "byteLength") ||
		!optionalBool(payload, "truncated") {
		return false
	}

	streamKind, _ := payload["streamKind"].(string)
	encoding, hasEncoding := payload["encoding"].(string)
	if streamKind == "command_output" {
		if !requiredTrimmedString(payload, "terminalId") ||
			!requiredEnum(payload, "encoding", "utf-8", "binary") ||
			!requiredNonNegativeInteger(payload, "byteOffset") ||
			!requiredNonNegativeInteger(payload, "byteLength") {
			return false
		}
		return validContentDeltaByteLength(payload, encoding)
	}

	if !hasEncoding {
		return true
	}
	if encoding == "binary" {
		return requiredNonNegativeInteger(payload, "byteLength") && validContentDeltaByteLength(payload, encoding)
	}
	if _, hasByteLength := payload["byteLength"]; hasByteLength {
		return validContentDeltaByteLength(payload, encoding)
	}
	return true
}

func validContentDeltaByteLength(payload map[string]any, encoding string) bool {
	delta, ok := payload["delta"].(string)
	if !ok || !requiredNonNegativeInteger(payload, "byteLength") {
		return false
	}
	want := numberValue(payload["byteLength"])
	switch encoding {
	case "utf-8":
		return utf8.ValidString(delta) && want == float64(len([]byte(delta)))
	case "binary":
		decoded, err := base64.StdEncoding.Strict().DecodeString(delta)
		return err == nil && base64.StdEncoding.EncodeToString(decoded) == delta && want == float64(len(decoded))
	default:
		return false
	}
}

func validTerminalLifecycleData(terminal map[string]any) bool {
	if !requiredTrimmedString(terminal, "terminalId") ||
		!optionalTrimmedString(terminal, "commandSummary") ||
		!optionalTrimmedString(terminal, "cwdLabel") {
		return false
	}
	eventType, ok := terminal["eventType"].(string)
	if !ok {
		return false
	}
	switch eventType {
	case "terminal.started":
		return true
	case "terminal.output.reference":
		return requiredCanonicalUUID(terminal, "artifactId") &&
			requiredNonNegativeInteger(terminal, "offset") &&
			requiredNonNegativeInteger(terminal, "length") &&
			requiredNonNegativeInteger(terminal, "segmentIndex") &&
			requiredEnum(terminal, "encoding", "utf-8", "binary")
	case "terminal.exited", "terminal.failed":
		if !requiredNonNegativeInteger(terminal, "totalBytes") ||
			!requiredNonNegativeInteger(terminal, "previewBytes") ||
			!requiredNonNegativeInteger(terminal, "segmentCount") ||
			!requiredBool(terminal, "truncated") ||
			!optionalInteger(terminal, "exitCode") ||
			!optionalTrimmedString(terminal, "signal") ||
			!optionalEnum(terminal, "failureKind", "exit", "signal", "timeout", "oom", "provider_error") {
			return false
		}
		return numberValue(terminal["previewBytes"]) <= numberValue(terminal["totalBytes"])
	default:
		return false
	}
}

func requiredCanonicalUUID(value map[string]any, key string) bool {
	text, ok := value[key].(string)
	if !ok {
		return false
	}
	parsed, err := uuid.Parse(text)
	return err == nil && strings.EqualFold(parsed.String(), text)
}

func requiredCanonicalRequestType(payload map[string]any) bool {
	return requiredEnum(payload, "requestType", "command_execution_approval", "file_read_approval",
		"file_change_approval", "apply_patch_approval", "exec_command_approval", "tool_user_input",
		"dynamic_tool_call", "auth_tokens_refresh", "unknown")
}

func validUserInputQuestion(question map[string]any) bool {
	options, ok := arrayField(question, "options")
	return ok && requiredTrimmedString(question, "id") && requiredTrimmedString(question, "header") &&
		requiredTrimmedString(question, "question") && optionalBool(question, "multiSelect") &&
		allObjects(options, func(option map[string]any) bool {
			return requiredTrimmedString(option, "label") && requiredTrimmedString(option, "description")
		})
}

func hasField(value map[string]any, key string) bool {
	_, ok := value[key]
	return ok
}

func objectField(value map[string]any, key string) (map[string]any, bool) {
	decoded, ok := value[key].(map[string]any)
	return decoded, ok && decoded != nil
}

func arrayField(value map[string]any, key string) ([]any, bool) {
	decoded, ok := value[key].([]any)
	return decoded, ok
}

func requiredObject(value map[string]any, key string) bool {
	_, ok := objectField(value, key)
	return ok
}

func optionalObject(value map[string]any, key string) bool {
	if _, found := value[key]; !found {
		return true
	}
	return requiredObject(value, key)
}

func requiredString(value map[string]any, key string) bool {
	_, ok := value[key].(string)
	return ok
}

func optionalString(value map[string]any, key string) bool {
	if _, found := value[key]; !found {
		return true
	}
	return requiredString(value, key)
}

func requiredTrimmedString(value map[string]any, key string) bool {
	decoded, ok := value[key].(string)
	return ok && strings.TrimSpace(decoded) != ""
}

func optionalTrimmedString(value map[string]any, key string) bool {
	if _, found := value[key]; !found {
		return true
	}
	return requiredTrimmedString(value, key)
}

func optionalNullableTrimmedString(value map[string]any, key string) bool {
	decoded, found := value[key]
	return !found || decoded == nil || (func() bool {
		text, ok := decoded.(string)
		return ok && strings.TrimSpace(text) != ""
	})()
}

func requiredEnum(value map[string]any, key string, allowed ...string) bool {
	decoded, ok := value[key].(string)
	return ok && containsRuntimeEventValue(allowed, decoded)
}

func optionalEnum(value map[string]any, key string, allowed ...string) bool {
	if _, found := value[key]; !found {
		return true
	}
	return requiredEnum(value, key, allowed...)
}

func requiredBool(value map[string]any, key string) bool {
	_, ok := value[key].(bool)
	return ok
}

func optionalBool(value map[string]any, key string) bool {
	if _, found := value[key]; !found {
		return true
	}
	return requiredBool(value, key)
}

func optionalInteger(value map[string]any, key string) bool {
	decoded, found := value[key]
	return !found || isJSONInteger(decoded)
}

func requiredNonNegativeInteger(value map[string]any, key string) bool {
	decoded, found := value[key]
	return found && isJSONInteger(decoded) && numberValue(decoded) >= 0
}

func optionalNonNegativeInteger(value map[string]any, key string) bool {
	decoded, found := value[key]
	return !found || (isJSONInteger(decoded) && numberValue(decoded) >= 0)
}

func requiredNonNegativeSafeInteger(value map[string]any, key string) bool {
	decoded, found := value[key]
	if !found || !isJSONInteger(decoded) {
		return false
	}
	number := numberValue(decoded)
	return number >= 0 && number <= 9_007_199_254_740_991
}

func requiredLowerSHA256(value map[string]any, key string) bool {
	encoded, ok := value[key].(string)
	if !ok || len(encoded) != 64 {
		return false
	}
	for _, character := range encoded {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func optionalNumber(value map[string]any, key string) bool {
	decoded, found := value[key]
	return !found || isJSONNumber(decoded)
}

func optionalBoundedNumber(value map[string]any, key string, minimum, maximum float64) bool {
	decoded, found := value[key]
	if !found {
		return true
	}
	number := numberValue(decoded)
	return isJSONNumber(decoded) && number >= minimum && number <= maximum
}

func optionalStringArray(value map[string]any, key string, requireNonEmpty bool) bool {
	values, found := value[key]
	if !found {
		return true
	}
	items, ok := values.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		text, ok := item.(string)
		if !ok || (requireNonEmpty && strings.TrimSpace(text) == "") {
			return false
		}
	}
	return true
}

func allObjects(values []any, validate func(map[string]any) bool) bool {
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok || object == nil || !validate(object) {
			return false
		}
	}
	return true
}

func exactObjectFields(value map[string]any, fields ...string) bool {
	if len(value) != len(fields) {
		return false
	}
	for _, field := range fields {
		if _, found := value[field]; !found {
			return false
		}
	}
	return true
}

func isJSONInteger(value any) bool {
	switch number := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return !math.IsNaN(float64(number)) && !math.IsInf(float64(number), 0) &&
			float32(math.Trunc(float64(number))) == number
	case float64:
		return !math.IsNaN(number) && !math.IsInf(number, 0) && math.Trunc(number) == number
	case json.Number:
		_, err := number.Int64()
		return err == nil
	default:
		return false
	}
}

func isJSONNumber(value any) bool {
	switch number := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return !math.IsNaN(float64(number)) && !math.IsInf(float64(number), 0)
	case float64:
		return !math.IsNaN(number) && !math.IsInf(number, 0)
	case json.Number:
		_, err := number.Float64()
		return err == nil
	default:
		return false
	}
}

func numberValue(value any) float64 {
	switch number := value.(type) {
	case int:
		return float64(number)
	case int8:
		return float64(number)
	case int16:
		return float64(number)
	case int32:
		return float64(number)
	case int64:
		return float64(number)
	case uint:
		return float64(number)
	case uint8:
		return float64(number)
	case uint16:
		return float64(number)
	case uint32:
		return float64(number)
	case uint64:
		return float64(number)
	case float32:
		return float64(number)
	case float64:
		return number
	case json.Number:
		decoded, _ := number.Float64()
		return decoded
	default:
		return math.NaN()
	}
}

func containsRuntimeEventValue(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func pendingInteractionKind(eventVersion int, eventType string) (string, bool) {
	switch {
	case eventVersion == RuntimeEventVersionV1 && eventType == "approval.requested":
		return "approval", true
	case eventVersion == RuntimeEventVersionV1 && eventType == "user-input.requested":
		return "user-input", true
	case eventVersion == RuntimeEventVersionV2 && eventType == "request.opened":
		return "approval", true
	case eventVersion == RuntimeEventVersionV2 && eventType == "user-input.requested":
		return "user-input", true
	default:
		return "", false
	}
}

func (s *Service) persistPendingInteraction(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	execution *persistence.AgentExecution,
	eventVersion int,
	kind string,
	payload map[string]any,
	requestedAt time.Time,
) error {
	requestID, ok := payload["requestId"].(string)
	requestID = strings.TrimSpace(requestID)
	if !ok || requestID == "" || len(requestID) > 200 || strings.ContainsAny(requestID, "\r\n\t") {
		return problem.New(400, "invalid_interaction_request_id", "Approval and user-input events require a valid requestId.")
	}
	provider := ""
	if execution.Provider != nil {
		provider = strings.TrimSpace(*execution.Provider)
	}
	if provider == "" {
		if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
			Select("provider").Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
			Scan(&provider).Error; err != nil {
			return problem.Wrap(500, "interaction_provider_load_failed", "The interaction Provider could not be loaded.", err)
		}
	}
	interaction := persistence.ExecutionInteraction{
		ID: uuid.New(), TenantID: execution.TenantID, ExecutionID: execution.ID, SessionID: execution.SessionID,
		TurnID: execution.TurnID, WorkerID: worker.ID, Generation: execution.Generation, Provider: provider,
		RequestID: requestID, EventVersion: eventVersion, Kind: kind, Status: "pending", Payload: payload,
		RequestedAt: requestedAt, ExpiresAt: requestedAt.Add(24 * time.Hour), DeliveryStatus: "not-ready",
	}
	if err := tx.WithContext(ctx).Create(&interaction).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "interaction_request_reused", "requestId was already used for this execution.")
		}
		return problem.Wrap(500, "interaction_persist_failed", "The pending interaction could not be persisted.", err)
	}
	updated := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ? AND worker_id = ? AND generation = ? AND status IN ?",
			execution.TenantID, execution.ID, worker.ID, execution.Generation,
			[]string{"leased", "running", "waiting-for-approval"}).
		Update("status", "waiting-for-approval")
	if err := expectOne(updated, 409, "execution_interaction_state_conflict", "The execution could not enter the approval wait state."); err != nil {
		return err
	}
	execution.Status = "waiting-for-approval"
	return nil
}

func sameJSON(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
