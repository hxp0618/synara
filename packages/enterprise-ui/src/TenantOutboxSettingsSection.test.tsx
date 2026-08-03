import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { TenantOutboxSettingsSection, tenantOutboxQueryKey } from "./TenantOutboxSettingsSection";
import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

function renderSection(canManage: boolean): string {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(tenantOutboxQueryKey("tenant-1", "dead-letter"), {
    items: [
      {
        id: "message-1",
        topic: "execution.queued",
        messageKey: "execution-1",
        status: "dead-letter",
        attempts: 5,
        availableAt: "2026-07-30T00:00:00Z",
        createdAt: "2026-07-30T00:00:00Z",
        deadLetteredAt: "2026-07-30T00:05:00Z",
        lastError: "secret provider response that must not be rendered",
      },
    ],
  });
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        <TenantOutboxSettingsSection canManage={canManage} tenantId="tenant-1" />
      </EnterpriseUiHostProvider>
    </QueryClientProvider>,
  );
}

describe("TenantOutboxSettingsSection", () => {
  it("shows redacted Outbox diagnostics to support read-only", () => {
    const markup = renderSection(false);
    expect(markup).toContain("Delivery Outbox");
    expect(markup).toContain("execution.queued");
    expect(markup).toContain("5 attempts");
    expect(markup).toContain("last error recorded (redacted from UI)");
    expect(markup).not.toContain("secret provider response");
    expect(markup).not.toContain(">Replay<");
  });

  it("offers audited dead-letter replay only to Outbox managers", () => {
    expect(renderSection(true)).toContain(">Replay<");
  });
});
