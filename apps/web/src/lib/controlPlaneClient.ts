import type {
  ProviderCapabilityMap,
  ProviderCapabilityProjection,
  ProviderHostProviderKind,
  ProviderInteractionMode,
  ProviderKind,
  ProviderReleasePolicy,
  ProviderRuntimeDescriptor,
  ProviderSupportTier,
  ProviderUserInputAnswers,
  RuntimeMode,
} from "@synara/contracts";

import { resolveWsHttpUrl } from "./wsHttpUrl";

export function resolveControlPlaneHttpUrl(path: string): string {
  if (
    typeof window !== "undefined" &&
    (window.location.protocol === "http:" || window.location.protocol === "https:")
  ) {
    return new URL(path, window.location.origin).toString();
  }
  return resolveWsHttpUrl(path);
}

export class ControlPlaneError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly requestId?: string,
    readonly details?: Readonly<Record<string, unknown>>,
  ) {
    super(message);
    this.name = "ControlPlaneError";
  }
}

export type ControlPlaneTenantAccess = {
  id: string;
  slug: string;
  name: string;
  status: "active" | "suspended";
  planCode: string;
  region: string;
  role: "owner" | "admin" | "security_admin" | "billing_admin" | "auditor" | "member";
};

export type ControlPlanePlatformProfile = {
  profile: "personal" | "single-node" | "enterprise";
  metadataStore: "sqlite" | "postgresql";
  artifactStore: "local" | "minio" | "s3";
  queueDriver: "in-process" | "postgres-outbox" | "external";
  controlPlaneReplicas: number;
  highAvailability: boolean;
  leaseEnabled: boolean;
  fencingEnabled: boolean;
  executionTargetKinds: ReadonlyArray<ControlPlaneExecutionTargetKind>;
  artifactPayloadMigration: boolean;
  metadataExportImport: boolean;
};

export type ControlPlaneSessionState = {
  authenticated: true;
  user: {
    userId: string;
    sessionId: string;
    activeTenantId: string | null;
    email: string;
    displayName: string;
  };
  tenants: ReadonlyArray<ControlPlaneTenantAccess>;
};

export type ControlPlaneOrganization = {
  id: string;
  tenantId: string;
  parentOrganizationId: string | null;
  slug: string;
  name: string;
  kind: "root" | "team" | "department" | "personal";
  status: "active" | "suspended";
  currentUserRole: "owner" | "admin" | "agent_operator" | "member" | "viewer" | null;
  settings: Record<string, unknown>;
  createdAt: string;
  updatedAt: string;
  archivedAt: string | null;
};

