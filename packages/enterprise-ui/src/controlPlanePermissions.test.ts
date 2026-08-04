import { describe, expect, it } from "vitest";

import type {
  ControlPlaneOrganization,
  ControlPlaneTenantAccess,
} from "@synara/control-plane-client";
import {
  isControlPlaneTenantOperational,
  resolveControlPlaneCapabilities,
} from "./controlPlanePermissions";

const tenant = (
  role: ControlPlaneTenantAccess["role"],
  status: ControlPlaneTenantAccess["status"] = "active",
  evaluationExpiresAt?: string | null,
): ControlPlaneTenantAccess => ({
  id: "tenant-1",
  slug: "tenant",
  name: "Tenant",
  status,
  ...(evaluationExpiresAt === undefined ? {} : { evaluationExpiresAt }),
  entitlementProfileCode: "enterprise",
  region: "default",
  role,
});

const organization = (
  currentUserRole: ControlPlaneOrganization["currentUserRole"],
  status: ControlPlaneOrganization["status"] = "active",
): ControlPlaneOrganization => ({
  id: "organization-1",
  tenantId: "tenant-1",
  parentOrganizationId: null,
  slug: "root",
  name: "Root",
  kind: "root",
  status,
  currentUserRole,
  settings: {},
  createdAt: "2026-07-12T00:00:00Z",
  updatedAt: "2026-07-12T00:00:00Z",
  archivedAt: null,
});

