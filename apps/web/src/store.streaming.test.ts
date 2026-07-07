// FILE: store.streaming.test.ts
// Purpose: Characterization tests for the `thread.message-sent` hot path — pins
// current observable behavior of applyThreadMessageSentEvent / writeThreadState
// (thread.messages, messageByThreadId, messageIdsByThreadId) before optimizing
// the reducer to avoid redundant scans/rebuilds (see plans/008).

import { MessageId, ProjectId, ThreadId, type OrchestrationEvent } from "@t3tools/contracts";
import { describe, expect, it } from "vitest";

import { applyOrchestrationEvents, type AppState } from "./store";
import { DEFAULT_INTERACTION_MODE, DEFAULT_RUNTIME_MODE, type Thread } from "./types";

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: ThreadId.makeUnsafe("thread-1"),
    codexThreadId: null,
    projectId: ProjectId.makeUnsafe("project-1"),
    title: "Thread",
    modelSelection: {
      provider: "codex",
      model: "gpt-5-codex",
    },
    runtimeMode: DEFAULT_RUNTIME_MODE,
    interactionMode: DEFAULT_INTERACTION_MODE,
    session: null,
    messages: [],
    turnDiffSummaries: [],
    activities: [],
    proposedPlans: [],
    error: null,
    createdAt: "2026-02-13T00:00:00.000Z",
    latestTurn: null,
    latestUserMessageAt: null,
    hasPendingApprovals: false,
    hasPendingUserInput: false,
    hasActionableProposedPlan: false,
    envMode: "local",
    branch: null,
    worktreePath: null,
    forkSourceThreadId: null,
    sidechatSourceThreadId: null,
    handoff: null,
    ...overrides,
  };
}

function makeState(thread: Thread): AppState {
  return {
    projects: [
      {
        id: ProjectId.makeUnsafe("project-1"),
        kind: "project",
        name: "Project",
        remoteName: "Project",
        folderName: "project",
        localName: null,
        cwd: "/tmp/project",
        defaultModelSelection: {
          provider: "codex",
          model: "gpt-5-codex",
        },
        expanded: true,
        scripts: [],
      },
    ],
    threads: [thread],
    sidebarThreadSummaryById: {},
    threadsHydrated: true,
  };
}

function makeMessageSentEvent(overrides: {
  messageId: string;
  role: "user" | "assistant";
  text: string;
  streaming: boolean;
  turnId?: string | null;
  createdAt?: string;
  updatedAt?: string;
  sequence?: number;
}): OrchestrationEvent {
  const threadId = ThreadId.makeUnsafe("thread-1");
  const messageId = MessageId.makeUnsafe(overrides.messageId);
  return {
    type: "thread.message-sent",
    payload: {
      threadId,
      messageId,
      role: overrides.role,
      text: overrides.text,
      attachments: [],
      turnId: overrides.turnId === undefined ? null : overrides.turnId,
      streaming: overrides.streaming,
      source: "native",
      createdAt: overrides.createdAt ?? "2026-02-27T00:00:00.000Z",
      updatedAt: overrides.updatedAt ?? "2026-02-27T00:00:00.000Z",
    },
    sequence: overrides.sequence ?? 1,
    eventId: `event-${overrides.messageId}-${overrides.sequence ?? 1}` as never,
    aggregateKind: "thread",
    aggregateId: threadId,
    occurredAt: "2026-02-27T00:00:00.000Z",
    commandId: null,
    causationEventId: null,
    correlationId: null,
    metadata: {},
  } as OrchestrationEvent;
}

