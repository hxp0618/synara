import { ThreadId } from "@synara/contracts";
import { describe, expect, it, vi } from "vitest";

import {
  ControlPlaneTurnDispatcher,
  clearSharedControlPlaneModelSwitchIdempotencyKey,
  reserveSharedControlPlaneModelSwitchIdempotencyKey,
  resetSharedControlPlaneTurnDispatcherForTests,
  type ControlPlaneTurnDispatchInput,
} from "./controlPlaneTurnDispatch";

describe("ControlPlaneTurnDispatcher", () => {
  it("reuses a model-switch key only inside the same retry window", () => {
    resetSharedControlPlaneTurnDispatcherForTests();
    const request = {
      draftThreadId: "draft-1",
      sessionId: "session-1",
      targetModel: "gpt-5.6-sol",
    };
    const first = reserveSharedControlPlaneModelSwitchIdempotencyKey(request);
    expect(reserveSharedControlPlaneModelSwitchIdempotencyKey(request)).toBe(first);
    clearSharedControlPlaneModelSwitchIdempotencyKey(request.draftThreadId);
    expect(reserveSharedControlPlaneModelSwitchIdempotencyKey(request)).not.toBe(first);
  });

  it("reuses Session and Turn idempotency keys after an uncertain Turn failure", async () => {
    let uuid = 0;
    const dispatcher = new ControlPlaneTurnDispatcher(() => `uuid-${++uuid}`);
    const createSession = vi.fn(async () => ({ id: "session-1" }));
    const createTurn = vi
      .fn()
      .mockRejectedValueOnce(new Error("connection reset"))
      .mockResolvedValueOnce({ id: "turn-1" });
    const input = {
      draftThreadId: "draft-1",
      persistedSessionId: null,
      projectId: "project-1",
      title: "Build it",
      provider: "codex",
      model: "gpt-5.6-sol",
      inputText: "Build it",
      runtimeMode: "approval-required" as const,
      interactionMode: "plan" as const,
      createSession,
      createTurn,
    } satisfies ControlPlaneTurnDispatchInput;

    await expect(dispatcher.dispatch(input)).rejects.toThrow("connection reset");
    await expect(dispatcher.dispatch(input)).resolves.toEqual({
      sessionId: "session-1",
      createdSession: false,
      turnId: "turn-1",
    });

    expect(createSession).toHaveBeenCalledTimes(1);
    expect(createTurn).toHaveBeenCalledTimes(2);
    expect(createTurn.mock.calls[0]?.[0].idempotencyKey).toBe(
      createTurn.mock.calls[1]?.[0].idempotencyKey,
    );
  });

  it("promotes a recovered draft and forwards its plan lineage", async () => {
    const dispatcher = new ControlPlaneTurnDispatcher(() => "uuid-1");
    const createSession = vi.fn(async () => ({ id: "session-1" }));
    const createTurn = vi
      .fn()
      .mockRejectedValueOnce(new Error("timed out"))
      .mockResolvedValueOnce({ id: "turn-1" });
    const onSessionResolved = vi.fn();
    const sourceProposedPlan = {
      threadId: ThreadId.makeUnsafe("source-session"),
      planId: "plan-1",
    };
    const input = {
      draftThreadId: "draft-1",
      persistedSessionId: null,
      projectId: "project-1",
      title: "Implementation",
      provider: "codex",
      inputText: "Implement it",
      runtimeMode: "full-access" as const,
      interactionMode: "default" as const,
      sourceProposedPlan,
      createSession,
      createTurn,
      onSessionResolved,
    } satisfies ControlPlaneTurnDispatchInput;

    await expect(dispatcher.dispatch(input)).rejects.toThrow("timed out");
    await expect(dispatcher.dispatch(input)).resolves.toEqual({
      sessionId: "session-1",
      createdSession: false,
      turnId: "turn-1",
    });
    expect(onSessionResolved).toHaveBeenNthCalledWith(1, {
      sessionId: "session-1",
      createdSession: true,
    });
    expect(onSessionResolved).toHaveBeenNthCalledWith(2, {
      sessionId: "session-1",
      createdSession: false,
    });
    expect(createTurn).toHaveBeenLastCalledWith(expect.objectContaining({ sourceProposedPlan }));
  });

  it("does not create a second Session for an existing remote thread", async () => {
    const dispatcher = new ControlPlaneTurnDispatcher(() => "uuid-1");
    const createSession = vi.fn(async () => ({ id: "unused" }));
    const createTurn = vi.fn(async () => ({ id: "turn-1" }));

    await dispatcher.dispatch({
      draftThreadId: "session-1",
      persistedSessionId: "session-1",
      projectId: "project-1",
      title: "Existing",
      provider: "codex",
      inputText: "Continue",
      runtimeMode: "full-access",
      interactionMode: "default",
      createSession,
      createTurn,
    });

    expect(createSession).not.toHaveBeenCalled();
    expect(createTurn).toHaveBeenCalledWith(
      expect.objectContaining({
        sessionId: "session-1",
        runtimeMode: "full-access",
        interactionMode: "default",
        idempotencyKey: "web-turn-uuid-1",
      }),
    );
  });
});
