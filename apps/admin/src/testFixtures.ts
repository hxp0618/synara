// FILE: testFixtures.ts
// Purpose: Provide deterministic Platform fixtures for unit and real-browser UI checks.
// Layer: Admin test support

import type {
  ControlPlaneEntitlementSnapshot,
  ControlPlanePlatformDesktopAccess,
  ControlPlanePlatformTenantOverview,
  ControlPlaneSessionState,
  ControlPlaneSupportAccessGrant,
} from "@synara/control-plane-client";

export const platformTenant: ControlPlanePlatformTenantOverview["items"][number] = {
  id: "tenant-customer",
  slug: "northstar-labs",
  name: "Northstar Labs",
  status: "active",
  entitlementProfileCode: "enterprise",
  region: "us-east-1",
  activeMemberCount: 12,
  organizationCount: 2,
  sessionCount: 18,
  executionTargetCount: 3,
  workerCount: 6,
  offlineWorkerCount: 1,
  activeExecutionCount: 4,
  queuedExecutionCount: 2,
  oldestQueuedAt: "2026-07-30T09:15:00Z",
  failedExecutionCount24h: 1,
  artifactCount: 24,
  artifactBytes: 1_572_864,
  pendingArtifactCount: 2,
  activeCredentialCount: 3,
  unavailableCredentialCount: 1,
  activeIdentityConnectionCount: 1,
  disabledIdentityConnectionCount: 1,
};

export function platformOverview(
  operatorRole: ControlPlanePlatformTenantOverview["operatorRole"] = "owner",
): ControlPlanePlatformTenantOverview {
  return {
    operatorRole,
    generatedAt: "2026-07-30T10:30:00Z",
    items: [
      platformTenant,
      {
        ...platformTenant,
        id: "tenant-meridian",
        slug: "meridian-health",
        name: "Meridian Health",
        status: "suspended",
        region: "eu-west-1",
      },
      {
        ...platformTenant,
        id: "tenant-halcyon",
        slug: "halcyon-studio",
        name: "Halcyon Studio",
        status: "evaluation",
        entitlementProfileCode: "standard",
        region: "us-west-2",
      },
      {
        ...platformTenant,
        id: "tenant-acme",
        slug: "acme-research",
        name: "Acme Research",
        region: "ap-southeast-2",
      },
    ],
  };
}

export const adminSession: ControlPlaneSessionState = {
  authenticated: true,
  user: {
    userId: "platform-operator",
    sessionId: "session-platform",
    activeTenantId: "tenant-operator",
    supportAccessGrantId: null,
    audience: "web",
    email: "platform@synara.example",
    displayName: "Platform Operator",
  },
  tenants: [
    {
      id: "tenant-operator",
      slug: "synara-operations",
      name: "Synara Operations",
      status: "active",
      lifecycleVersion: 1,
      entitlementProfileCode: "enterprise",
      region: "global",
      role: "owner",
    },
  ],
};

export const desktopAccess: ControlPlanePlatformDesktopAccess = {
  controlPlaneOrigin: "https://control.synara.example",
  enrollmentTtlSeconds: 180,
  subjects: [
    {
      userId: "platform-operator",
      email: "platform@synara.example",
      displayName: "Platform Operator",
      role: "owner",
      membershipStatus: "active",
      selfEnrollable: true,
    },
    {
      userId: "customer-user",
      email: "customer@example.com",
      displayName: "Internal User",
      role: "member",
      membershipStatus: "active",
      selfEnrollable: false,
    },
  ],
  enrollments: [],
  devices: [],
};

export const pendingGrant: ControlPlaneSupportAccessGrant = {
  id: "grant-pending",
  tenantId: platformTenant.id,
  tenantName: platformTenant.name,
  requesterUserId: "security-operator",
  requesterEmail: "security@example.invalid",
  requesterDisplayName: "Security Operator",
  status: "pending",
  version: 1,
  reason: "Investigate incident INC-42",
  requestedDurationSeconds: 1800,
  requestedAt: "2026-07-30T00:00:00Z",
  decidedBy: null,
  decisionReason: null,
  decidedAt: null,
  expiresAt: null,
  revokedBy: null,
  revocationReason: null,
  revokedAt: null,
  createdAt: "2026-07-30T00:00:00Z",
  updatedAt: "2026-07-30T00:00:00Z",
};

export const entitlementSnapshot: ControlPlaneEntitlementSnapshot = {
  tenantId: platformTenant.id,
  profile: {
    code: "enterprise",
    displayName: "Enterprise",
    status: "active",
    version: 1,
    evaluationDays: 0,
  },
  profileAssignment: {
    status: "active",
    version: 3,
    evaluationEndsAt: null,
    reportingPeriodStart: "2026-07-01T00:00:00Z",
    reportingPeriodEnd: "2026-08-01T00:00:00Z",
    assignmentSource: "platform_admin",
  },
  entitlements: {},
  features: {},
};
