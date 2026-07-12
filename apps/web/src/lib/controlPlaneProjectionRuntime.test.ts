import { describe, expect, it, vi } from "vitest";

import type {
  ControlPlaneAgentSession,
  ControlPlaneSessionEvent,
  ControlPlaneSessionEventPage,
} from "./controlPlaneClient";
import { ControlPlaneProjectionRuntime } from "./controlPlaneProjectionRuntime";

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
  lastEventSequence: 2,
  createdAt: "2026-07-12T00:00:00Z",
  updatedAt: "2026-07-12T00:00:00Z",
  archivedAt: null,
};

function event(sequence: number, eventType = "runtime.output.delta"): ControlPlaneSessionEvent {
  return {
    eventId: `event-${sequence}`,
    eventVersion: 1,
    tenantId: session.tenantId,
    organizationId: session.organizationId,
    projectId: session.projectId,
    sessionId: session.id,
    executionId: "execution-1",
    workerId: null,
    generation: null,
    sequence,
    eventType,
    actorType: sequence === 1 ? "user" : "worker",
    actorId: "actor-1",
    payload:
      sequence === 1
        ? { turnId: "turn-1", inputText: "Build it" }
        : { turnId: "turn-1", text: String(sequence) },
    occurredAt: `2026-07-12T00:00:0${sequence}Z`,
  };
}

describe("ControlPlaneProjectionRuntime", () => {
  it("shares one SSE listener and resumes after durable REST catch-up", async () => {
    let handlers:
      | {
          onEvent: (event: ControlPlaneSessionEvent) => void;
          onOpen?: () => void;
          onError?: () => void;
        }
      | undefined;
    const close = vi.fn();
    const listSessionEvents = vi.fn(
      async (_sessionId: string, afterSequence: number): Promise<ControlPlaneSessionEventPage> =>
        afterSequence === 0
          ? { items: [event(1, "turn.created"), event(2)], lastSequence: 2 }
          : { items: [], lastSequence: afterSequence },
    );
    const subscribeSessionEvents = vi.fn(
      (
        _sessionId: string,
        _afterSequence: number,
        nextHandlers: NonNullable<typeof handlers>,
      ) => {
        handlers = nextHandlers;
        return close;
      },
    );
    let latest = new Map();
    const runtime = new ControlPlaneProjectionRuntime({
      client: { listSessionEvents, subscribeSessionEvents },
      onChange: (projections) => {
        latest = new Map(projections);
      },
      reconnectDelayMs: 0,
    });
    runtime.setScope("tenant-1:organization-1", [session]);

    const unwatchOne = runtime.watch(session.id);
    const unwatchTwo = runtime.watch(session.id);
    await vi.waitFor(() => expect(subscribeSessionEvents).toHaveBeenCalledTimes(1));

    expect(subscribeSessionEvents).toHaveBeenCalledWith(
      session.id,
      2,
      expect.any(Object),
    );
    handlers?.onOpen?.();
    expect(latest.get(session.id)?.streamStatus).toBe("live");

    unwatchOne();
    expect(close).not.toHaveBeenCalled();
    unwatchTwo();
    expect(close).toHaveBeenCalledTimes(1);
    runtime.dispose();
  });

  it("closes the old Tenant stream when scope changes", async () => {
    const close = vi.fn();
    const runtime = new ControlPlaneProjectionRuntime({
      client: {
        listSessionEvents: async () => ({ items: [], lastSequence: 0 }),
        subscribeSessionEvents: () => close,
      },
      onChange: () => undefined,
    });
    runtime.setScope("tenant-1:organization-1", [{ ...session, lastEventSequence: 0 }]);
    runtime.watch(session.id);
    await vi.waitFor(() => expect(runtime.projections.get(session.id)?.streamStatus).toBe("connecting"));
    runtime.setScope("tenant-2:organization-2", []);

    expect(close).toHaveBeenCalledTimes(1);
    expect(runtime.projections.size).toBe(0);
    runtime.dispose();
  });
});
