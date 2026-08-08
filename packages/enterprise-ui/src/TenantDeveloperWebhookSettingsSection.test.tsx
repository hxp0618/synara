import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import {
  developerWebhookDeliveryQueryKey,
  developerWebhookQueryKey,
  TenantDeveloperWebhookSettingsSection,
} from "./TenantDeveloperWebhookSettingsSection";
import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

describe("TenantDeveloperWebhookSettingsSection", () => {
  it("renders signed delivery governance without rendering encrypted or response data", () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
    });
    queryClient.setQueryData(developerWebhookQueryKey("tenant-1"), {
      items: [
        {
          id: "webhook-1",
          tenantId: "tenant-1",
          name: "CI events",
          url: "https://example.com/hooks/polaris",
          status: "active",
          eventTypes: ["turn.completed", "execution.failed"],
          secretVersion: 2,
          lastDeliveredAt: null,
          lastFailedAt: null,
          lastFailureSummary: null,
          createdBy: "user-1",
          createdAt: "2026-08-04T00:00:00Z",
          updatedAt: "2026-08-04T00:00:00Z",
          revokedAt: null,
        },
      ],
    });
    queryClient.setQueryData(developerWebhookDeliveryQueryKey("tenant-1", "webhook-1"), {
      items: [
        {
          id: "delivery-1",
          endpointId: "webhook-1",
          sessionEventId: "event-1",
          eventType: "execution.failed",
          sessionId: "session-1",
          executionId: "execution-1",
          sequence: 17,
          status: "dead-letter",
          attempts: 5,
          availableAt: "2026-08-04T00:05:00Z",
          createdAt: "2026-08-04T00:00:00Z",
          publishedAt: null,
          deadLetteredAt: "2026-08-04T00:05:00Z",
          lastError: "Webhook destination returned HTTP 503.",
        },
      ],
    });
    const markup = renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
          <TenantDeveloperWebhookSettingsSection tenantId="tenant-1" canManage />
        </EnterpriseUiHostProvider>
      </QueryClientProvider>,
    );

    expect(markup).toContain("Developer Webhooks");
    expect(markup).toContain("HMAC signature");
    expect(markup).toContain("https://example.com/hooks/polaris");
    expect(markup).toContain("secret v2");
    expect(markup).toContain("Rotate secret");
    expect(markup).toContain("Delivery delivery");
    expect(markup).toContain("dead-letter");
    expect(markup).toContain("Replay");
    expect(markup).toContain("Revoke");
    expect(markup).not.toContain("EncryptedSecret");
  });
});
