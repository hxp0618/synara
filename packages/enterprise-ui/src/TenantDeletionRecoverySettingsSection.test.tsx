import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import {
  TenantDeletionRecoverySettingsSection,
  tenantDeletionRequestsQueryKey,
} from "./TenantDeletionRecoverySettingsSection";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

describe("TenantDeletionRecoverySettingsSection", () => {
  it("keeps an owner recovery path visible after the Tenant leaves normal session lists", () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
    });
    queryClient.setQueryData(tenantDeletionRequestsQueryKey, {
      items: [
        {
          id: "tenant-1",
          slug: "enterprise",
          name: "Enterprise Tenant",
          status: "deleting",
          lifecycleVersion: 8,
          deletionRequestedAt: "2026-07-30T00:00:00Z",
          evaluationExpiresAt: null,
          suspendedAt: null,
          closedAt: "2026-07-29T00:00:00Z",
          entitlementProfileCode: "enterprise",
          region: "ap-east-1",
          role: "owner",
        },
      ],
    });

    const markup = renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
          <TenantDeletionRecoverySettingsSection onRestored={async () => undefined} />
        </EnterpriseUiHostProvider>
      </QueryClientProvider>,
    );

    expect(markup).toContain("Tenant deletion recovery");
    expect(markup).toContain("Enterprise Tenant");
    expect(markup).toContain("lifecycle version 8");
    expect(markup).toContain("Restore as closed");
    expect(markup).toContain("Workspace cleanup is incomplete");
  });
});
