// FILE: EnterpriseSettingsRenderer.browser.tsx
// Purpose: Browser acceptance for the shared Stage 6 Tenant renderer inside the Web host.
// Layer: Browser UI test

import "../../index.css";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  EnterpriseControlPlaneRuntimeProvider,
  EnterpriseSettingsHostProvider,
  TenantOrganizationSettingsPanel,
  resolveControlPlaneCapabilities,
  type EnterpriseControlPlaneRuntime,
  type EnterpriseSettingsHostComponents,
  type EnterpriseSettingsSectionId,
} from "@synara/enterprise-ui";
import { cdp, page, userEvent } from "vitest/browser";
import { afterEach, describe, expect, it } from "vitest";
import { render } from "vitest-browser-react";
import { useState, type CSSProperties } from "react";

import type {
  ControlPlaneOrganization,
  ControlPlaneTenantAccess,
} from "@synara/control-plane-client";

import { WebEnterpriseUiHost } from "~/features/enterprise/WebEnterpriseUiHost";
import { getDensityCssVariables, type UiDensity } from "~/lib/appDensity";

const tenant: ControlPlaneTenantAccess = {
  id: "tenant-1",
  slug: "enterprise",
  name: "Enterprise Tenant",
  status: "active",
  lifecycleVersion: 7,
  entitlementProfileCode: "enterprise",
  region: "ap-east-1",
  role: "owner",
};

const alternateTenant: ControlPlaneTenantAccess = {
  ...tenant,
  id: "tenant-2",
  slug: "alternate",
  name: "Alternate Tenant",
};

const organization: ControlPlaneOrganization = {
  id: "organization-1",
  tenantId: tenant.id,
  parentOrganizationId: null,
  slug: "root",
  name: "Root Organization",
  kind: "root",
  status: "active",
  currentUserRole: "owner",
  settings: {},
  createdAt: "2026-07-30T00:00:00Z",
  updatedAt: "2026-07-30T00:00:00Z",
  archivedAt: null,
};

const SETTINGS_HOST = {
  ExecutionTargetsSection: () => <section>Execution Targets extension</section>,
  ProjectSessionsSection: () => <section>Project Sessions extension</section>,
} satisfies EnterpriseSettingsHostComponents;

function authenticatedRuntime(): EnterpriseControlPlaneRuntime {
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
        email: "owner@example.com",
        displayName: "Owner",
      },
      tenants: [tenant, alternateTenant],
    },
    activeTenant: tenant,
    organizations: [organization],
    capabilities: resolveControlPlaneCapabilities({ tenant, organization }),
    error: null,
    retry: async () => undefined,
    devLogin: async () => undefined,
    logout: async () => undefined,
    setActiveTenant: async () => undefined,
  };
}

function unauthenticatedRuntime(): EnterpriseControlPlaneRuntime {
  return {
    ...authenticatedRuntime(),
    authentication: "unauthenticated",
    session: null,
    activeTenant: null,
    organizations: [],
  };
}

function createQueryClient() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["control-plane", "tenants", tenant.id, "credentials"], {
    items: [],
  });
  queryClient.setQueryData(["control-plane", "tenants", "deletion-requests"], { items: [] });
  queryClient.setQueryData(["control-plane", "tenants", tenant.id, "execution-targets"], {
    items: [],
  });
  queryClient.setQueryData(["control-plane", "tenants", tenant.id, "worker-manifests"], {
    items: [],
  });
  queryClient.setQueryData(["control-plane", "tenants", tenant.id, "workers"], { items: [] });
  queryClient.setQueryData(["control-plane", "tenants", tenant.id, "usage"], {
    tenantId: tenant.id,
    entitlementProfileVersion: 1,
    periodStart: "2026-07-01T00:00:00Z",
    periodEnd: "2026-08-01T00:00:00Z",
    usage: {
      inputTokens: 100,
      cachedInputTokens: 20,
      outputTokens: 30,
      reasoningTokens: 10,
      totalTokens: 160,
      networkIngressBytes: 1024,
      networkEgressBytes: 2048,
      executionSeconds: 80,
      providerCostByCurrency: { USD: 15_000 },
      providerCostReportedCount: 3,
      providerCostMissingCount: 1,
      platformCharges: [
        { kind: "cpu", currencyCode: "USD", amountMicros: 9_000, source: "actual" },
      ],
      platformCostByCurrency: { USD: 9_000 },
      knownCostByCurrency: { USD: 24_000 },
    },
    softQuota: {
      metric: "execution_seconds",
      unit: "seconds",
      limit: 100,
      observed: 80,
      warningPercent: 80,
      percentageUsed: 80,
      state: "approaching_limit",
      enforcement: "soft",
      hardStop: false,
      recommendedAction: "Ask an internal administrator to adjust the entitlement profile.",
    },
    alerts: [],
  });
  return queryClient;
}

