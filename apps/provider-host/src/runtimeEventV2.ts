// FILE: runtimeEventV2.ts
// Purpose: Normalizes internal Provider Host runner events onto the canonical Runtime Event v2 wire.

import {
  PROVIDER_RUNTIME_EVENT_TYPES,
  PROVIDER_RUNTIME_EVENT_VERSION,
  type CanonicalItemType,
  type ProviderRuntimeEventType,
  type RuntimeItemStatus,
  type ThreadTokenUsageSnapshot,
} from "@synara/contracts";
import type { ProviderHostRuntimeEventPayload } from "@synara/contracts/provider-host";

import type { RunnerMessage } from "./providerHost";

type RunnerEvent = Extract<RunnerMessage, { type: "event" }>;

export type RuntimeEventV2WirePayload = ProviderHostRuntimeEventPayload;

const CANONICAL_EVENT_TYPES = new Set<string>(PROVIDER_RUNTIME_EVENT_TYPES);

export function normalizeRuntimeEventV2(message: RunnerEvent): RuntimeEventV2WirePayload {
  if (CANONICAL_EVENT_TYPES.has(message.eventType)) {
    return runtimeEvent(message.eventType as ProviderRuntimeEventType, message.payload);
  }

  switch (message.eventType) {
    case "runtime.output.delta":
      return runtimeEvent("content.delta", {
        streamKind: "assistant_text",
        delta: stringValue(message.payload.text) ?? "",
      });
    case "runtime.provider.activity":
      return normalizeProviderActivity(message.payload);
    case "runtime.usage":
      return runtimeEvent("thread.token-usage.updated", {
        usage: normalizeUsage(message.payload),
      });
    case "runtime.provider.warning":
      return runtimeEvent("runtime.warning", {
        message: stringValue(message.payload.message) ?? "Provider runtime reported a warning.",
        detail: safeProviderWarningDetail(message.payload),
      });
    default:
      return runtimeEvent("runtime.warning", {
        message: "Provider Host ignored an unsupported internal runtime event.",
        detail: { sourceEventType: boundedString(message.eventType, 160) },
      });
  }
}

function normalizeProviderActivity(payload: Record<string, unknown>): RuntimeEventV2WirePayload {
  const sourceItemType = stringValue(payload.itemType) ?? "unknown";
  const status = normalizeStatus(payload.status);
  const eventType =
    status === "inProgress"
      ? stringValue(payload.status) === "updated"
        ? "item.updated"
        : "item.started"
      : "item.completed";
  const providerItemId = stringValue(payload.itemId);

  return runtimeEvent(eventType, {
    itemType: canonicalItemType(sourceItemType),
    status,
    title: boundedString(sourceItemType, 200),
    data: {
      ...safeProviderDetail(payload),
      sourceItemType: boundedString(sourceItemType, 200),
      ...(providerItemId ? { providerItemId: boundedString(providerItemId, 200) } : {}),
    },
  });
}

function normalizeUsage(payload: Record<string, unknown>): ThreadTokenUsageSnapshot {
  const inputTokens = firstNonNegative(payload.inputTokens, payload.input_tokens) ?? 0;
  const cachedInputTokens =
    firstNonNegative(payload.cachedInputTokens, payload.cached_input_tokens) ??
    (firstNonNegative(payload.cache_creation_input_tokens) ?? 0) +
      (firstNonNegative(payload.cache_read_input_tokens) ?? 0);
  const outputTokens = firstNonNegative(payload.outputTokens, payload.output_tokens) ?? 0;
  const reasoningOutputTokens =
    firstNonNegative(payload.reasoningOutputTokens, payload.reasoning_output_tokens) ?? 0;
  const explicitUsedTokens = firstNonNegative(
    payload.usedTokens,
    payload.totalTokens,
    payload.total_tokens,
  );
  const derivedUsedTokens = inputTokens + cachedInputTokens + outputTokens + reasoningOutputTokens;
  const usedTokens = explicitUsedTokens ?? derivedUsedTokens;
  const totalProcessedTokens = firstNonNegative(
    payload.totalProcessedTokens,
    payload.total_processed_tokens,
  );
  const maxTokens = firstPositive(
    payload.maxTokens,
    payload.modelContextWindow,
    payload.model_context_window,
  );

  return {
    usedTokens,
    lastUsedTokens: usedTokens,
    ...(totalProcessedTokens !== undefined ? { totalProcessedTokens } : {}),
    ...(maxTokens !== undefined ? { maxTokens } : {}),
    ...(inputTokens > 0 ? { inputTokens, lastInputTokens: inputTokens } : {}),
    ...(cachedInputTokens > 0
      ? { cachedInputTokens, lastCachedInputTokens: cachedInputTokens }
      : {}),
    ...(outputTokens > 0 ? { outputTokens, lastOutputTokens: outputTokens } : {}),
    ...(reasoningOutputTokens > 0
      ? {
          reasoningOutputTokens,
          lastReasoningOutputTokens: reasoningOutputTokens,
        }
      : {}),
    ...(stringValue(payload.provider) === "codex" ? { compactsAutomatically: true } : {}),
  };
}

