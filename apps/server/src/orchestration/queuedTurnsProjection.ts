import type { OrchestrationQueuedTurn } from "@t3tools/contracts";

/**
 * Pure helpers for maintaining a thread's durable `queuedTurns` list — the
 * projected record of turns queued (`thread.turn-queued`) but not yet
 * dispatched (`thread.turn-start-requested` for the same `messageId`).
 *
 * `projector.ts` (in-memory reducer) and `Layers/ProjectionPipeline.ts`
 * (SQL-materializing reducer, used live and during full-log-replay boot) are
 * two parallel reducers over the same event log. Both call these exact same
 * functions so the two projections can never drift apart.
 *
 * `startupTurnReconciliation.ts`'s `planQueuedTurnRecovery` reads this field
 * at boot to re-drive any turn that was durably queued but never dispatched
 * before the previous process exited. Its idempotency depends entirely on
 * this field accurately reflecting "still queued, never dispatched, never
 * withdrawn" — see `clearQueuedTurn`, `clearAllQueuedTurns`, and
 * `clearQueuedTurnsForEditResend` below for the ways an entry stops being
 * "still queued".
 */

/**
 * Appends (or replaces) a queued-turn entry, keyed by `messageId` — a
 * re-queue of the same message replaces its entry rather than duplicating
 * it. Called on `thread.turn-queued`.
 */
export function addQueuedTurn(
  existing: ReadonlyArray<OrchestrationQueuedTurn> | null | undefined,
  payload: OrchestrationQueuedTurn,
): ReadonlyArray<OrchestrationQueuedTurn> {
  return [...(existing ?? []).filter((queued) => queued.messageId !== payload.messageId), payload];
}

/**
 * Clears the queued-turn entry matching `messageId`. Called on
 * `thread.turn-start-requested`: a dispatched turn is no longer queued. This
 * is the mechanism that makes `planQueuedTurnRecovery` idempotent — once a
 * queued turn actually starts, it can never be recovered/re-dispatched again
 * after a restart.
 */
export function clearQueuedTurn(
  existing: ReadonlyArray<OrchestrationQueuedTurn> | null | undefined,
  messageId: string,
): ReadonlyArray<OrchestrationQueuedTurn> {
  return (existing ?? []).filter((queued) => queued.messageId !== messageId);
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
  const isQueuedMessageEdit = list.some((queued) => queued.messageId === messageId);
  return isQueuedMessageEdit ? list.filter((queued) => queued.messageId !== messageId) : [];
}
