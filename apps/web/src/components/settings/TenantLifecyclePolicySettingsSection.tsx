import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent, type ReactNode } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneInlineError,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import {
  controlPlaneClient,
  type ControlPlaneResourceLifecycleConfig,
  type ControlPlaneResourceLifecycleEffective,
  type ControlPlaneResourceLifecycleIntBounds,
  type ControlPlaneResourceLifecycleOverrideInput,
  type ControlPlaneResourceLifecycleOverrides,
  type ControlPlaneResourceLifecyclePolicy,
  type ControlPlaneResourceLifecyclePolicyUpdateInput,
  type ControlPlaneResourceLifecycleWarmPoolMode,
} from "~/lib/controlPlaneClient";
import { cn } from "~/lib/utils";

const lifecycleValueClassName =
  "rounded-full border border-border bg-foreground/4 px-2 py-0.5 text-[10px] font-medium text-muted-foreground";

export function tenantResourceLifecyclePolicyQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "resource-lifecycle-policy"] as const;
}

export function projectResourceLifecyclePolicyQueryKey(projectId: string) {
  return ["control-plane", "projects", projectId, "resource-lifecycle-policy"] as const;
}

export function formatLifecycleWarmPoolMode(
  mode: ControlPlaneResourceLifecycleWarmPoolMode | null,
): string {
  if (mode === null) return "Inherited";
  return mode.replaceAll("-", " ");
}

export function formatLifecycleDurationSeconds(value: number): string {
  const days = Math.floor(value / 86_400);
  const hours = Math.floor((value % 86_400) / 3_600);
  const minutes = Math.floor((value % 3_600) / 60);
  const seconds = value % 60;
  const parts: string[] = [];
  if (days > 0) parts.push(`${days}d`);
  if (hours > 0) parts.push(`${hours}h`);
  if (minutes > 0) parts.push(`${minutes}m`);
  if (parts.length === 0 || (parts.length < 2 && seconds > 0)) parts.push(`${seconds}s`);
  return parts.slice(0, 2).join(" ");
}

export function formatLifecycleAbsoluteLifetime(value: number | null): string {
  return value === null ? "No absolute expiry" : formatLifecycleDurationSeconds(value);
}

export function formatLifecycleTimestamp(
  value: string | null | undefined,
  emptyLabel = "Not scheduled",
): string {
  if (!value) return emptyLabel;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return `${parsed.toISOString().slice(0, 16).replace("T", " ")} UTC`;
}

export function summarizeLifecycleEffective(
  effective: ControlPlaneResourceLifecycleEffective,
): string {
  return [
    `wait ${formatLifecycleDurationSeconds(effective.waitingKeepAliveSeconds)}`,
    `suspend ${formatLifecycleDurationSeconds(effective.suspendAfterIdleSeconds)}`,
    `absolute ${formatLifecycleAbsoluteLifetime(effective.absoluteSessionLifetimeSeconds)}`,
    `retain ${effective.workspaceRetentionDays}d`,
    `warm ${formatLifecycleWarmPoolMode(effective.warmPoolMode)}`,
  ].join(" · ");
}

export function summarizeLifecycleOverrides(
  overrides: ControlPlaneResourceLifecycleOverrides,
  inheritedFromLabel: string,
): string {
  const entries: string[] = [];
  if (overrides.waitingKeepAliveSeconds !== null) {
    entries.push(`wait ${formatLifecycleDurationSeconds(overrides.waitingKeepAliveSeconds)}`);
  }
  if (overrides.suspendAfterIdleSeconds !== null) {
    entries.push(`suspend ${formatLifecycleDurationSeconds(overrides.suspendAfterIdleSeconds)}`);
  }
  if (overrides.absoluteSessionLifetimeSeconds !== null) {
    entries.push(
      `absolute ${formatLifecycleDurationSeconds(overrides.absoluteSessionLifetimeSeconds)}`,
    );
  }
  if (overrides.workspaceRetentionDays !== null) {
    entries.push(`retain ${overrides.workspaceRetentionDays}d`);
  }
  if (overrides.warmPoolMode !== null) {
    entries.push(`warm ${formatLifecycleWarmPoolMode(overrides.warmPoolMode)}`);
  }
  if (entries.length === 0) return `Inheriting every value from ${inheritedFromLabel}.`;
  return entries.join(" · ");
}

