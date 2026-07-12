import { describe, expect, it, vi } from "vitest";

import { ControlPlaneTurnDispatcher } from "./controlPlaneTurnDispatch";

describe("ControlPlaneTurnDispatcher", () => {
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
      createSession,
      createTurn,
    };

    await expect(dispatcher.dispatch(input)).rejects.toThrow("connection reset");
    await expect(dispatcher.dispatch(input)).resolves.toEqual({
      sessionId: "session-1",
      createdSession: false,
    });

    expect(createSession).toHaveBeenCalledTimes(1);
    expect(createTurn).toHaveBeenCalledTimes(2);
    expect(createTurn.mock.calls[0]?.[0].idempotencyKey).toBe(
      createTurn.mock.calls[1]?.[0].idempotencyKey,
    );
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
      createSession,
      createTurn,
    });

    expect(createSession).not.toHaveBeenCalled();
    expect(createTurn).toHaveBeenCalledWith(
      expect.objectContaining({ sessionId: "session-1", idempotencyKey: "web-turn-uuid-1" }),
    );
  });
});
