import { afterEach, describe, expect, it, vi } from "vitest";

import {
  controlPlaneClient,
  type ControlPlaneAgentSession,
  type ControlPlaneSessionEvent,
  type ControlPlaneSessionEventPage,
} from "./controlPlaneClient";
import { ControlPlaneProjectionRuntime } from "./controlPlaneProjectionRuntime";

class FakeEventSource {
  static instances: FakeEventSource[] = [];

  readonly close = vi.fn();
  readonly listeners = new Map<string, (event: MessageEvent<string>) => void>();
  onerror: (() => void) | null = null;
  onopen: (() => void) | null = null;

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (event: MessageEvent<string>) => void) {
    this.listeners.set(type, listener);
  }

  emitSessionEvent(nextEvent: ControlPlaneSessionEvent) {
    this.listeners.get("session-event")?.({
      data: JSON.stringify(nextEvent),
    } as MessageEvent<string>);
  }
}

afterEach(() => {
  FakeEventSource.instances = [];
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

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
      (_sessionId: string, _afterSequence: number, nextHandlers: NonNullable<typeof handlers>) => {
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

    expect(subscribeSessionEvents).toHaveBeenCalledWith(session.id, 2, expect.any(Object));
    handlers?.onOpen?.();
    expect(latest.get(session.id)?.streamStatus).toBe("live");

    unwatchOne();
    expect(close).not.toHaveBeenCalled();
    unwatchTwo();
    expect(close).toHaveBeenCalledTimes(1);
    runtime.dispose();
  });

  it("starts a watcher that is registered before its Session scope is published", async () => {
    const close = vi.fn();
    const listSessionEvents = vi.fn(
      async (_sessionId: string, afterSequence: number): Promise<ControlPlaneSessionEventPage> => ({
        items: afterSequence === 0 ? [event(1, "turn.created"), event(2)] : [],
        lastSequence: 2,
      }),
    );
    const subscribeSessionEvents = vi.fn(() => close);
    const runtime = new ControlPlaneProjectionRuntime({
      client: { listSessionEvents, subscribeSessionEvents },
      onChange: () => undefined,
    });

    const stopWatching = runtime.watch(session.id);
    expect(subscribeSessionEvents).not.toHaveBeenCalled();

    runtime.setScope("tenant-1:organization-1", [session]);
    await vi.waitFor(() => expect(subscribeSessionEvents).toHaveBeenCalledTimes(1));
    expect(subscribeSessionEvents).toHaveBeenCalledWith(session.id, 2, expect.any(Object));

    stopWatching();
    expect(close).toHaveBeenCalledTimes(1);
    runtime.dispose();
  });

  it("keeps watched catch-up failures reconnecting until the live stream opens", async () => {
    let handlers:
      | {
          onEvent: (event: ControlPlaneSessionEvent) => void;
          onOpen?: () => void;
          onError?: () => void;
        }
      | undefined;
    let catchUpAttempt = 0;
    const listSessionEvents = vi.fn(
      async (
        _sessionId: string,
        _afterSequence: number,
        _limit: number,
      ): Promise<ControlPlaneSessionEventPage> => {
        catchUpAttempt += 1;
        if (catchUpAttempt === 1) throw new Error("Control Plane unavailable");
        return { items: [event(1, "turn.created"), event(2)], lastSequence: 2 };
      },
    );
    const subscribeSessionEvents = vi.fn(
      (_sessionId: string, _afterSequence: number, nextHandlers: NonNullable<typeof handlers>) => {
        handlers = nextHandlers;
        return () => undefined;
      },
    );
    const runtime = new ControlPlaneProjectionRuntime({
      client: { listSessionEvents, subscribeSessionEvents },
      onChange: () => undefined,
      reconnectDelayMs: 0,
    });
    runtime.setScope("tenant-1:organization-1", [session]);

    runtime.watch(session.id);
    await vi.waitFor(() => expect(subscribeSessionEvents).toHaveBeenCalledTimes(1));
    expect(listSessionEvents).toHaveBeenCalledTimes(2);
    expect(runtime.projections.get(session.id)?.streamStatus).toBe("reconnecting");

    handlers?.onOpen?.();
    expect(runtime.projections.get(session.id)?.streamStatus).toBe("live");

    runtime.dispose();
  });

  it("does not let an in-flight catch-up hide a concurrent stream failure", async () => {
    let handlers:
      | {
          onEvent: (event: ControlPlaneSessionEvent) => void;
          onOpen?: () => void;
          onError?: () => void;
        }
      | undefined;
    let resolveCatchUp: (page: ControlPlaneSessionEventPage) => void = () => undefined;
    const pendingCatchUp = new Promise<ControlPlaneSessionEventPage>((resolve) => {
      resolveCatchUp = resolve;
    });
    let catchUpAttempt = 0;
    const listSessionEvents = vi.fn(async (): Promise<ControlPlaneSessionEventPage> => {
      catchUpAttempt += 1;
      if (catchUpAttempt === 1) {
        return { items: [event(1, "turn.created"), event(2)], lastSequence: 2 };
      }
      return pendingCatchUp;
    });
    const subscribeSessionEvents = vi.fn(
      (_sessionId: string, _afterSequence: number, nextHandlers: NonNullable<typeof handlers>) => {
        handlers = nextHandlers;
        return () => undefined;
      },
    );
    const runtime = new ControlPlaneProjectionRuntime({
      client: { listSessionEvents, subscribeSessionEvents },
      onChange: () => undefined,
      reconnectDelayMs: 60_000,
    });
    runtime.setScope("tenant-1:organization-1", [session]);
    runtime.watch(session.id);
    await vi.waitFor(() => expect(subscribeSessionEvents).toHaveBeenCalledTimes(1));
    handlers?.onOpen?.();

    const catchUp = runtime.catchUp(session.id);
    await vi.waitFor(() => expect(listSessionEvents).toHaveBeenCalledTimes(2));
    handlers?.onError?.();
    resolveCatchUp({ items: [], lastSequence: 2 });
    await catchUp;

    expect(runtime.projections.get(session.id)?.streamStatus).toBe("reconnecting");
    runtime.dispose();
  });

  it("keeps an active stream live during a successful manual catch-up", async () => {
    let handlers:
      | {
          onEvent: (event: ControlPlaneSessionEvent) => void;
          onOpen?: () => void;
          onError?: () => void;
        }
      | undefined;
    let resolveCatchUp: (page: ControlPlaneSessionEventPage) => void = () => undefined;
    const pendingCatchUp = new Promise<ControlPlaneSessionEventPage>((resolve) => {
      resolveCatchUp = resolve;
    });
    let catchUpAttempt = 0;
    const listSessionEvents = vi.fn(async (): Promise<ControlPlaneSessionEventPage> => {
      catchUpAttempt += 1;
      if (catchUpAttempt === 1) {
        return { items: [event(1, "turn.created"), event(2)], lastSequence: 2 };
      }
      return pendingCatchUp;
    });
    const subscribeSessionEvents = vi.fn(
      (_sessionId: string, _afterSequence: number, nextHandlers: NonNullable<typeof handlers>) => {
        handlers = nextHandlers;
        return () => undefined;
      },
    );
    const runtime = new ControlPlaneProjectionRuntime({
      client: { listSessionEvents, subscribeSessionEvents },
      onChange: () => undefined,
    });
    runtime.setScope("tenant-1:organization-1", [session]);
    runtime.watch(session.id);
    await vi.waitFor(() => expect(subscribeSessionEvents).toHaveBeenCalledTimes(1));
    handlers?.onOpen?.();

    const catchUp = runtime.catchUp(session.id);
    await vi.waitFor(() => expect(listSessionEvents).toHaveBeenCalledTimes(2));
    expect(runtime.projections.get(session.id)?.streamStatus).toBe("live");

    resolveCatchUp({ items: [], lastSequence: 2 });
    await catchUp;

    expect(runtime.projections.get(session.id)?.streamStatus).toBe("live");
    runtime.dispose();
  });

  it("does not replace an active live stream when manual catch-up fails", async () => {
    vi.useFakeTimers();
    let handlers:
      | {
          onEvent: (event: ControlPlaneSessionEvent) => void;
          onOpen?: () => void;
          onError?: () => void;
        }
      | undefined;
    const close = vi.fn();
    let catchUpAttempt = 0;
    const listSessionEvents = vi.fn(async (): Promise<ControlPlaneSessionEventPage> => {
      catchUpAttempt += 1;
      if (catchUpAttempt === 1) {
        return { items: [event(1, "turn.created"), event(2)], lastSequence: 2 };
      }
      throw new Error("Control Plane unavailable");
    });
    const subscribeSessionEvents = vi.fn(
      (_sessionId: string, _afterSequence: number, nextHandlers: NonNullable<typeof handlers>) => {
        handlers = nextHandlers;
        return close;
      },
    );
    const runtime = new ControlPlaneProjectionRuntime({
      client: { listSessionEvents, subscribeSessionEvents },
      onChange: () => undefined,
      reconnectDelayMs: 2_000,
    });
    runtime.setScope("tenant-1:organization-1", [session]);
    runtime.watch(session.id);
    await vi.waitFor(() => expect(subscribeSessionEvents).toHaveBeenCalledTimes(1));
    handlers?.onOpen?.();

    await expect(runtime.catchUp(session.id)).rejects.toThrow(
      "Failed to catch up Session Events for session-1.",
    );
    expect(runtime.projections.get(session.id)?.streamStatus).toBe("live");
    expect(close).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(2_000);
    expect(subscribeSessionEvents).toHaveBeenCalledTimes(1);

    runtime.dispose();
    expect(close).toHaveBeenCalledTimes(1);
  });

  it("keeps a single reconnect loop when using the real SSE client", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("window", { location: new URL("https://synara.example/settings") });
    vi.stubGlobal("EventSource", FakeEventSource);
    const listSessionEvents = vi.fn(
      async (_sessionId: string, afterSequence: number): Promise<ControlPlaneSessionEventPage> =>
        afterSequence === 0
          ? { items: [event(1, "turn.created"), event(2)], lastSequence: 2 }
          : { items: [], lastSequence: afterSequence },
    );
    const runtime = new ControlPlaneProjectionRuntime({
      client: {
        listSessionEvents,
        subscribeSessionEvents: controlPlaneClient.subscribeSessionEvents,
      },
      onChange: () => undefined,
      reconnectDelayMs: 2_000,
    });
    runtime.setScope("tenant-1:organization-1", [session]);
    runtime.watch(session.id);
    await vi.waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));

    const firstSource = FakeEventSource.instances[0]!;
    expect(firstSource.url).toBe(
      "https://synara.example/v1/sessions/session-1/events/stream?afterSequence=2",
    );
    firstSource.onopen?.();
    expect(runtime.projections.get(session.id)?.streamStatus).toBe("live");
    firstSource.emitSessionEvent(event(3));
    expect(runtime.projections.get(session.id)?.lastAppliedSequence).toBe(3);
    firstSource.onerror?.();
    expect(firstSource.close).toHaveBeenCalledTimes(1);
    expect(runtime.projections.get(session.id)?.streamStatus).toBe("reconnecting");

    await vi.advanceTimersByTimeAsync(1_999);
    expect(FakeEventSource.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    await vi.waitFor(() => expect(FakeEventSource.instances).toHaveLength(2));
    expect(listSessionEvents).toHaveBeenCalledTimes(2);
    expect(listSessionEvents).toHaveBeenLastCalledWith(session.id, 3, 500);
    expect(FakeEventSource.instances[1]!.url).toBe(
      "https://synara.example/v1/sessions/session-1/events/stream?afterSequence=3",
    );
    expect(runtime.projections.get(session.id)?.streamStatus).toBe("reconnecting");
    FakeEventSource.instances[1]!.onopen?.();
    expect(runtime.projections.get(session.id)?.streamStatus).toBe("live");

    runtime.dispose();
    expect(FakeEventSource.instances[1]!.close).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(FakeEventSource.instances).toHaveLength(2);
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
    await vi.waitFor(() =>
      expect(runtime.projections.get(session.id)?.streamStatus).toBe("connecting"),
    );
    runtime.setScope("tenant-2:organization-2", []);

    expect(close).toHaveBeenCalledTimes(1);
    expect(runtime.projections.size).toBe(0);
    runtime.dispose();
  });
});