function EnterpriseRendererHarness(props: {
  density: UiDensity;
  destination?: EnterpriseSettingsSectionId;
  runtime?: EnterpriseControlPlaneRuntime;
}) {
  const [queryClient] = useState(createQueryClient);
  const style = getDensityCssVariables(props.density) as CSSProperties;

  return (
    <div
      className="w-full max-w-full overflow-x-clip p-2"
      data-testid="enterprise-root"
      style={style}
    >
      <QueryClientProvider client={queryClient}>
        <WebEnterpriseUiHost>
          <EnterpriseControlPlaneRuntimeProvider runtime={props.runtime ?? authenticatedRuntime()}>
            <EnterpriseSettingsHostProvider components={SETTINGS_HOST}>
              <TenantOrganizationSettingsPanel
                destination={props.destination ?? "organization-overview"}
              />
            </EnterpriseSettingsHostProvider>
          </EnterpriseControlPlaneRuntimeProvider>
        </WebEnterpriseUiHost>
      </QueryClientProvider>
    </div>
  );
}

async function emulateReducedMotion(value: "reduce" | "no-preference") {
  const session = (await cdp()) as {
    send(method: string, params: Record<string, unknown>): Promise<unknown>;
  };
  await session.send("Emulation.setEmulatedMedia", {
    features: [{ name: "prefers-reduced-motion", value }],
  });
}

describe("EnterpriseSettingsRenderer browser boundary", () => {
  afterEach(async () => {
    await emulateReducedMotion("no-preference");
    await page.viewport(1280, 720);
    document.body.innerHTML = "";
  });

  it("keeps spacious Overview usable without horizontal overflow at a narrow viewport", async () => {
    await page.viewport(390, 844);
    await render(<EnterpriseRendererHarness density="spacious" />);

    const root = page.getByTestId("enterprise-root").element() as HTMLElement;
    expect(root.scrollWidth).toBeLessThanOrEqual(root.clientWidth + 1);
    await expect.element(page.getByRole("heading", { name: "Enterprise Tenant" })).toBeVisible();
    await expect.element(page.getByLabelText("Active tenant")).toBeVisible();
    await expect.element(page.getByText("Execution Targets extension")).toBeVisible();
    expect(page.getByRole("button", { name: "Payments" }).elements()).toHaveLength(0);
    expect(page.getByRole("button", { name: "Billing exercises" }).elements()).toHaveLength(0);
  });

  it("consumes compact and spacious density variables in shared settings rows", async () => {
    const mounted = await render(<EnterpriseRendererHarness density="compact" />);
    const firstRow = document.querySelector<HTMLElement>('[data-slot="settings-row"]');
    expect(firstRow).not.toBeNull();
    const compactPadding = Number.parseFloat(getComputedStyle(firstRow!).paddingTop);

    await mounted.rerender(<EnterpriseRendererHarness density="spacious" />);
    const spaciousPadding = Number.parseFloat(getComputedStyle(firstRow!).paddingTop);

    expect(spaciousPadding).toBeGreaterThan(compactPadding);
  });

  it("renders Token and internal cost data without Plan-upgrade positioning", async () => {
    await render(
      <EnterpriseRendererHarness density="comfortable" destination="organization-usage" />,
    );

    await expect
      .element(page.getByRole("heading", { name: "Usage & internal cost" }))
      .toBeVisible();
    await expect.element(page.getByText("160 total")).toBeVisible();
    await expect.element(page.getByText("$0.024")).toBeVisible();
    expect(page.getByText(/Plan usage|Upgrade the Plan/i).elements()).toHaveLength(0);
  });

  it("keeps the unauthenticated entry flow in keyboard order", async () => {
    await render(
      <EnterpriseRendererHarness density="comfortable" runtime={unauthenticatedRuntime()} />,
    );

    await userEvent.keyboard("{Tab}");
    expect(document.activeElement).toBe(page.getByLabelText("Email").element());
    await userEvent.keyboard("owner@example.com");
    expect((document.activeElement as HTMLInputElement).value).toBe("owner@example.com");
    await userEvent.keyboard("{Tab}");
    expect(document.activeElement).toBe(page.getByLabelText("Display name").element());
  });

  it("renders and remains keyboard-operable with reduced motion requested", async () => {
    await emulateReducedMotion("reduce");
    expect(window.matchMedia("(prefers-reduced-motion: reduce)").matches).toBe(true);

    await render(
      <EnterpriseRendererHarness density="comfortable" runtime={unauthenticatedRuntime()} />,
    );
    await userEvent.keyboard("{Tab}");

    expect(document.activeElement).toBe(page.getByLabelText("Email").element());
    await expect.element(page.getByRole("button", { name: "Create local identity" })).toBeVisible();
  });
});