export type ControlPlaneTenantMember = {
  tenantId: string;
  userId: string;
  email: string;
  displayName: string;
  role: string;
  status: string;
  joinedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneTenantQuota = {
  tenantId: string;
  maxConcurrentExecutions: number | null;
  maxArtifactBytes: number | null;
};

export type ControlPlaneRetentionPolicy = {
  tenantId: string;
  sessionArchiveAfterDays: number | null;
  artifactDeleteAfterDays: number | null;
  updatedBy: string;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneIdentityConnection = {
  id: string;
  tenantId: string;
  kind: "oidc" | "saml";
  name: string;
  status: "active" | "disabled";
  issuer: string;
  clientId: string | null;
  configuration: {
    scopes?: ReadonlyArray<string>;
    allowedDomains?: ReadonlyArray<string>;
    groupsClaim?: string;
    defaultTenantRole?: string;
    metadataUrl?: string;
    entityId?: string;
    emailAttribute?: string;
    displayNameAttribute?: string;
    groupsAttribute?: string;
  };
  createdAt: string;
  updatedAt: string;
};

export type ControlPlanePublicIdentityConnection = Pick<
  ControlPlaneIdentityConnection,
  "id" | "tenantId" | "kind" | "name"
>;

export type ControlPlaneIdentityGroupMapping = {
  id: string;
  externalGroup: string;
  tenantRole: string | null;
  organizationId: string | null;
  organizationRole: string | null;
};

export type ControlPlaneServiceAccount = {
  id: string;
  tenantId: string;
  organizationId: string | null;
  name: string;
  description: string;
  status: "active" | "revoked";
  scopes: ReadonlyArray<string>;
  createdAt: string;
  updatedAt: string;
  revokedAt: string | null;
};

export type ControlPlaneIssuedServiceAccount = {
  account: ControlPlaneServiceAccount;
  token: string;
};

export type ControlPlaneCredentialPurpose = "provider" | "git" | "registry" | "package";
export type ControlPlaneCredentialScope = "user" | "organization" | "tenant" | "platform";

export type ControlPlaneCredential = {
  id: string;
  tenantId: string;
  organizationId: string | null;
  scope: ControlPlaneCredentialScope;
  scopeUserId: string | null;
  selectorOrganizationId: string | null;
  selectorModel: string | null;
  autoSelectEnabled: boolean;
  name: string;
  purpose: ControlPlaneCredentialPurpose;
  provider: string;
  credentialType: string;
  kmsProvider: string;
  kmsKeyId: string;
  version: number;
  createdBy: string;
  updatedBy: string;
  createdAt: string;
  updatedAt: string;
  expiresAt: string | null;
  revokedAt: string | null;
};

export type ControlPlaneProviderCredentialScopePolicy = {
  tenantId: string;
  platformCredentialsEnabled: boolean;
  platformCredentialAutoSelect: boolean;
  updatedBy: string | null;
  createdAt: string | null;
  updatedAt: string | null;
};

export type ControlPlaneCredentialBindingKind =
  | "git_fetch"
  | "git_push"
  | "registry_pull"
  | "registry_push"
  | "package_read"
  | "package_publish"
  | "worker_image_pull";

export type ControlPlaneCredentialBinding = {
  id: string;
  tenantId: string;
  organizationId: string | null;
  projectId: string | null;
  executionTargetId: string | null;
  credentialId: string;
  bindingKind: ControlPlaneCredentialBindingKind;
  selector: string;
  createdBy: string;
  createdAt: string;
  disabledAt: string | null;
  disabledBy: string | null;
};

export type ControlPlaneAuditLogEntry = {
  eventId: string;
  tenantId: string;
  actorType: "user" | "service_account" | "worker" | "system";
  actorId: string | null;
  action: string;
  resourceType: string;
  resourceId: string | null;
  organizationId: string | null;
  requestId: string;
  metadata: Record<string, unknown>;
  occurredAt: string;
};

export type ControlPlaneAuditLogFilters = {
  action?: string;
  actorType?: ControlPlaneAuditLogEntry["actorType"] | "";
  resourceType?: string;
  organizationId?: string;
  occurredAfter?: string;
  occurredBefore?: string;
};

export type ControlPlaneAuditLogPage = {
  items: ReadonlyArray<ControlPlaneAuditLogEntry>;
  nextCursor: string | null;
};

export type ControlPlaneProject = {
  id: string;
  tenantId: string;
  organizationId: string;
  name: string;
  repositoryUrl: string | null;
  defaultBranch: string;
  gitCredentialId: string | null;
  visibility: "private" | "organization" | "tenant";
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  archivedAt: string | null;
};

export type ControlPlaneAgentSession = {
  id: string;
  tenantId: string;
  organizationId: string;
  projectId: string;
  createdBy: string;
  title: string;
  status: "active" | "suspended" | "archived";
  visibility: "private" | "project" | "organization";
  provider: ProviderKind;
  model: string | null;
  providerCredentialId: string | null;
  executionTargetId: string;
  lastEventSequence: number;
  createdAt: string;
  updatedAt: string;
  archivedAt: string | null;
};

export type ControlPlaneExecutionTargetKind = "local" | "ssh" | "docker" | "kubernetes";

export type ControlPlaneExecutionTarget = {
  id: string;
  tenantId: string | null;
  organizationId: string | null;
  kind: ControlPlaneExecutionTargetKind;
  name: string;
  status: "active" | "disabled" | "offline";
  capabilities: Record<string, unknown>;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneProviderCompatibilityStatus =
  | "compatible"
  | "incompatible"
  | "unavailable"
  | "local-only"
  | "disabled";

export type ControlPlaneWorkerProviderManifest = {
  provider: ProviderHostProviderKind;
  supportTier: ProviderSupportTier;
  compatibilityStatus: ControlPlaneProviderCompatibilityStatus;
  runtime: ProviderRuntimeDescriptor;
  releasePolicy: ProviderReleasePolicy;
  incompatibilityCode?: string;
  incompatibilityMessage?: string;
  capabilities: ProviderCapabilityMap;
};

export type ControlPlaneWorkerManifest = {
  executionTargetId: string;
  manifestId: string;
  workerStatusCounts: {
    online: number;
    draining: number;
    offline: number;
  };
  lastHeartbeatAt: string;
  workerBuild: {
    version: string;
    gitSha?: string;
    imageDigest?: string;
    operatingSystem: string;
    architecture: string;
  };
  workerProtocol: {
    minimum: number;
    maximum: number;
  };
  runtimeEvent: {
    minimum: number;
    maximum: number;
  };
  providers: ReadonlyArray<ControlPlaneWorkerProviderManifest>;
};

export type ControlPlaneWorkerReleaseRevision = {
  id: string;
  tenantId: string;
  executionTargetId: string;
  revision: number;
  workerManifestId: string;
  workerBuildVersion: string;
  workerBuildGitSha?: string;
  imageDigest?: string;
  description: string;
  createdBy: string;
  createdAt: string;
};

export type ControlPlaneWorkerReleasePolicy = {
  tenantId: string;
  executionTargetId: string;
  policyVersion: number;
  promotedRevisionId: string;
  canaryRevisionId?: string;
  canaryPercent: number;
  updatedBy: string;
  updatedAt: string;
};

export type ControlPlaneWorkerReleaseTransition = {
  id: string;
  tenantId: string;
  executionTargetId: string;
  policyVersion: number;
  action: "promote" | "canary" | "rollback" | "abort-canary";
  fromPromotedRevisionId?: string;
  fromCanaryRevisionId?: string;
  toPromotedRevisionId: string;
  toCanaryRevisionId?: string;
  canaryPercent: number;
  reason: string;
  actorId: string;
  requestId?: string;
  occurredAt: string;
};

export type ControlPlaneWorkerReleaseOverview = {
  policy: ControlPlaneWorkerReleasePolicy | null;
  revisions: ReadonlyArray<ControlPlaneWorkerReleaseRevision>;
  transitions: ReadonlyArray<ControlPlaneWorkerReleaseTransition>;
};

export type ControlPlaneSSHProvisionResult = {
  targetId: string;
  operation: "install" | "upgrade" | "revoke";
  status: "active" | "offline" | "disabled";
  serviceName: string;
  binarySha256?: string;
};

export type ControlPlaneArtifactKind =
  | "attachment"
  | "generated_file"
  | "terminal_log"
  | "workspace_snapshot"
  | "checkpoint";

export type ControlPlaneArtifact = {
  id: string;
  tenantId: string;
  organizationId: string;
  projectId: string;
  sessionId: string;
  executionId: string | null;
  kind: ControlPlaneArtifactKind;
  status: "pending" | "ready" | "deleting" | "deleted" | "failed";
  originalName: string | null;
  contentType: string | null;
  sizeBytes: number | null;
  sha256: string | null;
  createdByType: "user" | "service_account" | "worker" | "system";
  createdById: string;
  readyAt: string | null;
  createdAt: string;
  expiresAt: string | null;
  deletedAt: string | null;
};

export type ControlPlaneArtifactUploadGrant = {
  artifact: ControlPlaneArtifact;
  method: "PUT";
  url: string;
  headers: Readonly<Record<string, string>>;
  expiresAt: string;
};

export type ControlPlaneArtifactDownloadGrant = {
  artifact: ControlPlaneArtifact;
  url: string;
  expiresAt: string;
};

export type ControlPlaneAgentTurn = {
  id: string;
  tenantId: string;
  sessionId: string;
  createdBy: string;
  status: "queued" | "running" | "completed" | "failed" | "cancelled" | "interrupted";
  inputText: string;
  turnKind?: "message" | "compact" | "review" | "rollback" | "fork";
  runtimeMode: RuntimeMode;
  interactionMode: ProviderInteractionMode;
  startedAt: string | null;
  completedAt: string | null;
  createdAt: string;
};

export type ControlPlaneReviewTarget =
  | { type: "uncommittedChanges" }
  | { type: "baseBranch"; branch?: string };

export type ControlPlaneControlCommand = {
  id: string;
  executionId: string;
  sessionId: string;
  turnId: string;
  provider: string;
  commandType: string;
  commandId: string;
  payload: Record<string, unknown>;
  status: "pending" | "delivered" | "acknowledged" | "superseded" | "outcome_unknown";
  requestedBy: string;
  requestedAt: string;
  deliveryWorkerId?: string;
  deliveryGeneration?: number;
  deliveryAttempts: number;
  deliveryAvailableAt: string;
  deliveredAt?: string;
  acknowledgedAt?: string;
  deliveryError?: string;
};

export type ControlPlaneAdvancedCommandResult = {
  type: "compact" | "review";
  turn: ControlPlaneAgentTurn;
  executionId: string;
  controlCommand: ControlPlaneControlCommand;
};

export type ControlPlaneRollbackResult = {
  sessionId: string;
  eventId: string;
  eventSequence: number;
  fromSessionId: string;
  fromTurnId: string;
  fromSequence: number;
  removedTurnCount: number;
  supportMode: "emulated";
  workspaceDisposition: "unchanged";
  externalSideEffectsReverted: false;
};

export type ControlPlaneForkResult = {
  session: ControlPlaneAgentSession;
  sourceSessionId: string;
  sourceEventSequence: number;
  supportMode: "emulated";
};

export type ControlPlanePendingInteraction = {
  id: string;
  executionId: string;
  turnId: string;
  provider: string;
  requestId: string;
  kind: "approval" | "user-input";
  payload: Record<string, unknown>;
  requestedAt: string;
  expiresAt: string;
};

export type ControlPlanePendingInteractionSnapshot = {
  items: ReadonlyArray<ControlPlanePendingInteraction>;
  snapshotSequence: number;
};

export type ControlPlaneInteractionResolution = {
  id: string;
  executionId: string;
  sessionId: string;
  requestId: string;
  kind: "approval" | "user-input";
  status: "resolved" | "expired";
};

export type ControlPlaneSessionEvent = {
  eventId: string;
  eventVersion: number;
  tenantId: string;
  organizationId: string;
  projectId: string;
  sessionId: string;
  executionId: string | null;
  workerId: string | null;
  generation: number | null;
  sequence: number;
  eventType: string;
  actorType: "user" | "service_account" | "worker" | "system";
  actorId: string | null;
  payload: Record<string, unknown>;
  occurredAt: string;
};

export type ControlPlaneSessionEventPage = {
  items: ReadonlyArray<ControlPlaneSessionEvent>;
  lastSequence: number;
};

export type ControlPlaneIdempotencyOptions = {
  idempotencyKey: string;
};

export type TenantInvitation = {
  id: string;
  tenantId: string;
  email: string;
  role: string;
  token?: string;
  expiresAt: string;
  createdAt: string;
};

type ErrorEnvelope = {
  error?: {
    code?: string;
    message?: string;
    requestId?: string;
    details?: Record<string, unknown> | null;
  };
};

function normalizeSessionEventSequence(sequence: number): number {
  return Number.isSafeInteger(sequence) && sequence > 0 ? sequence : 0;
}

export function resolveSessionEventStreamUrl(sessionId: string, afterSequence: number): string {
  const query = new URLSearchParams({
    afterSequence: String(normalizeSessionEventSequence(afterSequence)),
  });
  return resolveControlPlaneHttpUrl(
    `/v1/sessions/${encodeURIComponent(sessionId)}/events/stream?${query.toString()}`,
  );
}

function subscribeSessionEvents(
  sessionId: string,
  afterSequence: number,
  handlers: {
    onEvent: (event: ControlPlaneSessionEvent) => void;
    onOpen?: () => void;
    onError?: () => void;
  },
): () => void {
  const reconnectDelayMs = 2_000;
  let closed = false;
  let source: EventSource | null = null;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  let lastSequence = normalizeSessionEventSequence(afterSequence);

  const scheduleReconnect = () => {
    if (closed || reconnectTimer !== null) return;
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null;
      connect();
    }, reconnectDelayMs);
  };

  const fail = (failedSource: EventSource) => {
    if (closed || source !== failedSource) return;
    failedSource.close();
    source = null;
    try {
      handlers.onError?.();
    } finally {
      scheduleReconnect();
    }
  };

  const connect = () => {
    if (closed || source !== null) return;
    const nextSource = new EventSource(resolveSessionEventStreamUrl(sessionId, lastSequence), {
      withCredentials: true,
    });
    source = nextSource;
    nextSource.onopen = () => {
      if (!closed && source === nextSource) handlers.onOpen?.();
    };
    nextSource.onerror = () => fail(nextSource);
    nextSource.addEventListener("session-event", (message) => {
      if (closed || source !== nextSource) return;
      try {
        const event = JSON.parse(message.data) as ControlPlaneSessionEvent;
        if (!Number.isSafeInteger(event.sequence) || event.sequence < 1) {
          throw new Error("Session Event sequence must be a positive safe integer.");
        }
        if (event.sequence <= lastSequence) return;
        if (event.sequence !== lastSequence + 1) {
          fail(nextSource);
          return;
        }
        handlers.onEvent(event);
        lastSequence = event.sequence;
      } catch {
        fail(nextSource);
      }
    });
  };

  connect();
  return () => {
    closed = true;
    if (reconnectTimer !== null) clearTimeout(reconnectTimer);
    reconnectTimer = null;
    source?.close();
    source = null;
  };
}

function resolveArtifactGrantUrl(url: string): string {
  return /^https?:\/\//i.test(url) ? url : resolveControlPlaneHttpUrl(url);
}

function auditLogSearchParams(
  filters: ControlPlaneAuditLogFilters,
  page?: { limit?: number; cursor?: string },
): URLSearchParams {
  const query = new URLSearchParams();
  for (const [name, value] of Object.entries(filters)) {
    if (typeof value === "string" && value.trim() !== "") query.set(name, value.trim());
  }
  if (page?.limit !== undefined) query.set("limit", String(page.limit));
  if (page?.cursor) query.set("cursor", page.cursor);
  return query;
}

export function resolveAuditLogExportUrl(
  tenantId: string,
  format: "jsonl" | "csv",
  filters: ControlPlaneAuditLogFilters = {},
): string {
  const query = auditLogSearchParams(filters);
  query.set("format", format);
  return resolveControlPlaneHttpUrl(
    `/v1/tenants/${encodeURIComponent(tenantId)}/audit-logs/export?${query.toString()}`,
  );
}

async function uploadArtifactPayload(
  grant: ControlPlaneArtifactUploadGrant,
  payload: Blob | ArrayBuffer | ArrayBufferView,
  contentType: string,
): Promise<void> {
  const headers = new Headers(grant.headers);
  headers.set("Content-Type", contentType);
  const response = await fetch(resolveArtifactGrantUrl(grant.url), {
    method: grant.method,
    headers,
    body: payload as BodyInit,
  });
  if (!response.ok) {
    throw new ControlPlaneError(
      response.status,
      "artifact_upload_failed",
      `Artifact upload failed (${response.status}).`,
    );
  }
}

async function controlPlaneRequest<T>(
  path: string,
  init: Omit<RequestInit, "body"> & { body?: unknown } = {},
): Promise<T> {
  const { body: inputBody, ...requestInit } = init;
  const headers = new Headers(requestInit.headers);
  let body: BodyInit | undefined;
  if (inputBody !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(inputBody);
  }
  const response = await fetch(resolveControlPlaneHttpUrl(path), {
    ...requestInit,
    headers,
    ...(body === undefined ? {} : { body }),
    credentials: "include",
  });
  if (!response.ok) {
    const payload = (await response.json().catch(() => null)) as ErrorEnvelope | null;
    throw new ControlPlaneError(
      response.status,
      payload?.error?.code ?? "control_plane_request_failed",
      payload?.error?.message ?? `Control-plane request failed (${response.status}).`,
      payload?.error?.requestId,
      payload?.error?.details ?? undefined,
    );
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

function idempotencyRequestHeaders(
  options?: ControlPlaneIdempotencyOptions,
): { headers: HeadersInit } | Record<never, never> {
  return options ? { headers: { "Idempotency-Key": options.idempotencyKey } } : {};
}

export const controlPlaneClient = {
  getPlatformProfile: () =>
    controlPlaneRequest<ControlPlanePlatformProfile>("/v1/platform/profile"),
  getSession: () => controlPlaneRequest<ControlPlaneSessionState>("/v1/auth/session"),
  devLogin: (input: { email: string; displayName: string }) =>
    controlPlaneRequest<ControlPlaneSessionState>("/v1/auth/dev-login", {
      method: "POST",
      body: input,
    }),
  listPublicIdentityConnections: (tenantSlug: string) => {
    const query = new URLSearchParams({ tenantSlug });
    return controlPlaneRequest<{ items: ReadonlyArray<ControlPlanePublicIdentityConnection> }>(
      `/v1/auth/sso/connections?${query.toString()}`,
    );
  },
  startSSO: (connectionId: string, returnTo = "/settings?section=tenancy") => {
    const query = new URLSearchParams({ returnTo });
    return controlPlaneRequest<{ authorizationUrl: string }>(
      `/v1/auth/sso/${encodeURIComponent(connectionId)}/start?${query.toString()}`,
    );
  },
  logout: () => controlPlaneRequest<void>("/v1/auth/logout", { method: "POST" }),
  setActiveTenant: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneSessionState>("/v1/auth/active-tenant", {
      method: "PUT",
      body: { tenantId },
    }),
  getTenantQuota: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneTenantQuota>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/quota`,
    ),
  updateTenantQuota: (
    tenantId: string,
    input: Pick<ControlPlaneTenantQuota, "maxConcurrentExecutions" | "maxArtifactBytes">,
  ) =>
    controlPlaneRequest<ControlPlaneTenantQuota>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/quota`,
      { method: "PUT", body: input },
    ),
  getRetentionPolicy: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneRetentionPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/retention-policy`,
    ),
  updateRetentionPolicy: (
    tenantId: string,
    input: Pick<ControlPlaneRetentionPolicy, "sessionArchiveAfterDays" | "artifactDeleteAfterDays">,
  ) =>
    controlPlaneRequest<ControlPlaneRetentionPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/retention-policy`,
      { method: "PUT", body: input },
    ),
  listIdentityConnections: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneIdentityConnection> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections`,
    ),
  createIdentityConnection: (
    tenantId: string,
    input: {
      kind: "oidc" | "saml";
      name: string;
      issuer: string;
      clientId?: string;
      clientSecret?: string;
      oidc?: {
        scopes?: ReadonlyArray<string>;
        allowedDomains?: ReadonlyArray<string>;
        groupsClaim?: string;
        defaultTenantRole?: string;
      };
      saml?: {
        metadataUrl?: string;
        entityId?: string;
        emailAttribute?: string;
        displayNameAttribute?: string;
        groupsAttribute?: string;
        allowedDomains?: ReadonlyArray<string>;
        defaultTenantRole?: string;
      };
    },
  ) =>
    controlPlaneRequest<ControlPlaneIdentityConnection>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections`,
      { method: "POST", body: input },
    ),
  disableIdentityConnection: (tenantId: string, connectionId: string) =>
    controlPlaneRequest<void>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections/${encodeURIComponent(connectionId)}/disable`,
      { method: "POST" },
    ),
  listIdentityGroupMappings: (tenantId: string, connectionId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneIdentityGroupMapping> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections/${encodeURIComponent(connectionId)}/group-mappings`,
    ),
  replaceIdentityGroupMappings: (
    tenantId: string,
    connectionId: string,
    items: ReadonlyArray<Omit<ControlPlaneIdentityGroupMapping, "id">>,
  ) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneIdentityGroupMapping> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections/${encodeURIComponent(connectionId)}/group-mappings`,
      { method: "PUT", body: { items } },
    ),
  listServiceAccounts: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneServiceAccount> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/service-accounts`,
    ),
  createServiceAccount: (
    tenantId: string,
    input: {
      organizationId?: string;
      name: string;
      description: string;
      scopes: ReadonlyArray<string>;
      expiresAt?: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneIssuedServiceAccount>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/service-accounts`,
      { method: "POST", body: input },
    ),
  rotateServiceAccountToken: (tenantId: string, serviceAccountId: string) =>
    controlPlaneRequest<{ token: string; expiresAt: string | null }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/service-accounts/${encodeURIComponent(serviceAccountId)}/rotate-token`,
      { method: "POST", body: { expiresAt: null } },
    ),
  revokeServiceAccount: (tenantId: string, serviceAccountId: string) =>
    controlPlaneRequest<void>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/service-accounts/${encodeURIComponent(serviceAccountId)}/revoke`,
      { method: "POST" },
    ),
  listCredentials: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneCredential> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials`,
    ),
  createCredential: (
    tenantId: string,
    input: {
      organizationId?: string;
      scope?: ControlPlaneCredentialScope;
      scopeUserId?: string;
      selectorOrganizationId?: string;
      selectorModel?: string;
      autoSelectEnabled?: boolean;
      name: string;
      purpose: ControlPlaneCredentialPurpose;
      provider: string;
      credentialType: string;
      payload: Record<string, unknown>;
      expiresAt?: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneCredential>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials`,
      { method: "POST", body: input },
    ),
  rotateCredential: (
    tenantId: string,
    credentialId: string,
    input: {
      expectedVersion: number;
      payload: Record<string, unknown>;
      expiresAt: string | null;
    },
  ) =>
    controlPlaneRequest<ControlPlaneCredential>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials/${encodeURIComponent(credentialId)}/rotate`,
      { method: "POST", body: input },
    ),
  revokeCredential: (tenantId: string, credentialId: string) =>
    controlPlaneRequest<void>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials/${encodeURIComponent(credentialId)}/revoke`,
      { method: "POST" },
    ),
  setCredentialAutoSelect: (tenantId: string, credentialId: string, enabled: boolean) =>
    controlPlaneRequest<ControlPlaneCredential>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials/${encodeURIComponent(credentialId)}/auto-select`,
      { method: "PUT", body: { enabled } },
    ),
  getProviderCredentialScopePolicy: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneProviderCredentialScopePolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/provider-credential-scope-policy`,
    ),
  updateProviderCredentialScopePolicy: (
    tenantId: string,
    input: {
      platformCredentialsEnabled: boolean;
      platformCredentialAutoSelect: boolean;
    },
  ) =>
    controlPlaneRequest<ControlPlaneProviderCredentialScopePolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/provider-credential-scope-policy`,
      { method: "PUT", body: input },
    ),
  listCredentialBindings: (
    tenantId: string,
    owner: { projectId: string } | { executionTargetId: string },
  ) => {
    const query = new URLSearchParams(owner).toString();
    return controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneCredentialBinding> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credential-bindings?${query}`,
    );
  },
  createCredentialBinding: (
    tenantId: string,
    input: {
      projectId?: string;
      executionTargetId?: string;
      credentialId: string;
      bindingKind: ControlPlaneCredentialBindingKind;
      selector?: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneCredentialBinding>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credential-bindings`,
      { method: "POST", body: input },
    ),
  disableCredentialBinding: (tenantId: string, bindingId: string) =>
    controlPlaneRequest<ControlPlaneCredentialBinding>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credential-bindings/${encodeURIComponent(bindingId)}/disable`,
      { method: "POST" },
    ),
  listAuditLogs: (
    tenantId: string,
    filters: ControlPlaneAuditLogFilters = {},
    page: { limit?: number; cursor?: string } = {},
  ) => {
    const query = auditLogSearchParams(filters, page);
    const suffix = query.size > 0 ? `?${query.toString()}` : "";
    return controlPlaneRequest<ControlPlaneAuditLogPage>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/audit-logs${suffix}`,
    );
  },
  listOrganizations: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneOrganization> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/organizations`,
    ),
  createOrganization: (
    tenantId: string,
    input: { slug: string; name: string; kind: "team" | "department" | "personal" },
  ) =>
    controlPlaneRequest<ControlPlaneOrganization>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/organizations`,
      { method: "POST", body: { ...input, settings: {} } },
    ),
  listProjects: (tenantId: string, organizationId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneProject> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/organizations/${encodeURIComponent(organizationId)}/projects`,
    ),
  createProject: (
    tenantId: string,
    organizationId: string,
    input: {
      name: string;
      repositoryUrl?: string;
      defaultBranch: string;
      visibility: ControlPlaneProject["visibility"];
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneProject>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/organizations/${encodeURIComponent(organizationId)}/projects`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  updateProject: (
    projectId: string,
    input: {
      name?: string;
      repositoryUrl?: string;
      defaultBranch?: string;
      visibility?: ControlPlaneProject["visibility"];
    },
  ) =>
    controlPlaneRequest<ControlPlaneProject>(`/v1/projects/${encodeURIComponent(projectId)}`, {
      method: "PATCH",
      body: input,
    }),
  listProjectSessions: (projectId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneAgentSession> }>(
      `/v1/projects/${encodeURIComponent(projectId)}/sessions`,
    ),
  getProjectProviderCapabilities: (projectId: string, executionTargetId?: string) => {
    const query = executionTargetId
      ? `?${new URLSearchParams({ executionTargetId }).toString()}`
      : "";
    return controlPlaneRequest<ProviderCapabilityProjection>(
      `/v1/projects/${encodeURIComponent(projectId)}/provider-capabilities${query}`,
    );
  },
  createSession: (
    projectId: string,
    input: {
      title: string;
      visibility: ControlPlaneAgentSession["visibility"];
      provider: ProviderKind;
      model?: string;
      providerCredentialId?: string;
      executionTargetId?: string;
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneAgentSession>(
      `/v1/projects/${encodeURIComponent(projectId)}/sessions`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  getAgentSession: (sessionId: string) =>
    controlPlaneRequest<ControlPlaneAgentSession>(`/v1/sessions/${encodeURIComponent(sessionId)}`),
  switchSessionModel: (
    sessionId: string,
    input: {
      model: string;
      expectedModel: string | null;
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneAgentSession>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/model-switch`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  getSessionProviderCapabilities: (sessionId: string) =>
    controlPlaneRequest<ProviderCapabilityProjection>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/provider-capabilities`,
    ),
  listExecutionTargets: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneExecutionTarget> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets`,
    ),
  listWorkerManifests: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneWorkerManifest> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/worker-manifests`,
    ),
  listWorkerReleases: (tenantId: string, targetId: string) =>
    controlPlaneRequest<ControlPlaneWorkerReleaseOverview>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/worker-releases`,
    ),
  createWorkerRelease: (
    tenantId: string,
    targetId: string,
    input: { workerManifestId: string; description: string },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneWorkerReleaseRevision>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/worker-releases`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  transitionWorkerRelease: (
    tenantId: string,
    targetId: string,
    revisionId: string,
    action: "canary" | "promote" | "rollback",
    input: { expectedPolicyVersion: number; reason: string; canaryPercent?: number },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneWorkerReleasePolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/worker-releases/${encodeURIComponent(revisionId)}/${action}`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  createExecutionTarget: (
    tenantId: string,
    input: {
      organizationId?: string;
      kind: ControlPlaneExecutionTargetKind;
      name: string;
      configuration: Record<string, unknown>;
      capabilities: Record<string, unknown>;
    },
  ) =>
    controlPlaneRequest<ControlPlaneExecutionTarget>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets`,
      { method: "POST", body: input },
    ),
  updateExecutionTargetProviderPolicy: (
    tenantId: string,
    targetId: string,
    experimentalProviders: ReadonlyArray<ProviderHostProviderKind>,
  ) =>
    controlPlaneRequest<ControlPlaneExecutionTarget>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/provider-policy`,
      { method: "PATCH", body: { experimentalProviders } },
    ),
  provisionSSHExecutionTarget: (
    tenantId: string,
    targetId: string,
    operation: ControlPlaneSSHProvisionResult["operation"],
  ) =>
    controlPlaneRequest<ControlPlaneSSHProvisionResult>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/ssh/${operation}`,
      { method: "POST" },
    ),
  createTurn: (
    sessionId: string,
    inputText: string,
    options?: ControlPlaneIdempotencyOptions,
    modes?: { runtimeMode: RuntimeMode; interactionMode: ProviderInteractionMode },
  ) =>
    controlPlaneRequest<ControlPlaneAgentTurn>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { inputText, ...modes },
      },
    ),
  compactSession: (
    sessionId: string,
    expectedLastEventSequence: number,
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneAdvancedCommandResult>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/compact`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { expectedLastEventSequence },
      },
    ),
  startReview: (
    sessionId: string,
    input: {
      expectedLastEventSequence: number;
      runtimeMode: RuntimeMode;
      target: ControlPlaneReviewTarget;
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneAdvancedCommandResult>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/reviews`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  rollbackSession: (
    sessionId: string,
    input: { expectedLastEventSequence: number; fromTurnId: string },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneRollbackResult>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/rollback`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  forkSession: (
    sessionId: string,
    input: {
      expectedLastEventSequence: number;
      title: string;
      visibility: ControlPlaneAgentSession["visibility"];
      providerCredentialId?: string;
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneForkResult>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/fork`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  steerActiveTurn: (
    sessionId: string,
    inputText: string,
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneControlCommand>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns/active/steer`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { inputText },
      },
    ),
  interruptActiveTurn: (sessionId: string, options?: ControlPlaneIdempotencyOptions) =>
    controlPlaneRequest<ControlPlaneControlCommand>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns/active/interrupt`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
      },
    ),
  listPendingInteractions: (sessionId: string) =>
    controlPlaneRequest<ControlPlanePendingInteractionSnapshot>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/interactions`,
    ),
  resolveApproval: (
    executionId: string,
    requestId: string,
    decision: "accept" | "decline",
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneInteractionResolution>(
      `/v1/executions/${encodeURIComponent(executionId)}/approvals/${encodeURIComponent(requestId)}/resolve`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { decision },
      },
    ),
  resolveUserInput: (
    executionId: string,
    requestId: string,
    answers: ProviderUserInputAnswers,
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneInteractionResolution>(
      `/v1/executions/${encodeURIComponent(executionId)}/user-input/${encodeURIComponent(requestId)}/resolve`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { answers },
      },
    ),
  listSessionEvents: (sessionId: string, afterSequence = 0, limit = 500) => {
    const query = new URLSearchParams({
      afterSequence: String(normalizeSessionEventSequence(afterSequence)),
      limit: String(Math.max(1, Math.min(500, limit))),
    });
    return controlPlaneRequest<ControlPlaneSessionEventPage>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/events?${query.toString()}`,
    );
  },
  listArtifacts: (sessionId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneArtifact> }>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/artifacts`,
    ),
  createArtifact: (
    sessionId: string,
    input: {
      kind: ControlPlaneArtifactKind;
      originalName?: string;
      executionId?: string;
      expiresAt?: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneArtifactUploadGrant>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/artifacts`,
      { method: "POST", body: input },
    ),
  uploadArtifactPayload,
  completeArtifact: (
    artifactId: string,
    input: { sizeBytes: number; sha256: string; contentType: string },
  ) =>
    controlPlaneRequest<ControlPlaneArtifact>(
      `/v1/artifacts/${encodeURIComponent(artifactId)}/complete`,
      { method: "POST", body: input },
    ),
  issueArtifactDownload: (artifactId: string) =>
    controlPlaneRequest<ControlPlaneArtifactDownloadGrant>(
      `/v1/artifacts/${encodeURIComponent(artifactId)}/download`,
      { method: "POST" },
    ),
  deleteArtifact: (artifactId: string) =>
    controlPlaneRequest<void>(`/v1/artifacts/${encodeURIComponent(artifactId)}`, {
      method: "DELETE",
    }),
  subscribeSessionEvents,
  listTenantMembers: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneTenantMember> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/members`,
    ),
  inviteTenantMember: (tenantId: string, input: { email: string; role: string }) =>
    controlPlaneRequest<TenantInvitation>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/invitations`,
      { method: "POST", body: input },
    ),
};
