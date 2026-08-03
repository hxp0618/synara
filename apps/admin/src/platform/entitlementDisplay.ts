// FILE: entitlementDisplay.ts
// Purpose: Translate retained database profile/status codes into internal self-hosted product copy.
// Layer: Admin Platform presentation

export function entitlementProfileLabel(code: string): string {
  if (code === "enterprise") return "Enterprise profile";
  if (code === "standard") return "Standard profile";
  return `${code} profile`;
}

export function tenantLifecycleLabel(status: string): string {
  return status;
}
