# Plan 008: Make per-delta streaming store updates O(1) instead of O(thread history)

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**:
> `git diff --stat d94f416d9..HEAD -- apps/web/src/store.ts`
> If the file changed since this plan was written, compare the "Current state"
> excerpts against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: MED
- **Depends on**: none
- **Category**: perf
- **Planned at**: commit `d94f416d9`, 2026-07-07

## Why this matters

`thread.message-sent` is the highest-frequency streaming event in the app — it
fires on every assistant text delta. Its reducer currently scans the whole
per-thread messages array **twice** (`find` then `findIndex` for the same id),
copies the entire array with `.map()`, and then the state-writer rebuilds the
`messageByThreadId` id→message `Record` from scratch via `Object.fromEntries` over
every message in the thread. On a long conversation (capped at 2,000 messages)
this is O(history) work repeated dozens-to-hundreds of times per turn, and each
new array/record reference cascades into re-derivations downstream. "Performance
first" is the repo's stated #1 priority; this is the hottest path in the client.

## Current state

File: `apps/web/src/store.ts`. Relevant constants: `MAX_THREAD_MESSAGES = 2_000`
(`store.ts:107`).

The double scan in `applyThreadMessageSentEvent` (`store.ts:3025-3061`):

```ts
function applyThreadMessageSentEvent(thread: Thread, event: ThreadMessageSentEvent): Thread {
  const payload = event.payload;
  const incomingMessage = normalizeChatMessage(
    {
      /* …payload fields… */
    },
    thread.messages.find((message) => message.id === payload.messageId), // scan #1
  );
  const existingIndex = thread.messages.findIndex((message) => message.id === payload.messageId); // scan #2 (same id)
  let messages = thread.messages;

  if (existingIndex >= 0) {
    const existingMessage = thread.messages[existingIndex];
    if (!existingMessage) {
      return thread;
    }
    const mergedMessage = mergeStreamingMessage(existingMessage, incomingMessage);
    if (mergedMessage !== null) {
      messages = thread.messages.map((message, index) =>
        index === existingIndex ? mergedMessage : message,
      );
    }
  } else {
    messages = [...thread.messages, incomingMessage].slice(-MAX_THREAD_MESSAGES);
  }
  // …builds turnDiffSummaries etc., returns updated thread…
}
```

