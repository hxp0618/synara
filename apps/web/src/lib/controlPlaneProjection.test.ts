import { describe, expect, it } from "vitest";

import type {
  ControlPlaneAgentSession,
  ControlPlaneSessionEvent,
} from "./controlPlaneClient";
import {
  applyControlPlaneSessionEvent,
  createControlPlaneSessionProjection,
  projectControlPlaneThreads,
  withControlPlaneStreamStatus,
} from "./controlPlaneProjection";

const session: ControlPlaneAgentSession = {
  id: "session-1",
  tenantId: "tenant-1",
  organizationId: "organization-1",
  projectId: "project-1",
  createdBy: "user-1",
  title: "Remote session",
  status: "active",
  visibility: "private",
  provider: "codex",
  model: "gpt-5.6-sol",
  providerCredentialId: null,
  executionTargetId: "target-1",
  lastEventSequence: 0,
  createdAt: "2026-07-12T00:00:00Z",
  updatedAt: "2026-07-12T00:00:00Z",
  archivedAt: null,
};

function event(
  sequence: number,
  eventType: string,
  payload: Record<string, unknown>,
  eventVersion = 1,
): ControlPlaneSessionEvent {
  return {
    eventId: `event-${sequence}`,
    eventVersion,
    tenantId: session.tenantId,
    organizationId: session.organizationId,
    projectId: session.projectId,
    sessionId: session.id,
    executionId: sequence === 1 ? "execution-1" : "execution-1",
    workerId: null,
    generation: null,
    sequence,
    eventType,
    actorType: sequence === 1 ? "user" : "worker",
    actorId: "actor-1",
    payload,
    occurredAt: `2026-07-12T00:00:0${sequence}Z`,
  };
}

