import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import {
  applyLifecycleOverrides,
  describeLifecycleBounds,
  tenantResourceLifecyclePolicyQueryKey,
  TenantLifecyclePolicySettingsSection,
} from "./TenantLifecyclePolicySettingsSection";
import type {
  ControlPlaneResourceLifecycleConfig,
  ControlPlaneResourceLifecyclePolicy,
} from "~/lib/controlPlaneClient";

const resourceLifecycleConfig: ControlPlaneResourceLifecycleConfig = {
  defaults: {
    waitingKeepAliveSeconds: 900,
    suspendAfterIdleSeconds: 1800,
    absoluteSessionLifetimeSeconds: null,
    workspaceRetentionDays: 30,
    warmPoolMode: "balanced",
  },
  bounds: {
    waitingKeepAliveSeconds: { min: 60, max: 86_400 },
    suspendAfterIdleSeconds: { min: 60, max: 604_800 },
    absoluteSessionLifetimeSeconds: { min: 3_600, max: 31_536_000 },
    workspaceRetentionDays: { min: 1, max: 3_650 },
    warmPoolModes: ["disabled", "balanced", "low-latency"],
  },
};

const tenantPolicy: ControlPlaneResourceLifecyclePolicy = {
  scope: "tenant",
  tenantId: "tenant-1",
  overrides: {
    waitingKeepAliveSeconds: 600,
    suspendAfterIdleSeconds: null,
    absoluteSessionLifetimeSeconds: null,
    workspaceRetentionDays: 14,
    warmPoolMode: "low-latency",
  },
  effective: {
    waitingKeepAliveSeconds: 600,
    suspendAfterIdleSeconds: 1800,
    absoluteSessionLifetimeSeconds: null,
    workspaceRetentionDays: 14,
    warmPoolMode: "low-latency",
  },
  version: 4,
  updatedBy: "user-12345678",
  createdAt: "2026-07-24T02:00:00Z",
  updatedAt: "2026-07-24T03:30:00Z",
};

function renderLifecycleSection(policy = tenantPolicy): string {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(tenantResourceLifecyclePolicyQueryKey("tenant-1"), policy);
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <TenantLifecyclePolicySettingsSection
        canManage={false}
        config={resourceLifecycleConfig}
        tenantId="tenant-1"
      />
    </QueryClientProvider>,
  );
}

describe("TenantLifecyclePolicySettingsSection", () => {
  it("applies non-null overrides over the inherited effective policy and formats bounds", () => {
    expect(
      applyLifecycleOverrides(resourceLifecycleConfig.defaults, {
        waitingKeepAliveSeconds: 600,
        suspendAfterIdleSeconds: null,
        absoluteSessionLifetimeSeconds: 86_400,
        workspaceRetentionDays: null,
        warmPoolMode: "low-latency",
      }),
    ).toEqual({
      waitingKeepAliveSeconds: 600,
      suspendAfterIdleSeconds: 1800,
      absoluteSessionLifetimeSeconds: 86_400,
      workspaceRetentionDays: 30,
      warmPoolMode: "low-latency",
    });
    expect(describeLifecycleBounds(resourceLifecycleConfig)).toContain("wait 60-86,400s");
    expect(describeLifecycleBounds(resourceLifecycleConfig)).toContain(
      "absolute 3,600-31,536,000s",
    );
  });

  it("renders effective values, platform defaults, and server-side renewal guidance", () => {
    const markup = renderLifecycleSection();

    expect(markup).toContain("Resource lifecycle");
    expect(markup).toContain("Only server-side execution activity renews the resource timers");
    expect(markup).toContain("browser heartbeats, stream reads, and reconnect noise");
    expect(markup).toContain("Platform defaults");
    expect(markup).toContain("wait 15m");
    expect(markup).toContain("warm balanced");
    expect(markup).toContain("Current overrides");
    expect(markup).toContain("wait 10m");
    expect(markup).toContain("retain 14d");
    expect(markup).toContain("warm low latency");
  });

  it("documents full inheritance when no tenant overrides exist", () => {
    const markup = renderLifecycleSection({
      ...tenantPolicy,
      overrides: {
        waitingKeepAliveSeconds: null,
        suspendAfterIdleSeconds: null,
        absoluteSessionLifetimeSeconds: null,
        workspaceRetentionDays: null,
        warmPoolMode: null,
      },
      effective: resourceLifecycleConfig.defaults,
      version: 0,
      updatedBy: null,
      createdAt: null,
      updatedAt: null,
    });

    expect(markup).toContain("Inheriting every value from platform defaults.");
    expect(markup).toContain("Used when no override exists");
  });
});
