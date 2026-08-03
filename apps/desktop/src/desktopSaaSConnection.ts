// FILE: desktopSaaSConnection.ts
// Purpose: Own the Desktop Enrollment lifecycle without exposing device credentials to a renderer.
// Layer: Desktop main-process security boundary

import * as Crypto from "node:crypto";

import type { DesktopSaaSConnectionState } from "@synara/contracts";

import {
  DesktopCredentialStore,
  DesktopCredentialStoreError,
  type DesktopSaaSConnectionSecret,
} from "./desktopCredentialStore";
import type { DesktopEnrollmentLink } from "./desktopEnrollmentLink";

const REDEMPTION_PROOF_DOMAIN = "synara.desktop-enrollment.redeem.v1";
const ROTATION_PROOF_DOMAIN = "synara.desktop-session.rotate.v1";
const DEFAULT_REQUEST_TIMEOUT_MS = 15_000;
const DEFAULT_ROTATION_WINDOW_MS = 24 * 60 * 60 * 1000;

type DesktopFetch = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;

type RedeemedSession = {
  readonly audience: "desktop";
  readonly credential: string;
  readonly credentialExpiresAt: string;
  readonly sessionId: string;
  readonly credentialFamilyId: string;
  readonly device: { readonly id: string };
  readonly defaultTenantId: string;
  readonly defaultOrganizationId: string | null;
};

type SessionHydration = {
  readonly user: {
    readonly userId: string;
    readonly sessionId: string;
    readonly activeTenantId: string | null;
    readonly audience: "desktop";
    readonly desktopDeviceId: string | null;
    readonly email: string;
    readonly displayName: string;
  };
  readonly tenants: ReadonlyArray<{
    readonly id: string;
    readonly name: string;
    readonly status: string;
  }>;
};

type OrganizationHydration = {
  readonly items: ReadonlyArray<{
    readonly id: string;
    readonly tenantId: string;
    readonly name: string;
    readonly status: string;
  }>;
};

export class DesktopSaaSConnectionError extends Error {
  constructor(
    readonly code: string,
    message: string,
    readonly status: number | null = null,
  ) {
    super(message);
    this.name = "DesktopSaaSConnectionError";
  }
}

export type DesktopSaaSConnectionManagerOptions = {
  readonly store: DesktopCredentialStore;
  readonly fetch?: DesktopFetch;
  readonly platform: string;
  readonly appVersion: string;
  readonly deviceLabel: string;
  readonly requestTimeoutMs?: number;
  readonly rotationWindowMs?: number;
  readonly now?: () => Date;
  readonly onState?: (state: DesktopSaaSConnectionState) => void;
  readonly isControlPlaneBaseUrlAllowed?: (baseUrl: string) => boolean;
};

const disconnectedState = (message: string | null = null): DesktopSaaSConnectionState => ({
  status: "disconnected",
  controlPlaneBaseUrl: null,
  deviceId: null,
  userId: null,
  email: null,
  displayName: null,
  tenantId: null,
  tenantName: null,
  organizationId: null,
  organizationName: null,
  credentialExpiresAt: null,
  message,
});

function stateFromConnection(
  connection: DesktopSaaSConnectionSecret,
  status: DesktopSaaSConnectionState["status"],
  message: string | null = null,
): DesktopSaaSConnectionState {
  return {
    status,
    controlPlaneBaseUrl: connection.controlPlaneBaseUrl,
    deviceId: connection.deviceId,
    userId: connection.userId,
    email: connection.email,
    displayName: connection.displayName,
    tenantId: connection.tenantId,
    tenantName: connection.tenantName,
    organizationId: connection.organizationId,
    organizationName: connection.organizationName,
    credentialExpiresAt: connection.credentialExpiresAt,
    message,
  };
}

function requireRecord(value: unknown, code: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new DesktopSaaSConnectionError(code, "The Control Plane returned an invalid response.");
  }
  return value as Record<string, unknown>;
}

function requireString(record: Record<string, unknown>, key: string, code: string): string {
  const value = record[key];
  if (typeof value !== "string" || value.length < 1 || value.length > 4096) {
    throw new DesktopSaaSConnectionError(code, "The Control Plane returned an invalid response.");
  }
  return value;
}