describe("Control Plane Session projection", () => {
  it("preserves Droid identity and fails closed for an unknown Provider", () => {
    const droidSession = { ...session, provider: "droid" as const, model: "claude-opus-4-8" };
    expect(projectControlPlaneThreads([droidSession], new Map())[0]?.modelSelection.provider).toBe(
      "droid",
    );

    expect(() =>
      projectControlPlaneThreads(
        [{ ...session, provider: "unknown-provider" as ControlPlaneAgentSession["provider"] }],
        new Map(),
      ),
    ).toThrow("unsupported Provider");
  });

  it.each(["claude", "claudeagent", "claudeAgent"])(
    "hydrates the persisted Claude Provider alias %s canonically",
    (provider) => {
      expect(
        projectControlPlaneThreads(
          [{ ...session, provider: provider as ControlPlaneAgentSession["provider"] }],
          new Map(),
        )[0]?.modelSelection.provider,
      ).toBe("claudeAgent");
    },
  );

  it("deduplicates Sequence values and reports gaps before applying later events", () => {
    const initial = createControlPlaneSessionProjection(session);
    const first = applyControlPlaneSessionEvent(
      initial,
      event(1, "turn.created", {
        turnId: "turn-1",
        executionId: "execution-1",
        inputText: "Build it",
      }),
    ).projection;

    expect(applyControlPlaneSessionEvent(first, event(1, "turn.created", {})).projection).toBe(
      first,
    );
    expect(
      applyControlPlaneSessionEvent(
        first,
        event(3, "runtime.output.delta", { turnId: "turn-1", text: "done" }),
      ).gap,
    ).toEqual({ afterSequence: 1, receivedSequence: 3 });
  });

  it("projects durable user and assistant messages without coupling SSE state to running", () => {
    let projection = createControlPlaneSessionProjection(session);
    projection = applyControlPlaneSessionEvent(
      projection,
      event(1, "turn.created", {
        turnId: "turn-1",
        inputText: "Build it",
        runtimeMode: "approval-required",
        interactionMode: "plan",
      }),
    ).projection;
    projection = applyControlPlaneSessionEvent(
      projection,
      event(2, "execution.started", {
        turnId: "turn-1",
        startedAt: "2026-07-12T00:00:02Z",
      }),
    ).projection;
    projection = applyControlPlaneSessionEvent(
      projection,
      event(3, "runtime.output.delta", { turnId: "turn-1", text: "Hello " }),
    ).projection;
    projection = applyControlPlaneSessionEvent(
      projection,
      event(4, "runtime.output.delta", { turnId: "turn-1", text: "world" }),
    ).projection;
    projection = withControlPlaneStreamStatus(projection, "reconnecting");

    expect(projection.messages.map((message) => message.text)).toEqual(["Build it", "Hello world"]);
    expect(projection.orchestrationStatus).toBe("running");
    expect(projection.streamStatus).toBe("reconnecting");

    const thread = projectControlPlaneThreads([session], new Map([[session.id, projection]]))[0]!;
    expect(thread.session?.orchestrationStatus).toBe("running");
    expect(thread.runtimeMode).toBe("approval-required");
    expect(thread.interactionMode).toBe("plan");
  });

  it("projects canonical content.delta once and keeps non-assistant streams out of transcript", () => {
    let projection = createControlPlaneSessionProjection(session);
    for (const item of [
      event(1, "turn.created", { turnId: "turn-1", inputText: "Build it" }),
      event(2, "execution.started", { turnId: "turn-1" }),
      event(
        3,
        "content.delta",
        { turnId: "turn-1", streamKind: "assistant_text", delta: "Canonical" },
        2,
      ),
      event(
        4,
        "content.delta",
        { turnId: "turn-1", streamKind: "reasoning_text", delta: "Hidden reasoning" },
        2,
      ),
    ]) {
      projection = applyControlPlaneSessionEvent(projection, item).projection;
    }

    expect(projection.messages.map((message) => message.text)).toEqual(["Build it", "Canonical"]);
    expect(projection.activities.map((activity) => activity.kind)).toEqual(["execution.started"]);
    expect(applyControlPlaneSessionEvent(projection, event(4, "content.delta", {}, 2)).projection).toBe(
      projection,
    );
  });

  it("degrades canonical lifecycle, warning, and future v2 events into stable activities", () => {
    let projection = createControlPlaneSessionProjection(session);
    for (const item of [
      event(
        1,
        "item.started",
        { itemType: "command_execution", status: "inProgress", title: "Run tests" },
        2,
      ),
      event(2, "runtime.warning", { message: "Resume cursor expired" }, 2),
      event(3, "provider.extension.future", { safe: true }, 2),
    ]) {
      projection = applyControlPlaneSessionEvent(projection, item).projection;
    }

    expect(projection.activities.map((activity) => activity.summary)).toEqual([
      "Run tests",
      "Resume cursor expired",
      "provider.extension.future",
    ]);
    expect(projection.activities.map((activity) => activity.tone)).toEqual(["tool", "tool", "info"]);
  });

  it("finishes assistant output only from the durable completion Event", () => {
    let projection = createControlPlaneSessionProjection(session);
    for (const item of [
      event(1, "turn.created", { turnId: "turn-1", inputText: "Build it" }),
      event(2, "execution.started", { turnId: "turn-1" }),
      event(3, "runtime.output.delta", { turnId: "turn-1", text: "Done" }),
      event(4, "execution.completed", {
        turnId: "turn-1",
        finishedAt: "2026-07-12T00:00:04Z",
      }),
    ]) {
      projection = applyControlPlaneSessionEvent(projection, item).projection;
    }

    expect(projection.messages[1]).toEqual(
      expect.objectContaining({ text: "Done", streaming: false }),
    );
    expect(projection.latestTurn?.state).toBe("completed");
    expect(projection.orchestrationStatus).toBe("ready");
  });

  it("projects durable interrupt intent and confirmation without closing the Session", () => {
    let projection = createControlPlaneSessionProjection(session);
    for (const item of [
      event(1, "turn.created", { turnId: "turn-1", inputText: "Long task" }),
      event(2, "execution.started", { turnId: "turn-1" }),
      event(3, "runtime.output.delta", { turnId: "turn-1", text: "Partial" }),
      event(4, "turn.interrupt-requested", {
        turnId: "turn-1",
        controlCommandId: "control-1",
      }),
      event(5, "execution.interrupted", {
        turnId: "turn-1",
        finishedAt: "2026-07-12T00:00:05Z",
      }),
    ]) {
      projection = applyControlPlaneSessionEvent(projection, item).projection;
    }

    expect(projection.messages[1]).toEqual(
      expect.objectContaining({ text: "Partial", streaming: false }),
    );
    expect(projection.latestTurn?.state).toBe("interrupted");
    expect(projection.orchestrationStatus).toBe("interrupted");
    expect(projection.activities.map((activity) => activity.kind)).toEqual(
      expect.arrayContaining(["turn.interrupt-requested", "execution.interrupted"]),
    );
    const thread = projectControlPlaneThreads([session], new Map([[session.id, projection]]))[0]!;
    expect(thread.session?.status).toBe("ready");
  });

  it("projects durable Steer intent as a marked user message and acknowledgement activity", () => {
    let projection = createControlPlaneSessionProjection(session);
    for (const item of [
      event(1, "turn.created", { turnId: "turn-1", inputText: "Long task" }),
      event(2, "execution.started", { turnId: "turn-1" }),
      event(3, "turn.steer-requested", {
        turnId: "turn-1",
        controlCommandId: "control-steer-1",
        inputText: "Focus on the failing test",
      }),
      event(4, "turn.steered", {
        turnId: "turn-1",
        controlCommandId: "control-steer-1",
      }),
    ]) {
      projection = applyControlPlaneSessionEvent(projection, item).projection;
    }

    expect(projection.messages).toHaveLength(2);
    expect(projection.messages[1]).toEqual(
      expect.objectContaining({
        text: "Focus on the failing test",
        dispatchMode: "steer",
        turnId: "turn-1",
      }),
    );
    expect(projection.latestTurn?.turnId).toBe("turn-1");
    expect(projection.orchestrationStatus).toBe("running");
    expect(projection.activities.map((activity) => activity.kind)).toEqual(
      expect.arrayContaining(["turn.steer-requested", "turn.steered"]),
    );
  });

  it("projects Workspace restore, dirty, and Checkpoint lifecycle events explicitly", () => {
    let projection = createControlPlaneSessionProjection(session);
    for (const item of [
      event(1, "workspace.ready", {
        turnId: "turn-1",
        workspaceId: "workspace-1",
        restoredCheckpointId: "checkpoint-previous",
      }),
      event(2, "workspace.dirty", {
        turnId: "turn-1",
        workspaceId: "workspace-1",
      }),
      event(3, "checkpoint.created", {
        turnId: "turn-1",
        workspaceId: "workspace-1",
        checkpointId: "checkpoint-1",
      }),
      event(4, "checkpoint.ready", {
        turnId: "turn-1",
        workspaceId: "workspace-1",
        checkpointId: "checkpoint-1",
      }),
      event(5, "checkpoint.failed", {
        turnId: "turn-1",
        workspaceId: "workspace-1",
        checkpointId: "checkpoint-2",
        failureMessage: "Object storage unavailable",
      }),
    ]) {
      projection = applyControlPlaneSessionEvent(projection, item).projection;
    }

    expect(projection.activities.map((activity) => activity.summary)).toEqual([
      "Workspace restored",
      "Workspace changed",
      "Workspace checkpoint started",
      "Workspace checkpoint ready",
      "Object storage unavailable",
    ]);
    expect(projection.activities.at(-1)?.tone).toBe("error");
  });

  it("applies session.model.changed to the projected Session authority and activity feed", () => {
    let projection = createControlPlaneSessionProjection(session);
    projection = applyControlPlaneSessionEvent(
      projection,
      event(1, "session.model.changed", {
        provider: "codex",
        previousModel: "gpt-5.5",
        model: "gpt-5.6-sol",
        supportMode: "emulated",
      }),
    ).projection;

    expect(projection.session.model).toBe("gpt-5.6-sol");
    expect(projection.session.updatedAt).toBe("2026-07-12T00:00:01Z");
    expect(projection.activities.at(-1)).toEqual(
      expect.objectContaining({
        kind: "session.model.changed",
        summary: "Model switched to gpt-5.6-sol",
      }),
    );
  });

  it("projects top-level thread model and timestamps from the projection Session authority", () => {
    const projection = {
      ...createControlPlaneSessionProjection(session),
      session: {
        ...session,
        model: "gpt-5.6-sol",
        updatedAt: "2026-07-12T00:00:09Z",
      },
    };

    const thread = projectControlPlaneThreads([session], new Map([[session.id, projection]]))[0]!;

    expect(thread.modelSelection).toEqual({ provider: "codex", model: "gpt-5.6-sol" });
    expect(thread.updatedAt).toBe("2026-07-12T00:00:09Z");
  });
});
