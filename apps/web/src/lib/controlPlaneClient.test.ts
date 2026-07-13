import { afterEach, describe, expect, it, vi } from "vitest";

import {
  controlPlaneClient,
  ControlPlaneError,
  resolveAuditLogExportUrl,
  resolveControlPlaneHttpUrl,
  resolveSessionEventStreamUrl,
} from "./controlPlaneClient";

afterEach(() => {
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
  });

  it("probes the public platform profile before requiring authentication", async () => {
    const fetchMock = vi.fn(async () =>
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
    const fetchMock = vi.fn(async () =>
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
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            error: {
              code: "tenant_forbidden",
              message: "Tenant access denied.",
              requestId: "request-1",
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
      }),
    );
  });

  it("creates agent sessions through the active-tenant project route", async () => {
    const fetchMock = vi.fn(async () =>
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
    const request = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(new Headers(request.headers).get("Idempotency-Key")).toBe("web-session-request-1");
  });

  it("creates projects with private Git access and can explicitly unbind it", async () => {
    const responses = [
      new Response(JSON.stringify({ id: "project-1", gitCredentialId: "git-credential-1" }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(JSON.stringify({ id: "project-1", gitCredentialId: null }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    ];
    const fetchMock = vi.fn(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    await controlPlaneClient.createProject(
      "tenant/one",
      "organization/one",
      {
        name: "Private repository",
        repositoryUrl: "https://github.com/company/private.git",
        defaultBranch: "main",
        gitCredentialId: "git-credential-1",
        visibility: "organization",
      },
      { idempotencyKey: "web-project-request-1" },
    );
    await controlPlaneClient.updateProject("project/one", { gitCredentialId: null });

    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/tenants/tenant%2Fone/organizations/organization%2Fone/projects",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          name: "Private repository",
          repositoryUrl: "https://github.com/company/private.git",
          defaultBranch: "main",
          gitCredentialId: "git-credential-1",
          visibility: "organization",
        }),
      }),
    );
    const createRequest = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(new Headers(createRequest.headers).get("Idempotency-Key")).toBe(
      "web-project-request-1",
    );
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/v1/projects/project%2Fone",
      expect.objectContaining({
        method: "PATCH",
        body: JSON.stringify({ gitCredentialId: null }),
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
    const fetchMock = vi.fn(async () => responses.shift()!);
    vi.stubGlobal("fetch", fetchMock);

    const page = await controlPlaneClient.listSessionEvents("session/one", -5, 5_000);
    await controlPlaneClient.createTurn("session/one", "Continue", {
      idempotencyKey: "web-turn-request-1",
    }, {
      runtimeMode: "approval-required",
      interactionMode: "plan",
    });

    expect(page.lastSequence).toBe(23);
    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      "/v1/sessions/session%2Fone/events?afterSequence=0&limit=500",
      expect.objectContaining({ credentials: "include" }),
    );
    const turnRequest = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(new Headers(turnRequest.headers).get("Idempotency-Key")).toBe(
      "web-turn-request-1",
    );
    expect(JSON.parse(String(turnRequest.body))).toEqual({
      inputText: "Continue",
      runtimeMode: "approval-required",
      interactionMode: "plan",
    });
  });

  it("requests an idempotent durable interrupt for the active Turn", async () => {
    const fetchMock = vi.fn(async () =>
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
    const request = fetchMock.mock.calls[0]?.[1] as RequestInit;
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
    const fetchMock = vi.fn(async () => responses.shift()!);
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
    expect(
      new Headers((fetchMock.mock.calls[1]?.[1] as RequestInit).headers).get("Idempotency-Key"),
    ).toBe("web-approval-1");
    expect(
      new Headers((fetchMock.mock.calls[2]?.[1] as RequestInit).headers).get("Idempotency-Key"),
    ).toBe("web-input-1");
  });

  it("requests an idempotent durable Steer payload for the active Turn", async () => {
    const fetchMock = vi.fn(async () =>
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
    const request = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(new Headers(request.headers).get("Idempotency-Key")).toBe("web-steer-1");
    expect(JSON.parse(String(request.body))).toEqual({ inputText: "Focus on tests" });
  });

  it("creates execution targets without expecting secret configuration in the response", async () => {
    const fetchMock = vi.fn(async () =>
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

  it("runs SSH target lifecycle operations through the tenant-scoped API", async () => {
    const fetchMock = vi.fn(async () =>
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
    const fetchMock = vi.fn(async () =>
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
    const fetchMock = vi.fn(async () =>
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
    const fetchMock = vi.fn(async () =>
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

  it("keeps identity and Service Account secrets confined to explicit create responses", async () => {
    const responses = [
      new Response(JSON.stringify({ id: "connection-1", kind: "oidc", status: "active" }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(
        JSON.stringify({
          account: { id: "service-account-1", name: "SCIM", status: "active", scopes: ["scim.write"] },
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
    const fetchMock = vi.fn(async () =>
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
    const fetchMock = vi.fn(async () =>
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
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
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
    const request = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(new Headers(request.headers).get("Content-Type")).toBe("application/octet-stream");
    expect(new Headers(request.headers).get("X-Artifact-Header")).toBe("value");
  });
});
