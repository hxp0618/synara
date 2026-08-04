import { afterEach, describe, expect, it, vi } from "vitest";

import { Polaris, PolarisError, PolarisSequenceGapError } from "./index";

type SDKFetch = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

afterEach(() => {
  vi.useRealTimers();
});

describe("Polaris", () => {
  it("creates and completes an Artifact with stable retry keys", async () => {
    const artifact = {
      id: "artifact-1",
      status: "pending",
    };
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(
        jsonResponse(
          {
            artifact,
            uploadRequired: true,
            method: "PUT",
            url: "/v1/artifact-content/artifact-1?token=secret",
            expiresAt: "2026-08-04T00:15:00Z",
          },
          201,
        ),
      )
      .mockResolvedValueOnce(jsonResponse({ ...artifact, status: "ready" }, 200))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const created = await polaris.artifacts.create(
      "session-1",
      { kind: "generated_file", originalName: "result.txt" },
      { idempotencyKey: "artifact-create" },
    );
    const completed = await polaris.artifacts.complete(
      created.data.artifact.id,
      { sizeBytes: 17, sha256: "a".repeat(64), contentType: "text/plain" },
      { idempotencyKey: "artifact-complete" },
    );
    await polaris.artifacts.delete(completed.data.id, { idempotencyKey: "artifact-delete" });
    await polaris.artifacts.delete(completed.data.id, { idempotencyKey: "artifact-delete" });

    expect(completed.data.status).toBe("ready");
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "https://control.example.test/v1/sessions/session-1/artifacts",
      "https://control.example.test/v1/artifacts/artifact-1/complete",
      "https://control.example.test/v1/artifacts/artifact-1",
      "https://control.example.test/v1/artifacts/artifact-1",
    ]);
    expect(new Headers(fetchMock.mock.calls[0]![1]?.headers).get("Idempotency-Key")).toBe(
      "artifact-create",
    );
    expect(new Headers(fetchMock.mock.calls[1]![1]?.headers).get("Idempotency-Key")).toBe(
      "artifact-complete",
    );
    expect(new Headers(fetchMock.mock.calls[2]![1]?.headers).get("Idempotency-Key")).toBe(
      "artifact-delete",
    );
  });

  it("lists and reads payload-free Artifact metadata", async () => {
    const artifact = {
      id: "artifact/one",
      tenantId: "tenant-1",
      organizationId: "organization-1",
      projectId: "project-1",
      sessionId: "session/one",
      executionId: null,
      kind: "generated_file",
      status: "ready",
      originalName: "result.txt",
      contentType: "text/plain",
      sizeBytes: 17,
      sha256: "a".repeat(64),
      createdByType: "service_account",
      createdById: "service-account-1",
      readyAt: "2026-08-04T00:00:00Z",
      createdAt: "2026-08-04T00:00:00Z",
      expiresAt: null,
      deletedAt: null,
    } as const;
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse({ items: [artifact], nextCursor: null }, 200))
      .mockResolvedValueOnce(jsonResponse(artifact, 200))
      .mockResolvedValueOnce(
        jsonResponse(
          { artifact, url: "/v1/artifact-content/token", expiresAt: "2026-08-04T00:15:00Z" },
          200,
        ),
      );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const page = await polaris.artifacts.list("session/one", { limit: 25 });
    const read = await polaris.artifacts.get(page.data.items[0]!.id);
    const grant = await polaris.artifacts.download(read.data.id);

    expect(read.data.id).toBe("artifact/one");
    expect(grant.data.url).toBe("/v1/artifact-content/token");
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "https://control.example.test/v1/sessions/session%2Fone/artifacts?limit=25",
      "https://control.example.test/v1/artifacts/artifact%2Fone",
      "https://control.example.test/v1/artifacts/artifact%2Fone/download",
    ]);
    expect(fetchMock.mock.calls[2]![1]?.body).toBeNull();
    expect(new Headers(fetchMock.mock.calls[2]![1]?.headers).has("Idempotency-Key")).toBe(false);
  });

  it("lists bounded pending and historical Interactions", async () => {
    const pending = {
      id: "interaction-1",
      executionId: "execution/one",
      turnId: "turn-1",
      provider: "codex",
      requestId: "approval-1",
      kind: "approval",
      payload: {},
      requestedAt: "2026-08-04T00:00:00Z",
      expiresAt: "2026-08-04T01:00:00Z",
    } as const;
    const history = {
      ...pending,
      sessionId: "session-1",
      status: "pending",
      resolvedAt: null,
    } as const;
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(sessionFixture(), 200))
      .mockResolvedValueOnce(
        jsonResponse({ items: [pending], nextCursor: null, snapshotSequence: 7 }, 200),
      )
      .mockResolvedValueOnce(jsonResponse({ items: [history], nextCursor: null }, 200))
      .mockResolvedValueOnce(
        jsonResponse({ ...history, kind: "user-input", status: "resolved" }, 200),
      );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const session = await polaris.sessions.get("session-1");
    const pendingPage = await session.pendingInteractions({ limit: 25 });
    const historyPage = await polaris.interactions.list("execution/one", { limit: 25 });
    const resolved = await polaris.interactions.resolveUserInput(
      "execution/one",
      "input/one",
      { environment: "staging" },
      { idempotencyKey: "input-key" },
    );

    expect(pendingPage.data.snapshotSequence).toBe(7);
    expect(historyPage.data.items[0]?.requestId).toBe("approval-1");
    expect(resolved.data.status).toBe("resolved");
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "https://control.example.test/v1/sessions/session-1",
      "https://control.example.test/v1/sessions/session-1/interactions?limit=25",
      "https://control.example.test/v1/executions/execution%2Fone/interactions?limit=25",
      "https://control.example.test/v1/executions/execution%2Fone/user-input/input%2Fone/resolve",
    ]);
    expect(new Headers(fetchMock.mock.calls[3]![1]?.headers).get("Idempotency-Key")).toBe(
      "input-key",
    );
    expect(JSON.parse(String(fetchMock.mock.calls[3]![1]?.body))).toEqual({
      answers: { environment: "staging" },
    });
  });

  it("lists and reads Sessions through bounded handles", async () => {
    const session = sessionFixture();
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse({ items: [session], nextCursor: "next-page" }, 200))
      .mockResolvedValueOnce(jsonResponse(session, 200))
      .mockResolvedValueOnce(
        jsonResponse({ executionTargetId: "target-1", targetKind: "ssh", basis: "target", items: [] }, 200),
      )
      .mockResolvedValueOnce(jsonResponse({ tenantId: "tenant-1", sessionId: "session-1", items: [] }, 200))
      .mockResolvedValueOnce(jsonResponse({ ...session, model: "model-next" }, 200))
      .mockResolvedValueOnce(jsonResponse({ ...session, status: "suspended" }, 200))
      .mockResolvedValueOnce(jsonResponse({ ...session, status: "active" }, 200))
      .mockResolvedValueOnce(
        jsonResponse({
          id: "execution-1", sessionId: "session-1", turnId: "turn-1", attempt: 1,
          status: "recovering", executionTargetId: "target-1", targetKind: "ssh", provider: "codex",
          queuedAt: "2026-08-04T00:00:00Z", startedAt: null, finishedAt: null, failureCode: null,
        }, 202),
      )
      .mockResolvedValueOnce(
        jsonResponse({
          id: "command-1", executionId: "execution-1", sessionId: "session-1", turnId: "turn-1",
          provider: "codex", commandType: "SteerTurn", status: "pending",
          requestedAt: "2026-08-04T00:00:00Z",
        }, 202),
      )
      .mockResolvedValueOnce(
        jsonResponse({
          id: "command-2", executionId: "execution-1", sessionId: "session-1", turnId: "turn-1",
          provider: "codex", commandType: "InterruptTurn", status: "pending",
          requestedAt: "2026-08-04T00:00:00Z",
        }, 202),
      )
      .mockResolvedValueOnce(jsonResponse({ ...session, status: "archived" }, 200));
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const page = await polaris.sessions.list("project/one", { limit: 25, cursor: "cursor+/=" });
    const handle = await polaris.sessions.get(page.data.items[0]!.id);
    await handle.capabilities();
    const usage = await handle.usage();
    const switched = await handle.switchModel(
      { model: "model-next", expectedModel: null },
      { idempotencyKey: "session-model-switch" },
    );
    const suspended = await handle.suspend({ idempotencyKey: "session-suspend" });
    const resumed = await handle.resume({ idempotencyKey: "session-resume" });
    const resumedTurn = await handle.resumeActiveTurn({ idempotencyKey: "turn-resume" });
    const steered = await handle.steer(
      { inputText: "Continue with the narrower fix." },
      { idempotencyKey: "turn-steer" },
    );
    const interrupted = await handle.interrupt({ idempotencyKey: "turn-interrupt" });
    const archived = await handle.archive({ idempotencyKey: "session-archive" });

    expect(handle.id).toBe("session-1");
    expect(usage.data.items).toEqual([]);
    expect(archived.data.status).toBe("archived");
    expect(switched.data.model).toBe("model-next");
    expect(suspended.data.status).toBe("suspended");
    expect(resumed.data.status).toBe("active");
    expect(resumedTurn.data.status).toBe("recovering");
    expect(steered.data.commandType).toBe("SteerTurn");
    expect(interrupted.data.commandType).toBe("InterruptTurn");
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "https://control.example.test/v1/projects/project%2Fone/sessions?limit=25&cursor=cursor%2B%2F%3D",
      "https://control.example.test/v1/sessions/session-1",
      "https://control.example.test/v1/sessions/session-1/provider-capabilities",
      "https://control.example.test/v1/sessions/session-1/usage",
      "https://control.example.test/v1/sessions/session-1/model-switch",
      "https://control.example.test/v1/sessions/session-1/suspend",
      "https://control.example.test/v1/sessions/session-1/resume",
      "https://control.example.test/v1/sessions/session-1/turns/active/resume",
      "https://control.example.test/v1/sessions/session-1/turns/active/steer",
      "https://control.example.test/v1/sessions/session-1/turns/active/interrupt",
      "https://control.example.test/v1/sessions/session-1/archive",
    ]);
    expect(new Headers(fetchMock.mock.calls[4]![1]?.headers).get("Idempotency-Key")).toBe(
      "session-model-switch",
    );
    expect(new Headers(fetchMock.mock.calls[5]![1]?.headers).get("Idempotency-Key")).toBe(
      "session-suspend",
    );
    expect(new Headers(fetchMock.mock.calls[6]![1]?.headers).get("Idempotency-Key")).toBe(
      "session-resume",
    );
    expect(new Headers(fetchMock.mock.calls[7]![1]?.headers).get("Idempotency-Key")).toBe(
      "turn-resume",
    );
    expect(new Headers(fetchMock.mock.calls[8]![1]?.headers).get("Idempotency-Key")).toBe(
      "turn-steer",
    );
    expect(new Headers(fetchMock.mock.calls[9]![1]?.headers).get("Idempotency-Key")).toBe(
      "turn-interrupt",
    );
    expect(new Headers(fetchMock.mock.calls[10]![1]?.headers).get("Idempotency-Key")).toBe(
      "session-archive",
    );
  });

  it("creates, lists, reads, updates, and archives Projects through the bounded public surface", async () => {
    const project = {
      id: "project/one",
      tenantId: "tenant/one",
      organizationId: "organization/one",
      name: "SDK Project",
      repositoryUrl: null,
      defaultBranch: "main",
      gitCredentialId: null,
      visibility: "organization",
      createdBy: "service-account-1",
      createdAt: "2026-08-04T00:00:00Z",
      updatedAt: "2026-08-04T00:00:00Z",
      archivedAt: null,
    } as const;
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(project, 201))
      .mockResolvedValueOnce(jsonResponse({ items: [project], nextCursor: null }, 200))
      .mockResolvedValueOnce(jsonResponse(project, 200))
      .mockResolvedValueOnce(jsonResponse({ ...project, name: "Updated Project" }, 200))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(
        jsonResponse({ executionTargetId: "target/one", targetKind: "ssh", basis: "target", items: [] }, 200),
      );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const created = await polaris.projects.create(
      "tenant/one",
      "organization/one",
      { name: "SDK Project" },
      { idempotencyKey: "project-create" },
    );
    const listed = await polaris.projects.list("tenant/one", "organization/one", { limit: 25 });
    const read = await polaris.projects.get(created.data.id);
    const updated = await polaris.projects.update(
      created.data.id,
      { name: "Updated Project" },
      { idempotencyKey: "project-update" },
    );
    await polaris.projects.archive(created.data.id, { idempotencyKey: "project-archive" });
    const capabilities = await polaris.projects.capabilities(created.data.id, {
      executionTargetId: "target/one",
    });

    expect(listed.data.items[0]?.id).toBe("project/one");
    expect(read.data.id).toBe("project/one");
    expect(updated.data.name).toBe("Updated Project");
    expect(capabilities.data.basis).toBe("target");
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "https://control.example.test/v1/tenants/tenant%2Fone/organizations/organization%2Fone/projects",
      "https://control.example.test/v1/tenants/tenant%2Fone/organizations/organization%2Fone/projects?limit=25",
      "https://control.example.test/v1/projects/project%2Fone",
      "https://control.example.test/v1/projects/project%2Fone",
      "https://control.example.test/v1/projects/project%2Fone",
      "https://control.example.test/v1/projects/project%2Fone/provider-capabilities?executionTargetId=target%2Fone",
    ]);
    expect(new Headers(fetchMock.mock.calls[0]![1]?.headers).get("Idempotency-Key")).toBe(
      "project-create",
    );
    expect(new Headers(fetchMock.mock.calls[3]![1]?.headers).get("Idempotency-Key")).toBe(
      "project-update",
    );
    expect(new Headers(fetchMock.mock.calls[4]![1]?.headers).get("Idempotency-Key")).toBe(
      "project-archive",
    );
  });

  it("creates a Session with bearer authentication, idempotency, and response metadata", async () => {
    const fetchMock = vi.fn<SDKFetch>(async () =>
      jsonResponse(sessionFixture(), 201, {
        "X-Request-ID": "request-1",
        "Idempotency-Replayed": "true",
        "RateLimit-Limit": "600",
        "RateLimit-Remaining": "599",
        "RateLimit-Reset": "42",
      }),
    );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test/base/",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const session = await polaris.sessions.create(
      { projectId: "project/one", title: "SDK Session", provider: "codex" },
      { idempotencyKey: "create-session-1" },
    );

    expect(session.id).toBe("session-1");
    expect(session.response).toEqual({
      requestId: "request-1",
      idempotencyReplayed: true,
      rateLimit: { limit: 600, remaining: 599, resetAfterSeconds: 42 },
    });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("https://control.example.test/base/v1/projects/project%2Fone/sessions");
    expect(init?.method).toBe("POST");
    expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer syna_sa_test-token");
    expect(new Headers(init?.headers).get("Idempotency-Key")).toBe("create-session-1");
    expect(JSON.parse(String(init?.body))).toEqual({ title: "SDK Session", provider: "codex" });
  });

  it("registers and reads a safe BYO Execution Target projection", async () => {
    const target = {
      id: "target/one",
      tenantId: "tenant/one",
      organizationId: "organization-1",
      kind: "ssh",
      name: "build host",
      status: "offline",
      capabilities: {},
      isolationProfile: "single-tenant-trusted-v1",
      platformSharedEligible: false,
      productBoundary: "single-tenant-trusted",
      createdAt: "2026-08-04T00:00:00Z",
      updatedAt: "2026-08-04T00:00:00Z",
    };
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(target, 201))
      .mockResolvedValueOnce(jsonResponse(target, 200));
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const created = await polaris.targets.create(
      "tenant/one",
      {
        organizationId: "organization-1",
        kind: "ssh",
        name: "build host",
        configuration: {},
        capabilities: {},
      },
      { idempotencyKey: "target-create" },
    );
    const read = await polaris.targets.get("tenant/one", created.data.id);

    expect(read.data.id).toBe("target/one");
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "https://control.example.test/v1/tenants/tenant%2Fone/execution-targets",
      "https://control.example.test/v1/tenants/tenant%2Fone/execution-targets/target%2Fone",
    ]);
    expect(new Headers(fetchMock.mock.calls[0]![1]?.headers).get("Idempotency-Key")).toBe(
      "target-create",
    );
    expect(JSON.parse(String(fetchMock.mock.calls[0]![1]?.body))).toHaveProperty(
      "configuration",
      {},
    );
  });

  it("starts and polls a durable Execution Target provisioning operation", async () => {
    vi.useFakeTimers();
    const base = {
      id: "operation/one",
      targetId: "target/one",
      action: "install",
      attempt: 0,
      createdAt: "2026-08-04T00:00:00Z",
      updatedAt: "2026-08-04T00:00:00Z",
    } as const;
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse({ ...base, state: "accepted" }, 202))
      .mockResolvedValueOnce(jsonResponse({ ...base, state: "running", attempt: 1 }, 200))
      .mockResolvedValueOnce(
        jsonResponse(
          {
            ...base,
            state: "succeeded",
            attempt: 1,
            result: { targetId: "target/one", action: "install", status: "active" },
          },
          200,
        ),
      );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const accepted = await polaris.targets.provision("tenant/one", "target/one", "install", {
      idempotencyKey: "install-once",
    });
    const pending = polaris.targets.waitForProvisioning(
      "tenant/one",
      "target/one",
      accepted.data.id,
      { pollIntervalMs: 10, timeoutMs: 1_000 },
    );
    await vi.advanceTimersByTimeAsync(10);
    const completed = await pending;

    expect(completed.data.state).toBe("succeeded");
    expect(new Headers(fetchMock.mock.calls[0]![1]?.headers).get("Idempotency-Key")).toBe(
      "install-once",
    );
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "https://control.example.test/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/provisioning-operations",
      "https://control.example.test/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/provisioning-operations/operation%2Fone",
      "https://control.example.test/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/provisioning-operations/operation%2Fone",
    ]);
  });

  it("maps the stable error envelope to PolarisError", async () => {
    const fetchMock = vi.fn<SDKFetch>(async () =>
      jsonResponse(
        {
          error: {
            code: "developer_api_rate_limited",
            message: "Try later.",
            requestId: "request-429",
            details: { scope: "key" },
          },
        },
        429,
        { "Retry-After": "17" },
      ),
    );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const error = await polaris.sessions
      .create(
        { projectId: "project-1", title: "SDK Session", provider: "codex" },
        { idempotencyKey: "rate-limited" },
      )
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(PolarisError);
    expect(error).toMatchObject({
      status: 429,
      code: "developer_api_rate_limited",
      requestId: "request-429",
      retryAfterSeconds: 17,
      details: { scope: "key" },
    });
  });

  it("encodes sequence-guarded compact, review, rollback, and fork operations", async () => {
    const session = sessionFixture();
    const command = {
      id: "command-1", executionId: "execution-1", sessionId: session.id, turnId: "turn-1",
      provider: "codex", commandType: "CompactSession", status: "pending",
      requestedAt: "2026-08-04T00:00:00Z",
    };
    const operation = {
      type: "compact", turn: { id: "turn-1" }, executionId: "execution-1", controlCommand: command,
    };
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(session, 200))
      .mockResolvedValueOnce(jsonResponse(operation, 202))
      .mockResolvedValueOnce(jsonResponse({ ...operation, type: "review" }, 202))
      .mockResolvedValueOnce(jsonResponse({ sessionId: session.id, removedTurnCount: 1 }, 200))
      .mockResolvedValueOnce(
        jsonResponse({ session: { ...session, id: "session-fork" }, sourceSessionId: session.id, sourceEventSequence: 7, supportMode: "emulated" }, 201),
      );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token", baseUrl: "https://control.example.test", fetch: fetchMock, maxRetries: 0,
    });
    const handle = await polaris.sessions.get(session.id);

    await handle.compact({ expectedLastEventSequence: 7 }, { idempotencyKey: "compact-key" });
    await handle.startReview(
      { expectedLastEventSequence: 7, target: { type: "baseBranch", branch: "main" } },
      { idempotencyKey: "review-key" },
    );
    await handle.rollback(
      { expectedLastEventSequence: 7, fromTurnId: "turn-1" },
      { idempotencyKey: "rollback-key" },
    );
    await handle.fork(
      { expectedLastEventSequence: 7, title: "Forked", visibility: "organization" },
      { idempotencyKey: "fork-key" },
    );

    expect(fetchMock.mock.calls.slice(1).map(([url]) => url)).toEqual([
      "https://control.example.test/v1/sessions/session-1/compact",
      "https://control.example.test/v1/sessions/session-1/reviews",
      "https://control.example.test/v1/sessions/session-1/rollback",
      "https://control.example.test/v1/sessions/session-1/fork",
    ]);
    expect(fetchMock.mock.calls.slice(1).map(([, init]) => new Headers(init?.headers).get("Idempotency-Key"))).toEqual([
      "compact-key", "review-key", "rollback-key", "fork-key",
    ]);
  });

  it("cancels an Execution through the sanitized developer projection", async () => {
    const execution = {
      id: "execution/one",
      sessionId: "session-1",
      turnId: "turn-1",
      attempt: 1,
      status: "cancelled",
      executionTargetId: "target-1",
      targetKind: "ssh",
      provider: "codex",
      queuedAt: "2026-08-04T00:00:00Z",
      startedAt: null,
      finishedAt: "2026-08-04T00:00:01Z",
      failureCode: null,
    } as const;
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(execution, 200))
      .mockResolvedValueOnce(jsonResponse({ ...execution, status: "recovering", finishedAt: null }, 202));
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });

    const cancelled = await polaris.executions.cancel("execution/one", {
      idempotencyKey: "execution-cancel",
    });
    const resumed = await polaris.executions.resumeActiveTurn("execution/one", {
      idempotencyKey: "execution-resume",
    });

    expect(cancelled.data.status).toBe("cancelled");
    expect(resumed.data.status).toBe("recovering");
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "https://control.example.test/v1/executions/execution%2Fone/cancel",
    );
    expect(new Headers(fetchMock.mock.calls[0]![1]?.headers).get("Idempotency-Key")).toBe(
      "execution-cancel",
    );
    expect(fetchMock.mock.calls[1]?.[0]).toBe(
      "https://control.example.test/v1/executions/execution%2Fone/resume",
    );
    expect(new Headers(fetchMock.mock.calls[1]![1]?.headers).get("Idempotency-Key")).toBe(
      "execution-resume",
    );
  });

  it("retries 429 responses after Retry-After with the same idempotency key", async () => {
    vi.useFakeTimers();
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(
        jsonResponse(
          { error: { code: "developer_api_rate_limited", message: "Try later." } },
          429,
          { "Retry-After": "1" },
        ),
      )
      .mockResolvedValueOnce(jsonResponse(sessionFixture(), 201));
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 1,
    });

    const pending = polaris.sessions.create(
      { projectId: "project-1", title: "Retry", provider: "codex" },
      { idempotencyKey: "stable-retry-key" },
    );
    await vi.advanceTimersByTimeAsync(1000);
    const session = await pending;

    expect(session.id).toBe("session-1");
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(
      fetchMock.mock.calls.map(([, init]) => new Headers(init?.headers).get("Idempotency-Key")),
    ).toEqual(["stable-retry-key", "stable-retry-key"]);
  });

  it("creates Turns and resolves approvals through the codegen-ready routes", async () => {
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(sessionFixture(), 201))
      .mockResolvedValueOnce(
        jsonResponse({ id: "turn-1", sessionId: "session-1", status: "queued" }, 201),
      )
      .mockResolvedValueOnce(
        jsonResponse({ id: "interaction-1", status: "resolved", kind: "approval" }, 200),
      );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });
    const session = await polaris.sessions.create(
      { projectId: "project-1", title: "SDK Session", provider: "codex" },
      { idempotencyKey: "create" },
    );

    await session.sendTurn(
      { inputText: "Fix the tests", runtimeMode: "full-access", interactionMode: "default" },
      { idempotencyKey: "turn" },
    );
    await polaris.approvals.resolve("execution/one", "approval/one", "accept", {
      idempotencyKey: "approval",
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "https://control.example.test/v1/projects/project-1/sessions",
      "https://control.example.test/v1/sessions/session-1/turns",
      "https://control.example.test/v1/executions/execution%2Fone/approvals/approval%2Fone/resolve",
    ]);
  });

  it("decodes resumable SSE frames, ignores replays, and rejects sequence gaps", async () => {
    const sequentialFetch = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(sessionFixture(), 201))
      .mockResolvedValueOnce(
        eventStreamResponse([
          eventFrame(1, "session.created"),
          eventFrame(1, "session.created"),
          eventFrame(2, "turn.created"),
        ]),
      );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: sequentialFetch,
      maxRetries: 0,
    });
    const session = await polaris.sessions.create(
      { projectId: "project-1", title: "SDK Session", provider: "codex" },
      { idempotencyKey: "create" },
    );
    const events = [];
    for await (const event of session.events({ reconnect: false })) events.push(event);
    expect(events.map((event) => event.sequence)).toEqual([1, 2]);
    expect(sequentialFetch.mock.calls[1]![0]).toBe(
      "https://control.example.test/v1/sessions/session-1/events/stream?afterSequence=0",
    );

    const gapFetch = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(sessionFixture(), 201))
      .mockResolvedValueOnce(eventStreamResponse([eventFrame(2, "turn.created")]));
    const gapPolaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: gapFetch,
      maxRetries: 0,
    });
    const gapSession = await gapPolaris.sessions.create(
      { projectId: "project-1", title: "Gap", provider: "codex" },
      { idempotencyKey: "gap" },
    );
    const consumeGap = async () => {
      for await (const event of gapSession.events({ reconnect: false })) void event;
    };
    await expect(consumeGap()).rejects.toBeInstanceOf(PolarisSequenceGapError);
  });

  it("filters an Execution stream after validating the full Session sequence", async () => {
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(sessionFixture(), 201))
      .mockResolvedValueOnce(
        eventStreamResponse([
          eventFrame(1, "execution.started", "execution-other"),
          eventFrame(2, "execution.completed", "execution-wanted"),
        ]),
      );
    const session = await new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    }).sessions.create({ projectId: "project-1", title: "Filter", provider: "codex" });
    const events = [];
    for await (const event of session.eventsForExecution("execution-wanted", { reconnect: false }))
      events.push(event);
    expect(events.map((event) => [event.sequence, event.executionId])).toEqual([
      [2, "execution-wanted"],
    ]);
  });

  it("falls back to ordered event polling when the SSE connection pool is full", async () => {
    const fetchMock = vi
      .fn<SDKFetch>()
      .mockResolvedValueOnce(jsonResponse(sessionFixture(), 201))
      .mockResolvedValueOnce(
        jsonResponse(
          { error: { code: "sse_tenant_connection_limit", message: "Stream pool is full." } },
          429,
          { "Retry-After": "2" },
        ),
      )
      .mockResolvedValueOnce(
        jsonResponse(
          {
            items: [eventFixture(1, "session.created"), eventFixture(2, "turn.created")],
            lastSequence: 2,
          },
          200,
        ),
      );
    const polaris = new Polaris({
      apiKey: "syna_sa_test-token",
      baseUrl: "https://control.example.test",
      fetch: fetchMock,
      maxRetries: 0,
    });
    const session = await polaris.sessions.create(
      { projectId: "project-1", title: "Polling fallback", provider: "codex" },
      { idempotencyKey: "polling-fallback" },
    );
    const events = [];
    for await (const event of session.events({ reconnect: false, pollingIntervalMs: 0 })) {
      events.push(event);
      if (event.sequence === 2) break;
    }

    expect(events.map((event) => event.sequence)).toEqual([1, 2]);
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "https://control.example.test/v1/projects/project-1/sessions",
      "https://control.example.test/v1/sessions/session-1/events/stream?afterSequence=0",
      "https://control.example.test/v1/sessions/session-1/events?afterSequence=0&limit=50",
    ]);
  });

  it("rejects ambiguous or credential-bearing transport configuration", () => {
    expect(
      () => new Polaris({ apiKey: "not-an-api-key", baseUrl: "https://control.example.test" }),
    ).toThrow(/syna_sa_/);
    expect(
      () =>
        new Polaris({
          apiKey: "syna_sa_test-token",
          baseUrl: "https://user:secret@control.example.test",
        }),
    ).toThrow(/without embedded credentials/);
  });
});

