// FILE: queuedTurnsProjection.ts
// Purpose: Keeps pure and SQL-backed queued-turn projections behaviorally identical.
// Used by: projector.ts and Layers/ProjectionPipeline.ts.

import type { OrchestrationQueuedTurn } from "@t3tools/contracts";

type QueuedTurnInput = Omit<OrchestrationQueuedTurn, "dispatchState">;

/**
 * Appends (or replaces) a queued-turn entry, keyed by `messageId` — a
 * re-queue of the same message replaces its entry rather than duplicating
 * it. Called on `thread.turn-queued`.
 */
export function addQueuedTurn(
  existing: ReadonlyArray<OrchestrationQueuedTurn> | null | undefined,
  payload: QueuedTurnInput,
): ReadonlyArray<OrchestrationQueuedTurn> {
  const remaining = (existing ?? []).filter((queued) => queued.messageId !== payload.messageId);
  const queued = { ...payload, dispatchState: "queued" as const };
  return payload.dispatchMode === "steer" ? [queued, ...remaining] : [...remaining, queued];
}

// Marks the selected queue entry in-flight without making it unrecoverable yet.
export function markQueuedTurnDispatchRequested(
  existing: ReadonlyArray<OrchestrationQueuedTurn> | null | undefined,
  messageId: string,
): ReadonlyArray<OrchestrationQueuedTurn> {
  return (existing ?? []).map((queued) =>
    queued.messageId === messageId
      ? { ...queued, dispatchState: "dispatch-requested" as const }
      : queued,
  );
}

// Clears an entry only after provider runtime start is durably projected.
export function clearQueuedTurn(
  existing: ReadonlyArray<OrchestrationQueuedTurn> | null | undefined,
  messageId: string,
): ReadonlyArray<OrchestrationQueuedTurn> {
  return (existing ?? []).filter((queued) => queued.messageId !== messageId);
}

/**
 * Clears the single queued turn whose provider start was requested. A running
 * session does not carry the originating message id, so ambiguous projection
 * state is left intact for restart recovery instead of guessing incorrectly.
 */
export function clearStartedQueuedTurn(
  existing: ReadonlyArray<OrchestrationQueuedTurn> | null | undefined,
): ReadonlyArray<OrchestrationQueuedTurn> {
  const list = existing ?? [];
  const requested = list.filter((queued) => queued.dispatchState === "dispatch-requested");
  return requested.length === 1 ? clearQueuedTurn(list, requested[0]!.messageId) : list;
}

/**
 * Drops every queued-turn entry for a thread. Mirrors
 * `queuedTurnStartsByThread.delete(threadId)` in `ProviderCommandReactor.ts`,
 * called on events that reset or invalidate a thread's provider conversation
 * state wholesale — checkpoint revert (`thread.reverted`) and conversation
 * rollback (`thread.conversation-rolled-back`). Without this, a turn the
 * user already reverted/cancelled away would still look "queued" in the
 * durable projection and get resurrected (re-dispatched) by restart
 * recovery.
 */
export function clearAllQueuedTurns(): ReadonlyArray<OrchestrationQueuedTurn> {
  return [];
}

/**
 * Mirrors `ProviderCommandReactor.ts`'s `processMessageEditResendPayload`
 * exactly: if the edited message is itself a currently-queued (not yet
 * dispatched) turn, only that entry is removed — a fresh queued turn for the
 * same `messageId` is re-added immediately after by the resend. Otherwise
 * (the edited message already dispatched/completed) every queued turn for
 * the thread is dropped, because editing a past message invalidates
 * anything queued to run after it. Called on
 * `thread.message-edit-resend-requested`.
 */
export function clearQueuedTurnsForEditResend(
  existing: ReadonlyArray<OrchestrationQueuedTurn> | null | undefined,
  messageId: string,
): ReadonlyArray<OrchestrationQueuedTurn> {
  const list = existing ?? [];
  const isQueuedMessageEdit = list.some(
    (queued) => queued.messageId === messageId && queued.dispatchState !== "dispatch-requested",
  );
  return isQueuedMessageEdit ? list.filter((queued) => queued.messageId !== messageId) : [];
}
