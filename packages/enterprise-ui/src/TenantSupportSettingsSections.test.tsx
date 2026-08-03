import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { auditLogQueryKey, TenantAuditSettingsSection } from "./TenantAuditSettingsSection";
import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import {
  supportGrantsQueryKey,
  supportPolicyQueryKey,
  TenantSupportAccessSettingsSection,
} from "./TenantSupportAccessSettingsSection";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

function renderWithClient(queryClient: QueryClient, child: ReactNode): string {
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        {child}
      </EnterpriseUiHostProvider>
    </QueryClientProvider>,
  );
}

describe("shared Support destination sections", () => {
  it("renders immutable Audit rows and host-neutral export links", () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
    });
    queryClient.setQueryData(auditLogQueryKey("tenant-1", {}), {
      items: [
        {
          eventId: "event-1",
          tenantId: "tenant-1",
          actorType: "user",
          actorId: "user-1",
          action: "tenant.member.updated",
          resourceType: "tenant_member",
          resourceId: "member-1",
          requestId: "request-1",
          traceId: "trace-1",
          reason: "role change",
          metadata: {},
          occurredAt: "2026-07-30T00:00:00Z",
        },
      ],
      nextCursor: null,
    });

    const markup = renderWithClient(
      queryClient,
      <TenantAuditSettingsSection tenantId="tenant-1" />,
    );

    expect(markup).toContain("Search and export");
    expect(markup).toContain("tenant.member.updated");
    expect(markup).toContain("Download JSONL");
    expect(markup).toContain("/v1/tenants/tenant-1/audit-logs/export?format=jsonl");
  });

  it("renders Tenant-controlled Support policy and active-grant revocation", () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
    });
    queryClient.setQueryData(supportPolicyQueryKey("tenant-1"), {
      tenantId: "tenant-1",
      supportAccessEnabled: true,
      version: 2,
      reason: "approved support policy",
      updatedBy: "owner-1",
      createdAt: "2026-07-30T00:00:00Z",
      updatedAt: "2026-07-30T00:00:00Z",
    });
    queryClient.setQueryData(supportGrantsQueryKey("tenant-1"), {
      items: [
        {
          id: "grant-1",
          tenantId: "tenant-1",
          tenantName: "Tenant One",
          requesterUserId: "support-1",
          requesterEmail: "support@example.com",
          requesterDisplayName: "Support Engineer",
          status: "active",
          version: 3,
          reason: "Investigate request request-1",
          requestedDurationSeconds: 1800,
          requestedAt: "2026-07-30T00:00:00Z",
          decidedBy: "admin-1",
          decisionReason: "approved",
          decidedAt: "2026-07-30T00:01:00Z",
          expiresAt: "2026-07-30T00:31:00Z",
          revokedBy: null,
          revocationReason: null,
          revokedAt: null,
          createdAt: "2026-07-30T00:00:00Z",
          updatedAt: "2026-07-30T00:01:00Z",
        },
      ],
    });

    const markup = renderWithClient(
      queryClient,
      <TenantSupportAccessSettingsSection tenantId="tenant-1" canManage canReadQuota />,
    );

    expect(markup).toContain("Controlled access enabled");
    expect(markup).toContain("Tenant Support Access policy");
    expect(markup).toContain("Support Engineer");
    expect(markup).toContain("Revoke active Support Access");
    expect(markup).toContain("Support diagnostic snapshot");
    expect(markup).toContain("Download JSON");

    const auditOnlyMarkup = renderWithClient(
      queryClient,
      <TenantSupportAccessSettingsSection
        tenantId="tenant-1"
        canManage={false}
        canReadQuota={false}
      />,
    );
    expect(auditOnlyMarkup).not.toContain("Support diagnostic snapshot");
  });
});
