// FILE: EnvironmentSessionUsageSection.tsx
// Purpose: Explain authoritative Control Plane cost per Session and per Turn.
// Layer: Environment panel section
// Exports: EnvironmentSessionUsageSection

import { useQuery } from "@tanstack/react-query";

import { useControlPlane } from "~/controlPlaneContext";
import { controlPlaneClient } from "@synara/control-plane-client";
import {
  formatUsageBytes,
  formatUsageCost,
  formatUsageDuration,
  formatUsageTokens,
  groupSessionUsageByTurn,
} from "@synara/enterprise-ui";
import { ClockIcon, RefreshCwIcon } from "~/lib/icons";

import {
  ENVIRONMENT_ROW_ICON_CLASS_NAME,
  EnvironmentCollapsibleSection,
  EnvironmentRow,
  EnvironmentSectionDivider,
} from "./EnvironmentRow";

function sessionUsageQueryKey(tenantId: string | null, sessionId: string | null) {
  return ["control-plane", "tenants", tenantId, "sessions", sessionId, "usage"] as const;
}

function addCostTotals(target: Record<string, number>, source: Readonly<Record<string, number>>) {
  for (const [currency, amount] of Object.entries(source)) {
    target[currency] = (target[currency] ?? 0) + amount;
  }
}

function formatCostBreakdown(
  provider: Readonly<Record<string, number>>,
  platform: Readonly<Record<string, number>>,
  total: Readonly<Record<string, number>>,
  providerCostReportedCount: number,
  providerCostMissingCount: number,
): string {
  const providerText =
    providerCostReportedCount === 0
      ? "Provider cost unavailable"
      : providerCostMissingCount > 0
        ? `Provider ${formatCostTotals(provider)} known (${providerCostMissingCount} ${providerCostMissingCount === 1 ? "report" : "reports"} unavailable)`
        : `Provider ${formatCostTotals(provider)}`;
  if (Object.keys(platform).length === 0) {
    return `${providerText} · platform allocation unavailable`;
  }
  return `${providerText} + platform ${formatCostTotals(platform)} = known subtotal ${formatCostTotals(total)}`;
}

function formatCostTotals(totals: Readonly<Record<string, number>>): string {
  const values = Object.entries(totals)
    .toSorted(([left], [right]) => left.localeCompare(right))
    .map(([currency, amount]) => formatUsageCost(amount, currency));
  return values.length > 0 ? values.join(" · ") : "No cost reported";
}

