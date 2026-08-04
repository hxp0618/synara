import { ThreadId } from "@synara/contracts";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  configureControlPlaneClientTransport,
  controlPlaneClient,
  ControlPlaneError,
  resolveAuditLogExportUrl,
  resolveInternalCostAllocationExportUrl,
  resolveControlPlaneInternalStatusBoardURL,
  resolveControlPlaneHttpUrl,
  resolveSessionEventStreamUrl,
} from "./index";

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
  configureControlPlaneClientTransport();
  FakeEventSource.instances = [];
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("controlPlaneClient", () => {
  it("resolves only a configured credential-free HTTPS internal Status Board URL", () => {
    expect(
      resolveControlPlaneInternalStatusBoardURL({
        internalStatusBoard: { configured: true, url: "https://status.synara.example/history" },
      }),
    ).toBe("https://status.synara.example/history");
    expect(
      resolveControlPlaneInternalStatusBoardURL({ internalStatusBoard: { configured: false } }),
    ).toBeNull();

    for (const url of [
      "http://status.synara.example",
      "https://user:secret@status.synara.example",
      "https://status.synara.example?tenant=secret",
      "https://status.synara.example#internal",
      "javascript:alert(1)",
    ]) {
      expect(
        resolveControlPlaneInternalStatusBoardURL({
          internalStatusBoard: { configured: true, url },
        }),
      ).toBeNull();
    }
  });

  it("accepts a host URL resolver without importing host runtime code", () => {
    configureControlPlaneClientTransport({
      resolveHttpUrl: (path) => `synara-bridge:${path}`,
    });

    expect(resolveControlPlaneHttpUrl("/v1/auth/session")).toBe("synara-bridge:/v1/auth/session");
  });

  it("keeps browser-hosted requests same-origin when a desktop fallback is configured", () => {
    vi.stubGlobal("window", { location: new URL("https://synara.example/settings") });
    configureControlPlaneClientTransport({
      resolveHttpUrl: (path) => `synara-bridge:${path}`,
    });

    expect(resolveControlPlaneHttpUrl("/v1/auth/session")).toBe(
      "https://synara.example/v1/auth/session",
    );
  });

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
            internalStatusBoard: { configured: false },
            commercializationMode: "internal-self-hosted",
            resourceLifecyclePolicy: {
              defaults: {
                waitingKeepAliveSeconds: 900,
                suspendAfterIdleSeconds: 1800,
                absoluteSessionLifetimeSeconds: null,
                workspaceRetentionDays: 30,
                warmPoolMode: "balanced",
              },
              bounds: {
                waitingKeepAliveSeconds: { min: 60, max: 86_400 },
                suspendAfterIdleSeconds: { min: 60, max: 604_800 },
                absoluteSessionLifetimeSeconds: { min: 3_600, max: 31_536_000 },
                workspaceRetentionDays: { min: 1, max: 3_650 },
                warmPoolModes: ["disabled", "balanced", "low-latency"],
              },
            },
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const profile = await controlPlaneClient.getPlatformProfile();

    expect(profile.profile).toBe("single-node");
    expect(profile.resourceLifecyclePolicy.defaults.warmPoolMode).toBe("balanced");
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
              supportAccessGrantId: null,
              audience: "web",
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

  it("separates Standard self-service Tenant creation from Platform provisioning", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(async (input) => {
      const path = String(input);
      return new Response(
        JSON.stringify({
          id: path === "/v1/tenants" ? "tenant-free" : "tenant-enterprise",
          slug: "customer-tenant",
          name: "Customer Tenant",
          status: "active",
          lifecycleVersion: 1,
          evaluationExpiresAt: null,
          entitlementProfileCode: path === "/v1/tenants" ? "standard" : "enterprise",
          region: "default",
          role: "owner",
          ownerUserId: "owner-1",
          ownerEmail: "owner@example.com",
          createdAt: "2026-07-30T00:00:00Z",
        }),
        { status: 201, headers: { "Content-Type": "application/json" } },
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.createTenant({
      slug: "customer-tenant",
      name: "Customer Tenant",
      region: "default",
    });
    await controlPlaneClient.provisionPlatformTenant({
      ownerEmail: "owner@example.com",
      slug: "enterprise-tenant",
      name: "Enterprise Tenant",
      region: "eu-west",
      entitlementProfileCode: "enterprise",
      status: "active",
    });
    await controlPlaneClient.getPlatformTenantEntitlements("tenant/enterprise");
    await controlPlaneClient.assignPlatformTenantEntitlementProfile("tenant/enterprise", {
      entitlementProfileCode: "standard",
      status: "active",
      expectedVersion: 1,
      reportingPeriodStart: "2026-07-01T00:00:00Z",
      reportingPeriodEnd: "2026-08-01T00:00:00Z",
      reason: "Approved internal entitlement profile change.",
    });
    await controlPlaneClient.getPlatformDesktopAccess("tenant/enterprise");
    await controlPlaneClient.issuePlatformDesktopEnrollment("tenant/enterprise", {
      subjectUserId: "user/owner",
      mode: "connect_existing",
      reason: "Connect the customer owner's managed desktop.",
    });
    await controlPlaneClient.markPlatformDesktopEnrollmentOpened("enrollment/one", 1);
    await controlPlaneClient.revokePlatformDesktopEnrollment("enrollment/one", {
      expectedVersion: 2,
      reason: "The one-time Desktop link is no longer required.",
    });
    await controlPlaneClient.revokePlatformDesktopDevice("device/one", {
      expectedVersion: 3,
      reason: "The customer requested immediate device revocation.",
    });

    expect(fetchMock.mock.calls[0]![0]).toBe("/v1/tenants");
    expect(fetchMock.mock.calls[0]![1]).toEqual(
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          slug: "customer-tenant",
          name: "Customer Tenant",
          region: "default",
          entitlementProfileCode: "standard",
          status: "active",
        }),
      }),
    );
    expect(fetchMock.mock.calls[1]![0]).toBe("/v1/platform/tenants");
    expect(fetchMock.mock.calls[1]![1]).toEqual(
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          ownerEmail: "owner@example.com",
          slug: "enterprise-tenant",
          name: "Enterprise Tenant",
          region: "eu-west",
          entitlementProfileCode: "enterprise",
          status: "active",
        }),
      }),
    );
    expect(fetchMock.mock.calls[2]![0]).toBe(
      "/v1/platform/tenants/tenant%2Fenterprise/entitlements",
    );
    expect(fetchMock.mock.calls[3]![0]).toBe(
      "/v1/platform/tenants/tenant%2Fenterprise/entitlement-profile",
    );
    expect(fetchMock.mock.calls[3]![1]).toEqual(
      expect.objectContaining({
        method: "PUT",
        body: JSON.stringify({
          entitlementProfileCode: "standard",
          status: "active",
          expectedVersion: 1,
          reportingPeriodStart: "2026-07-01T00:00:00Z",
          reportingPeriodEnd: "2026-08-01T00:00:00Z",
          reason: "Approved internal entitlement profile change.",
        }),
      }),
    );
    expect(fetchMock.mock.calls.slice(4).map((call) => call[0])).toEqual([
      "/v1/platform/tenants/tenant%2Fenterprise/desktop-access",
      "/v1/platform/tenants/tenant%2Fenterprise/desktop-enrollments",
      "/v1/platform/desktop-enrollments/enrollment%2Fone/opened",
      "/v1/platform/desktop-enrollments/enrollment%2Fone/revoke",
      "/v1/platform/desktop-devices/device%2Fone/revoke",
    ]);
    expect(fetchMock.mock.calls[5]![1]).toEqual(
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          subjectUserId: "user/owner",
          mode: "connect_existing",
          reason: "Connect the customer owner's managed desktop.",
        }),
      }),
    );
    expect(fetchMock.mock.calls[6]![1]).toEqual(
      expect.objectContaining({ method: "POST", body: JSON.stringify({ expectedVersion: 1 }) }),
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
            providerCredentialId: null,
            executionTargetId: "target-1",
            requestedExecutionTargetId: "target-1",
            executionTargetGroupId: "target-group-1",
            routingPolicyVersion: 3,
            preferredExecutionRegion: "ap-southeast-1",
            lastEventSequence: 1,
            resourceState: "active",
            meaningfulActivityAt: "2026-07-12T00:00:00Z",
            resourceIdleSince: null,
            absoluteExpiresAt: null,
            resourceLifecyclePolicy: {
              waitingKeepAliveSeconds: 900,
              suspendAfterIdleSeconds: 1800,
              absoluteSessionLifetimeSeconds: null,
              workspaceRetentionDays: 30,
              warmPoolMode: "balanced",
            },
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
        executionTargetGroupId: "target-group-1",
        preferredExecutionRegion: "ap-southeast-1",
        resourceLifecyclePolicy: {
          waitingKeepAliveSeconds: 600,
          warmPoolMode: "low-latency",
        },
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
          executionTargetGroupId: "target-group-1",
          preferredExecutionRegion: "ap-southeast-1",
          resourceLifecyclePolicy: {
            waitingKeepAliveSeconds: 600,
            warmPoolMode: "low-latency",
          },
        }),
      }),
    );
    const request = fetchMock.mock.calls[0]![1];
    expect(new Headers(request.headers).get("Idempotency-Key")).toBe("web-session-request-1");
  });

  it("loads and updates tenant resource lifecycle policy with explicit inherit-via-null fields", async () => {
    const responses = [
      new Response(
        JSON.stringify({
          scope: "tenant",
          tenantId: "tenant/one",
          overrides: {
            waitingKeepAliveSeconds: null,
            suspendAfterIdleSeconds: null,
            absoluteSessionLifetimeSeconds: null,
            workspaceRetentionDays: null,
            warmPoolMode: null,
          },
          effective: {
            waitingKeepAliveSeconds: 900,
            suspendAfterIdleSeconds: 1800,
            absoluteSessionLifetimeSeconds: null,
            workspaceRetentionDays: 30,
            warmPoolMode: "balanced",
          },
          version: 0,
          updatedBy: null,
          createdAt: null,
          updatedAt: null,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
      new Response(
        JSON.stringify({
          scope: "tenant",
          tenantId: "tenant/one",
          overrides: {
            waitingKeepAliveSeconds: 600,
            suspendAfterIdleSeconds: null,
            absoluteSessionLifetimeSeconds: null,
            workspaceRetentionDays: 14,
            warmPoolMode: "low-latency",
          },
          effective: {
            waitingKeepAliveSeconds: 600,
            suspendAfterIdleSeconds: 1800,
            absoluteSessionLifetimeSeconds: null,
            workspaceRetentionDays: 14,
            warmPoolMode: "low-latency",
          },
          version: 1,
          updatedBy: "user-1",
          createdAt: "2026-07-24T00:00:00Z",
          updatedAt: "2026-07-24T00:01:00Z",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.getTenantResourceLifecyclePolicy("tenant/one");
    await controlPlaneClient.updateTenantResourceLifecyclePolicy("tenant/one", {
      expectedVersion: 0,
      waitingKeepAliveSeconds: 600,
      suspendAfterIdleSeconds: null,
      absoluteSessionLifetimeSeconds: null,
      workspaceRetentionDays: 14,
      warmPoolMode: "low-latency",
    });

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/tenants/tenant%2Fone/resource-lifecycle-policy",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/tenants/tenant%2Fone/resource-lifecycle-policy",
      expect.objectContaining({
        method: "PUT",
        credentials: "include",
        body: JSON.stringify({
          expectedVersion: 0,
          waitingKeepAliveSeconds: 600,
          suspendAfterIdleSeconds: null,
          absoluteSessionLifetimeSeconds: null,
          workspaceRetentionDays: 14,
          warmPoolMode: "low-latency",
        }),
      }),
    );
  });

  it("loads and updates project resource lifecycle policy through the project route", async () => {
    const responses = [
      new Response(
        JSON.stringify({
          scope: "project",
          tenantId: "tenant-1",
          projectId: "project/one",
          overrides: {
            waitingKeepAliveSeconds: 900,
            suspendAfterIdleSeconds: null,
            absoluteSessionLifetimeSeconds: null,
            workspaceRetentionDays: null,
            warmPoolMode: null,
          },
          effective: {
            waitingKeepAliveSeconds: 900,
            suspendAfterIdleSeconds: 1800,
            absoluteSessionLifetimeSeconds: null,
            workspaceRetentionDays: 30,
            warmPoolMode: "balanced",
          },
          version: 3,
          updatedBy: "user-1",
          createdAt: "2026-07-24T00:00:00Z",
          updatedAt: "2026-07-24T00:01:00Z",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
      new Response(
        JSON.stringify({
          scope: "project",
          tenantId: "tenant-1",
          projectId: "project/one",
          overrides: {
            waitingKeepAliveSeconds: null,
            suspendAfterIdleSeconds: 7200,
            absoluteSessionLifetimeSeconds: 86_400,
            workspaceRetentionDays: null,
            warmPoolMode: "disabled",
          },
          effective: {
            waitingKeepAliveSeconds: 900,
            suspendAfterIdleSeconds: 7200,
            absoluteSessionLifetimeSeconds: 86_400,
            workspaceRetentionDays: 30,
            warmPoolMode: "disabled",
          },
          version: 4,
          updatedBy: "user-1",
          createdAt: "2026-07-24T00:00:00Z",
          updatedAt: "2026-07-24T00:02:00Z",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.getProjectResourceLifecyclePolicy("project/one");
    await controlPlaneClient.updateProjectResourceLifecyclePolicy("project/one", {
      expectedVersion: 3,
      waitingKeepAliveSeconds: null,
      suspendAfterIdleSeconds: 7200,
      absoluteSessionLifetimeSeconds: 86_400,
      workspaceRetentionDays: null,
      warmPoolMode: "disabled",
    });

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/projects/project%2Fone/resource-lifecycle-policy",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/projects/project%2Fone/resource-lifecycle-policy",
      expect.objectContaining({
        method: "PUT",
        credentials: "include",
        body: JSON.stringify({
          expectedVersion: 3,
          waitingKeepAliveSeconds: null,
          suspendAfterIdleSeconds: 7200,
          absoluteSessionLifetimeSeconds: 86_400,
          workspaceRetentionDays: null,
          warmPoolMode: "disabled",
        }),
      }),
    );
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

  it("routes Session settle and archive mutations with independent idempotency keys", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(JSON.stringify({ id: "session-1" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.setSessionSettled("session/one", true, {
      idempotencyKey: "settle-key",
    });
    await controlPlaneClient.archiveSession("session/one", {
      idempotencyKey: "archive-key",
    });

    expect(fetchMock.mock.calls[0]).toEqual([
      "/v1/sessions/session%2Fone/settled",
      expect.objectContaining({
        method: "PUT",
        credentials: "include",
        body: JSON.stringify({ settled: true }),
      }),
    ]);
    expect(new Headers(fetchMock.mock.calls[0]![1].headers).get("Idempotency-Key")).toBe(
      "settle-key",
    );
    expect(fetchMock.mock.calls[1]).toEqual([
      "/v1/sessions/session%2Fone/archive",
      expect.objectContaining({ method: "POST", credentials: "include" }),
    ]);
    expect(new Headers(fetchMock.mock.calls[1]![1].headers).get("Idempotency-Key")).toBe(
      "archive-key",
    );
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

  it("requests an idempotent explicit resume for an idle-suspended active Turn", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            id: "execution-1",
            sessionId: "session/one",
            turnId: "turn-1",
            status: "recovering",
            generation: 1,
          }),
          { status: 202, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const resumed = await controlPlaneClient.resumeActiveTurn("session/one", {
      idempotencyKey: "web-active-resume-1",
    });

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/sessions/session%2Fone/turns/active/resume",
      expect.objectContaining({ method: "POST", credentials: "include" }),
    );
    expect(new Headers(fetchMock.mock.calls[0]![1].headers).get("Idempotency-Key")).toBe(
      "web-active-resume-1",
    );
    expect(resumed).toMatchObject({ id: "execution-1", status: "recovering", generation: 1 });
  });

  it("loads bounded runtime isolation decisions without exposing attestation payloads", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            items: [
              {
                generation: 2,
                executionTargetId: "target-1",
                allocationBackend: "native-pod",
                requestedRuntime: "gvisor",
                requestedProfile: "gvisor-sandboxed-v1",
                effectiveRuntime: "gvisor",
                effectiveProfile: "gvisor-sandboxed-v1",
                policySource: "target-explicit",
                decision: "selected",
                decisionReasonCode: null,
                runtimeClassName: "synara-gvisor",
                attestedAt: "2026-07-30T08:00:00Z",
                attestationExpiresAt: "2026-07-30T08:00:45Z",
                createdAt: "2026-07-30T08:00:01Z",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const decisions =
      await controlPlaneClient.listExecutionRuntimeIsolationDecisions("execution/one");

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/executions/execution%2Fone/runtime-isolation",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(decisions.items[0]).toMatchObject({
      generation: 2,
      effectiveRuntime: "gvisor",
      effectiveProfile: "gvisor-sandboxed-v1",
    });
    expect(decisions.items[0]).not.toHaveProperty("attestationDigest");
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
                processContainment: {
                  mode: "none",
                  trustState: "none",
                  reasonCode: "no-attestation",
                },
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
      processContainment: {
        mode: "none",
        trustState: "none",
        reasonCode: "no-attestation",
      },
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

  it("manages target-local Worker Pools and placement policy with CAS versions", async () => {
    const state = {
      pools: [],
      policy: {
        tenantId: "tenant/one",
        executionTargetId: "target/one",
        version: 1,
        defaultPoolId: "pool/default",
        balancedPoolId: null,
        lowLatencyPoolId: null,
        updatedBy: null,
        updatedAt: "2026-07-25T00:00:00Z",
      },
    };
    const pool = {
      id: "pool/warm",
      tenantId: "tenant/one",
      executionTargetId: "target/one",
      name: "interactive-warm",
      mode: "warm",
      capacityClass: "interactive",
      tenantIsolation: "pinned",
      clusterId: "cluster-a",
      region: "cn-east-1",
      namespace: "synara-workers",
      desiredIdleUnits: 2,
      minIdleUnits: 0,
      maxActiveUnits: 8,
      schedulingTemplate: {},
      status: "active",
      version: 1,
      createdAt: "2026-07-25T00:00:00Z",
      updatedAt: "2026-07-25T00:00:00Z",
    };
    const fetchMock = vi
      .fn<RequiredInitFetch>()
      .mockResolvedValueOnce(
        new Response(JSON.stringify(state), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(pool), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ ...state, policy: { ...state.policy, version: 2 } }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.getExecutionPlacement("tenant/one", "target/one");
    await controlPlaneClient.createWorkerPool("tenant/one", "target/one", {
      name: "interactive-warm",
      mode: "warm",
      capacityClass: "interactive",
      tenantIsolation: "pinned",
      clusterId: "cluster-a",
      region: "cn-east-1",
      namespace: "synara-workers",
      desiredIdleUnits: 2,
      minIdleUnits: 0,
      maxActiveUnits: 8,
      schedulingTemplate: {},
      status: "active",
    });
    await controlPlaneClient.updateExecutionPlacementPolicy("tenant/one", "target/one", {
      expectedVersion: 1,
      defaultPoolId: "pool/default",
      balancedPoolId: "pool/warm",
      lowLatencyPoolId: "pool/warm",
    });

    expect(fetchMock.mock.calls[0]![0]).toBe(
      "/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/worker-pools",
    );
    expect(fetchMock.mock.calls[1]![1]).toMatchObject({ method: "POST" });
    expect(JSON.parse(String(fetchMock.mock.calls[1]![1].body))).toMatchObject({
      mode: "warm",
      desiredIdleUnits: 2,
      maxActiveUnits: 8,
    });
    expect(fetchMock.mock.calls[2]![0]).toBe(
      "/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/placement-policy",
    );
    expect(JSON.parse(String(fetchMock.mock.calls[2]![1].body))).toEqual({
      expectedVersion: 1,
      defaultPoolId: "pool/default",
      balancedPoolId: "pool/warm",
      lowLatencyPoolId: "pool/warm",
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

  it("updates an Execution Target runtime isolation policy without sending target secrets", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            id: "target/one",
            tenantId: "tenant/one",
            organizationId: null,
            kind: "kubernetes",
            name: "Kubernetes workers",
            status: "offline",
            capabilities: {},
            isolationProfile: "gvisor-sandboxed-v1",
            platformSharedEligible: true,
            productBoundary: "multi-tenant-restricted",
            runtimeIsolationPolicy: {
              mode: "explicit",
              requestedRuntime: "gvisor",
              preferred: [],
              minimumProfile: "gvisor-sandboxed-v1",
              fallbackPolicy: "fail-closed",
              runtimeClassName: "synara-gvisor",
              gvisorCompatibleProviders: ["codex"],
            },
            createdAt: "2026-07-14T00:00:00Z",
            updatedAt: "2026-07-14T01:00:00Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const policy = {
      mode: "explicit" as const,
      runtime: "gvisor" as const,
      minimumProfile: "gvisor-sandboxed-v1" as const,
      fallbackPolicy: "fail-closed" as const,
      runtimeClassName: "synara-gvisor",
      gvisorCompatibleProviders: ["codex" as const],
    };

    const target = await controlPlaneClient.updateExecutionTargetRuntimeIsolationPolicy(
      "tenant/one",
      "target/one",
      policy,
    );

    expect(target.runtimeIsolationPolicy?.gvisorCompatibleProviders).toEqual(["codex"]);
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/runtime-isolation-policy",
      expect.objectContaining({
        method: "PUT",
        credentials: "include",
        body: JSON.stringify(policy),
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

  it("loads encoded Tenant and Session usage routes", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(JSON.stringify({ items: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.getTenantUsage("tenant/one");
    await controlPlaneClient.getInternalCostAllocationReport("tenant/one");
    await controlPlaneClient.putProjectCostAllocation("tenant/one", "project/one", {
      costCenterCode: "CC-ENG",
      departmentCode: "platform",
      expectedVersion: 1,
    });
    await controlPlaneClient.getSessionUsage("session/one");

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/tenants/tenant%2Fone/usage",
      "/v1/tenants/tenant%2Fone/cost-accounting/report",
      "/v1/tenants/tenant%2Fone/cost-accounting/projects/project%2Fone",
      "/v1/sessions/session%2Fone/usage",
    ]);
    expect(fetchMock.mock.calls[2]?.[1]).toMatchObject({
      method: "PUT",
      body: JSON.stringify({
        costCenterCode: "CC-ENG",
        departmentCode: "platform",
        expectedVersion: 1,
      }),
    });
    expect(resolveInternalCostAllocationExportUrl("tenant/one")).toBe(
      "/v1/tenants/tenant%2Fone/cost-accounting/export.csv",
    );
  });

  it("preserves the byte-bound Tenant Usage export", async () => {
    const body = JSON.stringify({
      tenantId: "tenant/one",
      entitlementProfileVersion: 3,
      periodStart: "2026-08-01T00:00:00Z",
      periodEnd: "2026-09-01T00:00:00Z",
      usage: { totalTokens: 180, providerCostMissingCount: 1 },
      softQuota: { metric: "execution_seconds", enforcement: "soft", hardStop: false },
      alerts: [],
    });
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(body, {
          status: 200,
          headers: {
            "X-Synara-Usage-Export-Schema": "json-v1",
            "X-Synara-Usage-Export-SHA256":
              "6558384791e93e16a941d65732d6250eacef71b36beec638236f2bc6964b6188",
            "X-Synara-Usage-Export-Bytes": String(new TextEncoder().encode(body).byteLength),
          },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const exported = await controlPlaneClient.getTenantUsageExport("tenant/one");

    expect(exported.body).toBe(body);
    expect(exported.usage.entitlementProfileVersion).toBe(3);
    expect(exported.bytes).toBe(new TextEncoder().encode(body).byteLength);
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/usage/export.json",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("rejects a Tenant Usage export whose SHA-256 does not match the raw body", async () => {
    const body = "{}";
    vi.stubGlobal(
      "fetch",
      vi.fn<RequiredInitFetch>(
        async () =>
          new Response(body, {
            status: 200,
            headers: {
              "X-Synara-Usage-Export-Schema": "json-v1",
              "X-Synara-Usage-Export-SHA256": "a".repeat(64),
              "X-Synara-Usage-Export-Bytes": String(new TextEncoder().encode(body).byteLength),
            },
          }),
      ),
    );

    await expect(controlPlaneClient.getTenantUsageExport("tenant/one")).rejects.toMatchObject({
      code: "tenant_usage_export_integrity_invalid",
    });
  });

  it("preserves and verifies the byte-bound Tenant Support diagnostic", async () => {
    const body =
      '{"schemaVersion":"json-v1","generatedAt":"2026-08-03T00:00:00Z","tenant":{"id":"tenant/one"},"usage":{"tenantId":"tenant/one"}}';
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(body, {
          status: 200,
          headers: {
            "X-Synara-Support-Diagnostic-Schema": "json-v1",
            "X-Synara-Support-Diagnostic-SHA256":
              "3242f5ce7625ef260123a7f33321705e73ab179e124aa17def5a105ddbfecb38",
            "X-Synara-Support-Diagnostic-Bytes": String(new TextEncoder().encode(body).byteLength),
          },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const exported = await controlPlaneClient.getTenantSupportDiagnosticExport("tenant/one");

    expect(exported.body).toBe(body);
    expect(exported.diagnostic.schemaVersion).toBe("json-v1");
    expect(exported.bytes).toBe(new TextEncoder().encode(body).byteLength);
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/support-diagnostic.json",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("uses explicit Support Access policy and four-eyes decision routes", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(JSON.stringify({ items: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.updateTenantSupportPolicy("tenant/one", {
      supportAccessEnabled: true,
      version: 2,
      reason: "Enable bounded support access.",
    });
    await controlPlaneClient.requestPlatformSupportAccess({
      tenantId: "tenant/one",
      reason: "Investigate execution delay in read-only mode.",
      requestedDurationSeconds: 1800,
    });
    await controlPlaneClient.approvePlatformSupportAccess("grant/one", {
      expectedVersion: 1,
      reason: "Approved for bounded read-only diagnostics.",
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/tenants/tenant%2Fone/support-policy",
      "/v1/platform/support-access/requests",
      "/v1/platform/support-access/grant%2Fone/approve",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[0]![1].body))).toEqual({
      supportAccessEnabled: true,
      expectedVersion: 2,
      reason: "Enable bounded support access.",
    });
  });

  it("uses scoped Legal Hold creation and versioned release routes", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(JSON.stringify({ items: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.createLegalHold("tenant/one", {
      scopeType: "session",
      scopeId: "session/one",
      name: "Investigation preservation",
      matterReference: "MAT-2026-001",
      reason: "Preserve the affected session while the investigation remains open.",
    });
    await controlPlaneClient.releaseLegalHold("tenant/one", "hold/one", {
      expectedVersion: 1,
      reason: "The controlling matter is closed and counsel approved release.",
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/tenants/tenant%2Fone/legal-holds",
      "/v1/tenants/tenant%2Fone/legal-holds/hold%2Fone/release",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[1]![1].body))).toEqual({
      expectedVersion: 1,
      reason: "The controlling matter is closed and counsel approved release.",
    });
  });

  it("uses versioned Privacy Request export and erasure routes", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(JSON.stringify({ items: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.createPrivacyRequest("tenant/one", {
      requestType: "access_export",
      reason: "Provide my Tenant-scoped personal data export.",
    });
    await controlPlaneClient.transitionPrivacyRequest("tenant/one", "request/one", {
      expectedVersion: 1,
      toStatus: "verified",
      reason: "Identity was verified against the enterprise directory.",
    });
    await controlPlaneClient.executePrivacyExport("tenant/one", "request/one", 3);
    await controlPlaneClient.executePrivacyErasure("tenant/one", "request/two", 3);
    await controlPlaneClient.executeTenantDataExport("tenant/one");

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/tenants/tenant%2Fone/privacy-requests",
      "/v1/tenants/tenant%2Fone/privacy-requests/request%2Fone/transitions",
      "/v1/tenants/tenant%2Fone/privacy-requests/request%2Fone/export",
      "/v1/tenants/tenant%2Fone/privacy-requests/request%2Ftwo/erasure",
      "/v1/tenants/tenant%2Fone/data-export",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[2]![1].body))).toEqual({ expectedVersion: 3 });
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

  it("uses versioned Tenant execution scheduling policy routes", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(JSON.stringify({ scope: {}, effective: {} }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const document = {
      denyAll: false,
      target: { mode: "any" as const, values: [] },
      region: { mode: "allow" as const, values: ["cn-east-1"] },
      cluster: { mode: "any" as const, values: [] },
      provider: { mode: "any" as const, values: [] },
      capacityClass: { mode: "any" as const, values: [] },
    };

    await controlPlaneClient.getTenantExecutionSchedulingPolicy("tenant/one");
    await controlPlaneClient.updateTenantExecutionSchedulingPolicy("tenant/one", {
      expectedVersion: 2,
      document,
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/tenants/tenant%2Fone/execution-scheduling-policy",
      "/v1/tenants/tenant%2Fone/execution-scheduling-policy",
    ]);
    expect(fetchMock.mock.calls[1]![1]).toEqual(
      expect.objectContaining({
        method: "PUT",
        body: JSON.stringify({ expectedVersion: 2, document }),
      }),
    );
  });

  it("loads the server-authoritative data residency statement route", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            schemaVersion: "synara-data-residency-statement-v1",
            generatedAt: "2026-08-03T00:00:00Z",
            tenant: { id: "tenant/one", name: "Tenant One", homeRegion: "cn-east-1" },
            execution: {
              status: "enforced",
              allowedRegions: ["cn-east-1"],
              policyVersion: 2,
              policyDigest: "sha256:policy",
              enforcement: "candidate-selection-and-commit",
            },
            dataPlanes: {
              metadata: "deployment-annex-required",
              artifacts: "deployment-annex-required",
              kms: "deployment-annex-required",
            },
            limitation: "execution only",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const statement = await controlPlaneClient.getTenantDataResidencyStatement("tenant/one");

    expect(statement.execution.allowedRegions).toEqual(["cn-east-1"]);
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/tenants/tenant%2Fone/data-residency-statement",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("preserves the server-authoritative data residency response bytes for downloads", async () => {
    const body = JSON.stringify({
      schemaVersion: "synara-data-residency-statement-v1",
      generatedAt: "2026-08-03T00:00:00Z",
      tenant: { id: "tenant/one", name: "Tenant One", homeRegion: "cn-east-1" },
      execution: {
        status: "enforced",
        allowedRegions: ["cn-east-1"],
        policyVersion: 2,
        policyDigest: "sha256:policy",
        enforcement: "candidate-selection-and-commit",
      },
      dataPlanes: {
        metadata: "deployment-annex-required",
        artifacts: "deployment-annex-required",
        kms: "deployment-annex-required",
      },
      limitation: "execution only",
    });
    const digest = "a".repeat(64);
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(body, {
          status: 200,
          headers: {
            "Content-Type": "application/json",
            "X-Synara-Data-Residency-SHA256": digest,
            "X-Synara-Data-Residency-Bytes": String(new TextEncoder().encode(body).byteLength),
          },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const downloaded =
      await controlPlaneClient.getTenantDataResidencyStatementDownload("tenant/one");

    expect(downloaded.body).toBe(body);
    expect(downloaded.sha256).toBe(digest);
    expect(downloaded.bytes).toBe(new TextEncoder().encode(body).byteLength);
    expect(downloaded.statement.tenant.id).toBe("tenant/one");
  });

  it("rejects a data residency download whose byte header is stale", async () => {
    const body = "{}";
    vi.stubGlobal(
      "fetch",
      vi.fn<RequiredInitFetch>(
        async () =>
          new Response(body, {
            status: 200,
            headers: {
              "X-Synara-Data-Residency-SHA256": "a".repeat(64),
              "X-Synara-Data-Residency-Bytes": "1",
            },
          }),
      ),
    );

    await expect(
      controlPlaneClient.getTenantDataResidencyStatementDownload("tenant/one"),
    ).rejects.toMatchObject({ code: "data_residency_statement_integrity_invalid" });
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
      new Response(JSON.stringify({ authorizationUrl: "https://id.example.com/admin" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    ];
    const fetchMock = vi.fn(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listPublicIdentityConnections("tenant one");
    await controlPlaneClient.startSSO("connection/one", "/settings?section=tenancy");
    await controlPlaneClient.startPlatformAdminSSO("connection/admin");

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
    expect(fetchMock).toHaveBeenNthCalledWith(
      3,
      "/v1/auth/sso/connection%2Fadmin/start?returnTo=%2F__synara%2Fplatform-admin",
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

  it("uses versioned Tenant lifecycle, deletion inventory, and recovery routes", async () => {
    const tenant = {
      id: "tenant/one",
      slug: "enterprise",
      name: "Enterprise Tenant",
      status: "closed",
      lifecycleVersion: 8,
      entitlementProfileCode: "enterprise",
      region: "ap-east-1",
      role: "owner",
    };
    const responses = [
      new Response(JSON.stringify({ ...tenant, status: "suspended", lifecycleVersion: 7 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(null, { status: 204 }),
      new Response(
        JSON.stringify({
          items: [
            {
              ...tenant,
              status: "deleting",
              deletionRequestedAt: "2026-07-30T00:00:00Z",
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
      new Response(JSON.stringify(tenant), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.transitionTenant("tenant/one", {
      toStatus: "suspended",
      expectedVersion: 6,
      reason: "scheduled operational pause",
    });
    await controlPlaneClient.requestTenantDeletion("tenant/one", {
      expectedVersion: 7,
      reason: "customer approved deletion",
    });
    const requests = await controlPlaneClient.listTenantDeletionRequests();
    await controlPlaneClient.restoreTenant("tenant/one", {
      expectedVersion: 8,
      reason: "customer withdrew deletion",
    });

    expect(requests.items[0]).toMatchObject({ status: "deleting", lifecycleVersion: 8 });
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/tenants/tenant%2Fone/lifecycle-transitions",
      "/v1/tenants/tenant%2Fone/deletion-requests",
      "/v1/tenants/deletion-requests",
      "/v1/tenants/tenant%2Fone/restore",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[0]![1].body))).toEqual({
      toStatus: "suspended",
      expectedVersion: 6,
      reason: "scheduled operational pause",
    });
    expect(JSON.parse(String(fetchMock.mock.calls[1]![1].body))).toEqual({
      expectedVersion: 7,
      reason: "customer approved deletion",
    });
    expect(JSON.parse(String(fetchMock.mock.calls[3]![1].body))).toEqual({
      expectedVersion: 8,
      reason: "customer withdrew deletion",
    });
  });

  it("uses the audited Tenant member lifecycle and Session revocation routes", async () => {
    const responses = [
      new Response(
        JSON.stringify({
          tenantId: "tenant/one",
          userId: "user/one",
          email: "member@example.com",
          displayName: "Member",
          role: "auditor",
          status: "suspended",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
      new Response(JSON.stringify({ revokedCount: 2 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(null, { status: 204 }),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.updateTenantMember("tenant/one", "user/one", {
      role: "auditor",
      status: "suspended",
    });
    const revoked = await controlPlaneClient.revokeTenantUserSessions("tenant/one", "user/one");
    await controlPlaneClient.removeTenantMember("tenant/one", "user/one");

    expect(revoked.revokedCount).toBe(2);
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/tenants/tenant%2Fone/members/user%2Fone",
      "/v1/tenants/tenant%2Fone/members/user%2Fone/revoke-sessions",
      "/v1/tenants/tenant%2Fone/members/user%2Fone",
    ]);
    expect(fetchMock.mock.calls[0]![1]).toEqual(
      expect.objectContaining({
        method: "PATCH",
        body: JSON.stringify({ role: "auditor", status: "suspended" }),
      }),
    );
    expect(fetchMock.mock.calls[1]![1]).toEqual(expect.objectContaining({ method: "POST" }));
    expect(fetchMock.mock.calls[2]![1]).toEqual(expect.objectContaining({ method: "DELETE" }));
  });

  it("lists redacted Tenant Outbox messages and replays a dead letter", async () => {
    const responses = [
      new Response(
        JSON.stringify({
          items: [
            {
              id: "message-1",
              topic: "execution.queued",
              messageKey: "execution-1",
              status: "dead-letter",
              attempts: 5,
              availableAt: "2026-07-30T00:00:00Z",
              createdAt: "2026-07-30T00:00:00Z",
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
      new Response(
        JSON.stringify({
          id: "message-1",
          topic: "execution.queued",
          messageKey: "execution-1",
          attempts: 0,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listTenantOutboxMessages("tenant/one", {
      status: "dead-letter",
      limit: 500,
    });
    await controlPlaneClient.replayTenantOutboxMessage("tenant/one", "message/one");

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/tenants/tenant%2Fone/outbox-messages?status=dead-letter&limit=200",
      "/v1/tenants/tenant%2Fone/outbox-messages/message%2Fone/replay",
    ]);
    expect(fetchMock.mock.calls[1]![1]).toEqual(expect.objectContaining({ method: "POST" }));
  });

  it("uses Tenant Domain Verification and versioned SSO Enforcement routes", async () => {
    const domain = {
      id: "domain/one",
      tenantId: "tenant/one",
      domain: "example.com",
      status: "pending",
      verificationRecordName: "_synara.example.com",
      verificationExpiresAt: "2026-07-31T00:00:00Z",
      verifiedAt: null,
      revokedAt: null,
      createdAt: "2026-07-30T00:00:00Z",
      updatedAt: "2026-07-30T00:00:00Z",
    };
    const policy = {
      tenantId: "tenant/one",
      ssoEnforcement: "optional",
      version: 2,
      recoveryUserId: null,
      enforcementSetAt: null,
      updatedAt: "2026-07-30T00:00:00Z",
    };
    const responses = [
      new Response(JSON.stringify({ items: [domain] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(JSON.stringify({ ...domain, verificationRecordValue: "challenge" }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(JSON.stringify({ ...domain, status: "verified" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(null, { status: 204 }),
      new Response(JSON.stringify(policy), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(
        JSON.stringify({
          ...policy,
          ssoEnforcement: "required",
          recoveryUserId: "owner/one",
          version: 3,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listIdentityDomains("tenant/one");
    await controlPlaneClient.createIdentityDomain("tenant/one", "example.com");
    await controlPlaneClient.verifyIdentityDomain("tenant/one", "domain/one");
    await controlPlaneClient.revokeIdentityDomain("tenant/one", "domain/one");
    await controlPlaneClient.getTenantIdentityPolicy("tenant/one");
    await controlPlaneClient.updateTenantIdentityPolicy("tenant/one", {
      ssoEnforcement: "required",
      recoveryUserId: "owner/one",
      expectedVersion: 2,
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/tenants/tenant%2Fone/identity-domains",
      "/v1/tenants/tenant%2Fone/identity-domains",
      "/v1/tenants/tenant%2Fone/identity-domains/domain%2Fone/verify",
      "/v1/tenants/tenant%2Fone/identity-domains/domain%2Fone/revoke",
      "/v1/tenants/tenant%2Fone/identity-policy",
      "/v1/tenants/tenant%2Fone/identity-policy",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[1]![1].body))).toEqual({
      domain: "example.com",
    });
    expect(JSON.parse(String(fetchMock.mock.calls[5]![1].body))).toEqual({
      ssoEnforcement: "required",
      recoveryUserId: "owner/one",
      expectedVersion: 2,
    });
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

  it("uses encoded Platform compliance governance routes and preserves request bodies", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [] }), { status: 200 }),
      ...Array.from({ length: 6 }, () => new Response(JSON.stringify({}), { status: 200 })),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listPlatformStage6CompliancePrograms();
    await controlPlaneClient.createPlatformStage6ComplianceProgram({
      programKey: "soc2-2026",
      framework: "soc2_type2",
      scopeVersion: "2026.1",
      scopeSummary: "Synara SaaS and production operations scope.",
      executiveSponsorUserId: "sponsor/one",
      auditorOrganization: "Independent Auditor LLP",
      auditorEngagementReference: "https://evidence.example/auditor",
      observationStart: "2026-08-01T00:00:00Z",
      observationEnd: "2027-02-01T00:00:00Z",
      evidenceRepositoryReference: "https://evidence.example/repository",
      evidenceAccessPolicyReference: "https://evidence.example/access-policy",
      evidenceRetentionDays: 365,
      vendorRegisterReference: "https://evidence.example/vendors",
      riskRegisterReference: "https://evidence.example/risks",
    });
    await controlPlaneClient.createPlatformStage6ComplianceControl("program/one", {
      controlId: "CC6.1",
      family: "logical_access",
      title: "Access review",
      description: "Review privileged access assignments every quarter.",
      ownerUserId: "owner/one",
      cadence: "quarterly",
      evidenceRequirement: "Retain the review population and remediation evidence.",
    });
    await controlPlaneClient.submitPlatformStage6ComplianceEvidence("program/one", {
      controlRecordId: "control/one",
      evidenceId: "access-review-q3",
      evidenceType: "access_review",
      periodStart: "2026-07-01T00:00:00Z",
      periodEnd: "2026-07-31T00:00:00Z",
      sourceReference: "https://evidence.example/access-review",
      sha256: "a".repeat(64),
      mediaType: "application/json",
      classification: "restricted",
      collectedAt: "2026-08-01T00:00:00Z",
      retentionUntil: "2027-08-01T00:00:00Z",
    });
    await controlPlaneClient.reviewPlatformStage6ComplianceEvidence("program/one", "evidence/one", {
      decision: "accepted",
      reviewRole: "security",
      reason: "Reviewed the exact digest and immutable reference.",
      evidenceReference: "https://evidence.example/review",
      evidenceSha256: `sha256:${"b".repeat(64)}`,
    });
    await controlPlaneClient.recordPlatformStage6ComplianceDecision("program/one", {
      decisionRole: "security",
      decision: "approved",
      reason: "Approved the bounded Security start-gate record.",
      evidenceReference: "https://evidence.example/decision",
      evidenceSha256: `sha256:${"c".repeat(64)}`,
    });
    await controlPlaneClient.transitionPlatformStage6ComplianceProgram("program/one", {
      expectedVersion: 1,
      targetState: "ready_for_review",
      reason: "Submit the bounded record for separated review.",
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/compliance-programs",
      "/v1/platform/compliance-programs",
      "/v1/platform/compliance-programs/program%2Fone/controls",
      "/v1/platform/compliance-programs/program%2Fone/evidence",
      "/v1/platform/compliance-programs/program%2Fone/evidence/evidence%2Fone/review",
      "/v1/platform/compliance-programs/program%2Fone/decisions",
      "/v1/platform/compliance-programs/program%2Fone/transitions",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[6]![1].body))).toEqual({
      expectedVersion: 1,
      targetState: "ready_for_review",
      reason: "Submit the bounded record for separated review.",
    });
  });

  it("uses encoded Platform Provider commercial authorization routes", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [] }), { status: 200 }),
      ...Array.from({ length: 3 }, () => new Response(JSON.stringify({}), { status: 200 })),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listPlatformProviderCommercialAuthorizations();
    await controlPlaneClient.createPlatformProviderCommercialAuthorization({
      authorizationKey: "openai-api-2026",
      provider: "codex",
      providerProduct: "OpenAI API",
      accountType: "Enterprise API organization",
      contractingEntity: "OpenAI contracting entity",
      credentialMode: "customer_byok",
      allowedCredentialScopes: ["organization", "tenant"],
      allowedRegions: ["us-east-1"],
      dataUsePolicy: "no_training",
      retentionPolicy: "Approved API retention policy applies.",
      termsEffectiveAt: "2026-08-01T00:00:00Z",
      termsReference: "https://evidence.example/terms",
      termsSha256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      agreementReference: "https://evidence.example/agreement",
      agreementSha256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      dpaReference: "https://evidence.example/dpa",
      dpaSha256: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      prohibitedUseSummary: "Consumer login sharing and safety-control bypass are prohibited.",
      terminationRunbookReference: "https://evidence.example/termination",
      terminationRunbookSha256:
        "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      reviewExpiresAt: "2026-11-01T00:00:00Z",
    });
    await controlPlaneClient.recordPlatformProviderCommercialApproval("authorization/one", {
      role: "security",
      decision: "approved",
      reason: "Approved the exact hosted Provider boundary.",
      evidenceReference: "https://evidence.example/security",
      evidenceSha256: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
    });
    await controlPlaneClient.transitionPlatformProviderCommercialAuthorization(
      "authorization/one",
      {
        expectedVersion: 2,
        targetState: "active",
        reason: "Activate after all required approvals committed.",
      },
    );

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/provider-commercial-authorizations",
      "/v1/platform/provider-commercial-authorizations",
      "/v1/platform/provider-commercial-authorizations/authorization%2Fone/approvals",
      "/v1/platform/provider-commercial-authorizations/authorization%2Fone/transitions",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body))).toMatchObject({
      termsSha256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      agreementSha256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      dpaSha256: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      terminationRunbookSha256:
        "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
    });
    expect(JSON.parse(String(fetchMock.mock.calls[2]?.[1]?.body))).toMatchObject({
      evidenceSha256: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
    });
  });

  it("binds exact evidence bytes on an encoded Incident resolution approval route", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(async () =>
      Promise.resolve(new Response(JSON.stringify({}), { status: 200 })),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.recordPlatformStage6IncidentResolutionApproval("incident/one", {
      decision: "approved",
      reason: "Security and Privacy approved the exact containment and recovery evidence bytes.",
      evidenceReference: "https://evidence.example/incidents/security-resolution",
      evidenceSha256: `sha256:${"b".repeat(64)}`,
    });

    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/platform/incidents/incident%2Fone/resolution-approval",
    );
    expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toMatchObject({
      evidenceSha256: `sha256:${"b".repeat(64)}`,
    });
  });

  it("uses encoded Platform Penetration governance routes", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [] }), { status: 200 }),
      new Response(JSON.stringify({}), { status: 201 }),
      new Response(JSON.stringify({}), { status: 200 }),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listPlatformStage6PenetrationEngagements();
    await controlPlaneClient.importPlatformStage6PenetrationEngagement({
      candidateRecordId: "candidate/one",
      receiptBase64: "e30=",
      receiptSha256: `sha256:${"a".repeat(64)}`,
    });
    await controlPlaneClient.recordPlatformStage6PenetrationApproval("engagement/one", {
      role: "security",
      decision: "approved",
      reason: "Security approved the exact candidate-bound receipt.",
      evidenceReference: "https://evidence.example/penetration/security",
      evidenceSha256: `sha256:${"b".repeat(64)}`,
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/penetration-engagements",
      "/v1/platform/penetration-engagements",
      "/v1/platform/penetration-engagements/engagement%2Fone/approvals",
    ]);
  });

  it("uses encoded Platform Capacity governance routes", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [] }), { status: 200 }),
      new Response(JSON.stringify({}), { status: 201 }),
      new Response(JSON.stringify({}), { status: 200 }),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listPlatformStage6CapacityRuns();
    await controlPlaneClient.importPlatformStage6CapacityRun({
      candidateRecordId: "candidate/one",
      receiptBase64: "e30=",
      receiptSha256: `sha256:${"a".repeat(64)}`,
    });
    await controlPlaneClient.recordPlatformStage6CapacityApproval("run/one", {
      role: "operations",
      decision: "approved",
      reason: "Operations approved the exact candidate-bound Capacity receipt.",
      evidenceReference: "https://evidence.example/capacity/operations",
      evidenceSha256: `sha256:${"b".repeat(64)}`,
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/capacity-runs",
      "/v1/platform/capacity-runs",
      "/v1/platform/capacity-runs/run%2Fone/approvals",
    ]);
  });

  it("uses encoded Platform Incident exercise governance routes", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [] }), { status: 200 }),
      new Response(JSON.stringify({}), { status: 201 }),
      new Response(JSON.stringify({}), { status: 200 }),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listPlatformStage6IncidentExercises();
    await controlPlaneClient.importPlatformStage6IncidentExercise({
      candidateRecordId: "candidate/one",
      receiptBase64: "e30=",
      receiptSha256: `sha256:${"a".repeat(64)}`,
    });
    await controlPlaneClient.recordPlatformStage6IncidentExerciseApproval("exercise/one", {
      role: "communications",
      decision: "approved",
      reason: "Communications approved the exact candidate-bound Incident exercise receipt.",
      evidenceReference: "https://evidence.example/incident/communications",
      evidenceSha256: `sha256:${"b".repeat(64)}`,
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/incident-exercises",
      "/v1/platform/incident-exercises",
      "/v1/platform/incident-exercises/exercise%2Fone/approvals",
    ]);
  });

  it("uses encoded Platform Operations exercise governance routes", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [] }), { status: 200 }),
      new Response(JSON.stringify({}), { status: 201 }),
      new Response(JSON.stringify({}), { status: 200 }),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listPlatformStage6OperationsExercises();
    await controlPlaneClient.importPlatformStage6OperationsExercise({
      candidateRecordId: "candidate/one",
      receiptBase64: "e30=",
      receiptSha256: `sha256:${"a".repeat(64)}`,
    });
    await controlPlaneClient.recordPlatformStage6OperationsExerciseApproval("exercise/one", {
      role: "security",
      decision: "approved",
      reason: "Security approved the exact candidate-bound Operations exercise receipt.",
      evidenceReference: "https://evidence.example/operations/security",
      evidenceSha256: `sha256:${"b".repeat(64)}`,
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/operations-exercises",
      "/v1/platform/operations-exercises",
      "/v1/platform/operations-exercises/exercise%2Fone/approvals",
    ]);
  });

  it("loads exact-candidate Release readiness through an encoded route", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(
          JSON.stringify({
            candidateRecordId: "candidate/one",
            candidateId: "stage6-rc1",
            candidateState: "ready_for_review",
            internalGatesSatisfied: false,
            approvalTransitionEligible: false,
            finalReviewGate: {
              id: "final_review",
              label: "Final Review release binding",
              satisfied: false,
              internalAssessment: "eligible-exact-final-review-bound",
              externalVerificationRequired: true,
              externalBoundary: "External GA authority remains required.",
            },
            releaseTransitionEligible: false,
            externalGaStatus: "required_not_verified_by_synara",
            gates: [],
          }),
          { status: 200 },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const readiness = await controlPlaneClient.getPlatformStage6ReleaseReadiness("candidate/one");

    expect(readiness.externalGaStatus).toBe("required_not_verified_by_synara");
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/release-candidates/candidate%2Fone/readiness",
    ]);
  });

  it("binds exact Provider commercial authorization IDs when creating a Release candidate", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () => new Response(JSON.stringify({ id: "candidate-one" }), { status: 201 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.createPlatformStage6ReleaseCandidate({
      candidateId: "stage6-rc1",
      sourceCommit: "a".repeat(40),
      lockfileSha256: `sha256:${"b".repeat(64)}`,
      evidenceBundleSha256: `sha256:${"c".repeat(64)}`,
      evidenceBundleReceiptBase64: "e30=",
      finalAssetSetSha256: `sha256:${"d".repeat(64)}`,
      environmentId: "stage6/production-like",
      impactDomains: ["code_change", "provider_commercial"],
      providerCommercialAuthorizationIds: ["authorization/one"],
    });

    expect(fetchMock.mock.calls[0]?.[1]).toMatchObject({ method: "POST" });
    expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toMatchObject({
      providerCommercialAuthorizationIds: ["authorization/one"],
    });
  });

  it("binds exact evidence bytes when recording a Release approval", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () => new Response(JSON.stringify({ id: "candidate/one" }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.recordPlatformStage6ReleaseApproval("candidate/one", {
      role: "security",
      decision: "approved",
      reason: "Security reviewed the exact immutable release evidence bytes.",
      evidenceReference: "https://evidence.example.test/release/security",
      evidenceSha256: `sha256:${"e".repeat(64)}`,
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/release-candidates/candidate%2Fone/approvals",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toMatchObject({
      evidenceSha256: `sha256:${"e".repeat(64)}`,
    });
  });

  it("binds an exact Final Review receipt through an encoded candidate route", async () => {
    const fetchMock = vi.fn<RequiredInitFetch>(
      async () =>
        new Response(JSON.stringify({ id: "candidate/one", finalReview: {} }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.recordPlatformStage6ReleaseFinalReview("candidate/one", {
      receiptBase64: "e30=",
      receiptSha256: `sha256:${"a".repeat(64)}`,
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/release-candidates/candidate%2Fone/final-review",
    ]);
    expect(fetchMock.mock.calls[0]?.[1]).toMatchObject({ method: "POST" });
  });

  it("uses encoded Platform governance authority routes", async () => {
    const responses = [
      new Response(JSON.stringify({ items: [], authorityKeys: [], operators: [] }), {
        status: 200,
      }),
      new Response(JSON.stringify({}), { status: 201 }),
      new Response(JSON.stringify({}), { status: 200 }),
    ];
    const fetchMock = vi.fn<RequiredInitFetch>(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.listPlatformGovernanceAuthorities();
    await controlPlaneClient.createPlatformGovernanceAuthority({
      userId: "operator/one",
      authorityKey: "release.engineering",
      expiresAt: "2026-12-01T00:00:00Z",
      reason: "Owner verified the exact Engineering approval function.",
      evidenceReference: "https://evidence.example/authority/engineering",
      evidenceSha256: `sha256:${"a".repeat(64)}`,
    });
    await controlPlaneClient.revokePlatformGovernanceAuthority("grant/one", {
      expectedVersion: 1,
      reason: "Owner removed the Engineering approval responsibility.",
    });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/v1/platform/governance-authorities",
      "/v1/platform/governance-authorities",
      "/v1/platform/governance-authorities/grant%2Fone/revoke",
    ]);
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body))).toMatchObject({
      evidenceSha256: `sha256:${"a".repeat(64)}`,
    });
  });
});
