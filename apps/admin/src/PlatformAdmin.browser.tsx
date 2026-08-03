// FILE: PlatformAdmin.browser.tsx
// Purpose: Verify responsive and keyboard behavior in a real Chromium Admin host.
// Layer: Admin browser UI test

import "./index.css";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cdp, page, userEvent } from "vitest/browser";
import { afterEach, describe, expect, it } from "vitest";
import { render } from "vitest-browser-react";
import { useState } from "react";

import { PlatformAdminShell } from "./platform/PlatformAdminShell";
import { platformQueryKeys } from "./platform/platformQueries";
import {
  adminSession,
  desktopAccess,
  entitlementSnapshot,
  pendingGrant,
  platformOverview,
  platformTenant,
} from "./testFixtures";

function createQueryClient() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(platformQueryKeys.supportAccess, { items: [pendingGrant] });
  client.setQueryData(platformQueryKeys.entitlements(platformTenant.id), entitlementSnapshot);
  client.setQueryData(platformQueryKeys.desktopAccess(platformTenant.id), desktopAccess);
  client.setQueryData(platformQueryKeys.profile, {
    internalStatusBoard: { configured: true, url: "https://status.synara.example" },
  });
  client.setQueryData(platformQueryKeys.incidents, { items: [] });
  client.setQueryData(platformQueryKeys.incidentExercises, { items: [] });
  client.setQueryData(platformQueryKeys.operationsExercises, { items: [] });
  client.setQueryData(platformQueryKeys.internalCostReviews, { items: [] });
  client.setQueryData(platformQueryKeys.sloWindows, { items: [] });
  client.setQueryData(platformQueryKeys.recoveryDrills, { items: [] });
  client.setQueryData(platformQueryKeys.penetrationEngagements, { items: [] });
  client.setQueryData(platformQueryKeys.capacityRuns, { items: [] });
  client.setQueryData(platformQueryKeys.releaseCandidates, { items: [] });
  client.setQueryData(
    ["control-plane", "platform", "incident-operators", adminSession.user.activeTenantId],
    {
      items: [
        {
          tenantId: adminSession.user.activeTenantId,
          userId: adminSession.user.userId,
          email: adminSession.user.email,
          displayName: adminSession.user.displayName,
          role: "owner",
          status: "active",
          joinedAt: "2026-08-01T00:00:00Z",
          createdAt: "2026-08-01T00:00:00Z",
          updatedAt: "2026-08-01T00:00:00Z",
        },
        {
          tenantId: adminSession.user.activeTenantId,
          userId: "incident-browser-comms",
          email: "incident-browser-comms@example.test",
          displayName: "Incident Browser Comms",
          role: "admin",
          status: "active",
          joinedAt: "2026-08-01T00:00:00Z",
          createdAt: "2026-08-01T00:00:00Z",
          updatedAt: "2026-08-01T00:00:00Z",
        },
      ],
    },
  );
  return client;
}

function PlatformAdminHarness(props: { readonly role?: "owner" | "security_admin" }) {
  const [client] = useState(createQueryClient);
  return (
    <QueryClientProvider client={client}>
      <PlatformAdminShell
        session={adminSession}
        overview={platformOverview(props.role ?? "owner")}
      />
    </QueryClientProvider>
  );
}

async function emulateReducedMotion(value: "reduce" | "no-preference") {
  const session = (await cdp()) as {
    send(method: string, params: Record<string, unknown>): Promise<unknown>;
  };
  await session.send("Emulation.setEmulatedMedia", {
    features: [{ name: "prefers-reduced-motion", value }],
  });
}

