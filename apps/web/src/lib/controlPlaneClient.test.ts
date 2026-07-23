import { ThreadId } from "@synara/contracts";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  controlPlaneClient,
  ControlPlaneError,
  resolveAuditLogExportUrl,
  resolveControlPlaneHttpUrl,
  resolveSessionEventStreamUrl,
} from "./controlPlaneClient";

type RequiredInitFetch = (input: RequestInfo | URL, init: RequestInit) => Promise<Response>;

class FakeEventSource {
  static instances: FakeEventSource[] = [];

  readonly close = vi.fn();
  readonly listeners = new Map<string, (event: MessageEvent<string>) => void>();
  onerror: (() => void) | null = null;
  onopen: (() => void) | null = null;

  constructor(
    readonly url: string,
    readonly options: EventSourceInit,
  ) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (event: MessageEvent<string>) => void) {
    this.listeners.set(type, listener);
  }

  emitSessionEvent(event: { sequence: unknown }) {
    this.listeners.get("session-event")?.({ data: JSON.stringify(event) } as MessageEvent<string>);
  }
}

afterEach(() => {
  FakeEventSource.instances = [];
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("controlPlaneClient", () => {
  it("uses the page origin for browser-hosted web builds", () => {
    vi.stubGlobal("window", { location: new URL("http://localhost:8891/settings") });
    expect(resolveControlPlaneHttpUrl("/v1/auth/session")).toBe(
      "http://localhost:8891/v1/auth/session",
    );
  });

  it("builds a resumable same-origin session event stream URL", () => {
    vi.stubGlobal("window", { location: new URL("https://synara.example/settings") });
    expect(resolveSessionEventStreamUrl("session/one", 12)).toBe(
      "https://synara.example/v1/sessions/session%2Fone/events/stream?afterSequence=12",
    );
    expect(resolveSessionEventStreamUrl("session/one", Number.NaN)).toBe(
      "https://synara.example/v1/sessions/session%2Fone/events/stream?afterSequence=0",
    );
  });

  it("reconnects session event streams from the last applied sequence", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("window", { location: new URL("https://synara.example/settings") });
    vi.stubGlobal("EventSource", FakeEventSource);
    const onEvent = vi.fn();
    const onError = vi.fn();
    const onOpen = vi.fn();
    const close = controlPlaneClient.subscribeSessionEvents("session/one", 12, {
      onEvent,
      onError,
      onOpen,
    });

    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0]).toMatchObject({
      options: { withCredentials: true },
      url: "https://synara.example/v1/sessions/session%2Fone/events/stream?afterSequence=12",
    });
    FakeEventSource.instances[0]!.onopen?.();
    FakeEventSource.instances[0]!.emitSessionEvent({ sequence: 13 });
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(onEvent).toHaveBeenCalledWith({ sequence: 13 });

    FakeEventSource.instances[0]!.onerror?.();
    expect(FakeEventSource.instances[0]!.close).toHaveBeenCalledTimes(1);
    expect(onError).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2_000);

    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.instances[1]!.url).toBe(
      "https://synara.example/v1/sessions/session%2Fone/events/stream?afterSequence=13",
    );
    FakeEventSource.instances[1]!.onopen?.();
    expect(onOpen).toHaveBeenCalledTimes(2);

    onEvent.mockImplementationOnce(() => {
      throw new Error("projection rejected event");
    });
    FakeEventSource.instances[1]!.emitSessionEvent({ sequence: 14 });
    expect(FakeEventSource.instances[1]!.close).toHaveBeenCalledTimes(1);
    expect(onError).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(2_000);

    expect(FakeEventSource.instances).toHaveLength(3);
    expect(FakeEventSource.instances[2]!.url).toBe(
      "https://synara.example/v1/sessions/session%2Fone/events/stream?afterSequence=13",
    );

    close();
    expect(FakeEventSource.instances[2]!.close).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(FakeEventSource.instances).toHaveLength(3);
  });

  it.each([0, -1, 1.5, "13", null, Number.MAX_SAFE_INTEGER + 1])(
    "rejects invalid session event sequence %s without advancing the cursor",
    async (sequence) => {
      vi.useFakeTimers();
      vi.stubGlobal("window", { location: new URL("https://synara.example/settings") });
      vi.stubGlobal("EventSource", FakeEventSource);
      const onEvent = vi.fn();
      const onError = vi.fn();
      const close = controlPlaneClient.subscribeSessionEvents("session-one", 12, {
        onEvent,
        onError,
      });

      FakeEventSource.instances[0]!.emitSessionEvent({ sequence });
      expect(FakeEventSource.instances[0]!.close).toHaveBeenCalledTimes(1);
      expect(onEvent).not.toHaveBeenCalled();
      expect(onError).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(2_000);

      expect(FakeEventSource.instances).toHaveLength(2);
      expect(FakeEventSource.instances[1]!.url).toBe(
        "https://synara.example/v1/sessions/session-one/events/stream?afterSequence=12",
      );
      close();
    },
  );

  it("deduplicates events, reconnects gaps, and ignores stale sources", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("window", { location: new URL("https://synara.example/settings") });
    vi.stubGlobal("EventSource", FakeEventSource);
    const onEvent = vi.fn();
    const onError = vi.fn();
    const close = controlPlaneClient.subscribeSessionEvents("session-one", 12, {
      onEvent,
      onError,
    });

    const firstSource = FakeEventSource.instances[0]!;
    firstSource.emitSessionEvent({ sequence: 13 });
    firstSource.emitSessionEvent({ sequence: 13 });
    expect(onEvent).toHaveBeenCalledTimes(1);
    expect(firstSource.close).not.toHaveBeenCalled();

    firstSource.emitSessionEvent({ sequence: 15 });
    expect(firstSource.close).toHaveBeenCalledTimes(1);
    expect(onError).toHaveBeenCalledTimes(1);

    firstSource.emitSessionEvent({ sequence: 14 });
    expect(onEvent).toHaveBeenCalledTimes(1);
    expect(onError).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2_000);

    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.instances[1]!.url).toBe(
      "https://synara.example/v1/sessions/session-one/events/stream?afterSequence=13",
    );
    FakeEventSource.instances[1]!.emitSessionEvent({ sequence: 14 });
    expect(onEvent).toHaveBeenCalledTimes(2);
    expect(onEvent).toHaveBeenLastCalledWith({ sequence: 14 });

    close();
    FakeEventSource.instances[1]!.emitSessionEvent({ sequence: 15 });
    expect(onEvent).toHaveBeenCalledTimes(2);
  });

  it("probes the public platform profile before requiring authentication", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            profile: "single-node",
            metadataStore: "postgresql",
            artifactStore: "minio",
            queueDriver: "postgres-outbox",
            controlPlaneReplicas: 1,
            highAvailability: false,
            leaseEnabled: true,
            fencingEnabled: true,
            executionTargetKinds: ["docker", "kubernetes", "local", "ssh"],
            artifactPayloadMigration: true,
            metadataExportImport: true,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const profile = await controlPlaneClient.getPlatformProfile();

    expect(profile.profile).toBe("single-node");
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/platform/profile",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("sends JSON with same-origin credentials", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            authenticated: true,
            user: {
              userId: "user-1",
              sessionId: "session-1",
              activeTenantId: "tenant-1",
              email: "owner@example.com",
              displayName: "Owner",
            },
            tenants: [],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.devLogin({ email: "owner@example.com", displayName: "Owner" });

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/auth/dev-login",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        body: JSON.stringify({ email: "owner@example.com", displayName: "Owner" }),
      }),
    );
  });

  it("preserves stable API error details", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              error: {
                code: "tenant_forbidden",
                message: "Tenant access denied.",
                requestId: "request-1",
                details: { currentPolicyVersion: 4 },
              },
            }),
            { status: 403, headers: { "Content-Type": "application/json" } },
          ),
      ),
    );

    await expect(controlPlaneClient.listOrganizations("tenant-1")).rejects.toEqual(
      expect.objectContaining<Partial<ControlPlaneError>>({
        status: 403,
        code: "tenant_forbidden",
        requestId: "request-1",
        details: { currentPolicyVersion: 4 },
      }),
    );
  });

  it("creates agent sessions through the active-tenant project route", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            id: "session-1",
            tenantId: "tenant-1",
            organizationId: "organization-1",
            projectId: "project-1",
            createdBy: "user-1",
            title: "Session",
            status: "active",
            visibility: "private",
            provider: "codex",
            model: "gpt-5.6-sol",
            executionTargetId: "target-1",
            lastEventSequence: 1,
            createdAt: "2026-07-12T00:00:00Z",
            updatedAt: "2026-07-12T00:00:00Z",
            archivedAt: null,
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.createSession(
      "project-1",
      {
        title: "Session",
        visibility: "private",
        provider: "codex",
        model: "gpt-5.6-sol",
        executionTargetId: "target-1",
      },
      { idempotencyKey: "web-session-request-1" },
    );

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/projects/project-1/sessions",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        body: JSON.stringify({
          title: "Session",
          visibility: "private",
          provider: "codex",
          model: "gpt-5.6-sol",
          executionTargetId: "target-1",
        }),
      }),
    );
    const request = fetchMock.mock.calls[0]![1];
    expect(new Headers(request.headers).get("Idempotency-Key")).toBe("web-session-request-1");
  });

  it("loads project Provider capabilities with the resolved or explicit execution target", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            executionTargetId: "target/one",
            targetKind: "kubernetes",
            basis: "target",
            items: [],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.getProjectProviderCapabilities("project/one");
    await controlPlaneClient.getProjectProviderCapabilities("project/one", "target/one");

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/projects/project%2Fone/provider-capabilities",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/projects/project%2Fone/provider-capabilities?executionTargetId=target%2Fone",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("loads execution-bound Session Provider capabilities", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            executionTargetId: "target-1",
            targetKind: "docker",
            executionId: "execution-1",
            basis: "execution",
            items: [
              {
                provider: "droid",
                capabilityId: "send-turn",
                status: "unsupported",
                reasonCode: "capability_unsupported",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const result = await controlPlaneClient.getSessionProviderCapabilities("session/one");

    expect(result.basis).toBe("execution");
    expect(result.items[0]).toMatchObject({ provider: "droid", status: "unsupported" });
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/sessions/session%2Fone/provider-capabilities",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("switches a Session model through the dedicated model-switch route", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            id: "session-1",
            tenantId: "tenant-1",
            organizationId: "organization-1",
            projectId: "project-1",
            createdBy: "user-1",
            title: "Session",
            status: "active",
            visibility: "private",
            provider: "codex",
            model: "gpt-5.6-sol",
            executionTargetId: "target-1",
            lastEventSequence: 7,
            createdAt: "2026-07-12T00:00:00Z",
            updatedAt: "2026-07-12T00:00:07Z",
            archivedAt: null,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.switchSessionModel(
      "session/one",
      { model: "gpt-5.6-sol", expectedModel: "gpt-5" },
      { idempotencyKey: "web-session-model-switch-1" },
    );

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/sessions/session%2Fone/model-switch",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        body: JSON.stringify({ model: "gpt-5.6-sol", expectedModel: "gpt-5" }),
      }),
    );
    const request = fetchMock.mock.calls[0]![1];
    expect(new Headers(request.headers).get("Idempotency-Key")).toBe("web-session-model-switch-1");
  });

  it("sends expectedModel null explicitly when the Session has no current model", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            id: "session-1",
            tenantId: "tenant-1",
            organizationId: "organization-1",
            projectId: "project-1",
            createdBy: "user-1",
            title: "Session",
            status: "active",
            visibility: "private",
            provider: "claudeAgent",
            model: "claude-sonnet-5",
            executionTargetId: "target-1",
            lastEventSequence: 7,
            createdAt: "2026-07-12T00:00:00Z",
            updatedAt: "2026-07-12T00:00:07Z",
            archivedAt: null,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.switchSessionModel("session/one", {
      model: "claude-sonnet-5",
      expectedModel: null,
    });

    const request = fetchMock.mock.calls[0]![1];
    expect(request.body).toBe(JSON.stringify({ model: "claude-sonnet-5", expectedModel: null }));
  });

  it("creates and updates Project metadata without the retired Git Credential field", async () => {
    const responses = [
      new Response(JSON.stringify({ id: "project-1", gitCredentialId: null }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(JSON.stringify({ id: "project-1", gitCredentialId: null }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.createProject(
      "tenant/one",
      "organization/one",
      {
        name: "Private repository",
        repositoryUrl: "https://github.com/company/private.git",
        defaultBranch: "main",
        visibility: "organization",
      },
      { idempotencyKey: "web-project-request-1" },
    );
    await controlPlaneClient.updateProject("project/one", { defaultBranch: "trunk" });

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/tenants/tenant%2Fone/organizations/organization%2Fone/projects",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          name: "Private repository",
          repositoryUrl: "https://github.com/company/private.git",
          defaultBranch: "main",
          visibility: "organization",
        }),
      }),
    );
    const createRequest = fetchMock.mock.calls[0]![1];
    expect(new Headers(createRequest.headers).get("Idempotency-Key")).toBe("web-project-request-1");
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/projects/project%2Fone",
      expect.objectContaining({
        method: "PATCH",
        body: JSON.stringify({ defaultBranch: "trunk" }),
      }),
    );
  });

  it("loads bounded durable event backlog and sends idempotent Turns", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [], lastSequence: 23 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(
        JSON.stringify({
          id: "turn-1",
          tenantId: "tenant-1",
          sessionId: "session-1",
          createdBy: "user-1",
          status: "queued",
          inputText: "Continue",
          startedAt: null,
          completedAt: null,
          createdAt: "2026-07-12T00:00:00Z",
        }),
        { status: 201, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    const page = await controlPlaneClient.listSessionEvents("session/one", -5, 5_000);
    await controlPlaneClient.createTurn(
      "session/one",
      "Continue",
      {
        idempotencyKey: "web-turn-request-1",
      },
      {
        runtimeMode: "approval-required",
        interactionMode: "plan",
      },
      {
        threadId: ThreadId.makeUnsafe("source-session"),
        planId: "source-plan",
      },
    );

    expect(page.lastSequence).toBe(23);
    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/sessions/session%2Fone/events?afterSequence=0&limit=500",
      expect.objectContaining({ credentials: "include" }),
    );
    const turnRequest = fetchMock.mock.calls[1]![1];
    expect(new Headers(turnRequest.headers).get("Idempotency-Key")).toBe("web-turn-request-1");
    expect(JSON.parse(String(turnRequest.body))).toEqual({
      inputText: "Continue",
      runtimeMode: "approval-required",
      interactionMode: "plan",
      sourceProposedPlan: {
        threadId: "source-session",
        planId: "source-plan",
      },
    });
  });

  it("routes advanced Session commands through encoded idempotent Control Plane endpoints", async () => {
    const specialResult = (type: "compact" | "review", sessionId = "session/one") => ({
      type,
      turn: {
        id: `turn-${type}`,
        tenantId: "tenant-1",
        sessionId,
        createdBy: "user-1",
        status: "queued",
        inputText: type,
        turnKind: type,
        runtimeMode: "approval-required",
        interactionMode: "default",
        startedAt: null,
        completedAt: null,
        createdAt: "2026-07-15T00:00:00Z",
      },
      executionId: `execution-${type}`,
      controlCommand: {
        id: `control-${type}`,
        executionId: `execution-${type}`,
        sessionId,
        turnId: `turn-${type}`,
        provider: "codex",
        commandType: type === "compact" ? "CompactSession" : "StartReview",
        commandId: `${type}:control-${type}`,
        payload: {},
        status: "pending",
        requestedBy: "user-1",
        requestedAt: "2026-07-15T00:00:00Z",
        deliveryAttempts: 0,
        deliveryAvailableAt: "2026-07-15T00:00:00Z",
      },
    });
    const responses = [
      new Response(JSON.stringify(specialResult("compact")), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(JSON.stringify(specialResult("review", "review/session")), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(
        JSON.stringify({
          sessionId: "session/one",
          eventId: "event-rollback",
          eventSequence: 42,
          fromSessionId: "session/one",
          fromTurnId: "turn/one",
          fromSequence: 17,
          removedTurnCount: 1,
          supportMode: "emulated",
          workspaceDisposition: "unchanged",
          externalSideEffectsReverted: false,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
      new Response(
        JSON.stringify({
          session: { id: "fork/session" },
          sourceSessionId: "session/one",
          sourceEventSequence: 41,
          supportMode: "emulated",
        }),
        { status: 201, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.compactSession("session/one", 41, {
      idempotencyKey: "compact-key",
    });
    await controlPlaneClient.startReview(
      "session/one",
      {
        expectedLastEventSequence: 41,
        runtimeMode: "approval-required",
        target: { type: "baseBranch", branch: "main" },
      },
      { idempotencyKey: "review-key" },
    );
    const rollback = await controlPlaneClient.rollbackSession(
      "session/one",
      { expectedLastEventSequence: 41, fromTurnId: "turn/one" },
      { idempotencyKey: "rollback-key" },
    );
    await controlPlaneClient.forkSession(
      "session/one",
      { expectedLastEventSequence: 41, title: "Forked", visibility: "private" },
      { idempotencyKey: "fork-key" },
    );

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/sessions/session%2Fone/compact",
      "/v1/sessions/session%2Fone/reviews",
      "/v1/sessions/session%2Fone/rollback",
      "/v1/sessions/session%2Fone/fork",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[0]![1].body))).toEqual({
      expectedLastEventSequence: 41,
    });
    expect(JSON.parse(String(fetchMock.mock.calls[1]![1].body))).toEqual({
      expectedLastEventSequence: 41,
      runtimeMode: "approval-required",
      target: { type: "baseBranch", branch: "main" },
    });
    expect(JSON.parse(String(fetchMock.mock.calls[2]![1].body))).toEqual({
      expectedLastEventSequence: 41,
      fromTurnId: "turn/one",
    });
    expect(JSON.parse(String(fetchMock.mock.calls[3]![1].body))).toEqual({
      expectedLastEventSequence: 41,
      title: "Forked",
      visibility: "private",
    });
    expect(
      fetchMock.mock.calls.map(([, init]) => new Headers(init.headers).get("Idempotency-Key")),
    ).toEqual(["compact-key", "review-key", "rollback-key", "fork-key"]);
    expect(rollback).toMatchObject({
      supportMode: "emulated",
      workspaceDisposition: "unchanged",
      externalSideEffectsReverted: false,
    });
  });

  it("requests an idempotent durable interrupt for the active Turn", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            id: "control-1",
            executionId: "execution-1",
            sessionId: "session/one",
            turnId: "turn-1",
            provider: "codex",
            commandType: "InterruptTurn",
            commandId: "interrupt:control-1",
            payload: { turnId: "turn-1" },
            status: "pending",
            requestedBy: "user-1",
            requestedAt: "2026-07-13T00:00:00Z",
            deliveryAttempts: 0,
            deliveryAvailableAt: "2026-07-13T00:00:00Z",
          }),
          { status: 202, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.interruptActiveTurn("session/one", {
      idempotencyKey: "web-interrupt-1",
    });

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/sessions/session%2Fone/turns/active/interrupt",
      expect.objectContaining({ method: "POST", credentials: "include" }),
    );
    const request = fetchMock.mock.calls[0]![1];
    expect(new Headers(request.headers).get("Idempotency-Key")).toBe("web-interrupt-1");
  });

  it("loads the Session pending Interaction snapshot and resolves through encoded durable routes", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [], snapshotSequence: 17 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(
        JSON.stringify({
          id: "interaction-1",
          executionId: "execution/one",
          sessionId: "session/one",
          requestId: "approval/one",
          kind: "approval",
          status: "resolved",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
      new Response(
        JSON.stringify({
          id: "interaction-2",
          executionId: "execution/one",
          sessionId: "session/one",
          requestId: "input/one",
          kind: "user-input",
          status: "resolved",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    const snapshot = await controlPlaneClient.listPendingInteractions("session/one");
    await controlPlaneClient.resolveApproval("execution/one", "approval/one", "accept", {
      idempotencyKey: "web-approval-1",
    });
    await controlPlaneClient.resolveUserInput(
      "execution/one",
      "input/one",
      { environment: "staging" },
      { idempotencyKey: "web-input-1" },
    );

    expect(snapshot.snapshotSequence).toBe(17);
    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/sessions/session%2Fone/interactions",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/executions/execution%2Fone/approvals/approval%2Fone/resolve",
      expect.objectContaining({ method: "POST", body: JSON.stringify({ decision: "accept" }) }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      3,
      "/v1/executions/execution%2Fone/user-input/input%2Fone/resolve",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ answers: { environment: "staging" } }),
      }),
    );
    expect(new Headers(fetchMock.mock.calls[1]![1].headers).get("Idempotency-Key")).toBe(
      "web-approval-1",
    );
    expect(new Headers(fetchMock.mock.calls[2]![1].headers).get("Idempotency-Key")).toBe(
      "web-input-1",
    );
  });

  it("requests an idempotent durable Steer payload for the active Turn", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            id: "control-steer-1",
            executionId: "execution-1",
            sessionId: "session/one",
            turnId: "turn-1",
            provider: "codex",
            commandType: "SteerTurn",
            commandId: "steer:control-steer-1",
            payload: { turnId: "turn-1", inputText: "Focus on tests" },
            status: "pending",
            requestedBy: "user-1",
            requestedAt: "2026-07-13T00:00:00Z",
            deliveryAttempts: 0,
            deliveryAvailableAt: "2026-07-13T00:00:00Z",
          }),
          { status: 202, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.steerActiveTurn("session/one", "Focus on tests", {
      idempotencyKey: "web-steer-1",
    });

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/sessions/session%2Fone/turns/active/steer",
      expect.objectContaining({ method: "POST", credentials: "include" }),
    );
    const request = fetchMock.mock.calls[0]![1];
    expect(new Headers(request.headers).get("Idempotency-Key")).toBe("web-steer-1");
    expect(JSON.parse(String(request.body))).toEqual({ inputText: "Focus on tests" });
  });

  it("creates execution targets without expecting secret configuration in the response", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            id: "target-1",
            tenantId: "tenant-1",
            organizationId: "organization-1",
            kind: "ssh",
            name: "Build host",
            status: "active",
            capabilities: { workspaceModes: ["local"] },
            createdAt: "2026-07-12T00:00:00Z",
            updatedAt: "2026-07-12T00:00:00Z",
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const target = await controlPlaneClient.createExecutionTarget("tenant-1", {
      organizationId: "organization-1",
      kind: "ssh",
      name: "Build host",
      configuration: { host: "build.internal" },
      capabilities: { workspaceModes: ["local"] },
    });

    expect(target).not.toHaveProperty("configuration");
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant-1/execution-targets",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        body: JSON.stringify({
          organizationId: "organization-1",
          kind: "ssh",
          name: "Build host",
          configuration: { host: "build.internal" },
          capabilities: { workspaceModes: ["local"] },
        }),
      }),
    );
  });

  it("lists tenant-scoped observed Worker Manifests", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            items: [
              {
                executionTargetId: "target-1",
                manifestId: "manifest-1",
                workerStatusCounts: { online: 2, draining: 1, offline: 0 },
                lastHeartbeatAt: "2026-07-14T08:00:00Z",
                workerBuild: {
                  version: "0.5.2",
                  gitSha: "abc123",
                  imageDigest: "sha256:worker",
                  operatingSystem: "linux",
                  architecture: "arm64",
                },
                workerProtocol: { minimum: 2, maximum: 2 },
                runtimeEvent: { minimum: 2, maximum: 2 },
                providers: [
                  {
                    provider: "codex",
                    supportTier: "experimental",
                    compatibilityStatus: "compatible",
                    runtime: {
                      kind: "cli",
                      name: "codex-cli",
                      version: "0.145.0",
                      available: true,
                      versionSource: "probe",
                      compatibleRange: {
                        minimumInclusive: "0.145.0",
                        maximumExclusive: "0.146.0",
                      },
                      compatible: true,
                    },
                    releasePolicy: {
                      requiresExplicitEnablement: true,
                      enabled: true,
                    },
                    capabilities: { discovery: "native" },
                  },
                ],
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const page = await controlPlaneClient.listWorkerManifests("tenant/one");

    expect(page.items[0]).toMatchObject({
      executionTargetId: "target-1",
      manifestId: "manifest-1",
      providers: [
        expect.objectContaining({
          provider: "codex",
          compatibilityStatus: "compatible",
        }),
      ],
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/worker-manifests",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("lists tenant Workers and revokes the exact incarnation with CAS idempotency", async () => {
    const fetchMock = vi
      .fn<RequiredInitFetch>()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [
              {
                id: "worker-1",
                incarnation: 2,
                instanceUid: "pod-uid-1",
                executionTargetId: "target-1",
                targetKind: "kubernetes",
                clusterId: "cluster-a",
                namespace: "synara-workers",
                podName: "synara-exec-1-g2",
                version: "0.6.0",
                protocolVersion: 2,
                currentManifestId: "manifest-1",
                compatibilityStatus: "compatible",
                compatibilityReason: null,
                compatibilityCheckedAt: "2026-07-18T18:50:20.000Z",
                workerReleaseRevisionId: "release-2",
                workerReleaseChannel: "promoted",
                workerReleaseStatus: "active",
                workerReleaseReason: null,
                workerReleaseCheckedAt: "2026-07-18T18:50:21.000Z",
                leaseSupported: true,
                fencingSupported: true,
                status: "online",
                administrativeStatus: "active",
                registeredAt: "2026-07-18T18:49:19.000Z",
                lastHeartbeatAt: "2026-07-18T18:50:19.000Z",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            worker: {
              id: "worker-1",
              incarnation: 2,
              instanceUid: "pod-uid-1",
              executionTargetId: "target-1",
              targetKind: "kubernetes",
              clusterId: "cluster-a",
              namespace: "synara-workers",
              podName: "synara-exec-1-g2",
              version: "0.6.0",
              protocolVersion: 2,
              compatibilityStatus: "compatible",
              workerReleaseRevisionId: "release-2",
              workerReleaseChannel: "promoted",
              workerReleaseStatus: "active",
              leaseSupported: true,
              fencingSupported: true,
              status: "online",
              administrativeStatus: "revoked",
              registeredAt: "2026-07-18T18:49:19.000Z",
              lastHeartbeatAt: "2026-07-18T18:50:19.000Z",
              revokedAt: "2026-07-18T18:55:19.000Z",
              revocationReason: "Confirmed Worker identity compromise",
            },
            releasedExecutionLeases: 1,
            recoveringExecutions: 1,
            outcomeUnknownExecutions: 0,
            checkpointUnconfirmedExecutions: 0,
            requeuedWorkspaceCleanups: 1,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    const workers = await controlPlaneClient.listWorkers("tenant/one");
    const result = await controlPlaneClient.revokeWorker(
      "tenant/one",
      "worker-1",
      {
        expectedIncarnation: 2,
        reason: "Confirmed Worker identity compromise",
      },
      { idempotencyKey: "worker-revoke-1" },
    );

    expect(workers.items[0]).toMatchObject({
      id: "worker-1",
      executionTargetId: "target-1",
      incarnation: 2,
      namespace: "synara-workers",
      podName: "synara-exec-1-g2",
      workerReleaseRevisionId: "release-2",
      workerReleaseChannel: "promoted",
      status: "online",
      administrativeStatus: "active",
    });
    expect(result).toMatchObject({
      worker: expect.objectContaining({
        id: "worker-1",
        administrativeStatus: "revoked",
      }),
      releasedExecutionLeases: 1,
      recoveringExecutions: 1,
      requeuedWorkspaceCleanups: 1,
    });
    expect(fetchMock.mock.calls[0]![0]).toBe("/v1/tenants/tenant%2Fone/workers");
    expect(fetchMock.mock.calls[1]![0]).toBe("/v1/tenants/tenant%2Fone/workers/worker-1/revoke");
    const revokeRequest = fetchMock.mock.calls[1]![1];
    expect(new Headers(revokeRequest.headers).get("Idempotency-Key")).toBe("worker-revoke-1");
    expect(JSON.parse(String(revokeRequest.body))).toEqual({
      expectedIncarnation: 2,
      reason: "Confirmed Worker identity compromise",
    });
  });

  it("lists and transitions immutable Worker release revisions with CAS idempotency", async () => {
    const fetchMock = vi
      .fn<RequiredInitFetch>()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ policy: null, revisions: [], transitions: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            id: "release/one",
            tenantId: "tenant/one",
            executionTargetId: "target/one",
            revision: 1,
            workerManifestId: "manifest/one",
            workerBuildVersion: "0.6.0",
            imageDigest: "sha256:release",
            description: "Initial release",
            createdBy: "user-1",
            createdAt: "2026-07-15T00:00:00Z",
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            tenantId: "tenant/one",
            executionTargetId: "target/one",
            policyVersion: 1,
            promotedRevisionId: "release/one",
            canaryPercent: 0,
            updatedBy: "user-1",
            updatedAt: "2026-07-15T00:01:00Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listWorkerReleases("tenant/one", "target/one");
    await controlPlaneClient.createWorkerRelease(
      "tenant/one",
      "target/one",
      { workerManifestId: "manifest/one", description: "Initial release" },
      { idempotencyKey: "worker-release-create-1" },
    );
    await controlPlaneClient.transitionWorkerRelease(
      "tenant/one",
      "target/one",
      "release/one",
      "promote",
      { expectedPolicyVersion: 0, reason: "Establish baseline" },
      { idempotencyKey: "worker-release-policy-1" },
    );

    expect(fetchMock.mock.calls[0]![0]).toBe(
      "/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/worker-releases",
    );
    const createRequest = fetchMock.mock.calls[1]![1];
    expect(new Headers(createRequest.headers).get("Idempotency-Key")).toBe(
      "worker-release-create-1",
    );
    expect(JSON.parse(String(createRequest.body))).toEqual({
      workerManifestId: "manifest/one",
      description: "Initial release",
    });
    const transitionRequest = fetchMock.mock.calls[2]![1];
    expect(fetchMock.mock.calls[2]![0]).toBe(
      "/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/worker-releases/release%2Fone/promote",
    );
    expect(new Headers(transitionRequest.headers).get("Idempotency-Key")).toBe(
      "worker-release-policy-1",
    );
    expect(JSON.parse(String(transitionRequest.body))).toEqual({
      expectedPolicyVersion: 0,
      reason: "Establish baseline",
    });
  });

  it("updates an Execution Target Provider Policy without replacing the target", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            id: "target/one",
            tenantId: "tenant/one",
            organizationId: null,
            kind: "docker",
            name: "Docker workers",
            status: "active",
            capabilities: {
              workspaceModes: ["local", "worktree"],
              providerPolicy: { experimentalProviders: ["codex"] },
            },
            createdAt: "2026-07-14T00:00:00Z",
            updatedAt: "2026-07-14T01:00:00Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const target = await controlPlaneClient.updateExecutionTargetProviderPolicy(
      "tenant/one",
      "target/one",
      ["codex"],
    );

    expect(target.capabilities.providerPolicy).toEqual({ experimentalProviders: ["codex"] });
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/provider-policy",
      expect.objectContaining({
        method: "PATCH",
        credentials: "include",
        body: JSON.stringify({ experimentalProviders: ["codex"] }),
      }),
    );
  });

  it("runs SSH target lifecycle operations through the tenant-scoped API", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            targetId: "target-1",
            operation: "upgrade",
            status: "active",
            serviceName: "synara-agentd-target-1.service",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.provisionSSHExecutionTarget("tenant/one", "target/one", "upgrade");

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/ssh/upgrade",
      expect.objectContaining({ method: "POST", credentials: "include" }),
    );
  });

  it("updates tenant quotas with explicit unlimited values", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            tenantId: "tenant/one",
            maxConcurrentExecutions: 4,
            maxArtifactBytes: null,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.updateTenantQuota("tenant/one", {
      maxConcurrentExecutions: 4,
      maxArtifactBytes: null,
    });

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/quota",
      expect.objectContaining({
        method: "PUT",
        credentials: "include",
        body: JSON.stringify({ maxConcurrentExecutions: 4, maxArtifactBytes: null }),
      }),
    );
  });

  it("updates retention with explicit disabled values", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            tenantId: "tenant/one",
            sessionArchiveAfterDays: 30,
            artifactDeleteAfterDays: null,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.updateRetentionPolicy("tenant/one", {
      sessionArchiveAfterDays: 30,
      artifactDeleteAfterDays: null,
    });

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/retention-policy",
      expect.objectContaining({
        method: "PUT",
        credentials: "include",
        body: JSON.stringify({
          sessionArchiveAfterDays: 30,
          artifactDeleteAfterDays: null,
        }),
      }),
    );
  });

  it("sends credential secrets only in create and rotate request bodies", async () => {
    const responses = [
      new Response(JSON.stringify({ id: "credential-1", version: 1 }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(JSON.stringify({ id: "credential-1", version: 2 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    ];
    const fetchMock = vi.fn(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.createCredential("tenant/one", {
      name: "OpenAI",
      purpose: "provider",
      provider: "openai",
      credentialType: "api_key",
      payload: { apiKey: "create-secret" },
    });
    await controlPlaneClient.rotateCredential("tenant/one", "credential/one", {
      expectedVersion: 1,
      payload: { apiKey: "rotate-secret" },
      expiresAt: null,
    });

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/tenants/tenant%2Fone/credentials",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        body: JSON.stringify({
          name: "OpenAI",
          purpose: "provider",
          provider: "openai",
          credentialType: "api_key",
          payload: { apiKey: "create-secret" },
        }),
      }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/tenants/tenant%2Fone/credentials/credential%2Fone/rotate",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        body: JSON.stringify({
          expectedVersion: 1,
          payload: { apiKey: "rotate-secret" },
          expiresAt: null,
        }),
      }),
    );
  });

  it("creates a typed private HTTPS Git Credential", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ id: "credential-1", purpose: "git", version: 1 }), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.createCredential("tenant/one", {
      name: "GitHub private repositories",
      purpose: "git",
      provider: "git",
      credentialType: "https_token",
      payload: { host: "github.com", username: "x-access-token", token: "git-secret" },
    });

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/credentials",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          name: "GitHub private repositories",
          purpose: "git",
          provider: "git",
          credentialType: "https_token",
          payload: { host: "github.com", username: "x-access-token", token: "git-secret" },
        }),
      }),
    );
  });

  it("updates Credential auto-selection and the Platform scope policy", async () => {
    const responses = [
      new Response(JSON.stringify({ id: "credential-1", autoSelectEnabled: true }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(
        JSON.stringify({
          tenantId: "tenant-1",
          platformCredentialsEnabled: false,
          platformCredentialAutoSelect: false,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
      new Response(
        JSON.stringify({
          tenantId: "tenant-1",
          platformCredentialsEnabled: true,
          platformCredentialAutoSelect: true,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.setCredentialAutoSelect("tenant/one", "credential/one", true);
    await controlPlaneClient.getProviderCredentialScopePolicy("tenant/one");
    await controlPlaneClient.updateProviderCredentialScopePolicy("tenant/one", {
      platformCredentialsEnabled: true,
      platformCredentialAutoSelect: true,
    });

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/tenants/tenant%2Fone/credentials/credential%2Fone/auto-select",
      expect.objectContaining({
        method: "PUT",
        credentials: "include",
        body: JSON.stringify({ enabled: true }),
      }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/tenants/tenant%2Fone/provider-credential-scope-policy",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      3,
      "/v1/tenants/tenant%2Fone/provider-credential-scope-policy",
      expect.objectContaining({
        method: "PUT",
        credentials: "include",
        body: JSON.stringify({
          platformCredentialsEnabled: true,
          platformCredentialAutoSelect: true,
        }),
      }),
    );
  });

  it("lists, creates, and disables immutable Workspace Credential Bindings", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(JSON.stringify({ id: "binding-1", bindingKind: "registry_pull" }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(
        JSON.stringify({
          id: "binding-1",
          bindingKind: "registry_pull",
          disabledAt: "2026-07-15T00:00:00Z",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listCredentialBindings("tenant/one", { projectId: "project/one" });
    await controlPlaneClient.createCredentialBinding("tenant/one", {
      projectId: "project/one",
      credentialId: "credential/one",
      bindingKind: "registry_pull",
    });
    await controlPlaneClient.disableCredentialBinding("tenant/one", "binding/one");

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/tenants/tenant%2Fone/credential-bindings?projectId=project%2Fone",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/tenants/tenant%2Fone/credential-bindings",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          projectId: "project/one",
          credentialId: "credential/one",
          bindingKind: "registry_pull",
        }),
      }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      3,
      "/v1/tenants/tenant%2Fone/credential-bindings/binding%2Fone/disable",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("keeps identity and Service Account secrets confined to explicit create responses", async () => {
    const responses = [
      new Response(JSON.stringify({ id: "connection-1", kind: "oidc", status: "active" }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(
        JSON.stringify({
          account: {
            id: "service-account-1",
            name: "SCIM",
            status: "active",
            scopes: ["scim.write"],
          },
          token: "one-time-token",
        }),
        { status: 201, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    const connection = await controlPlaneClient.createIdentityConnection("tenant/one", {
      kind: "oidc",
      name: "Company SSO",
      issuer: "https://id.example.com",
      clientId: "synara",
      clientSecret: "oidc-secret",
      oidc: { allowedDomains: ["example.com"] },
    });
    const issued = await controlPlaneClient.createServiceAccount("tenant/one", {
      name: "SCIM",
      description: "Directory provisioning",
      scopes: ["scim.write"],
    });

    expect(connection).not.toHaveProperty("clientSecret");
    expect(issued.token).toBe("one-time-token");
    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/tenants/tenant%2Fone/identity-connections",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          kind: "oidc",
          name: "Company SSO",
          issuer: "https://id.example.com",
          clientId: "synara",
          clientSecret: "oidc-secret",
          oidc: { allowedDomains: ["example.com"] },
        }),
      }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/tenants/tenant%2Fone/service-accounts",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          name: "SCIM",
          description: "Directory provisioning",
          scopes: ["scim.write"],
        }),
      }),
    );
  });

  it("creates SAML connections with metadata and claim mapping configuration", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            id: "connection-1",
            kind: "saml",
            status: "active",
            configuration: { entityId: "urn:synara:saml:sp:connection-1" },
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const connection = await controlPlaneClient.createIdentityConnection("tenant/one", {
      kind: "saml",
      name: "Company SAML",
      issuer: "",
      saml: {
        metadataUrl: "https://id.example.com/saml/metadata",
        entityId: "",
        emailAttribute: "email",
        displayNameAttribute: "displayName",
        groupsAttribute: "groups",
        allowedDomains: ["example.com"],
        defaultTenantRole: "member",
      },
    });

    expect(connection).not.toHaveProperty("privateKey");
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/identity-connections",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        body: JSON.stringify({
          kind: "saml",
          name: "Company SAML",
          issuer: "",
          saml: {
            metadataUrl: "https://id.example.com/saml/metadata",
            entityId: "",
            emailAttribute: "email",
            displayNameAttribute: "displayName",
            groupsAttribute: "groups",
            allowedDomains: ["example.com"],
            defaultTenantRole: "member",
          },
        }),
      }),
    );
  });

  it("discovers SSO connections and starts login with a relative return path", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [{ id: "connection-1", kind: "oidc", name: "SSO" }] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(JSON.stringify({ authorizationUrl: "https://id.example.com/authorize" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    ];
    const fetchMock = vi.fn(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listPublicIdentityConnections("tenant one");
    await controlPlaneClient.startSSO("connection/one", "/settings?section=tenancy");

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/auth/sso/connections?tenantSlug=tenant+one",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/auth/sso/connection%2Fone/start?returnTo=%2Fsettings%3Fsection%3Dtenancy",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("builds filtered audit list and export URLs without buffering downloads", async () => {
    vi.stubGlobal("window", { location: new URL("https://synara.example/settings") });
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ items: [], nextCursor: null }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listAuditLogs(
      "tenant/one",
      { action: "session.created", actorType: "user" },
      { limit: 25, cursor: "next page" },
    );

    expect(fetchMock).toHaveBeenCalledWith(
      "https://synara.example/v1/tenants/tenant%2Fone/audit-logs?action=session.created&actorType=user&limit=25&cursor=next+page",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(
      resolveAuditLogExportUrl("tenant/one", "csv", {
        resourceType: "artifact",
        occurredAfter: "2026-07-12T00:00:00Z",
      }),
    ).toBe(
      "https://synara.example/v1/tenants/tenant%2Fone/audit-logs/export?resourceType=artifact&occurredAfter=2026-07-12T00%3A00%3A00Z&format=csv",
    );
  });

  it("uploads artifact payloads directly to the issued grant without JSON buffering", async () => {
    vi.stubGlobal("window", { location: new URL("https://synara.example/settings") });
    const fetchMock = vi.fn<RequiredInitFetch>(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    const payload = new Uint8Array([1, 2, 3]);

    await controlPlaneClient.uploadArtifactPayload(
      {
        artifact: {} as never,
        method: "PUT",
        url: "/v1/artifact-content/artifact-1?token=secret",
        headers: { "X-Artifact-Header": "value" },
        expiresAt: "2026-07-12T00:15:00Z",
      },
      payload,
      "application/octet-stream",
    );

    expect(fetchMock).toHaveBeenCalledWith(
      "https://synara.example/v1/artifact-content/artifact-1?token=secret",
      expect.objectContaining({ method: "PUT", body: payload }),
    );
    const request = fetchMock.mock.calls[0]![1];
    expect(new Headers(request.headers).get("Content-Type")).toBe("application/octet-stream");
    expect(new Headers(request.headers).get("X-Artifact-Header")).toBe("value");
  });
});