function optionalString(record: Record<string, unknown>, key: string, code: string): string | null {
  const value = record[key];
  if (value === null) return null;
  return requireString(record, key, code);
}

function parseRedeemedSession(value: unknown): RedeemedSession {
  const code = "desktop_enrollment_response_invalid";
  const record = requireRecord(value, code);
  const device = requireRecord(record.device, code);
  if (record.audience !== "desktop") {
    throw new DesktopSaaSConnectionError(code, "The Control Plane returned an invalid response.");
  }
  return {
    audience: "desktop",
    credential: requireString(record, "credential", code),
    credentialExpiresAt: requireString(record, "credentialExpiresAt", code),
    sessionId: requireString(record, "sessionId", code),
    credentialFamilyId: requireString(record, "credentialFamilyId", code),
    device: { id: requireString(device, "id", code) },
    defaultTenantId: requireString(record, "defaultTenantId", code),
    defaultOrganizationId: optionalString(record, "defaultOrganizationId", code),
  };
}

function parseRotatedSession(
  value: unknown,
): Pick<
  RedeemedSession,
  "audience" | "credential" | "credentialExpiresAt" | "sessionId" | "credentialFamilyId"
> {
  const code = "desktop_rotation_response_invalid";
  const record = requireRecord(value, code);
  if (record.audience !== "desktop") {
    throw new DesktopSaaSConnectionError(code, "The Control Plane returned an invalid response.");
  }
  return {
    audience: "desktop",
    credential: requireString(record, "credential", code),
    credentialExpiresAt: requireString(record, "credentialExpiresAt", code),
    sessionId: requireString(record, "sessionId", code),
    credentialFamilyId: requireString(record, "credentialFamilyId", code),
  };
}

function parseSessionHydration(value: unknown): SessionHydration {
  const code = "desktop_session_hydration_invalid";
  const record = requireRecord(value, code);
  const user = requireRecord(record.user, code);
  if (
    record.authenticated !== true ||
    user.audience !== "desktop" ||
    !Array.isArray(record.tenants)
  ) {
    throw new DesktopSaaSConnectionError(code, "The Desktop session could not be validated.");
  }
  return {
    user: {
      userId: requireString(user, "userId", code),
      sessionId: requireString(user, "sessionId", code),
      activeTenantId: optionalString(user, "activeTenantId", code),
      audience: "desktop",
      desktopDeviceId: optionalString(user, "desktopDeviceId", code),
      email: requireString(user, "email", code),
      displayName: requireString(user, "displayName", code),
    },
    tenants: record.tenants.map((item) => {
      const tenant = requireRecord(item, code);
      return {
        id: requireString(tenant, "id", code),
        name: requireString(tenant, "name", code),
        status: requireString(tenant, "status", code),
      };
    }),
  };
}

function parseOrganizations(value: unknown): OrganizationHydration {
  const code = "desktop_organization_hydration_invalid";
  const record = requireRecord(value, code);
  if (!Array.isArray(record.items)) {
    throw new DesktopSaaSConnectionError(code, "Desktop Organizations could not be validated.");
  }
  return {
    items: record.items.map((item) => {
      const organization = requireRecord(item, code);
      return {
        id: requireString(organization, "id", code),
        tenantId: requireString(organization, "tenantId", code),
        name: requireString(organization, "name", code),
        status: requireString(organization, "status", code),
      };
    }),
  };
}

function assertEntitlements(value: unknown, tenantId: string): void {
  const code = "desktop_entitlement_hydration_invalid";
  const record = requireRecord(value, code);
  // The Desktop client consumes the public internal-self-hosted contract. The
  // historical plan/subscription shape is deliberately not a valid fallback.
  const profile = requireRecord(record.profile, code);
  const profileAssignment = requireRecord(record.profileAssignment, code);
  if (
    record.tenantId !== tenantId ||
    typeof profile.code !== "string" ||
    profile.code.length < 1 ||
    typeof profileAssignment.status !== "string" ||
    profileAssignment.status.length < 1 ||
    !record.entitlements ||
    typeof record.entitlements !== "object" ||
    !record.features ||
    typeof record.features !== "object"
  ) {
    throw new DesktopSaaSConnectionError(
      code,
      "The internal entitlement profile could not be validated.",
    );
  }
}

