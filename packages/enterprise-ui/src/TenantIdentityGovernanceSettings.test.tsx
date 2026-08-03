import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import {
  identityDomainsQueryKey,
  identityPolicyQueryKey,
  TenantIdentityGovernanceSettings,
} from "./TenantIdentityGovernanceSettings";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

function renderGovernance(canManage: boolean): string {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(identityDomainsQueryKey("tenant-1"), {
    items: [
      {
        id: "domain-1",
        tenantId: "tenant-1",
        domain: "example.com",
        status: "verified",
        verificationRecordName: "_synara.example.com",
        verificationExpiresAt: "2026-07-31T00:00:00Z",
        verifiedAt: "2026-07-30T01:00:00Z",
        revokedAt: null,
        createdAt: "2026-07-30T00:00:00Z",
        updatedAt: "2026-07-30T01:00:00Z",
      },
    ],
  });
  queryClient.setQueryData(identityPolicyQueryKey("tenant-1"), {
    tenantId: "tenant-1",
    ssoEnforcement: "required",
    version: 3,
    recoveryUserId: "owner-1",
    enforcementSetAt: "2026-07-30T01:00:00Z",
    updatedAt: "2026-07-30T01:00:00Z",
  });
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        <TenantIdentityGovernanceSettings
          canManage={canManage}
          members={[
            {
              tenantId: "tenant-1",
              userId: "owner-1",
              email: "owner@example.com",
              displayName: "Owner",
              role: "owner",
              status: "active",
              joinedAt: "2026-07-30T00:00:00Z",
              createdAt: "2026-07-30T00:00:00Z",
              updatedAt: "2026-07-30T00:00:00Z",
            },
          ]}
          tenantId="tenant-1"
        />
      </EnterpriseUiHostProvider>
    </QueryClientProvider>,
  );
}

describe("TenantIdentityGovernanceSettings", () => {
  it("renders verified Domain and SSO policy state to read-only operators", () => {
    const markup = renderGovernance(false);
    expect(markup).toContain("Domain verification");
    expect(markup).toContain("example.com");
    expect(markup).toContain("SSO enforcement");
    expect(markup).toContain("version 3");
    expect(markup).not.toContain("Create DNS challenge");
    expect(markup).not.toContain("Save SSO enforcement");
  });

  it("exposes DNS and versioned enforcement controls only to identity managers", () => {
    const markup = renderGovernance(true);
    expect(markup).toContain("Create DNS challenge");
    expect(markup).toContain("Save SSO enforcement");
    expect(markup).toContain("Select an active recovery Owner");
    expect(markup).not.toContain("Verify DNS");
    expect(markup).toContain(">Revoke<");
  });
});
