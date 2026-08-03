// FILE: usageDisplay.ts
// Purpose: Pure display formatting shared by Tenant and Session usage surfaces.
// Layer: Control Plane presentation logic
// Exports: Duration, byte, token, cost, period, and progress formatters.

import type { ControlPlaneSessionUsage } from "@synara/control-plane-client";

export type SessionUsageTurnGroup = {
  turnId: string;
  providers: ReadonlyArray<string>;
  models: ReadonlyArray<string>;
  executionCount: number;
  totalTokens: number;
  networkIngressBytes: number;
  networkEgressBytes: number;
  durationMillis: number;
  providerCostByCurrency: Readonly<Record<string, number>>;
  providerCostReportedCount: number;
  providerCostMissingCount: number;
  platformCharges: ReadonlyArray<{
    kind: string;
    currencyCode: string;
    amountMicros: number;
    source: "estimated" | "actual";
  }>;
  platformCostByCurrency: Readonly<Record<string, number>>;
  totalCostByCurrency: Readonly<Record<string, number>>;
  final: boolean;
};

export function formatUsageDuration(seconds: number): string {
  const safeSeconds = Math.max(0, Math.trunc(seconds));
  if (safeSeconds < 60) return `${safeSeconds}s`;
  const hours = Math.floor(safeSeconds / 3600);
  const minutes = Math.floor((safeSeconds % 3600) / 60);
  if (hours === 0) return `${minutes}m`;
  return minutes === 0 ? `${hours}h` : `${hours}h ${minutes}m`;
}

export function formatUsageBytes(bytes: number): string {
  const safeBytes = Math.max(0, bytes);
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"] as const;
  let value = safeBytes;
  let unitIndex = 0;
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex += 1;
  }
  const precision = value >= 100 || unitIndex === 0 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(precision)} ${units[unitIndex]}`;
}

export function formatUsageTokens(tokens: number): string {
  return Math.max(0, Math.trunc(tokens)).toLocaleString("en-US");
}

export function formatUsageCost(amountMicros: number, currencyCode: string): string {
  const amount = Math.max(0, amountMicros) / 1_000_000;
  try {
    return new Intl.NumberFormat("en-US", {
      style: "currency",
      currency: currencyCode,
      minimumFractionDigits: 2,
      maximumFractionDigits: 4,
    }).format(amount);
  } catch {
    return `${currencyCode} ${amount.toFixed(4)}`;
  }
}

export function formatUsagePeriod(start: string, end: string): string {
  const formatter = new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    year: "numeric",
    timeZone: "UTC",
  });
  return `${formatter.format(new Date(start))} – ${formatter.format(new Date(end))}`;
}

export function usageProgressPercent(observed: number, limit: number | null): number {
  if (limit === null || limit <= 0) return 0;
  return Math.min(100, Math.max(0, (observed / limit) * 100));
}

export function groupSessionUsageByTurn(
  items: ControlPlaneSessionUsage["items"],
): ReadonlyArray<SessionUsageTurnGroup> {
  const groups = new Map<
    string,
    {
      turnId: string;
      providers: Set<string>;
      models: Set<string>;
      executionCount: number;
      totalTokens: number;
      networkIngressBytes: number;
      networkEgressBytes: number;
      durationMillis: number;
      providerCostByCurrency: Record<string, number>;
      providerCostReportedCount: number;
      providerCostMissingCount: number;
      platformCharges: Map<
        string,
        {
          kind: string;
          currencyCode: string;
          amountMicros: number;
          source: "estimated" | "actual";
        }
      >;
      platformCostByCurrency: Record<string, number>;
      totalCostByCurrency: Record<string, number>;
      final: boolean;
    }
  >();
  for (const item of items) {
    const current = groups.get(item.turnId) ?? {
      turnId: item.turnId,
      providers: new Set<string>(),
      models: new Set<string>(),
      executionCount: 0,
      totalTokens: 0,
      networkIngressBytes: 0,
      networkEgressBytes: 0,
      durationMillis: 0,
      providerCostByCurrency: {} as Record<string, number>,
      providerCostReportedCount: 0,
      providerCostMissingCount: 0,
      platformCharges: new Map(),
      platformCostByCurrency: {} as Record<string, number>,
      totalCostByCurrency: {} as Record<string, number>,
      final: true,
    };
    current.providers.add(item.provider);
    if (item.model) current.models.add(item.model);
    current.executionCount += 1;
    current.totalTokens += item.totalTokens;
    current.networkIngressBytes += item.networkIngressBytes;
    current.networkEgressBytes += item.networkEgressBytes;
    current.durationMillis += item.durationMillis;
    current.final &&= item.final;
    if (item.providerCostReported) {
      current.providerCostReportedCount += 1;
      current.providerCostByCurrency[item.providerCurrency] =
        (current.providerCostByCurrency[item.providerCurrency] ?? 0) + item.providerCostMicros;
    } else {
      current.providerCostMissingCount += 1;
    }
    for (const charge of item.platformCharges) {
      const key = `${charge.currencyCode}\u0000${charge.kind}\u0000${charge.source}`;
      const aggregate = current.platformCharges.get(key) ?? {
        kind: charge.kind,
        currencyCode: charge.currencyCode,
        amountMicros: 0,
        source: charge.source,
      };
      aggregate.amountMicros += charge.amountMicros;
      current.platformCharges.set(key, aggregate);
      current.platformCostByCurrency[charge.currencyCode] =
        (current.platformCostByCurrency[charge.currencyCode] ?? 0) + charge.amountMicros;
    }
    for (const [currency, amount] of Object.entries(item.totalCostByCurrency)) {
      current.totalCostByCurrency[currency] = (current.totalCostByCurrency[currency] ?? 0) + amount;
    }
    groups.set(item.turnId, current);
  }
  return [...groups.values()].map((group) => ({
    turnId: group.turnId,
    providers: [...group.providers].toSorted(),
    models: [...group.models].toSorted(),
    executionCount: group.executionCount,
    totalTokens: group.totalTokens,
    networkIngressBytes: group.networkIngressBytes,
    networkEgressBytes: group.networkEgressBytes,
    durationMillis: group.durationMillis,
    providerCostByCurrency: group.providerCostByCurrency,
    providerCostReportedCount: group.providerCostReportedCount,
    providerCostMissingCount: group.providerCostMissingCount,
    platformCharges: [...group.platformCharges.values()].toSorted(
      (left, right) =>
        left.currencyCode.localeCompare(right.currencyCode) ||
        left.kind.localeCompare(right.kind) ||
        left.source.localeCompare(right.source),
    ),
    platformCostByCurrency: group.platformCostByCurrency,
    totalCostByCurrency: group.totalCostByCurrency,
    final: group.final,
  }));
}
