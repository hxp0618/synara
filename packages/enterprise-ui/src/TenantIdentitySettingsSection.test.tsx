import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import {
  identityDomainsQueryKey,
  identityPolicyQueryKey,
} from "./TenantIdentityGovernanceSettings";
import {
  identityConnectionsQueryKey,
  TenantIdentitySettingsSection,
} from "./TenantIdentitySettingsSection";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

function renderIdentity(canManage: boolean): string {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(identityConnectionsQueryKey("tenant-1"), { items: [] });
  queryClient.setQueryData(identityDomainsQueryKey("tenant-1"), { items: [] });
  queryClient.setQueryData(identityPolicyQueryKey("tenant-1"), {
    tenantId: "tenant-1",
    ssoEnforcement: "optional",
    version: 1,
    recoveryUserId: null,
    enforcementSetAt: null,
    updatedAt: "2026-07-30T00:00:00Z",
  });

  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        <TenantIdentitySettingsSection
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
          organizations={[]}
          tenantId="tenant-1"
        />
      </EnterpriseUiHostProvider>
    </QueryClientProvider>,
  );
}

describe("TenantIdentitySettingsSection", () => {
  it("renders the complete read-only enterprise identity surface", () => {
    const markup = renderIdentity(false);

    expect(markup).toContain("Enterprise identity");
    expect(markup).toContain("Domain verification");
    expect(markup).toContain("SSO enforcement");
    expect(markup).toContain("OIDC single sign-on");
    expect(markup).toContain("SAML single sign-on");
    expect(markup).toContain("No enterprise identity connection");
    expect(markup).not.toContain("Create OIDC connection");
  });

  it("exposes connection and governance forms to identity managers", () => {
    const markup = renderIdentity(true);

    expect(markup).toContain("Create DNS challenge");
    expect(markup).toContain("Save SSO enforcement");
    expect(markup).toContain("Create OIDC connection");
    expect(markup).toContain("Create SAML connection");
  });
});
