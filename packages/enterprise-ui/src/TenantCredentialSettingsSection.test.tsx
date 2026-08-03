import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import {
  credentialScopePolicyQueryKey,
  credentialsQueryKey,
  TenantCredentialSettingsSection,
} from "./TenantCredentialSettingsSection";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

describe("TenantCredentialSettingsSection", () => {
  it("renders Credential metadata for support read-only without mutation controls", () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
    });
    queryClient.setQueryData(credentialsQueryKey("tenant-1"), {
      items: [
        {
          id: "credential-1",
          tenantId: "tenant-1",
          organizationId: null,
          scope: "tenant",
          scopeUserId: null,
          selectorOrganizationId: null,
          selectorModel: "gpt-5.6-sol",
          autoSelectEnabled: true,
          name: "Enterprise Provider",
          purpose: "provider",
          provider: "codex",
          credentialType: "api_key",
          kmsProvider: "aws-kms",
          kmsKeyId: "alias/synara",
          version: 3,
          createdBy: "owner-1",
          updatedBy: "owner-1",
          createdAt: "2026-07-30T00:00:00Z",
          updatedAt: "2026-07-30T01:00:00Z",
          expiresAt: null,
          revokedAt: null,
        },
      ],
    });
    queryClient.setQueryData(credentialScopePolicyQueryKey("tenant-1"), {
      tenantId: "tenant-1",
      platformCredentialsEnabled: true,
      platformCredentialAutoSelect: false,
      updatedBy: "owner-1",
      createdAt: "2026-07-30T00:00:00Z",
      updatedAt: "2026-07-30T01:00:00Z",
    });

    const markup = renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
          <TenantCredentialSettingsSection
            canManage={false}
            members={[]}
            organizations={[]}
            tenantId="tenant-1"
          />
        </EnterpriseUiHostProvider>
      </QueryClientProvider>,
    );

    expect(markup).toContain("Enterprise Provider");
    expect(markup).toContain("model gpt-5.6-sol");
    expect(markup).toContain("explicit only");
    expect(markup).not.toContain("Add encrypted Credential");
    expect(markup).not.toContain("Rotate Credential");
    expect(markup).not.toContain(">Revoke<");
    expect(markup).not.toContain("Automatic on");
  });
});