describe("resolveControlPlaneCapabilities", () => {
  it("grants Tenant administrators the Control Plane main-path operations", () => {
    const capabilities = resolveControlPlaneCapabilities({
      tenant: tenant("admin"),
      organization: organization(null),
    });

    expect(capabilities.canCreateProject).toBe(true);
    expect(capabilities.canUpdateProject).toBe(true);
    expect(capabilities.canCreateSession).toBe(true);
    expect(capabilities.canSettleSession).toBe(true);
    expect(capabilities.canArchiveSession).toBe(true);
    expect(capabilities.canCreateTurn).toBe(true);
    expect(capabilities.canSteerExecution).toBe(true);
    expect(capabilities.canInterruptExecution).toBe(true);
    expect(capabilities.canReadExecutionTargets).toBe(true);
  });

  it("keeps execution targets and Worker manifests behind worker.read", () => {
    const auditor = resolveControlPlaneCapabilities({
      tenant: tenant("auditor"),
      organization: organization(null),
    });
    const securityAdministrator = resolveControlPlaneCapabilities({
      tenant: tenant("security_admin"),
      organization: organization(null),
    });

    expect(auditor.canReadExecutionTargets).toBe(false);
    expect(securityAdministrator.canReadExecutionTargets).toBe(true);
  });

  it("maps lifecycle read and manage capabilities to the backend roles", () => {
    const admin = resolveControlPlaneCapabilities({
      tenant: tenant("admin"),
      organization: organization(null),
    });
    const securityAdministrator = resolveControlPlaneCapabilities({
      tenant: tenant("security_admin"),
      organization: organization(null),
    });
    const auditor = resolveControlPlaneCapabilities({
      tenant: tenant("auditor"),
      organization: organization(null),
    });

    expect(admin.canReadLifecycle).toBe(true);
    expect(admin.canManageLifecycle).toBe(true);
    expect(securityAdministrator.canReadLifecycle).toBe(true);
    expect(securityAdministrator.canManageLifecycle).toBe(false);
    expect(auditor.canReadLifecycle).toBe(true);
    expect(auditor.canManageLifecycle).toBe(false);
    expect(admin.canManageSchedulingPolicy).toBe(true);
    expect(securityAdministrator.canManageSchedulingPolicy).toBe(true);
    expect(auditor.canReadSchedulingPolicy).toBe(true);
    expect(auditor.canManageSchedulingPolicy).toBe(false);
  });

  it("shows cost controls only to operational owners and cost administrators", () => {
    for (const role of ["owner", "cost_admin"] as const) {
      expect(
        resolveControlPlaneCapabilities({
          tenant: tenant(role),
          organization: organization(null),
        }).canManageCost,
      ).toBe(true);
    }
    for (const role of [
      "admin",
      "security_admin",
      "auditor",
      "member",
      "support_readonly",
    ] as const) {
      expect(
        resolveControlPlaneCapabilities({
          tenant: tenant(role),
          organization: organization(null),
        }).canManageCost,
      ).toBe(false);
    }
    expect(
      resolveControlPlaneCapabilities({
        tenant: tenant("cost_admin", "suspended"),
        organization: organization(null),
      }).canManageCost,
    ).toBe(false);
  });

  it("uses Organization membership for ordinary Tenant members", () => {
    const manager = resolveControlPlaneCapabilities({
      tenant: tenant("member"),
      organization: organization("admin"),
    });
    const operator = resolveControlPlaneCapabilities({
      tenant: tenant("member"),
      organization: organization("agent_operator"),
    });
    const viewer = resolveControlPlaneCapabilities({
      tenant: tenant("member"),
      organization: organization("viewer"),
    });

    expect(manager.canCreateProject).toBe(true);
    expect(manager.canUpdateProject).toBe(true);
    expect(operator.canCreateSession).toBe(true);
    expect(operator.canUpdateProject).toBe(false);
    expect(operator.canApproveExecution).toBe(true);
    expect(viewer.canReadProjects).toBe(true);
    expect(viewer.canUpdateProject).toBe(false);
    expect(viewer.canCreateSession).toBe(false);
    expect(viewer.canSettleSession).toBe(false);
    expect(viewer.canArchiveSession).toBe(false);
    expect(viewer.canCreateTurn).toBe(false);
    expect(viewer.canSteerExecution).toBe(false);
    expect(viewer.canInterruptExecution).toBe(false);
  });

  it("removes mutation capabilities for suspended Tenant or Organization state", () => {
    const suspendedTenant = resolveControlPlaneCapabilities({
      tenant: tenant("owner", "suspended"),
      organization: organization("owner"),
    });
    expect(suspendedTenant.canReadProjects).toBe(true);
    expect(suspendedTenant.canUpdateProject).toBe(false);
    expect(suspendedTenant.canCreateTurn).toBe(false);
    expect(suspendedTenant.canSettleSession).toBe(false);
    expect(suspendedTenant.canArchiveSession).toBe(false);
    expect(suspendedTenant.canSteerExecution).toBe(false);
    expect(suspendedTenant.canInterruptExecution).toBe(false);
    expect(
      resolveControlPlaneCapabilities({
        tenant: tenant("owner"),
        organization: organization("owner", "suspended"),
      }).canCreateTurn,
    ).toBe(false);
  });

  it("only treats an unexpired evaluation as operational", () => {
    const now = Date.parse("2026-07-30T00:00:00Z");
    expect(
      isControlPlaneTenantOperational(tenant("owner", "evaluation", "2026-08-01T00:00:00Z"), now),
    ).toBe(true);
    expect(
      isControlPlaneTenantOperational(tenant("owner", "evaluation", "2026-07-29T00:00:00Z"), now),
    ).toBe(false);
    expect(isControlPlaneTenantOperational(tenant("owner", "evaluation", null), now)).toBe(false);
  });

  it("keeps Support Access diagnostic reads visible while denying every mutation", () => {
    const support = resolveControlPlaneCapabilities({
      tenant: tenant("support_readonly", "suspended"),
      organization: organization(null, "suspended"),
    });

    expect(support.canReadOrganizations).toBe(true);
    expect(support.canReadProjects).toBe(true);
    expect(support.canReadMembers).toBe(true);
    expect(support.canReadExecutionTargets).toBe(true);
    expect(support.canReadQuota).toBe(true);
    expect(support.canReadRetention).toBe(true);
    expect(support.canReadLifecycle).toBe(true);
    expect(support.canReadSchedulingPolicy).toBe(true);
    expect(support.canReadAudit).toBe(true);
    expect(support.canReadOutbox).toBe(true);
    expect(support.canManageOutbox).toBe(false);
    expect(support.canReadCredentials).toBe(true);
    expect(support.canReadIdentity).toBe(true);
    expect(support.canReadServiceAccounts).toBe(true);
    expect(support.canManageOrganizations).toBe(false);
    expect(support.canCreateProject).toBe(false);
    expect(support.canCreateSession).toBe(false);
    expect(support.canCreateTurn).toBe(false);
    expect(support.canInterruptExecution).toBe(false);
    expect(support.canManageExecutionTargets).toBe(false);
    expect(support.canManageQuota).toBe(false);
    expect(support.canManageCost).toBe(false);
    expect(support.canManageRetention).toBe(false);
    expect(support.canManageCredentials).toBe(false);
    expect(support.canManageSchedulingPolicy).toBe(false);
    expect(support.canManageIdentity).toBe(false);
  });
});