function proof(privateKeyPkcs8: Buffer, payload: string): string {
  const key = Crypto.createPrivateKey({ key: privateKeyPkcs8, type: "pkcs8", format: "der" });
  return Crypto.sign(null, Buffer.from(payload), key).toString("base64url");
}

function redemptionPayload(
  controlPlaneBaseUrl: string,
  enrollment: string,
  publicKey: string,
  nonce: string,
): string {
  return [REDEMPTION_PROOF_DOMAIN, controlPlaneBaseUrl, enrollment, publicKey, nonce].join("\n");
}

function rotationPayload(connection: DesktopSaaSConnectionSecret, nonce: string): string {
  return [
    ROTATION_PROOF_DOMAIN,
    connection.controlPlaneBaseUrl,
    connection.deviceId,
    connection.sessionId,
    nonce,
  ].join("\n");
}

function apiUrl(controlPlaneBaseUrl: string, path: string): string {
  return `${controlPlaneBaseUrl}${path}`;
}

export class DesktopSaaSConnectionManager {
  private readonly fetch: DesktopFetch;
  private readonly requestTimeoutMs: number;
  private readonly rotationWindowMs: number;
  private readonly now: () => Date;
  private state = disconnectedState();
  private connection: DesktopSaaSConnectionSecret | null = null;
  private operation: Promise<DesktopSaaSConnectionState> | null = null;
  private lastErrorCode: string | null = null;

  constructor(private readonly options: DesktopSaaSConnectionManagerOptions) {
    this.fetch = options.fetch ?? globalThis.fetch;
    this.requestTimeoutMs = options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS;
    this.rotationWindowMs = options.rotationWindowMs ?? DEFAULT_ROTATION_WINDOW_MS;
    this.now = options.now ?? (() => new Date());
  }

  getState(): DesktopSaaSConnectionState {
    return this.state;
  }

  getConnection(): DesktopSaaSConnectionSecret | null {
    return this.connection;
  }

  /**
   * Returns the last main-process error class without exposing it through the
   * renderer connection state. Startup uses this to distinguish an unavailable
   * OS credential store from a normal temporary Control Plane outage.
   */
  getLastErrorCode(): string | null {
    return this.lastErrorCode;
  }

  async initialize(
    options: { readonly requireCredentialStore?: boolean } = {},
  ): Promise<DesktopSaaSConnectionState> {
    return this.runExclusive(async () => {
      this.lastErrorCode = null;
      if (options.requireCredentialStore) {
        try {
          this.options.store.assertAvailable();
        } catch (error) {
          this.recordError(error);
          this.connection = null;
          this.setState({ ...disconnectedState(this.safeMessage(error)), status: "error" });
          return this.state;
        }
      }
      let stored: DesktopSaaSConnectionSecret | null;
      try {
        stored = this.options.store.readConnection();
      } catch (error) {
        this.recordError(error);
        this.connection = null;
        this.setState({ ...disconnectedState(this.safeMessage(error)), status: "error" });
        return this.state;
      }
      if (!stored) {
        this.connection = null;
        this.setState(disconnectedState());
        return this.state;
      }
      if (this.options.isControlPlaneBaseUrlAllowed?.(stored.controlPlaneBaseUrl) === false) {
        this.clearLocalConnection();
        this.setState(
          disconnectedState(
            "The saved Control Plane is not allowed by this Synara build. Connect this device again.",
          ),
        );
        return this.state;
      }

      this.connection = stored;
      this.setState(stateFromConnection(stored, "connecting"));
      try {
        const expiry = Date.parse(stored.credentialExpiresAt);
        if (!Number.isFinite(expiry) || expiry <= this.now().getTime()) {
          throw new DesktopSaaSConnectionError(
            "desktop_credential_expired",
            "The saved Desktop credential has expired. Connect this device again.",
            401,
          );
        }
        const active =
          expiry - this.now().getTime() <= this.rotationWindowMs
            ? await this.rotate(stored)
            : stored;
        const hydrated = await this.hydrate(active, {
          tenantId: active.tenantId,
          organizationId: active.organizationId,
          deviceId: active.deviceId,
          sessionId: active.sessionId,
        });
        this.connection = hydrated;
        this.options.store.saveConnection(hydrated);
        this.setState(stateFromConnection(hydrated, "connected"));
      } catch (error) {
        this.recordError(error);
        if (this.isAuthenticationFailure(error)) {
          this.clearLocalConnection();
          this.lastErrorCode = null;
          this.setState(
            disconnectedState("This Desktop connection is no longer valid. Connect it again."),
          );
        } else if (!this.connection) {
          this.setState({ ...disconnectedState(this.safeMessage(error)), status: "error" });
        } else {
          this.setState(stateFromConnection(this.connection, "error", this.safeMessage(error)));
        }
      }
      return this.state;
    });
  }

