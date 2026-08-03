import * as FS from "node:fs";
import * as OS from "node:os";
import * as Path from "node:path";

import { afterEach, describe, expect, it, vi } from "vitest";

import {
  DesktopCredentialStore,
  type DesktopSafeStorage,
  type DesktopSaaSConnectionSecret,
} from "./desktopCredentialStore";
import { DesktopSaaSConnectionError, DesktopSaaSConnectionManager } from "./desktopSaaSConnection";

const CONTROL_PLANE = "https://control.example.com/control-plane";
const TENANT_ID = "tenant-1";
const ORGANIZATION_ID = "organization-1";
const DEVICE_ID = "device-1";
const SESSION_ID = "session-1";
const CREDENTIAL = "c".repeat(43);
const temporaryDirectories: string[] = [];

function fakeSafeStorage(): DesktopSafeStorage {
  return {
    isEncryptionAvailable: () => true,
    getSelectedStorageBackend: () => "keychain",
    encryptString: (value) => Buffer.from(`cipher:${Buffer.from(value).toString("base64")}`),
    decryptString: (value) =>
      Buffer.from(value.toString().slice("cipher:".length), "base64").toString(),
  };
}

function storeFixture(): { store: DesktopCredentialStore; filePath: string } {
  const directory = FS.mkdtempSync(Path.join(OS.tmpdir(), "synara-desktop-saas-test-"));
  temporaryDirectories.push(directory);
  const filePath = Path.join(directory, "desktop-saas-connection.json");
  return {
    store: new DesktopCredentialStore(filePath, fakeSafeStorage(), "darwin"),
    filePath,
  };
}

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function redeemedSession(credential = CREDENTIAL) {
  return {
    audience: "desktop",
    credential,
    credentialExpiresAt: "2026-09-01T00:00:00Z",
    sessionId: SESSION_ID,
    credentialFamilyId: "family-1",
    device: { id: DEVICE_ID },
    defaultTenantId: TENANT_ID,
    defaultOrganizationId: ORGANIZATION_ID,
  };
}

function sessionHydration(sessionId = SESSION_ID) {
  return {
    authenticated: true,
    user: {
      userId: "user-1",
      sessionId,
      activeTenantId: TENANT_ID,
      supportAccessGrantId: null,
      audience: "desktop",
      desktopDeviceId: DEVICE_ID,
      email: "owner@example.com",
      displayName: "Owner",
    },
    tenants: [{ id: TENANT_ID, name: "Northstar Labs", status: "active" }],
  };
}

function organizations() {
  return {
    items: [
      {
        id: ORGANIZATION_ID,
        tenantId: TENANT_ID,
        name: "Platform",
        status: "active",
      },
    ],
  };
}

function entitlements() {
  return {
    tenantId: TENANT_ID,
    profile: {
      code: "Enterprise",
      displayName: "Enterprise",
      status: "active",
      version: 1,
      evaluationDays: 0,
    },
    profileAssignment: {
      status: "active",
      version: 1,
      evaluationEndsAt: null,
      reportingPeriodStart: "2026-08-01T00:00:00Z",
      reportingPeriodEnd: "2026-09-01T00:00:00Z",
      assignmentSource: "platform_admin",
    },
    entitlements: {},
    features: {},
  };
}

function connectedSecret(
  overrides: Partial<DesktopSaaSConnectionSecret> = {},
): DesktopSaaSConnectionSecret {
  return {
    controlPlaneBaseUrl: CONTROL_PLANE,
    sessionId: SESSION_ID,
    credentialFamilyId: "family-1",
    credentialExpiresAt: "2026-09-01T00:00:00Z",
    deviceId: DEVICE_ID,
    userId: "user-1",
    email: "owner@example.com",
    displayName: "Owner",
    tenantId: TENANT_ID,
    tenantName: "Northstar Labs",
    organizationId: ORGANIZATION_ID,
    organizationName: "Platform",
    credential: CREDENTIAL,
    ...overrides,
  };
}

function manager(
  store: DesktopCredentialStore,
  fetch: (input: string | URL | Request, init?: RequestInit) => Promise<Response>,
  overrides: Partial<ConstructorParameters<typeof DesktopSaaSConnectionManager>[0]> = {},
) {
  return new DesktopSaaSConnectionManager({
    store,
    fetch,
    platform: "darwin",
    appVersion: "0.6.3",
    deviceLabel: "Huang's Mac",
    now: () => new Date("2026-08-01T00:00:00Z"),
    ...overrides,
  });
}

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    FS.rmSync(directory, { recursive: true, force: true });
  }
});