describe("Platform Admin browser boundary", () => {
  afterEach(async () => {
    await emulateReducedMotion("no-preference");
    await page.viewport(1280, 800);
    document.body.innerHTML = "";
  });

  it("keeps authority, filters, and Tenant detail usable at 390px without page overflow", async () => {
    await page.viewport(390, 844);
    await render(<PlatformAdminHarness />);

    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(
      document.documentElement.clientWidth + 1,
    );
    await expect.element(page.getByText("Platform authority")).toBeVisible();
    await expect.element(page.getByRole("heading", { name: "Tenant operations" })).toBeVisible();
    await expect.element(page.getByRole("heading", { name: "Northstar Labs" })).toBeVisible();
    await expect.element(page.getByLabelText("Search Tenants")).toBeVisible();
  });

  it("keeps the first navigation and filters in keyboard order", async () => {
    await render(<PlatformAdminHarness />);

    await userEvent.keyboard("{Tab}");
    expect(document.activeElement).toBe(page.getByRole("button", { name: "Overview" }).element());
    await userEvent.keyboard("{Tab}");
    expect(document.activeElement).toBe(page.getByRole("button", { name: "Tenants" }).element());
  });

  it("navigates to Support Access and remains operable with reduced motion requested", async () => {
    await emulateReducedMotion("reduce");
    expect(window.matchMedia("(prefers-reduced-motion: reduce)").matches).toBe(true);
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "Support Access" }).click();
    await expect
      .element(page.getByRole("heading", { name: "Platform Support Access" }))
      .toBeVisible();
    await expect.element(page.getByRole("button", { name: "Submit for approval" })).toBeVisible();
  });

  it("shows exact Desktop Enrollment context before issuing a one-time handle", async () => {
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "Desktop access" }).click();
    await expect
      .element(page.getByRole("heading", { name: "Generate and open Synara Desktop" }))
      .toBeVisible();
    await expect.element(page.getByText("https://control.synara.example")).toBeVisible();
    await expect.element(page.getByText("3 minutes")).toBeVisible();
    await expect
      .element(page.getByRole("option", { name: /Internal User · member · IdP required/ }))
      .toBeDisabled();
  });

  it("opens incident governance without leaving Platform authority", async () => {
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "Incidents" }).click();
    await expect.element(page.getByRole("heading", { name: "Incident governance" })).toBeVisible();
    await expect
      .element(page.getByRole("button", { name: "Open governed incident" }))
      .toBeVisible();
    await expect.element(page.getByText("Platform authority")).toBeVisible();
  });

  it("opens Incident exercise governance without weakening external delivery authority", async () => {
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "Incident exercises" }).click();
    await expect.element(page.getByRole("heading", { name: "Incident exercises" })).toBeVisible();
    await expect
      .element(page.getByRole("heading", { name: "Import exact Incident exercise receipt" }))
      .toBeVisible();
    await expect.element(page.getByText("Platform authority")).toBeVisible();
  });

  it("opens Operations exercise governance without weakening deployed evidence authority", async () => {
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "Operations exercises" }).click();
    await expect.element(page.getByRole("heading", { name: "Operations exercises" })).toBeVisible();
    await expect
      .element(page.getByRole("heading", { name: "Import exact Operations exercise receipt" }))
      .toBeVisible();
    await expect.element(page.getByText("Platform authority")).toBeVisible();
  });

  it("does not expose payment or Billing exercise product surfaces", async () => {
    await render(<PlatformAdminHarness />);

    expect(page.getByRole("button", { name: "Billing exercises" }).elements()).toHaveLength(0);
    expect(page.getByRole("button", { name: "Payments" }).elements()).toHaveLength(0);
  });

  it("exposes only the configured credential-free Internal status origin", async () => {
    await render(<PlatformAdminHarness />);

    const statusLink = page.getByRole("link", { name: "Internal status" });
    await expect.element(statusLink).toBeVisible();
    expect(statusLink.element().getAttribute("href")).toBe("https://status.synara.example");
    expect(statusLink.element().getAttribute("target")).toBe("_blank");
  });

  it("opens internal usage and cost review without exposing payment semantics", async () => {
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "Usage & cost reviews" }).click();
    await expect
      .element(page.getByRole("heading", { name: "Internal usage & cost reviews" }))
      .toBeVisible();
    await expect
      .element(page.getByRole("heading", { name: "Import exact internal usage and cost receipt" }))
      .toBeVisible();
    expect(page.getByRole("button", { name: "Payments" }).elements()).toHaveLength(0);
    expect(page.getByText(/payment|invoice|checkout|stripe|billing/i).elements()).toHaveLength(0);
  });

  it("opens SLO governance with exact receipt import and retained authority context", async () => {
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "SLO windows" }).click();
    await expect.element(page.getByRole("heading", { name: "SLO windows" })).toBeVisible();
    await expect
      .element(page.getByRole("heading", { name: "Import exact SLO validator receipt" }))
      .toBeVisible();
    await expect.element(page.getByText("Platform authority")).toBeVisible();
  });

  it("opens Penetration governance without weakening the external assessor boundary", async () => {
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "Penetration reviews" }).click();
    await expect.element(page.getByRole("heading", { name: "Penetration reviews" })).toBeVisible();
    await expect
      .element(page.getByRole("heading", { name: "Import exact Penetration receipt" }))
      .toBeVisible();
    await expect.element(page.getByText("Platform authority")).toBeVisible();
  });

  it("opens Capacity governance without weakening the external execution boundary", async () => {
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "Capacity reviews" }).click();
    await expect.element(page.getByRole("heading", { name: "Capacity reviews" })).toBeVisible();
    await expect
      .element(page.getByRole("heading", { name: "Import exact Capacity receipt" }))
      .toBeVisible();
    await expect.element(page.getByText("Platform authority")).toBeVisible();
  });

  it("manages self-hosted entitlement profiles without Plan or subscription positioning", async () => {
    await render(<PlatformAdminHarness />);

    await page.getByRole("button", { name: "Entitlements" }).click();
    await expect
      .element(page.getByRole("heading", { name: "Manage internal entitlement profile" }))
      .toBeVisible();
    await expect.element(page.getByText("Entitlement profile", { exact: true })).toBeVisible();
    expect(page.getByRole("heading", { name: /Plan|Subscription/i }).elements()).toHaveLength(0);
    expect(page.getByText(/payment|invoice|checkout|stripe|billing/i).elements()).toHaveLength(0);
  });

  it("does not render write navigation for security-only authority", async () => {
    await render(<PlatformAdminHarness role="security_admin" />);

    await expect.element(page.getByText("security admin")).toBeVisible();
    await expect.element(page.getByRole("button", { name: "Support Access" })).toBeVisible();
    expect(page.getByRole("button", { name: "Entitlements" }).elements()).toHaveLength(0);
    expect(page.getByRole("button", { name: "Provision Tenant" }).elements()).toHaveLength(0);
  });
});