export function EnvironmentSessionUsageSection(props: {
  enabled: boolean;
  sessionId: string | null;
}) {
  const controlPlane = useControlPlane();
  const tenantId = controlPlane.activeTenant?.id ?? null;
  const canLoad =
    props.enabled && controlPlane.isAuthoritative && tenantId !== null && props.sessionId !== null;
  const usage = useQuery({
    queryKey: sessionUsageQueryKey(tenantId, props.sessionId),
    queryFn: () => controlPlaneClient.getSessionUsage(props.sessionId!),
    enabled: canLoad,
    retry: false,
    refetchInterval: canLoad ? 30_000 : false,
  });

  if (!controlPlane.isAuthoritative || props.sessionId === null) return null;

  const groups = groupSessionUsageByTurn(usage.data?.items ?? []);
  const totalTokens = groups.reduce((sum, group) => sum + group.totalTokens, 0);
  const totalDurationMillis = groups.reduce((sum, group) => sum + group.durationMillis, 0);
  const totalNetworkIngressBytes = groups.reduce(
    (sum, group) => sum + group.networkIngressBytes,
    0,
  );
  const totalNetworkEgressBytes = groups.reduce((sum, group) => sum + group.networkEgressBytes, 0);
  const providerCostByCurrency: Record<string, number> = {};
  const platformCostByCurrency: Record<string, number> = {};
  const totalCostByCurrency: Record<string, number> = {};
  let providerCostReportedCount = 0;
  let providerCostMissingCount = 0;
  for (const group of groups) {
    addCostTotals(providerCostByCurrency, group.providerCostByCurrency);
    addCostTotals(platformCostByCurrency, group.platformCostByCurrency);
    addCostTotals(totalCostByCurrency, group.totalCostByCurrency);
    providerCostReportedCount += group.providerCostReportedCount;
    providerCostMissingCount += group.providerCostMissingCount;
  }

  return (
    <>
      <EnvironmentSectionDivider />
      <EnvironmentCollapsibleSection label="Session usage" defaultOpen={false}>
        {usage.isPending ? (
          <p
            className="px-2 py-1 text-[length:var(--app-font-size-ui-sm,11px)] text-muted-foreground"
            role="status"
          >
            Loading Session usage…
          </p>
        ) : usage.error ? (
          <EnvironmentRow
            icon={<RefreshCwIcon className={ENVIRONMENT_ROW_ICON_CLASS_NAME} />}
            label="Could not load Session usage"
            trailing="Retry"
            onClick={() => void usage.refetch()}
          />
        ) : groups.length === 0 ? (
          <p className="px-2 py-1 text-[length:var(--app-font-size-ui-sm,11px)] text-muted-foreground">
            Usage will appear after the first runtime report.
          </p>
        ) : (
          <div className="flex flex-col">
            <div className="flex items-start gap-2 rounded-lg px-2 py-1 text-[length:var(--app-font-size-ui,12px)]">
              <ClockIcon className={ENVIRONMENT_ROW_ICON_CLASS_NAME} aria-hidden />
              <div className="min-w-0 flex-1">
                <div className="flex items-center justify-between gap-2">
                  <span className="font-medium text-foreground">Session total</span>
                  <span className="shrink-0 tabular-nums text-foreground">
                    {formatCostTotals(totalCostByCurrency)}
                  </span>
                </div>
                <p className="text-[length:var(--app-font-size-ui-sm,11px)] text-muted-foreground">
                  {formatUsageTokens(totalTokens)} tokens ·{" "}
                  {formatUsageDuration(Math.ceil(totalDurationMillis / 1000))} · {groups.length}{" "}
                  {groups.length === 1 ? "Turn" : "Turns"}
                </p>
                <p className="text-[length:var(--app-font-size-ui-sm,11px)] text-muted-foreground">
                  {formatCostBreakdown(
                    providerCostByCurrency,
                    platformCostByCurrency,
                    totalCostByCurrency,
                    providerCostReportedCount,
                    providerCostMissingCount,
                  )}
                </p>
                <p className="text-[length:var(--app-font-size-ui-sm,11px)] text-muted-foreground">
                  Network in {formatUsageBytes(totalNetworkIngressBytes)} · out{" "}
                  {formatUsageBytes(totalNetworkEgressBytes)}
                </p>
              </div>
            </div>
            {groups.map((group, index) => (
              <div
                key={group.turnId}
                className="rounded-lg px-2 py-1 pl-8 text-[length:var(--app-font-size-ui-sm,11px)]"
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="min-w-0 truncate font-medium text-foreground">
                    Turn {index + 1}
                  </span>
                  <span className="shrink-0 tabular-nums text-foreground">
                    {formatCostTotals(group.totalCostByCurrency)}
                  </span>
                </div>
                <p className="truncate text-muted-foreground">
                  {group.providers.join(", ")} · {group.models.join(", ") || "model unavailable"} ·{" "}
                  {formatUsageTokens(group.totalTokens)} tokens ·{" "}
                  {formatUsageDuration(Math.ceil(group.durationMillis / 1000))}
                  {group.executionCount > 1 ? ` · ${group.executionCount} executions` : ""}
                  {!group.final ? " · updating" : ""}
                </p>
                <p className="text-muted-foreground">
                  {formatCostBreakdown(
                    group.providerCostByCurrency,
                    group.platformCostByCurrency,
                    group.totalCostByCurrency,
                    group.providerCostReportedCount,
                    group.providerCostMissingCount,
                  )}
                </p>
                <p className="truncate text-muted-foreground">
                  Platform allocation:{" "}
                  {group.platformCharges
                    .map((charge) => `${charge.kind} (${charge.source})`)
                    .join(", ") || "none"}
                  {" · "}network in {formatUsageBytes(group.networkIngressBytes)} · out{" "}
                  {formatUsageBytes(group.networkEgressBytes)}
                </p>
              </div>
            ))}
          </div>
        )}
      </EnvironmentCollapsibleSection>
    </>
  );
}
