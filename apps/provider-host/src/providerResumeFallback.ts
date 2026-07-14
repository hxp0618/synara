import type { RunnerInput, RunnerMessage } from "./providerHost";

export type ProviderResumeFallbackReasonCode =
  | "session_resume_invalid"
  | "session_resume_expired";

type ResumeFallbackProvider = "codex" | "claudeAgent";

const NON_RESUME_FAILURE_MARKERS = [
  "api key",
  "authentication",
  "authorization",
  "credential",
  "forbidden",
  "rate limit",
  "rate-limit",
  "service unavailable",
  "timed out",
  "timeout",
  "token",
  "too many requests",
  "transport",
  "unauthorized",
] as const;

export function classifyProviderResumeFailure(
  error: unknown,
): ProviderResumeFallbackReasonCode | undefined {
  const message = error instanceof Error ? error.message : String(error);
  const normalized = message.trim().toLowerCase();
  if (!normalized || NON_RESUME_FAILURE_MARKERS.some((marker) => normalized.includes(marker))) {
    return undefined;
  }
  if (
    /\b(?:conversation|cursor|session|thread) (?:(?:id )?[a-z0-9_.:-]+ )?(?:is |has )?expired\b/u.test(
      normalized,
    ) ||
    /\bexpired (?:conversation|cursor|session|thread)\b/u.test(normalized)
  ) {
    return "session_resume_expired";
  }
  if (
    /\bno conversation found\b/u.test(normalized) ||
    /\bno (?:cursor|session|thread) found\b/u.test(normalized) ||
    /\bmissing (?:conversation|cursor|session|thread)\b/u.test(normalized) ||
    /\b(?:invalid|unknown) (?:resume )?(?:conversation|cursor|session|thread)\b/u.test(
      normalized,
    ) ||
    /\b(?:conversation|cursor|session|thread) (?:(?:id )?[a-z0-9_.:-]+ )?(?:is )?(?:invalid|missing|not found|unknown|does not exist)\b/u.test(
      normalized,
    ) ||
    /\bresume (?:cursor|target) (?:(?:id )?[a-z0-9_.:-]+ )?(?:is )?(?:invalid|missing|not found|unknown|does not exist)\b/u.test(
      normalized,
    )
  ) {
    return "session_resume_invalid";
  }
  return undefined;
}

export function providerResumeFallbackWarning(
  input: RunnerInput,
  provider: ResumeFallbackProvider,
  reasonCode: ProviderResumeFallbackReasonCode,
): Extract<RunnerMessage, { type: "event" }> {
  const authoritativeHistorySequence = resumeHistorySequence(input);
  const providerName = provider === "codex" ? "Codex" : "Claude";
  return {
    type: "event",
    eventType: "runtime.provider.warning",
    payload: {
      provider,
      message: `Native ${providerName} resume failed before turn activity; authoritative-history fallback selected.`,
      kind: "session_resume",
      attemptedStrategy: "native-cursor",
      selectedStrategy: "authoritative-history",
      outcome: "fallback_selected",
      reasonCode,
      fallbackSafety: "before_turn_activity",
      ...(authoritativeHistorySequence !== undefined ? { authoritativeHistorySequence } : {}),
    },
  };
}

function resumeHistorySequence(input: RunnerInput): number | undefined {
  const snapshot = input.workload.resumeSnapshot;
  const explicit = snapshot?.authoritativeHistorySequence;
  if (isSequence(explicit)) return explicit;
  const through = snapshot?.sourceSequenceRange?.through;
  return isSequence(through) ? through : undefined;
}

function isSequence(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}
