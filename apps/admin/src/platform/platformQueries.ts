// FILE: platformQueries.ts
// Purpose: Centralize cache keys and fixed Platform-role decisions for the Admin host.
// Layer: Admin Platform domain

import type { ControlPlanePlatformTenantOverview } from "@synara/control-plane-client";

export const platformQueryKeys = {
  profile: ["control-plane", "platform", "profile"] as const,
  tenants: ["control-plane", "platform", "tenants"] as const,
  supportAccess: ["control-plane", "platform", "support-access"] as const,
  releaseCandidates: ["control-plane", "platform", "release-candidates"] as const,
  releaseReadiness: (candidateRecordId: string) =>
    ["control-plane", "platform", "release-candidates", candidateRecordId, "readiness"] as const,
  incidents: ["control-plane", "platform", "incidents"] as const,
  incidentExercises: ["control-plane", "platform", "incident-exercises"] as const,
  operationsExercises: ["control-plane", "platform", "operations-exercises"] as const,
  internalCostReviews: ["control-plane", "platform", "internal-cost-reviews"] as const,
  sloWindows: ["control-plane", "platform", "slo-windows"] as const,
  recoveryDrills: ["control-plane", "platform", "recovery-drills"] as const,
  penetrationEngagements: ["control-plane", "platform", "penetration-engagements"] as const,
  capacityRuns: ["control-plane", "platform", "capacity-runs"] as const,
  compliancePrograms: ["control-plane", "platform", "compliance-programs"] as const,
  providerCommercialAuthorizations: [
    "control-plane",
    "platform",
    "provider-commercial-authorizations",
  ] as const,
  governanceAuthorities: ["control-plane", "platform", "governance-authorities"] as const,
  entitlements: (tenantId: string) =>
    ["control-plane", "platform", "tenants", tenantId, "entitlements"] as const,
  desktopAccess: (tenantId: string) =>
    ["control-plane", "platform", "tenants", tenantId, "desktop-access"] as const,
};

export type PlatformOperatorRole = ControlPlanePlatformTenantOverview["operatorRole"];

export function canManagePlatform(role: PlatformOperatorRole): boolean {
  return role === "owner" || role === "admin";
}