export function parseLifecycleIntegerInput(
  value: string,
  label: string,
  bounds: ControlPlaneResourceLifecycleIntBounds,
): number | null {
  const trimmed = value.trim();
  if (trimmed === "") return null;
  if (!/^\d+$/.test(trimmed)) {
    throw new Error(
      `${label} must be a whole number between ${bounds.min.toLocaleString("en-US")} and ${bounds.max.toLocaleString("en-US")}, or blank to inherit.`,
    );
  }
  const parsed = Number(trimmed);
  if (!Number.isSafeInteger(parsed) || parsed < bounds.min || parsed > bounds.max) {
    throw new Error(
      `${label} must be between ${bounds.min.toLocaleString("en-US")} and ${bounds.max.toLocaleString("en-US")}, or blank to inherit.`,
    );
  }
  return parsed;
}

export function applyLifecycleOverrides(
  effective: ControlPlaneResourceLifecycleEffective,
  overrides:
    | ControlPlaneResourceLifecycleOverrideInput
    | ControlPlaneResourceLifecycleOverrides,
): ControlPlaneResourceLifecycleEffective {
  const next: ControlPlaneResourceLifecycleEffective = {
    ...effective,
    absoluteSessionLifetimeSeconds: effective.absoluteSessionLifetimeSeconds,
  };
  if (overrides.waitingKeepAliveSeconds != null) {
    next.waitingKeepAliveSeconds = overrides.waitingKeepAliveSeconds;
  }
  if (overrides.suspendAfterIdleSeconds != null) {
    next.suspendAfterIdleSeconds = overrides.suspendAfterIdleSeconds;
  }
  if (overrides.absoluteSessionLifetimeSeconds != null) {
    next.absoluteSessionLifetimeSeconds = overrides.absoluteSessionLifetimeSeconds;
  }
  if (overrides.workspaceRetentionDays != null) {
    next.workspaceRetentionDays = overrides.workspaceRetentionDays;
  }
  if (overrides.warmPoolMode != null) {
    next.warmPoolMode = overrides.warmPoolMode;
  }
  return next;
}

function LifecycleValue(props: { label: string; value: string; accent?: boolean }) {
  return (
    <span
      className={cn(
        lifecycleValueClassName,
        props.accent &&
          "border-emerald-500/20 bg-emerald-500/8 text-emerald-700 dark:text-emerald-300",
      )}
    >
      {props.label}: {props.value}
    </span>
  );
}

function renderLifecycleValues(
  effective: ControlPlaneResourceLifecycleEffective,
  createdAt: string | null,
  updatedAt: string | null,
): ReactNode {
  return (
    <span className="flex flex-wrap gap-1.5">
      <LifecycleValue
        accent
        label="wait"
        value={formatLifecycleDurationSeconds(effective.waitingKeepAliveSeconds)}
      />
      <LifecycleValue
        accent
        label="suspend"
        value={formatLifecycleDurationSeconds(effective.suspendAfterIdleSeconds)}
      />
      <LifecycleValue
        label="absolute"
        value={formatLifecycleAbsoluteLifetime(effective.absoluteSessionLifetimeSeconds)}
      />
      <LifecycleValue label="retain" value={`${effective.workspaceRetentionDays}d`} />
      <LifecycleValue label="warm" value={formatLifecycleWarmPoolMode(effective.warmPoolMode)} />
      {createdAt ? <LifecycleValue label="created" value={formatLifecycleTimestamp(createdAt)} /> : null}
      {updatedAt ? <LifecycleValue label="updated" value={formatLifecycleTimestamp(updatedAt)} /> : null}
    </span>
  );
}

export function describeLifecycleBounds(config: ControlPlaneResourceLifecycleConfig): string {
  return [
    `wait ${config.bounds.waitingKeepAliveSeconds.min.toLocaleString("en-US")}-${config.bounds.waitingKeepAliveSeconds.max.toLocaleString("en-US")}s`,
    `suspend ${config.bounds.suspendAfterIdleSeconds.min.toLocaleString("en-US")}-${config.bounds.suspendAfterIdleSeconds.max.toLocaleString("en-US")}s`,
    `absolute ${config.bounds.absoluteSessionLifetimeSeconds.min.toLocaleString("en-US")}-${config.bounds.absoluteSessionLifetimeSeconds.max.toLocaleString("en-US")}s`,
    `retain ${config.bounds.workspaceRetentionDays.min.toLocaleString("en-US")}-${config.bounds.workspaceRetentionDays.max.toLocaleString("en-US")}d`,
  ].join(" · ");
}

type LifecycleScope = "tenant" | "project";