describe("DesktopSaaSConnectionManager", () => {
  it("redeems, fully hydrates, and commits the connection only after validation", async () => {
    const { store } = storeFixture();
    const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/v1/desktop-enrollments/redeem")) {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        expect(body).toEqual(
          expect.objectContaining({
            version: 1,
            controlPlaneOrigin: CONTROL_PLANE,
            enrollment: "e".repeat(43),
            platform: "darwin",
          }),
        );
        expect(String(body.proof)).toHaveLength(86);
        return json(redeemedSession());
      }
      if (url.endsWith("/v1/auth/session")) return json(sessionHydration());
      if (url.endsWith(`/v1/tenants/${TENANT_ID}/organizations`)) return json(organizations());
      if (url.endsWith(`/v1/tenants/${TENANT_ID}/entitlements`)) return json(entitlements());
      throw new Error("unexpected request");
    });
    const connection = manager(store, fetch);

    const state = await connection.connect({
      controlPlaneBaseUrl: CONTROL_PLANE,
      handle: "e".repeat(43),
    });

    expect(state).toEqual(
      expect.objectContaining({
        status: "connected",
        tenantName: "Northstar Labs",
        organizationName: "Platform",
        email: "owner@example.com",
      }),
    );
    expect(fetch).toHaveBeenCalledTimes(4);
    expect(store.readConnection()).toEqual(
      expect.objectContaining({ credential: CREDENTIAL, deviceId: DEVICE_ID }),
    );
  });

  it("reports an unavailable OS credential store as a startup error without persisting Cloud mode", async () => {
    const { store, filePath } = storeFixture();
    store.ensureDeviceIdentity();
    store.saveConnection(connectedSecret());
    const unavailableStore = new DesktopCredentialStore(
      filePath,
      {
        ...fakeSafeStorage(),
        isEncryptionAvailable: () => false,
      },
      "darwin",
    );
    const connection = manager(unavailableStore, async () => json({}));

    const state = await connection.initialize();

    expect(state.status).toBe("error");
    expect(state.message).toContain("credential store");
    expect(connection.getLastErrorCode()).toBe("os_credential_store_unavailable");
    expect(connection.getConnection()).toBeNull();
  });

  it("can require the OS credential store before accepting an empty Cloud mode", async () => {
    const { filePath } = storeFixture();
    const connection = manager(
      new DesktopCredentialStore(
        filePath,
        {
          ...fakeSafeStorage(),
          isEncryptionAvailable: () => false,
        },
        "darwin",
      ),
      async () => json({}),
    );

    const state = await connection.initialize({ requireCredentialStore: true });

    expect(state.status).toBe("error");
    expect(connection.getLastErrorCode()).toBe("os_credential_store_unavailable");
  });

  it("fails a first Cloud enrollment safely when the OS credential store cannot be opened", async () => {
    const connection = manager(
      new DesktopCredentialStore(
        Path.join(OS.tmpdir(), `synara-missing-keychain-${Date.now()}.json`),
        {
          ...fakeSafeStorage(),
          isEncryptionAvailable: () => {
            throw new Error("Keychain unavailable");
          },
        },
        "darwin",
      ),
      async () => json({}),
    );

    const state = await connection.connect({
      controlPlaneBaseUrl: CONTROL_PLANE,
      handle: "e".repeat(43),
    });

    expect(state.status).toBe("error");
    expect(state.message).toContain("credential store");
    expect(connection.getLastErrorCode()).toBe("os_credential_store_unavailable");
  });

  it("revokes and removes the local device identity when hydration fails after redemption", async () => {
    const { store, filePath } = storeFixture();
    const requests: string[] = [];
    const fetch = vi.fn(async (input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.endsWith("/v1/desktop-enrollments/redeem")) return json(redeemedSession());
      if (url.endsWith("/v1/auth/session")) return json({ authenticated: false });
      if (url.endsWith("/v1/desktop/disconnect")) return new Response(null, { status: 204 });
      throw new Error("unexpected request");
    });
    const connection = manager(store, fetch);

    const state = await connection.connect({
      controlPlaneBaseUrl: CONTROL_PLANE,
      handle: "e".repeat(43),
    });

    expect(state.status).toBe("error");
    expect(requests.at(-1)).toBe(`${CONTROL_PLANE}/v1/desktop/disconnect`);
    expect(FS.existsSync(filePath)).toBe(false);
    expect(JSON.stringify(state)).not.toContain(CREDENTIAL);
  });

  it("rotates an expiring credential during startup and atomically keeps the hydrated connection", async () => {
    const { store } = storeFixture();
    store.ensureDeviceIdentity();
    store.saveConnection(connectedSecret({ credentialExpiresAt: "2026-08-01T12:00:00Z" }));
    const rotatedCredential = "r".repeat(43);
    const rotatedSessionId = "session-2";
    const fetch = vi.fn(async (input: string | URL | Request) => {
      const url = String(input);
      if (url.endsWith("/v1/desktop-sessions/rotate")) {
        return json({
          audience: "desktop",
          credential: rotatedCredential,
          credentialExpiresAt: "2026-09-01T00:00:00Z",
          sessionId: rotatedSessionId,
          credentialFamilyId: "family-1",
        });
      }
      if (url.endsWith("/v1/auth/session")) return json(sessionHydration(rotatedSessionId));
      if (url.endsWith(`/v1/tenants/${TENANT_ID}/organizations`)) return json(organizations());
      if (url.endsWith(`/v1/tenants/${TENANT_ID}/entitlements`)) return json(entitlements());
      throw new Error("unexpected request");
    });
    const connection = manager(store, fetch);

    const state = await connection.initialize();

    expect(state.status).toBe("connected");
    expect(store.readConnection()).toEqual(
      expect.objectContaining({ credential: rotatedCredential, sessionId: rotatedSessionId }),
    );
  });

  it("keeps the new credential when hydration is temporarily unavailable after rotation", async () => {
    const { store } = storeFixture();
    store.ensureDeviceIdentity();
    store.saveConnection(connectedSecret({ credentialExpiresAt: "2026-08-01T12:00:00Z" }));
    const rotatedCredential = "n".repeat(43);
    const fetch = vi.fn(async (input: string | URL | Request) => {
      const url = String(input);
      if (url.endsWith("/v1/desktop-sessions/rotate")) {
        return json({
          audience: "desktop",
          credential: rotatedCredential,
          credentialExpiresAt: "2026-09-01T00:00:00Z",
          sessionId: "session-2",
          credentialFamilyId: "family-1",
        });
      }
      throw new Error("temporarily offline");
    });
    const connection = manager(store, fetch);

    const state = await connection.initialize();

    expect(state.status).toBe("error");
    expect(connection.getConnection()?.credential).toBe(rotatedCredential);
    expect(store.readConnection()?.credential).toBe(rotatedCredential);
  });

  it("clears local credentials even when remote disconnect is unavailable", async () => {
    const { store, filePath } = storeFixture();
    store.ensureDeviceIdentity();
    store.saveConnection(connectedSecret());
    const connection = manager(store, async () => {
      throw new Error("offline");
    });
    await connection.initialize();

    const state = await connection.disconnect();

    expect(state.status).toBe("disconnected");
    expect(state.message).toContain("administrator");
    expect(FS.existsSync(filePath)).toBe(false);
  });

  it("removes a saved connection whose Control Plane is no longer allowlisted", async () => {
    const { store, filePath } = storeFixture();
    store.ensureDeviceIdentity();
    store.saveConnection(connectedSecret());
    const fetch = vi.fn(async () => json({}));
    const connection = manager(store, fetch, {
      isControlPlaneBaseUrlAllowed: () => false,
    });

    const state = await connection.initialize();

    expect(state.status).toBe("disconnected");
    expect(state.message).toContain("not allowed");
    expect(fetch).not.toHaveBeenCalled();
    expect(FS.existsSync(filePath)).toBe(false);
  });

  it("refuses to replace an existing account without an explicit disconnect", async () => {
    const { store } = storeFixture();
    store.ensureDeviceIdentity();
    store.saveConnection(connectedSecret());
    const connection = manager(store, async () => json({}));

    await expect(
      connection.connect({ controlPlaneBaseUrl: CONTROL_PLANE, handle: "e".repeat(43) }),
    ).rejects.toEqual(
      expect.objectContaining<Partial<DesktopSaaSConnectionError>>({
        code: "desktop_connection_exists",
      }),
    );
  });

  it("rejects the historical payment-shaped entitlement response", async () => {
    const { store } = storeFixture();
    const fetch = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/v1/desktop-enrollments/redeem")) return json(redeemedSession());
      if (url.endsWith("/v1/auth/session")) return json(sessionHydration());
      if (url.endsWith(`/v1/tenants/${TENANT_ID}/organizations`)) return json(organizations());
      if (url.endsWith(`/v1/tenants/${TENANT_ID}/entitlements`)) {
        return json({
          tenantId: TENANT_ID,
          plan: { code: "enterprise" },
          subscription: { status: "active" },
          entitlements: {},
          features: {},
        });
      }
      if (url.endsWith("/v1/desktop/disconnect")) return new Response(null, { status: 204 });
      throw new Error(`unexpected request ${url} ${String(init?.method ?? "GET")}`);
    });
    const connection = manager(store, fetch);

    const state = await connection.connect({
      controlPlaneBaseUrl: CONTROL_PLANE,
      handle: "e".repeat(43),
    });

    expect(state.status).toBe("error");
    expect(connection.getLastErrorCode()).toBe("desktop_entitlement_hydration_invalid");
    expect(store.readConnection()).toBeNull();
  });
});