  async connect(link: DesktopEnrollmentLink): Promise<DesktopSaaSConnectionState> {
    return this.runExclusive(async () => {
      this.lastErrorCode = null;
      this.setState({ ...disconnectedState(), status: "connecting" });
      let existing: DesktopSaaSConnectionSecret | null;
      try {
        existing = this.options.store.readConnection();
      } catch (error) {
        this.recordError(error);
        this.setState({ ...disconnectedState(this.safeMessage(error)), status: "error" });
        throw error;
      }
      if (this.connection || existing) {
        throw new DesktopSaaSConnectionError(
          "desktop_connection_exists",
          "Disconnect the current Cloud Panel connection before connecting another one.",
        );
      }
      let redeemed: RedeemedSession | null = null;
      try {
        const identity = this.options.store.ensureDeviceIdentity();
        const nonce = Crypto.randomBytes(32).toString("base64url");
        redeemed = parseRedeemedSession(
          await this.requestJSON(
            apiUrl(link.controlPlaneBaseUrl, "/v1/desktop-enrollments/redeem"),
            {
              method: "POST",
              headers: { "Content-Type": "application/json", Accept: "application/json" },
              body: JSON.stringify({
                version: 1,
                controlPlaneOrigin: link.controlPlaneBaseUrl,
                enrollment: link.handle,
                devicePublicKey: identity.publicKey,
                nonce,
                proof: proof(
                  identity.privateKeyPkcs8,
                  redemptionPayload(
                    link.controlPlaneBaseUrl,
                    link.handle,
                    identity.publicKey,
                    nonce,
                  ),
                ),
                platform: this.options.platform,
                appVersion: this.options.appVersion,
                deviceLabel: this.options.deviceLabel,
              }),
            },
          ),
        );
        const hydrated = await this.hydrate(
          {
            controlPlaneBaseUrl: link.controlPlaneBaseUrl,
            credential: redeemed.credential,
            credentialExpiresAt: redeemed.credentialExpiresAt,
            sessionId: redeemed.sessionId,
            credentialFamilyId: redeemed.credentialFamilyId,
            deviceId: redeemed.device.id,
            userId: "pending",
            email: "pending",
            displayName: "pending",
            tenantId: redeemed.defaultTenantId,
            tenantName: "pending",
            organizationId: redeemed.defaultOrganizationId,
            organizationName: null,
          },
          {
            tenantId: redeemed.defaultTenantId,
            organizationId: redeemed.defaultOrganizationId,
            deviceId: redeemed.device.id,
            sessionId: redeemed.sessionId,
          },
        );
        this.options.store.saveConnection(hydrated);
        this.connection = hydrated;
        this.setState(stateFromConnection(hydrated, "connected"));
      } catch (error) {
        this.recordError(error);
        if (redeemed) {
          await this.bestEffortDisconnect(link.controlPlaneBaseUrl, redeemed.credential);
        }
        this.clearLocalConnection();
        this.setState({ ...disconnectedState(this.safeMessage(error)), status: "error" });
      }
      return this.state;
    });
  }

