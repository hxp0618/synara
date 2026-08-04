import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import {
  serviceAccountQueryKey,
  TenantServiceAccountSettingsSection,
} from "./TenantServiceAccountSettingsSection";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

describe("TenantServiceAccountSettingsSection", () => {
  it("renders SCIM machine identity governance through host primitives", () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
    });
    queryClient.setQueryData(serviceAccountQueryKey("tenant-1"), {
      items: [
        {
          id: "service-account-1",
          tenantId: "tenant-1",
          organizationId: null,
          name: "SCIM provisioner",
          description: "Directory sync",
          scopes: ["scim.read", "scim.write"],
          role: "member",
          rateLimitPerMinute: 600,
          lastUsedAt: null,
          status: "active",
          revokedAt: null,
          createdAt: "2026-07-30T00:00:00Z",
          updatedAt: "2026-07-30T00:00:00Z",
        },
      ],
    });

    const markup = renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
          <TenantServiceAccountSettingsSection tenantId="tenant-1" canManage />
        </EnterpriseUiHostProvider>
      </QueryClientProvider>,
    );

    expect(markup).toContain("Service Accounts and API Keys");
    expect(markup).toContain("api.access");
    expect(markup).toContain("Requests per minute");
    expect(markup).toContain("last used never");
    expect(markup).toContain("Create Service Account");
    expect(markup).toContain("SCIM provisioner");
    expect(markup).toContain("Rotate token");
    expect(markup).toContain("Revoke");
  });
});
