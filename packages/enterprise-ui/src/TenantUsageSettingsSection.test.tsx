import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import { quotaQueryKey, TenantQuotaSettingsSection } from "./TenantQuotaSettingsSection";
import { tenantUsageQueryKey, TenantUsageSettingsSection } from "./TenantUsageSettingsSection";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

function renderWithHost(queryClient: QueryClient, child: ReactNode): string {
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        {child}
      </EnterpriseUiHostProvider>
    </QueryClientProvider>,
  );
}

describe("shared Usage & limits destination sections", () => {
  it("renders internal usage and cost through injected host primitives with bounded progress motion", () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(tenantUsageQueryKey("tenant-1"), {
      tenantId: "tenant-1",
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
          {
            kind: "cpu",
            currencyCode: "USD",
            amountMicros: 9_000,
            source: "actual",
          },
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
        recommendedAction: "Review usage before the current reporting period ends.",
      },
      alerts: [],
    });

    const markup = renderWithHost(
      queryClient,
      <TenantUsageSettingsSection canManageCost={true} tenantId="tenant-1" />,
    );

    expect(markup).toContain("Usage &amp; internal cost");
    expect(markup).toContain("Export JSON");
    expect(markup).not.toContain("Plan usage");
    expect(markup).toContain('role="progressbar"');
    expect(markup).toContain("motion-reduce:transition-none");
    expect(markup).toContain("160 total");
    expect(markup).toContain("1 execution report is unavailable");
    expect(markup).toContain(
      "Excludes only the Provider portion of executions whose cost is unavailable",
    );
    expect(markup).toContain("$0.015");
    expect(markup).toContain("Internal platform allocation");
    expect(markup).toContain("cpu (actual)");
    expect(markup).toContain("$0.009");
    expect(markup).toContain("$0.024");
    expect(markup).not.toMatch(/invoice|payment|checkout|stripe|billing/i);
  });

  it("renders the managed quota form through injected Settings and form primitives", () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(quotaQueryKey("tenant-1"), {
      tenantId: "tenant-1",
      maxConcurrentExecutions: 12,
      maxArtifactBytes: null,
    });

    const markup = renderWithHost(
      queryClient,
      <TenantQuotaSettingsSection tenantId="tenant-1" canManage />,
    );

    expect(markup).toContain("Tenant quotas");
    expect(markup).toContain("Edit tenant quotas");
    expect(markup).toContain("test-form-grid");
    expect(markup).toContain("Save tenant quotas");
  });
});