`normalizeChatMessage(payload, existing?)` takes the existing message (found by
scan #1) as its second argument. Scan #2 re-finds the same message to get its
index. These can be a single pass.

The full-record rebuild in the state writer (`store.ts:481-491, 2344-2357`):

```ts
function buildMessageSlice(thread: Thread): {
  ids: MessageId[];
  byId: Record<MessageId, ChatMessage>;
} {
  return {
    ids: thread.messages.map((message) => message.id),
    byId: Object.fromEntries(
      thread.messages.map((message) => [message.id, message] as const),
    ) as Record<MessageId, ChatMessage>,
  };
}

// writeThreadState (store.ts:2344):
if (previousThread?.messages !== nextThread.messages) {
  const nextMessageSlice = buildMessageSlice(nextThread);
  nextState = {
    ...nextState,
    messageIdsByThreadId: {
      ...(nextState.messageIdsByThreadId ?? EMPTY_MESSAGE_IDS_BY_THREAD),
      [nextThread.id]: nextMessageSlice.ids,
    },
    messageByThreadId: {
      ...(nextState.messageByThreadId ?? EMPTY_MESSAGE_BY_THREAD),
      [nextThread.id]: nextMessageSlice.byId,
    },
  };
}
```

Every event that changes the `messages` array reference (i.e. every streaming
delta) rebuilds the entire `ids` array and `byId` record for the thread.

## Commands you will need

| Purpose             | Command                                         | Expected on success                                 |
| ------------------- | ----------------------------------------------- | --------------------------------------------------- |
| Focused store tests | `cd apps/web && bun run test src/store.test.ts` | all pass (if the file exists; otherwise see Step 1) |
| Typecheck (web)     | `cd apps/web && bun run typecheck`              | exit 0, no errors                                   |
| Full suite          | `bun run test`                                  | all pass                                            |

Repo rule (`AGENTS.md`): use `bun run test`, never `bun test`. First check
whether a store test file exists: `ls apps/web/src/store.test.ts`. If it does
not, model new tests after an existing web reducer test (e.g.
`apps/web/src/session-logic.test.ts` or `apps/web/src/storeSelectors.test.ts`) and
create `apps/web/src/store.streaming.test.ts`.

## Scope

**In scope**:

- `apps/web/src/store.ts` — `applyThreadMessageSentEvent`, `buildMessageSlice`,
  and the `writeThreadState` messages branch.
- `apps/web/src/store.streaming.test.ts` (create) or the existing store test file.

**Out of scope** (do NOT touch in this plan):

- Activities / proposedPlans / turnDiffSummaries slices (`buildActivitySlice`,
  `buildProposedPlanSlice`, `buildTurnDiffSlice`). They share the same pattern and
  are worth a follow-up, but bundling them here inflates risk. Note them in
  Maintenance notes only.
- `mergeStreamingMessage` semantics — do not change how merges compute text; only
  change how the message is located.
- Any downstream selector/component. The observable state shape
  (`messageByThreadId`, `messageIdsByThreadId`, `thread.messages`) must be
  byte-for-byte equivalent after this change.

## Git workflow

- Branch: `advisor/008-streaming-store-hot-path`
- Commit style: match `git log` (short imperative subjects). One commit per step.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Add characterization tests FIRST (lock current behavior)

Before changing anything, write tests that pin the current observable behavior so
the optimization can be proven equivalent. In `apps/web/src/store.streaming.test.ts`
(or the existing store test), cover, using the real reducer entry point that
applies a `thread.message-sent` event (find how existing tests dispatch events
into the store — reuse that harness):

1. **New assistant message** appended → appears in `thread.messages` and in
   `messageByThreadId[threadId]` with the right text.
2. **Streaming delta to an existing message** (same `messageId`, longer text) →
   the message text updates in place, array length unchanged, `messageByThreadId`
   entry updated.
3. **Out-of-order / duplicate delta** (same id, same-or-shorter text) → behavior
   matches current `mergeStreamingMessage` result (assert whatever the current
   code produces — run it once to observe, then encode that expectation).
4. **Cap behavior**: appending beyond `MAX_THREAD_MESSAGES` keeps the last 2,000
   and `messageByThreadId` contains exactly those ids.
5. **Reference-stability sanity**: after a delta to one message, unrelated
   messages are the same object references (`===`) as before.

**Verify**: `cd apps/web && bun run test src/store.streaming.test.ts` → all pass
against the **unchanged** implementation. If any assertion is wrong, fix the
assertion to match current behavior (this step documents reality, it does not
change it).

### Step 2: Collapse the double scan in `applyThreadMessageSentEvent`

Compute the index once and reuse it:

```ts
const existingIndex = thread.messages.findIndex((message) => message.id === payload.messageId);
const existingMessage = existingIndex >= 0 ? thread.messages[existingIndex] : undefined;
const incomingMessage = normalizeChatMessage(
  {
    /* …payload fields… */
  },
  existingMessage,
);
let messages = thread.messages;
if (existingIndex >= 0) {
  if (!existingMessage) return thread;
  const mergedMessage = mergeStreamingMessage(existingMessage, incomingMessage);
  if (mergedMessage !== null) {
    messages = thread.messages.map((message, index) =>
      index === existingIndex ? mergedMessage : message,
    );
  }
} else {
  messages = [...thread.messages, incomingMessage].slice(-MAX_THREAD_MESSAGES);
}
```

This removes one full-array scan per delta and changes nothing observable.

**Verify**: `cd apps/web && bun run test src/store.streaming.test.ts` → still all pass.
`cd apps/web && bun run typecheck` → exit 0.

### Step 3: Rebuild `messageByThreadId` incrementally instead of from scratch

Change the `writeThreadState` messages branch (`store.ts:2344`) to patch the
previous `byId` record for only the changed ids, instead of `Object.fromEntries`
over the whole array. The previous slice is available as
`nextState.messageByThreadId?.[nextThread.id]`.

Replace `buildMessageSlice(nextThread)` usage in this branch with an incremental
builder:

```ts
if (previousThread?.messages !== nextThread.messages) {
  const prevById = nextState.messageByThreadId?.[nextThread.id];
  const nextIds = nextThread.messages.map((message) => message.id);
  const nextById = buildMessageByIdIncremental(nextThread.messages, prevById);
  nextState = {
    ...nextState,
    messageIdsByThreadId: {
      ...(nextState.messageIdsByThreadId ?? EMPTY_MESSAGE_IDS_BY_THREAD),
      [nextThread.id]: nextIds,
    },
    messageByThreadId: {
      ...(nextState.messageByThreadId ?? EMPTY_MESSAGE_BY_THREAD),
      [nextThread.id]: nextById,
    },
  };
}
```

Implement `buildMessageByIdIncremental`:

```ts
function buildMessageByIdIncremental(
  messages: readonly ChatMessage[],
  previous: Record<MessageId, ChatMessage> | undefined,
): Record<MessageId, ChatMessage> {
  // Fast path: build fresh only when there is no previous slice.
  if (!previous) {
    return Object.fromEntries(messages.map((m) => [m.id, m] as const)) as Record<
      MessageId,
      ChatMessage
    >;
  }
  const next: Record<MessageId, ChatMessage> = {};
  for (const message of messages) {
    // Reuse the exact object reference when unchanged so downstream identity checks hold.
    const prior = previous[message.id];
    next[message.id] = prior === message ? prior : message;
  }
  return next;
}
```

Note: this still allocates a new record object (required — the state is
immutable), but it avoids the intermediate `.map()` array that `Object.fromEntries`
needs and preserves per-entry object identity for unchanged messages. Keep
`buildMessageSlice` for any other callers, but this branch no longer uses it. If
`buildMessageSlice` has no other callers after this change
(`grep -n "buildMessageSlice" apps/web/src/store.ts`), leave it in place only if
still referenced; if unreferenced, you MAY remove it, but confirm with grep first.

**Verify**: `cd apps/web && bun run test src/store.streaming.test.ts` → all pass,
including the reference-stability case (Step 1, case 5). `cd apps/web && bun run
typecheck` → exit 0.

### Step 4: Confirm no downstream selector regressed

Run the full web test suite to catch any selector that depended on the old
record's identity semantics.

**Verify**: `cd apps/web && bun run test` → all pass.

## Test plan

- `apps/web/src/store.streaming.test.ts` (Step 1): append, in-place delta,
  out-of-order/duplicate delta, cap at `MAX_THREAD_MESSAGES`, unchanged-message
  reference stability.
- Structural pattern: whatever existing web test dispatches events into the store
  (reuse its harness); fall back to `session-logic.test.ts` style if none exists.
- Verification: `cd apps/web && bun run test src/store.streaming.test.ts` → all pass.

## Done criteria

ALL must hold:

- [ ] `cd apps/web && bun run typecheck` exits 0.
- [ ] `bun run test` exits 0; new streaming tests exist and pass.
- [ ] `grep -n "thread.messages.find(" apps/web/src/store.ts` no longer shows the redundant `find` inside `applyThreadMessageSentEvent` (only the single `findIndex` remains for message-sent).
- [ ] The messages branch of `writeThreadState` no longer calls `Object.fromEntries` over the whole array on the incremental (has-previous) path.
- [ ] Observable state (`thread.messages`, `messageByThreadId`, `messageIdsByThreadId`) is equivalent to before for all Step-1 cases.
- [ ] `bun run fmt` and `bun run lint` pass (final validation pass).
- [ ] No files outside the in-scope list are modified (`git status`).
- [ ] `plans/README.md` status row for 008 updated.

## STOP conditions

Stop and report back (do not improvise) if:

- The excerpts in "Current state" do not match the live code (drift).
- Any Step-1 characterization test cannot be made to pass against the **unchanged**
  code — it means the current behavior differs from this plan's understanding;
  report before optimizing.
- After Step 3, a downstream test fails in a way that shows a selector relied on
  the whole `byId` record being a new object even when nothing changed — report;
  do not paper over it by forcing a full rebuild (that would undo the win).
- You find the reducer path for `thread.message-sent` is not
  `applyThreadMessageSentEvent` (drift) — report.

## Maintenance notes

- The same 2×-scan + `Object.fromEntries` rebuild pattern exists for
  **activities** (`buildActivitySlice`, `store.ts:493`, with a 500-item cap),
  **proposedPlans** (`buildProposedPlanSlice`), and **turnDiffSummaries**
  (`buildTurnDiffSlice`). They are deliberately out of scope here; a follow-up can
  apply the identical incremental treatment once this lands and is proven.
- `deriveWorkLogEntries` (`session-logic.ts`) re-runs whenever the activities
  slice reference changes; reducing activity-slice churn later will compound with
  this fix.
- Reviewer should scrutinize: object-identity preservation for unchanged messages
  (case 5) — losing it would cause downstream memoized components to re-render
  even after this change.
