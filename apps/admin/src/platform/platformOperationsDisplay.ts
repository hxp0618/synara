// FILE: platformOperationsDisplay.ts
// Purpose: Keep Platform triage copy and units consistent and testable.
// Layer: Admin Platform presentation

import type { ControlPlanePlatformTenantOverview } from "@synara/control-plane-client";

type PlatformTenant = ControlPlanePlatformTenantOverview["items"][number];

export function platformTenantOperationsLines(
  tenant: PlatformTenant,
  generatedAt: string,
): ReadonlyArray<string> {
  return [
    `${tenant.activeMemberCount} members · ${tenant.organizationCount} Organizations · ${tenant.sessionCount} Sessions`,
    `${tenant.executionTargetCount} Targets · ${tenant.workerCount} Workers · ${tenant.offlineWorkerCount} offline`,
    `${tenant.activeExecutionCount} active Executions · ${tenant.queuedExecutionCount} queued${formatOldestQueueAge(tenant.oldestQueuedAt, generatedAt)} · ${tenant.failedExecutionCount24h} failed in 24h`,
    `${tenant.artifactCount} Artifacts · ${tenant.pendingArtifactCount} pending · ${formatBytes(tenant.artifactBytes)}`,
    `${tenant.activeCredentialCount} active Credentials · ${tenant.unavailableCredentialCount} unavailable · ${tenant.activeIdentityConnectionCount} active / ${tenant.disabledIdentityConnectionCount} disabled Identity Connections`,
  ];
}

export function formatOldestQueueAge(oldestQueuedAt: string | null, generatedAt: string): string {
  if (oldestQueuedAt == null) return "";
  const ageMillis = Date.parse(generatedAt) - Date.parse(oldestQueuedAt);
  if (!Number.isFinite(ageMillis) || ageMillis <= 0) return "";
  const minutes = Math.floor(ageMillis / 60_000);
  if (minutes < 1) return " (under 1m)";
  if (minutes < 60) return ` (${minutes}m oldest)`;
  const hours = Math.floor(minutes / 60);
  const remainder = minutes % 60;
  return ` (${hours}h${remainder > 0 ? ` ${remainder}m` : ""} oldest)`;
}

export function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"] as const;
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  const amount = value / 1024 ** index;
  return `${amount >= 10 || index === 0 ? amount.toFixed(0) : amount.toFixed(1)} ${units[index]}`;
}
