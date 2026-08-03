import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import type { ControlPlaneTenantAccess } from "@synara/control-plane-client";

import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import {
  TenantStatusLifecycleSettingsSection,
  tenantLifecycleDisplayLabel,
  tenantLifecycleTargets,
} from "./TenantStatusLifecycleSettingsSection";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

function tenant(
  status: ControlPlaneTenantAccess["status"],
  role: ControlPlaneTenantAccess["role"] = "owner",
): ControlPlaneTenantAccess {
  return {
    id: "tenant-1",
    slug: "enterprise",
    name: "Enterprise Tenant",
    status,
    lifecycleVersion: 7,
    evaluationExpiresAt: status === "evaluation" ? "2026-08-30T00:00:00Z" : null,
    suspendedAt: status === "suspended" ? "2026-07-30T00:00:00Z" : null,
    closedAt: status === "closed" ? "2026-07-30T00:00:00Z" : null,
    entitlementProfileCode: "enterprise",
    region: "ap-east-1",
    role,
  };
}

function renderSection(value: ControlPlaneTenantAccess): string {
  const queryClient = new QueryClient();
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        <TenantStatusLifecycleSettingsSection tenant={value} onChanged={async () => undefined} />
      </EnterpriseUiHostProvider>
    </QueryClientProvider>,
  );
}

describe("TenantStatusLifecycleSettingsSection", () => {
  it("exposes only server-authorized lifecycle edges", () => {
    expect(tenantLifecycleTargets("evaluation")).toEqual(["active", "suspended", "closed"]);
    expect(tenantLifecycleTargets("active")).toEqual(["suspended", "closed"]);
    expect(tenantLifecycleTargets("suspended")).toEqual(["active", "closed"]);
    expect(tenantLifecycleTargets("closed")).toEqual(["active"]);
  });

  it("presents the public evaluation lifecycle", () => {
    expect(tenantLifecycleDisplayLabel("evaluation")).toBe("evaluation");
    const markup = renderSection(tenant("evaluation"));
    expect(markup).toContain("evaluation expires");
    expect(markup).not.toContain("trial expires");
  });

  it("shows versioned owner controls and keeps deletion as a closed-only second step", () => {
    const active = renderSection(tenant("active"));
    expect(active).toContain("Tenant lifecycle");
    expect(active).toContain("lifecycle version 7");
    expect(active).toContain("Change to suspended");
    expect(active).not.toContain("Request Tenant deletion");

    const closed = renderSection(tenant("closed"));
    expect(closed).toContain("Change to active");
    expect(closed).toContain("Request Tenant deletion");
    expect(closed).toContain("Legal Hold blocks it");
  });

  it("keeps non-owner status views read-only", () => {
    const markup = renderSection(tenant("suspended", "admin"));
    expect(markup).toContain("Owner approval required");
    expect(markup).not.toContain("Change to active");
  });
});