  async disconnect(): Promise<DesktopSaaSConnectionState> {
    return this.runExclusive(async () => {
      this.lastErrorCode = null;
      let current: DesktopSaaSConnectionSecret | null;
      try {
        current = this.connection ?? this.options.store.readConnection();
      } catch (error) {
        this.recordError(error);
        this.connection = null;
        this.setState({ ...disconnectedState(this.safeMessage(error)), status: "error" });
        return this.state;
      }
      if (!current) {
        this.clearLocalConnection();
        this.setState(disconnectedState());
        return this.state;
      }
      this.setState(stateFromConnection(current, "disconnecting"));
      let warning: string | null = null;
      try {
        await this.requestJSON(apiUrl(current.controlPlaneBaseUrl, "/v1/desktop/disconnect"), {
          method: "POST",
          headers: { Authorization: `Bearer ${current.credential}` },
        });
      } catch {
        warning =
          "Local Desktop credentials were removed. The remote device could not be revoked; ask an administrator to revoke it.";
      }
      this.clearLocalConnection();
      this.setState(disconnectedState(warning));
      return this.state;
    });
  }

  private async rotate(
    connection: DesktopSaaSConnectionSecret,
  ): Promise<DesktopSaaSConnectionSecret> {
    const identity = this.options.store.ensureDeviceIdentity();
    const nonce = Crypto.randomBytes(32).toString("base64url");
    const rotated = parseRotatedSession(
      await this.requestJSON(
        apiUrl(connection.controlPlaneBaseUrl, "/v1/desktop-sessions/rotate"),
        {
          method: "POST",
          headers: {
            Authorization: `Bearer ${connection.credential}`,
            "Content-Type": "application/json",
            Accept: "application/json",
          },
          body: JSON.stringify({
            nonce,
            proof: proof(identity.privateKeyPkcs8, rotationPayload(connection, nonce)),
          }),
        },
      ),
    );
    const next = {
      ...connection,
      credential: rotated.credential,
      credentialExpiresAt: rotated.credentialExpiresAt,
      sessionId: rotated.sessionId,
      credentialFamilyId: rotated.credentialFamilyId,
    };
    try {
      this.options.store.saveConnection(next);
    } catch (error) {
      await this.bestEffortDisconnect(next.controlPlaneBaseUrl, next.credential);
      this.clearLocalConnection();
      throw error;
    }
    this.connection = next;
    return next;
  }

  private async hydrate(
    connection: DesktopSaaSConnectionSecret,
    expected: {
      readonly tenantId: string;
      readonly organizationId: string | null;
      readonly deviceId: string;
      readonly sessionId: string;
    },
  ): Promise<DesktopSaaSConnectionSecret> {
    const authorization = {
      Authorization: `Bearer ${connection.credential}`,
      Accept: "application/json",
    };
    const session = parseSessionHydration(
      await this.requestJSON(apiUrl(connection.controlPlaneBaseUrl, "/v1/auth/session"), {
        headers: authorization,
      }),
    );
    if (
      session.user.activeTenantId !== expected.tenantId ||
      session.user.desktopDeviceId !== expected.deviceId ||
      session.user.sessionId !== expected.sessionId
    ) {
      throw new DesktopSaaSConnectionError(
        "desktop_session_binding_mismatch",
        "The Desktop session did not match the issued Tenant and device.",
      );
    }
    const tenant = session.tenants.find((candidate) => candidate.id === expected.tenantId);
    if (!tenant || tenant.status !== "active") {
      throw new DesktopSaaSConnectionError(
        "desktop_tenant_unavailable",
        "The issued Tenant is unavailable to this Desktop session.",
      );
    }
    const organizations = parseOrganizations(
      await this.requestJSON(
        apiUrl(
          connection.controlPlaneBaseUrl,
          `/v1/tenants/${encodeURIComponent(expected.tenantId)}/organizations`,
        ),
        { headers: authorization },
      ),
    );
    const organization = expected.organizationId
      ? organizations.items.find(
          (candidate) =>
            candidate.id === expected.organizationId &&
            candidate.tenantId === expected.tenantId &&
            candidate.status === "active",
        )
      : null;
    if (expected.organizationId && !organization) {
      throw new DesktopSaaSConnectionError(
        "desktop_organization_unavailable",
        "The issued Organization is unavailable to this Desktop session.",
      );
    }
    const entitlementSnapshot = await this.requestJSON(
      apiUrl(
        connection.controlPlaneBaseUrl,
        `/v1/tenants/${encodeURIComponent(expected.tenantId)}/entitlements`,
      ),
      { headers: authorization },
    );
    assertEntitlements(entitlementSnapshot, expected.tenantId);
    return {
      ...connection,
      userId: session.user.userId,
      email: session.user.email,
      displayName: session.user.displayName,
      tenantId: tenant.id,
      tenantName: tenant.name,
      organizationId: organization?.id ?? null,
      organizationName: organization?.name ?? null,
    };
  }