describe("thread.message-sent hot path (characterization)", () => {
  it("appends a new assistant message and populates messageByThreadId", () => {
    const state = makeState(makeThread());
    const next = applyOrchestrationEvents(state, [
      makeMessageSentEvent({
        messageId: "message-1",
        role: "assistant",
        text: "Hello",
        streaming: true,
      }),
    ]);

    const thread = next.threads[0];
    expect(thread?.messages).toHaveLength(1);
    expect(thread?.messages[0]?.text).toBe("Hello");

    const threadId = ThreadId.makeUnsafe("thread-1");
    const messageId = MessageId.makeUnsafe("message-1");
    expect(next.messageByThreadId?.[threadId]?.[messageId]?.text).toBe("Hello");
    expect(next.messageIdsByThreadId?.[threadId]).toEqual([messageId]);
  });

  it("merges a streaming delta into an existing message in place", () => {
    const state = makeState(makeThread());
    const first = applyOrchestrationEvents(state, [
      makeMessageSentEvent({
        messageId: "message-1",
        role: "assistant",
        text: "Hello",
        streaming: true,
        sequence: 1,
      }),
    ]);

    const second = applyOrchestrationEvents(first, [
      makeMessageSentEvent({
        messageId: "message-1",
        role: "assistant",
        text: " world",
        streaming: true,
        sequence: 2,
      }),
    ]);

    const thread = second.threads[0];
    expect(thread?.messages).toHaveLength(1);
    expect(thread?.messages[0]?.text).toBe("Hello world");

    const threadId = ThreadId.makeUnsafe("thread-1");
    const messageId = MessageId.makeUnsafe("message-1");
    expect(second.messageByThreadId?.[threadId]?.[messageId]?.text).toBe("Hello world");
    expect(second.messageIdsByThreadId?.[threadId]).toEqual([messageId]);
  });

  it("handles an out-of-order/duplicate delta the same way the current mergeStreamingMessage does", () => {
    const state = makeState(makeThread());
    const first = applyOrchestrationEvents(state, [
      makeMessageSentEvent({
        messageId: "message-1",
        role: "assistant",
        text: "Hello world",
        streaming: true,
        sequence: 1,
      }),
    ]);

    // Duplicate/out-of-order delta: shorter text than what's already accumulated,
    // and it is a prefix of the existing text. Observed current behavior: since
    // incomingMessage.streaming is true, mergeStreamingMessage concatenates
    // (existingMessage.text + incomingMessage.text) rather than detecting the
    // prefix relationship (that branch only applies when incoming is non-streaming
    // or empty-text). This test pins that observed behavior.
    const second = applyOrchestrationEvents(first, [
      makeMessageSentEvent({
        messageId: "message-1",
        role: "assistant",
        text: "Hello",
        streaming: true,
        sequence: 2,
      }),
    ]);

    const thread = second.threads[0];
    expect(thread?.messages).toHaveLength(1);
    expect(thread?.messages[0]?.text).toBe("Hello worldHello");
  });

  it("caps thread.messages at MAX_THREAD_MESSAGES and keeps messageByThreadId consistent", () => {
    const MAX_THREAD_MESSAGES = 2_000;
    const overflow = 5;
    const totalMessages = MAX_THREAD_MESSAGES + overflow;

    const state = makeState(makeThread());
    const events: OrchestrationEvent[] = [];
    for (let i = 0; i < totalMessages; i += 1) {
      events.push(
        makeMessageSentEvent({
          messageId: `message-${i}`,
          role: "user",
          text: `text-${i}`,
          streaming: false,
          sequence: i + 1,
        }),
      );
    }

    const next = applyOrchestrationEvents(state, events);
    const thread = next.threads[0];
    expect(thread?.messages).toHaveLength(MAX_THREAD_MESSAGES);
    expect(thread?.messages[0]?.id).toBe(MessageId.makeUnsafe(`message-${overflow}`));
    expect(thread?.messages.at(-1)?.id).toBe(MessageId.makeUnsafe(`message-${totalMessages - 1}`));

    const threadId = ThreadId.makeUnsafe("thread-1");
    const idsSlice = next.messageIdsByThreadId?.[threadId] ?? [];
    expect(idsSlice).toHaveLength(MAX_THREAD_MESSAGES);
    expect(idsSlice).toEqual(thread?.messages.map((message) => message.id));

    const byId = next.messageByThreadId?.[threadId] ?? {};
    expect(Object.keys(byId)).toHaveLength(MAX_THREAD_MESSAGES);
    for (const id of idsSlice) {
      expect(byId[id]).toBeDefined();
    }
    // The dropped-off message should no longer be present.
    expect(byId[MessageId.makeUnsafe("message-0")]).toBeUndefined();
  });

  it("keeps unrelated messages as the same object reference after a delta to one message", () => {
    const state = makeState(makeThread());
    const withTwoMessages = applyOrchestrationEvents(state, [
      makeMessageSentEvent({
        messageId: "message-1",
        role: "assistant",
        text: "First",
        streaming: true,
        sequence: 1,
      }),
      makeMessageSentEvent({
        messageId: "message-2",
        role: "assistant",
        text: "Second",
        streaming: true,
        sequence: 2,
      }),
    ]);

    const threadId = ThreadId.makeUnsafe("thread-1");
    const messageId1 = MessageId.makeUnsafe("message-1");
    const messageId2 = MessageId.makeUnsafe("message-2");

    const unrelatedMessageBefore = withTwoMessages.threads[0]?.messages.find(
      (message) => message.id === messageId2,
    );
    const unrelatedByIdBefore = withTwoMessages.messageByThreadId?.[threadId]?.[messageId2];
    expect(unrelatedMessageBefore).toBeDefined();
    expect(unrelatedByIdBefore).toBe(unrelatedMessageBefore);

    const afterDelta = applyOrchestrationEvents(withTwoMessages, [
      makeMessageSentEvent({
        messageId: "message-1",
        role: "assistant",
        text: " delta",
        streaming: true,
        sequence: 3,
      }),
    ]);

    const unrelatedMessageAfter = afterDelta.threads[0]?.messages.find(
      (message) => message.id === messageId2,
    );
    const unrelatedByIdAfter = afterDelta.messageByThreadId?.[threadId]?.[messageId2];

    expect(unrelatedMessageAfter).toBe(unrelatedMessageBefore);
    expect(unrelatedByIdAfter).toBe(unrelatedMessageBefore);

    const updatedMessage = afterDelta.threads[0]?.messages.find(
      (message) => message.id === messageId1,
    );
    expect(updatedMessage?.text).toBe("First delta");
  });
});
