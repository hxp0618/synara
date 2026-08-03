// FILE: TenantUsageSettingsSection.tsx
// Purpose: Show current Tenant reporting-period usage, soft quota state, and internal cost.
// Layer: Settings UI component
// Exports: TenantUsageSettingsSection

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import {
  controlPlaneClient,
  resolveInternalCostAllocationExportUrl,
  type ControlPlaneInternalCostAllocationRow,
} from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

import {
  formatUsageBytes,
  formatUsageCost,
  formatUsageDuration,
  formatUsagePeriod,
  formatUsageTokens,
  usageProgressPercent,
} from "./usageDisplay";

export function tenantUsageQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "usage"] as const;
}

export function internalCostAllocationQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "internal-cost-allocation"] as const;
}

function stateLabel(state: string): string {
  return state.replaceAll("_", " ");
}

export function TenantUsageSettingsSection(props: { tenantId: string; canManageCost: boolean }) {
  const queryClient = useQueryClient();
  const [editor, setEditor] = useState<{
    projectId: string;
    projectName: string;
    costCenterCode: string;
    departmentCode: string;
    expectedVersion: number;
  } | null>(null);
  const {
    Badge,
    Button,
    FormField,
    InlineError,
    Input,
    LinkButton,
    RefreshIcon,
    SettingsCard,
    SettingsListRow,
    SettingsSectionShell,
    StatusPill,
    downloadJsonFile,
  } = useEnterpriseUiHost();
  const usage = useQuery({
    queryKey: tenantUsageQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.getTenantUsage(props.tenantId),
    retry: false,
    refetchInterval: 30_000,
  });
  const allocation = useQuery({
    queryKey: internalCostAllocationQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.getInternalCostAllocationReport(props.tenantId),
    retry: false,
    refetchInterval: 30_000,
  });
  const assignAllocation = useMutation({
    mutationFn: (input: NonNullable<typeof editor>) =>
      controlPlaneClient.putProjectCostAllocation(props.tenantId, input.projectId, {
        costCenterCode: input.costCenterCode,
        departmentCode: input.departmentCode,
        expectedVersion: input.expectedVersion,
      }),
    onSuccess: async () => {
      setEditor(null);
      await queryClient.invalidateQueries({
        queryKey: internalCostAllocationQueryKey(props.tenantId),
      });
    },
  });
  const exportUsage = useMutation({
    mutationFn: () => controlPlaneClient.getTenantUsageExport(props.tenantId),
    onSuccess: (result) => {
      downloadJsonFile(
        `synara-tenant-usage-${props.tenantId}-${result.usage.periodStart.slice(0, 10)}.json`,
        result.body,
      );
    },
  });

  if (usage.isPending) {
    return (
      <SettingsSectionShell title="Usage & internal cost">
        <SettingsCard>
          <SettingsListRow title="Loading current usage…" />
        </SettingsCard>
      </SettingsSectionShell>
    );
  }
  if (usage.error) {
    return (
      <SettingsSectionShell title="Usage & internal cost">
        <SettingsCard>
          <SettingsListRow
            title="Could not load usage and internal cost"
            description={usage.error instanceof Error ? usage.error.message : "The request failed."}
            actions={
              <Button size="sm" variant="outline" onClick={() => void usage.refetch()}>
                Retry
              </Button>
            }
          />
        </SettingsCard>
      </SettingsSectionShell>
    );
  }

  const data = usage.data;
  const quota = data.softQuota;
  const progress = usageProgressPercent(quota.observed, quota.limit);
  const providerCosts = Object.entries(data.usage.providerCostByCurrency)
    .toSorted(([left], [right]) => left.localeCompare(right))
    .map(([currency, amount]) => formatUsageCost(amount, currency));
  const platformCosts = Object.entries(data.usage.platformCostByCurrency)
    .toSorted(([left], [right]) => left.localeCompare(right))
    .map(([currency, amount]) => formatUsageCost(amount, currency));
  const knownCosts = Object.entries(data.usage.knownCostByCurrency)
    .toSorted(([left], [right]) => left.localeCompare(right))
    .map(([currency, amount]) => formatUsageCost(amount, currency));
  const platformChargeSources = data.usage.platformCharges
    .map((charge) => `${charge.kind} (${charge.source})`)
    .join(", ");

  return (
    <SettingsSectionShell
      title="Usage & internal cost"
      action={
        <Button
          size="xs"
          variant="outline"
          disabled={usage.isFetching}
          onClick={() => void usage.refetch()}
        >
          <RefreshIcon aria-hidden />
          {usage.isFetching ? "Refreshing…" : "Refresh"}
        </Button>
      }
    >
      <SettingsCard>
        <SettingsListRow
          title="Current reporting period"
          description={formatUsagePeriod(data.periodStart, data.periodEnd)}
          actions={
            quota.state === "approaching_limit" || quota.state === "limit_reached" ? (
              <Badge variant="warning">{stateLabel(quota.state)}</Badge>
            ) : (
              <StatusPill value={stateLabel(quota.state)} active={false} />
            )
          }
        />
        <SettingsListRow
          title="Usage export"
          description="Download the current-period Token, Network, Provider-cost coverage, and internal platform allocation snapshot for internal accounting."
          actions={
            <Button
              size="xs"
              variant="outline"
              disabled={exportUsage.isPending}
              onClick={() => exportUsage.mutate()}
            >
              {exportUsage.isPending ? "Preparing…" : "Export JSON"}
            </Button>
          }
        />
        <InlineError error={exportUsage.error} />
        <SettingsListRow
          align="start"
          title="Execution time"
          description={
            quota.limit === null
              ? `${formatUsageDuration(quota.observed)} used · no assigned soft limit`
              : `${formatUsageDuration(quota.observed)} of ${formatUsageDuration(quota.limit)} · soft limit, never an immediate hard stop`
          }
          actions={
            quota.limit === null ? null : (
              <div
                className="w-full min-w-36 sm:w-40"
                role="progressbar"
                aria-label="Execution time usage"
                aria-valuemin={0}
                aria-valuemax={quota.limit}
                aria-valuenow={Math.min(quota.observed, quota.limit)}
                aria-valuetext={`${quota.percentageUsed?.toFixed(1)}% used; ${formatUsageDuration(quota.observed)} observed`}
              >
                <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                  <div
                    className={`h-full rounded-full transition-[width] duration-500 ease-out motion-reduce:transition-none ${
                      quota.state === "approaching_limit" || quota.state === "limit_reached"
                        ? "bg-warning"
                        : "bg-primary"
                    }`}
                    style={{ width: `${progress}%` }}
                  />
                </div>
                <p className="mt-1 text-right text-[length:var(--app-font-size-ui-xs,10px)] tabular-nums text-muted-foreground">
                  {quota.percentageUsed?.toFixed(1)}%
                </p>
              </div>
            )
          }
        />
        {quota.recommendedAction ? (
          <SettingsListRow
            title={
              quota.state === "limit_reached" ? "Soft limit reached" : "Approaching soft limit"
            }
            description={quota.recommendedAction}
            actions={<Badge variant="success">Work continues</Badge>}
          />
        ) : null}
        <SettingsListRow
          title="Tokens"
          description={`${formatUsageTokens(data.usage.inputTokens)} input · ${formatUsageTokens(data.usage.cachedInputTokens)} cached · ${formatUsageTokens(data.usage.outputTokens)} output · ${formatUsageTokens(data.usage.reasoningTokens)} reasoning`}
          actions={
            <span className="text-xs tabular-nums text-foreground">
              {formatUsageTokens(data.usage.totalTokens)} total
            </span>
          }
        />
        <SettingsListRow
          title="Network"
          description={`${formatUsageBytes(data.usage.networkIngressBytes)} ingress · ${formatUsageBytes(data.usage.networkEgressBytes)} egress`}
        />
        <SettingsListRow
          title="Provider cost"
          description={
            data.usage.providerCostMissingCount > 0
              ? `${data.usage.providerCostMissingCount} execution ${data.usage.providerCostMissingCount === 1 ? "report is" : "reports are"} unavailable and excluded from these known totals.`
              : `All ${data.usage.providerCostReportedCount} execution cost reports are represented in these known totals.`
          }
          actions={
            <span className="text-xs tabular-nums text-foreground">
              {providerCosts.length > 0
                ? providerCosts.join(" · ")
                : data.usage.providerCostMissingCount > 0
                  ? "Unavailable"
                  : "No executions"}
            </span>
          }
        />
        <SettingsListRow
          title="Internal platform allocation"
          description={
            platformChargeSources
              ? `${platformChargeSources}. Actual cloud-cost allocations replace their corresponding estimates.`
              : "No shared Target cost has been allocated to executions in this period."
          }
          actions={
            <span className="text-xs tabular-nums text-foreground">
              {platformCosts.length > 0 ? platformCosts.join(" · ") : "Unavailable"}
            </span>
          }
        />
        <SettingsListRow
          title="Known internal cost subtotal"
          description={
            data.usage.providerCostMissingCount > 0
              ? "Excludes only the Provider portion of executions whose cost is unavailable; reported platform allocation remains included for internal usage and cost visibility."
              : "Provider and authoritative platform allocation combined by currency; shown for internal cost visibility only."
          }
          actions={
            <span className="text-xs tabular-nums text-foreground">
              {knownCosts.length > 0 ? knownCosts.join(" · ") : "Unavailable"}
            </span>
          }
        />
      </SettingsCard>
      <SettingsCard>
        <SettingsListRow
          title="Cost center & department allocation"
          description={
            allocation.data
              ? `${allocation.data.rows.length} active project ${allocation.data.rows.length === 1 ? "allocation" : "allocations"} in this period · ${allocation.data.unallocatedProjectCount} unallocated`
              : allocation.isPending
                ? "Loading current-period project allocation…"
                : "The internal cost allocation report is unavailable."
          }
          actions={
            <LinkButton
              href={resolveInternalCostAllocationExportUrl(props.tenantId)}
              size="xs"
              variant="outline"
            >
              Export CSV
            </LinkButton>
          }
        />
        {allocation.error ? (
          <SettingsListRow
            title="Could not load internal cost allocation"
            description={
              allocation.error instanceof Error ? allocation.error.message : "The request failed."
            }
            actions={
              <Button size="sm" variant="outline" onClick={() => void allocation.refetch()}>
                Retry
              </Button>
            }
          />
        ) : null}
        {allocation.data?.rows.map((row) => (
          <SettingsListRow
            key={row.projectId}
            title={row.projectName}
            description={`${row.costCenterCode} · ${row.departmentCode} · ${formatUsageTokens(row.totalTokens)} tokens · ${formatAllocationKnownCost(row)}`}
            actions={
              props.canManageCost ? (
                <Button
                  size="xs"
                  variant="outline"
                  onClick={() =>
                    setEditor({
                      projectId: row.projectId,
                      projectName: row.projectName,
                      costCenterCode: row.costCenterCode,
                      departmentCode: row.departmentCode,
                      expectedVersion: row.version,
                    })
                  }
                >
                  Assign
                </Button>
              ) : null
            }
          />
        ))}
        {allocation.data && allocation.data.rows.length === 0 ? (
          <SettingsListRow
            title="No current-period project usage"
            description="A project appears here after its first Execution usage report in the current accounting period."
          />
        ) : null}
        {editor ? (
          <SettingsListRow
            align="start"
            title={`Assign ${editor.projectName}`}
            description={
              <form
                className="mt-2 grid gap-3 sm:grid-cols-2"
                onSubmit={(event) => {
                  event.preventDefault();
                  assignAllocation.mutate(editor);
                }}
              >
                <FormField label="Cost center code">
                  <Input
                    autoCapitalize="none"
                    maxLength={64}
                    onChange={(event) =>
                      setEditor((current) =>
                        current ? { ...current, costCenterCode: event.target.value } : current,
                      )
                    }
                    pattern="[A-Za-z0-9][A-Za-z0-9._/-]{0,63}"
                    required
                    value={editor.costCenterCode}
                  />
                </FormField>
                <FormField label="Department code">
                  <Input
                    autoCapitalize="none"
                    maxLength={64}
                    onChange={(event) =>
                      setEditor((current) =>
                        current ? { ...current, departmentCode: event.target.value } : current,
                      )
                    }
                    pattern="[A-Za-z0-9][A-Za-z0-9._/-]{0,63}"
                    required
                    value={editor.departmentCode}
                  />
                </FormField>
                <div className="flex items-center gap-2 sm:col-span-2">
                  <Button disabled={assignAllocation.isPending} size="sm" type="submit">
                    {assignAllocation.isPending ? "Saving…" : "Save allocation"}
                  </Button>
                  <Button size="sm" type="button" variant="ghost" onClick={() => setEditor(null)}>
                    Cancel
                  </Button>
                </div>
                <InlineError error={assignAllocation.error} />
              </form>
            }
          />
        ) : null}
      </SettingsCard>
    </SettingsSectionShell>
  );
}

function formatAllocationKnownCost(row: ControlPlaneInternalCostAllocationRow): string {
  const values = Object.entries(row.knownCostByCurrency)
    .toSorted(([left], [right]) => left.localeCompare(right))
    .map(([currency, amount]) => formatUsageCost(amount, currency));
  return values.length > 0 ? values.join(" · ") : "cost unavailable";
}