export function TenantLifecyclePolicySettingsSection(props: {
  tenantId: string;
  config: ControlPlaneResourceLifecycleConfig;
  canManage: boolean;
  scope?: LifecycleScope;
  projectId?: string;
  projectName?: string;
}) {
  const scope = props.scope ?? "tenant";
  const queryClient = useQueryClient();
  const [waitingKeepAliveSeconds, setWaitingKeepAliveSeconds] = useState("");
  const [suspendAfterIdleSeconds, setSuspendAfterIdleSeconds] = useState("");
  const [absoluteSessionLifetimeSeconds, setAbsoluteSessionLifetimeSeconds] = useState("");
  const [workspaceRetentionDays, setWorkspaceRetentionDays] = useState("");
  const [warmPoolMode, setWarmPoolMode] = useState<
    ControlPlaneResourceLifecycleWarmPoolMode | ""
  >("");
  const [inputError, setInputError] = useState<unknown>(null);

  const queryKey =
    scope === "tenant"
      ? tenantResourceLifecyclePolicyQueryKey(props.tenantId)
      : projectResourceLifecyclePolicyQueryKey(props.projectId!);
  const policy = useQuery({
    queryKey,
    queryFn: () =>
      scope === "tenant"
        ? controlPlaneClient.getTenantResourceLifecyclePolicy(props.tenantId)
        : controlPlaneClient.getProjectResourceLifecyclePolicy(props.projectId!),
    enabled: scope === "tenant" || props.projectId !== undefined,
    retry: false,
  });

  useEffect(() => {
    if (!policy.data) return;
    setWaitingKeepAliveSeconds(policy.data.overrides.waitingKeepAliveSeconds?.toString() ?? "");
    setSuspendAfterIdleSeconds(policy.data.overrides.suspendAfterIdleSeconds?.toString() ?? "");
    setAbsoluteSessionLifetimeSeconds(
      policy.data.overrides.absoluteSessionLifetimeSeconds?.toString() ?? "",
    );
    setWorkspaceRetentionDays(policy.data.overrides.workspaceRetentionDays?.toString() ?? "");
    setWarmPoolMode(policy.data.overrides.warmPoolMode ?? "");
  }, [policy.data]);

  const update = useMutation({
    mutationFn: (input: ControlPlaneResourceLifecyclePolicyUpdateInput) =>
      scope === "tenant"
        ? controlPlaneClient.updateTenantResourceLifecyclePolicy(props.tenantId, input)
        : controlPlaneClient.updateProjectResourceLifecyclePolicy(props.projectId!, input),
    onSuccess: (next) => {
      setInputError(null);
      queryClient.setQueryData(queryKey, next);
    },
  });

  const title = scope === "tenant" ? "Resource lifecycle" : "Project resource lifecycle";
  const inheritedFromLabel = scope === "tenant" ? "platform defaults" : "the Tenant policy";
  const subjectLabel =
    scope === "tenant"
      ? "new Sessions in this Tenant"
      : `new Sessions in ${props.projectName ?? "the selected Project"}`;

  if (policy.isPending) {
    return (
      <SettingsSection title={title}>
        <SettingsListRow title="Loading lifecycle policy…" />
      </SettingsSection>
    );
  }

  if (policy.error) {
    return (
      <SettingsSection title={title}>
        <SettingsListRow
          title="Could not load the lifecycle policy"
          description={policy.error instanceof Error ? policy.error.message : "The request failed."}
          actions={
            <Button size="sm" variant="outline" onClick={() => void policy.refetch()}>
              Retry
            </Button>
          }
        />
      </SettingsSection>
    );
  }

  const item = policy.data as ControlPlaneResourceLifecyclePolicy;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    try {
      const input: ControlPlaneResourceLifecyclePolicyUpdateInput = {
        expectedVersion: item.version,
        waitingKeepAliveSeconds: parseLifecycleIntegerInput(
          waitingKeepAliveSeconds,
          "Waiting keep-alive",
          props.config.bounds.waitingKeepAliveSeconds,
        ),
        suspendAfterIdleSeconds: parseLifecycleIntegerInput(
          suspendAfterIdleSeconds,
          "Suspend after idle",
          props.config.bounds.suspendAfterIdleSeconds,
        ),
        absoluteSessionLifetimeSeconds: parseLifecycleIntegerInput(
          absoluteSessionLifetimeSeconds,
          "Absolute session lifetime",
          props.config.bounds.absoluteSessionLifetimeSeconds,
        ),
        workspaceRetentionDays: parseLifecycleIntegerInput(
          workspaceRetentionDays,
          "Workspace retention",
          props.config.bounds.workspaceRetentionDays,
        ),
        warmPoolMode: warmPoolMode || null,
      };
      setInputError(null);
      update.mutate(input);
    } catch (error) {
      setInputError(error);
    }
  };

  return (
    <SettingsSection title={title}>
      <SettingsRow
        title="Effective policy"
        description={`Applies to ${subjectLabel}. Only server-side execution activity renews the resource timers; browser heartbeats, stream reads, and reconnect noise do not extend waiting or suspend deadlines.`}
        status={renderLifecycleValues(item.effective, item.createdAt, item.updatedAt)}
        control={
          <span className="text-[11px] text-muted-foreground">
            v{item.version}
            {item.updatedBy ? ` · updated by ${item.updatedBy.slice(0, 8)}` : ""}
          </span>
        }
      />
      <SettingsListRow
        title="Platform defaults"
        description={summarizeLifecycleEffective(props.config.defaults)}
        actions={
          <span className="text-[11px] text-muted-foreground">
            Used when no override exists
          </span>
        }
      />
      <SettingsListRow
        title="Current overrides"
        description={summarizeLifecycleOverrides(item.overrides, inheritedFromLabel)}
        actions={
          <span className="text-[11px] text-muted-foreground">
            {scope === "tenant" ? "Tenant-wide" : "Project-specific"}
          </span>
        }
      />
      <SettingsListRow
        title="Operator bounds"
        description={describeLifecycleBounds(props.config)}
        actions={
          <span className="flex flex-wrap justify-end gap-1.5">
            {props.config.bounds.warmPoolModes.map((mode) => (
              <LifecycleValue key={mode} label="warm" value={formatLifecycleWarmPoolMode(mode)} />
            ))}
          </span>
        }
      />
      {props.canManage ? (
        <SettingsRow
          title={scope === "tenant" ? "Edit Tenant overrides" : "Edit Project overrides"}
          description={`Leave any field blank to inherit from ${inheritedFromLabel}. The control plane enforces the operator bounds before saving.`}
        >
          <form className={CONTROL_PLANE_FORM_GRID_CLASS_NAME} onSubmit={submit}>
            <ControlPlaneFormField label="Waiting keep-alive (seconds)">
              <Input
                inputMode="numeric"
                max={props.config.bounds.waitingKeepAliveSeconds.max}
                min={props.config.bounds.waitingKeepAliveSeconds.min}
                placeholder="Inherit"
                type="number"
                value={waitingKeepAliveSeconds}
                onChange={(event) => setWaitingKeepAliveSeconds(event.target.value)}
              />
            </ControlPlaneFormField>
            <ControlPlaneFormField label="Suspend after idle (seconds)">
              <Input
                inputMode="numeric"
                max={props.config.bounds.suspendAfterIdleSeconds.max}
                min={props.config.bounds.suspendAfterIdleSeconds.min}
                placeholder="Inherit"
                type="number"
                value={suspendAfterIdleSeconds}
                onChange={(event) => setSuspendAfterIdleSeconds(event.target.value)}
              />
            </ControlPlaneFormField>
            <ControlPlaneFormField label="Absolute session lifetime (seconds)">
              <Input
                inputMode="numeric"
                max={props.config.bounds.absoluteSessionLifetimeSeconds.max}
                min={props.config.bounds.absoluteSessionLifetimeSeconds.min}
                placeholder="Inherit"
                type="number"
                value={absoluteSessionLifetimeSeconds}
                onChange={(event) => setAbsoluteSessionLifetimeSeconds(event.target.value)}
              />
            </ControlPlaneFormField>
            <ControlPlaneFormField label="Workspace retention (days)">
              <Input
                inputMode="numeric"
                max={props.config.bounds.workspaceRetentionDays.max}
                min={props.config.bounds.workspaceRetentionDays.min}
                placeholder="Inherit"
                type="number"
                value={workspaceRetentionDays}
                onChange={(event) => setWorkspaceRetentionDays(event.target.value)}
              />
            </ControlPlaneFormField>
            <ControlPlaneFormField label="Warm pool mode">
              <select
                className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
                value={warmPoolMode}
                onChange={(event) =>
                  setWarmPoolMode(event.target.value as ControlPlaneResourceLifecycleWarmPoolMode | "")
                }
              >
                <option value="">Inherit</option>
                {props.config.bounds.warmPoolModes.map((mode) => (
                  <option key={mode} value={mode}>
                    {formatLifecycleWarmPoolMode(mode)}
                  </option>
                ))}
              </select>
            </ControlPlaneFormField>
            <div className="sm:col-span-2">
              <p className="text-xs text-muted-foreground">
                Current effective values: {summarizeLifecycleEffective(item.effective)}.
              </p>
            </div>
            <div className="sm:col-span-2">
              <Button disabled={update.isPending} size="sm" type="submit">
                {update.isPending ? "Saving lifecycle policy…" : "Save lifecycle overrides"}
              </Button>
              <ControlPlaneInlineError error={inputError ?? update.error} />
            </div>
          </form>
        </SettingsRow>
      ) : null}
    </SettingsSection>
  );
}
