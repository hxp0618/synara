// FILE: navigation.ts
// Purpose: Define the bounded Platform Admin destinations and safe Tenant-Web handoff.
// Layer: Admin navigation

export type AdminDestination =
  | "tenants"
  | "provision"
  | "entitlements"
  | "desktop"
  | "support"
  | "releases"
  | "incidents"
  | "incident-exercises"
  | "operations-exercises"
  | "internal-cost-reviews"
  | "slo"
  | "recovery"
  | "penetration"
  | "capacity"
  | "compliance"
  | "providers"
  | "authorities";

function safeTenantAppUrl(value: unknown): string | null {
  if (typeof value !== "string" || value.trim().length === 0) return null;
  const resolved = new URL(value, window.location.origin);
  return resolved.protocol === "http:" || resolved.protocol === "https:"
    ? resolved.toString()
    : null;
}

export async function tenantAppUrl(): Promise<string> {
  const runtime = await fetch("/admin-config.json", {
    credentials: "same-origin",
    cache: "no-store",
  })
    .then(async (response) =>
      response.ok ? ((await response.json()) as { tenantAppUrl?: unknown }) : null,
    )
    .catch(() => null);
  return (
    safeTenantAppUrl(runtime?.tenantAppUrl) ??
    safeTenantAppUrl(import.meta.env.VITE_TENANT_APP_URL) ??
    window.location.origin
  );
}

export async function navigateToTenantApp(): Promise<void> {
  window.location.assign(await tenantAppUrl());
}
