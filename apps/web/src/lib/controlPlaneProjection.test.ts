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
): ControlPlaneSessionEvent {
  return {
    eventId: `event-${sequence}`,
    eventVersion: 1,
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
});