function canonicalItemType(value: string): CanonicalItemType {
  const normalized = value.replaceAll(/[^a-z0-9]/giu, "").toLowerCase();
  if (normalized === "plan") return "plan";
  if (normalized === "reasoning" || normalized === "thinking") return "reasoning";
  if (normalized.includes("command") || normalized === "bash" || normalized === "shell") {
    return "command_execution";
  }
  if (
    normalized.includes("filechange") ||
    normalized === "write" ||
    normalized === "edit" ||
    normalized === "multiedit" ||
    normalized === "applypatch" ||
    normalized === "diff"
  ) {
    return "file_change";
  }
  if (normalized.includes("websearch")) return "web_search";
  if (normalized.includes("imagegeneration") || normalized === "imagegen") {
    return "image_generation";
  }
  if (normalized.includes("imageview") || normalized === "viewimage") return "image_view";
  if (normalized.includes("collab") || normalized.includes("subagent")) {
    return "collab_agent_tool_call";
  }
  if (normalized.includes("mcp")) return "mcp_tool_call";
  return "dynamic_tool_call";
}

function normalizeStatus(value: unknown): RuntimeItemStatus {
  switch (stringValue(value)) {
    case "completed":
      return "completed";
    case "failed":
      return "failed";
    case "declined":
      return "declined";
    default:
      return "inProgress";
  }
}

function safeProviderDetail(payload: Record<string, unknown>): Record<string, unknown> {
  const provider = stringValue(payload.provider);
  return provider ? { provider: boundedString(provider, 80) } : {};
}

function safeProviderWarningDetail(payload: Record<string, unknown>): Record<string, unknown> {
  const detail = safeProviderDetail(payload);
  if (payload.kind === "session_resume") detail.kind = payload.kind;
  if (payload.attemptedStrategy === "native-cursor") {
    detail.attemptedStrategy = payload.attemptedStrategy;
  }
  if (payload.selectedStrategy === "authoritative-history") {
    detail.selectedStrategy = payload.selectedStrategy;
  }
  if (payload.outcome === "fallback_selected") detail.outcome = payload.outcome;
  if (
    payload.reasonCode === "session_resume_invalid" ||
    payload.reasonCode === "session_resume_expired"
  ) {
    detail.reasonCode = payload.reasonCode;
  }
  if (payload.fallbackSafety === "before_turn_activity") {
    detail.fallbackSafety = payload.fallbackSafety;
  }
  if (
    typeof payload.authoritativeHistorySequence === "number" &&
    Number.isSafeInteger(payload.authoritativeHistorySequence) &&
    payload.authoritativeHistorySequence >= 0
  ) {
    detail.authoritativeHistorySequence = payload.authoritativeHistorySequence;
  }
  return detail;
}

function runtimeEvent(
  eventType: ProviderRuntimeEventType,
  payload: Record<string, unknown>,
): RuntimeEventV2WirePayload {
  return { eventVersion: PROVIDER_RUNTIME_EVENT_VERSION, eventType, payload };
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

function boundedString(value: string, maximumLength: number): string {
  return value.slice(0, maximumLength);
}

function firstNonNegative(...values: ReadonlyArray<unknown>): number | undefined {
  for (const value of values) {
    if (typeof value === "number" && Number.isFinite(value) && value >= 0) return Math.round(value);
  }
  return undefined;
}

function firstPositive(...values: ReadonlyArray<unknown>): number | undefined {
  for (const value of values) {
    if (typeof value === "number" && Number.isFinite(value) && value > 0) return Math.round(value);
  }
  return undefined;
}