function jsonResponse(body: unknown, status: number, headers: HeadersInit = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...Object.fromEntries(new Headers(headers)) },
  });
}

function eventStreamResponse(frames: string[]) {
  const encoder = new TextEncoder();
  return new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        for (const frame of frames) controller.enqueue(encoder.encode(frame));
        controller.close();
      },
    }),
    { status: 200, headers: { "Content-Type": "text/event-stream" } },
  );
}

function eventFrame(
  sequence: number,
  eventType: string,
  executionId: string | null = null,
): string {
  return `id: ${sequence}\nevent: session-event\ndata: ${JSON.stringify({ ...eventFixture(sequence, eventType), executionId })}\n\n`;
}

function eventFixture(sequence: number, eventType: string) {
  return {
    eventId: `event-${sequence}`,
    eventVersion: 1,
    tenantId: "tenant-1",
    organizationId: "organization-1",
    projectId: "project-1",
    sessionId: "session-1",
    executionId: null,
    workerId: null,
    generation: null,
    sequence,
    eventType,
    actorType: "service_account",
    actorId: "service-account-1",
    payload: {},
    occurredAt: "2026-08-03T12:00:00Z",
  };
}

function sessionFixture() {
  return {
    id: "session-1",
    tenantId: "tenant-1",
    organizationId: "organization-1",
    projectId: "project-1",
    createdBy: "user-1",
    title: "SDK Session",
    status: "active",
    visibility: "project",
    provider: "codex",
    model: null,
    providerCredentialId: null,
    executionTargetId: "target-1",
    requestedExecutionTargetId: "target-1",
    lastEventSequence: 1,
    resourceState: "active",
    meaningfulActivityAt: "2026-08-03T12:00:00Z",
    resourceLifecyclePolicy: {
      waitingKeepAliveSeconds: 60,
      suspendAfterIdleSeconds: 600,
      absoluteSessionLifetimeSeconds: null,
      workspaceRetentionDays: 7,
      warmPoolMode: "disabled",
    },
    createdAt: "2026-08-03T12:00:00Z",
    updatedAt: "2026-08-03T12:00:00Z",
    archivedAt: null,
  };
}