  private async requestJSON(url: string, init: RequestInit): Promise<unknown> {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), this.requestTimeoutMs);
    try {
      const response = await this.fetch(url, {
        ...init,
        redirect: "manual",
        cache: "no-store",
        signal: controller.signal,
      });
      if (response.status >= 300 && response.status < 400) {
        throw new DesktopSaaSConnectionError(
          "desktop_control_plane_redirect_rejected",
          "The Control Plane returned an unexpected redirect.",
          response.status,
        );
      }
      if (!response.ok) {
        throw new DesktopSaaSConnectionError(
          response.status === 401
            ? "invalid_desktop_session"
            : "desktop_control_plane_request_failed",
          response.status === 401
            ? "The Desktop session is invalid or expired."
            : "The Control Plane request failed.",
          response.status,
        );
      }
      if (response.status === 204) return null;
      try {
        return await response.json();
      } catch {
        throw new DesktopSaaSConnectionError(
          "desktop_control_plane_response_invalid",
          "The Control Plane returned an invalid response.",
          response.status,
        );
      }
    } catch (error) {
      if (error instanceof DesktopSaaSConnectionError) throw error;
      if (controller.signal.aborted) {
        throw new DesktopSaaSConnectionError(
          "desktop_control_plane_timeout",
          "The Control Plane did not respond in time.",
        );
      }
      throw new DesktopSaaSConnectionError(
        "desktop_control_plane_unreachable",
        "The Control Plane could not be reached.",
      );
    } finally {
      clearTimeout(timeout);
    }
  }

  private async bestEffortDisconnect(
    controlPlaneBaseUrl: string,
    credential: string,
  ): Promise<void> {
    try {
      await this.requestJSON(apiUrl(controlPlaneBaseUrl, "/v1/desktop/disconnect"), {
        method: "POST",
        headers: { Authorization: `Bearer ${credential}` },
      });
    } catch {
      // Local rollback must continue even if the remote device cannot be reached.
    }
  }

  private clearLocalConnection(): void {
    this.connection = null;
    try {
      this.options.store.clearAll();
    } catch {
      // The state remains disconnected/error; no secret is exposed to the renderer.
    }
  }

  private isAuthenticationFailure(error: unknown): boolean {
    return (
      error instanceof DesktopSaaSConnectionError &&
      (error.status === 401 || error.code === "desktop_credential_expired")
    );
  }

  private safeMessage(error: unknown): string {
    if (
      error instanceof DesktopSaaSConnectionError ||
      error instanceof DesktopCredentialStoreError
    ) {
      return error.message;
    }
    return "The Desktop connection could not be completed safely.";
  }

  private recordError(error: unknown): void {
    if (
      error instanceof DesktopSaaSConnectionError ||
      error instanceof DesktopCredentialStoreError
    ) {
      this.lastErrorCode = error.code;
      return;
    }
    this.lastErrorCode = "unexpected";
  }

  private setState(state: DesktopSaaSConnectionState): void {
    this.state = state;
    this.options.onState?.(state);
  }

  private runExclusive(
    operation: () => Promise<DesktopSaaSConnectionState>,
  ): Promise<DesktopSaaSConnectionState> {
    if (this.operation) {
      return Promise.reject(
        new DesktopSaaSConnectionError(
          "desktop_connection_busy",
          "Another Desktop connection operation is already in progress.",
        ),
      );
    }
    const running = operation().finally(() => {
      if (this.operation === running) this.operation = null;
    });
    this.operation = running;
    return running;
  }
}
