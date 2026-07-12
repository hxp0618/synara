import { describe, expect, it } from "vitest";

import type { ControlPlaneOrganization, ControlPlaneTenantAccess } from "./controlPlaneClient";
import { resolveControlPlaneCapabilities } from "./controlPlanePermissions";

const tenant = (
  role: ControlPlaneTenantAccess["role"],
  status: ControlPlaneTenantAccess["status"] = "active",
): ControlPlaneTenantAccess => ({
  id: "tenant-1",
  slug: "tenant",
  name: "Tenant",
  status,
  planCode: "enterprise",
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
  it("grants Tenant administrators the SaaS main-path operations", () => {
    const capabilities = resolveControlPlaneCapabilities({
      tenant: tenant("admin"),
      organization: organization(null),
    });

    expect(capabilities.canCreateProject).toBe(true);
    expect(capabilities.canCreateSession).toBe(true);
    expect(capabilities.canCreateTurn).toBe(true);
  });

  it("uses Organization membership for ordinary Tenant members", () => {
    const operator = resolveControlPlaneCapabilities({
      tenant: tenant("member"),
      organization: organization("agent_operator"),
    });
    const viewer = resolveControlPlaneCapabilities({
      tenant: tenant("member"),
      organization: organization("viewer"),
    });

    expect(operator.canCreateSession).toBe(true);
    expect(operator.canApproveExecution).toBe(true);
    expect(viewer.canReadProjects).toBe(true);
    expect(viewer.canCreateSession).toBe(false);
    expect(viewer.canCreateTurn).toBe(false);
  });

  it("removes mutation capabilities for suspended Tenant or Organization state", () => {
    const suspendedTenant = resolveControlPlaneCapabilities({
      tenant: tenant("owner", "suspended"),
      organization: organization("owner"),
    });
    expect(suspendedTenant.canReadProjects).toBe(true);
    expect(suspendedTenant.canCreateTurn).toBe(false);
    expect(
      resolveControlPlaneCapabilities({
        tenant: tenant("owner"),
        organization: organization("owner", "suspended"),
      }).canCreateTurn,
    ).toBe(false);
  });
});
