import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import type {
  ControlPlaneOrganization,
  ControlPlaneTenantAccess,
} from "@synara/control-plane-client";

import {
  EnterpriseControlPlaneRuntimeProvider,
  type EnterpriseControlPlaneRuntime,
} from "./EnterpriseControlPlaneRuntime";
import {
  EnterpriseSettingsHostProvider,
  type EnterpriseSettingsHostComponents,
} from "./EnterpriseSettingsHost";
import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import { resolveControlPlaneCapabilities } from "./controlPlanePermissions";
import { TenantOrganizationSettingsPanel } from "./TenantOrganizationSettingsPanel";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

const tenant: ControlPlaneTenantAccess = {
  id: "tenant-1",
  slug: "enterprise",
  name: "Enterprise Tenant",
  status: "active",
  lifecycleVersion: 7,
  entitlementProfileCode: "enterprise",
  region: "ap-east-1",
  role: "admin",
};

const organization: ControlPlaneOrganization = {
  id: "organization-1",
  tenantId: tenant.id,
  parentOrganizationId: null,
  slug: "root",
  name: "Root Organization",
  kind: "root",
  status: "active",
  currentUserRole: "admin",
  settings: {},
  createdAt: "2026-07-30T00:00:00Z",
  updatedAt: "2026-07-30T00:00:00Z",
  archivedAt: null,
};

const SETTINGS_HOST = {
  ExecutionTargetsSection: () => <p>Web execution targets extension</p>,
  ProjectSessionsSection: () => <p>Web project sessions extension</p>,
} satisfies EnterpriseSettingsHostComponents;

function runtime(
  overrides: Partial<EnterpriseControlPlaneRuntime> = {},
): EnterpriseControlPlaneRuntime {
  return {
    availability: "available",
    authentication: "authenticated",
    profile: null,
    session: {
      authenticated: true,
      user: {
        userId: "user-1",
        sessionId: "session-1",
        activeTenantId: tenant.id,
        supportAccessGrantId: null,
        audience: "web",
        email: "admin@example.com",
        displayName: "Admin",
      },
      tenants: [tenant],
    },
    activeTenant: tenant,
    organizations: [organization],
    capabilities: resolveControlPlaneCapabilities({ tenant, organization }),
    error: null,
    retry: async () => undefined,
    devLogin: async () => undefined,
    logout: async () => undefined,
    setActiveTenant: async () => undefined,
    ...overrides,
  };
}

function renderPanel(
  destination: Parameters<typeof TenantOrganizationSettingsPanel>[0]["destination"],
  runtimeValue: EnterpriseControlPlaneRuntime,
): string {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        <EnterpriseControlPlaneRuntimeProvider runtime={runtimeValue}>
          <EnterpriseSettingsHostProvider components={SETTINGS_HOST}>
            <TenantOrganizationSettingsPanel destination={destination} />
          </EnterpriseSettingsHostProvider>
        </EnterpriseControlPlaneRuntimeProvider>
      </EnterpriseUiHostProvider>
    </QueryClientProvider>,
  );
}

describe("TenantOrganizationSettingsPanel", () => {
  it("keeps local mode independent from Control Plane Tenant state", () => {
    const markup = renderPanel(
      "organization-overview",
      runtime({
        availability: "local",
        authentication: "unknown",
        session: null,
        activeTenant: null,
        organizations: [],
      }),
    );

    expect(markup).toContain("Local mode");
    expect(markup).toContain("Local Projects and chats remain authoritative");
    expect(markup).not.toContain("Enterprise Tenant");
  });

  it("owns the unauthenticated local and enterprise SSO entry points", () => {
    const markup = renderPanel(
      "organization-overview",
      runtime({ authentication: "unauthenticated", session: null, activeTenant: null }),
    );

    expect(markup).toContain("Create local identity");
    expect(markup).toContain("Find SSO connections");
  });

  it("rejects a registered destination when the active context lacks its capability", () => {
    const memberTenant = { ...tenant, role: "member" as const };
    const viewerOrganization = { ...organization, currentUserRole: "viewer" as const };
    const markup = renderPanel(
      "organization-identity",
      runtime({
        activeTenant: memberTenant,
        organizations: [viewerOrganization],
        capabilities: resolveControlPlaneCapabilities({
          tenant: memberTenant,
          organization: viewerOrganization,
        }),
      }),
    );

    expect(markup).toContain("This destination is unavailable");
    expect(markup).not.toContain("Enterprise identity");
  });

  it("renders authenticated Overview through the injected Web extensions", () => {
    const markup = renderPanel("organization-overview", runtime());

    expect(markup).toContain("Tenant context");
    expect(markup).toContain("Enterprise Tenant");
    expect(markup).toContain("Tenant lifecycle");
    expect(markup).toContain("Root Organization");
    expect(markup).toContain("Web execution targets extension");
    expect(markup).toContain("Web project sessions extension");
  });
});
