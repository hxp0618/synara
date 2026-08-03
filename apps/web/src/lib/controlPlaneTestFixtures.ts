import type {
  ControlPlaneAgentSession,
  ControlPlanePlatformProfile,
  ControlPlaneResourceLifecycleConfig,
  ControlPlaneResourceLifecycleEffective,
} from "@synara/control-plane-client";

export const testResourceLifecycleEffective: ControlPlaneResourceLifecycleEffective = {
  waitingKeepAliveSeconds: 900,
  suspendAfterIdleSeconds: 1800,
  absoluteSessionLifetimeSeconds: null,
  workspaceRetentionDays: 30,
  warmPoolMode: "balanced",
};

export const testResourceLifecycleConfig: ControlPlaneResourceLifecycleConfig = {
  defaults: testResourceLifecycleEffective,
  bounds: {
    waitingKeepAliveSeconds: { min: 60, max: 86_400 },
    suspendAfterIdleSeconds: { min: 60, max: 604_800 },
    absoluteSessionLifetimeSeconds: { min: 3_600, max: 31_536_000 },
    workspaceRetentionDays: { min: 1, max: 3_650 },
    warmPoolModes: ["disabled", "balanced", "low-latency"],
  },
};

/**
 * Builds an authoritative Session in the shape the Control Plane returns.
 * Tests override only the fields they assert on so that adding a required
 * Session field stays a one-line change here instead of a sweep across
 * every fixture literal.
 */
export function createTestAgentSession(
  overrides: Partial<ControlPlaneAgentSession> = {},
): ControlPlaneAgentSession {
  const timestamp = "2026-07-12T00:00:00Z";
  return {
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
    resourceState: "idle",
    meaningfulActivityAt: timestamp,
    resourceIdleSince: null,
    absoluteExpiresAt: null,
    resourceLifecyclePolicy: testResourceLifecycleEffective,
    createdAt: timestamp,
    updatedAt: timestamp,
    archivedAt: null,
    ...overrides,
  };
}

export function createTestPlatformProfile(
  overrides: Partial<ControlPlanePlatformProfile> = {},
): ControlPlanePlatformProfile {
  return {
    profile: "enterprise",
    metadataStore: "postgresql",
    artifactStore: "minio",
    queueDriver: "postgres-outbox",
    controlPlaneReplicas: 1,
    highAvailability: false,
    leaseEnabled: true,
    fencingEnabled: true,
    executionTargetKinds: ["docker"],
    artifactPayloadMigration: false,
    metadataExportImport: false,
    resourceLifecyclePolicy: testResourceLifecycleConfig,
    internalStatusBoard: { configured: false },
    commercializationMode: "internal-self-hosted",
    ...overrides,
  };
}
