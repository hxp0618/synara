import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneInlineError,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { controlPlaneClient, type ControlPlaneTenantQuota } from "~/lib/controlPlaneClient";

const MAX_CONCURRENT_EXECUTIONS = 1_000_000;
const MAX_EXACT_ARTIFACT_BYTES = Number.MAX_SAFE_INTEGER;

function quotaQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "quota"] as const;
}

function parseOptionalPositiveInteger(
  value: string,
  label: string,
  maximum: number,
): number | null {
  const trimmed = value.trim();
  if (trimmed === "") return null;
  if (!/^\d+$/.test(trimmed)) {
    throw new Error(`${label} must be a positive whole number or blank for unlimited.`);
  }
  const parsed = Number(trimmed);
  if (!Number.isSafeInteger(parsed) || parsed <= 0 || parsed > maximum) {
    throw new Error(`${label} must be between 1 and ${maximum.toLocaleString("en-US")}.`);
  }
  return parsed;
}

function formatArtifactBytes(value: number | null): string {
  if (value === null) return "Unlimited";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"] as const;
  let display = value;
  let unitIndex = 0;
  while (display >= 1024 && unitIndex < units.length - 1) {
    display /= 1024;
    unitIndex += 1;
  }
  const precision = display >= 100 || unitIndex === 0 ? 0 : display >= 10 ? 1 : 2;
  return `${display.toFixed(precision)} ${units[unitIndex]}`;
}

export function TenantQuotaSettingsSection(props: { tenantId: string; canManage: boolean }) {
  const queryClient = useQueryClient();
  const quota = useQuery({
    queryKey: quotaQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.getTenantQuota(props.tenantId),
    retry: false,
  });

  if (quota.isPending) {
    return (
      <SettingsSection title="Tenant quotas">
        <SettingsListRow title="Loading tenant quotas…" />
      </SettingsSection>
    );
  }
  if (quota.error) {
    return (
      <SettingsSection title="Tenant quotas">
        <SettingsListRow
          title="Could not load tenant quotas"
          description={quota.error instanceof Error ? quota.error.message : "The request failed."}
          actions={
            <Button size="sm" variant="outline" onClick={() => void quota.refetch()}>
              Retry
            </Button>
          }
        />
      </SettingsSection>
    );
  }

  const item = quota.data;
  return (
    <SettingsSection title="Tenant quotas">
      <SettingsListRow
        title="Concurrent executions"
        description="Queued, leased, running, and recovering executions count toward this limit."
        actions={
          item.maxConcurrentExecutions === null ? (
            <ControlPlaneStatusPill value="unlimited" active={false} />
          ) : (
            <span className="text-xs tabular-nums text-foreground">
              {item.maxConcurrentExecutions.toLocaleString("en-US")}
            </span>
          )
        }
      />
      <SettingsListRow
        title="Artifact storage"
        description="Only ready Artifacts consume the tenant storage allowance."
        actions={
          item.maxArtifactBytes === null ? (
            <ControlPlaneStatusPill value="unlimited" active={false} />
          ) : (
            <span
              className="text-xs tabular-nums text-foreground"
              title={`${item.maxArtifactBytes.toLocaleString("en-US")} bytes`}
            >
              {formatArtifactBytes(item.maxArtifactBytes)}
            </span>
          )
        }
      />
      {props.canManage ? (
        <TenantQuotaForm
          key={`${props.tenantId}:${item.maxConcurrentExecutions ?? "none"}:${item.maxArtifactBytes ?? "none"}`}
          quota={item}
          onSaved={(updated) => queryClient.setQueryData(quotaQueryKey(props.tenantId), updated)}
        />
      ) : null}
    </SettingsSection>
  );
}

function TenantQuotaForm(props: {
  quota: ControlPlaneTenantQuota;
  onSaved: (quota: ControlPlaneTenantQuota) => void;
}) {
  const [maxConcurrentExecutions, setMaxConcurrentExecutions] = useState(
    props.quota.maxConcurrentExecutions?.toString() ?? "",
  );
  const [maxArtifactBytes, setMaxArtifactBytes] = useState(
    props.quota.maxArtifactBytes?.toString() ?? "",
  );
  const [inputError, setInputError] = useState<string | null>(null);
  const update = useMutation({
    mutationFn: (
      input: Pick<ControlPlaneTenantQuota, "maxConcurrentExecutions" | "maxArtifactBytes">,
    ) => controlPlaneClient.updateTenantQuota(props.quota.tenantId, input),
    onSuccess: props.onSaved,
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    try {
      const input = {
        maxConcurrentExecutions: parseOptionalPositiveInteger(
          maxConcurrentExecutions,
          "Concurrent executions",
          MAX_CONCURRENT_EXECUTIONS,
        ),
        maxArtifactBytes: parseOptionalPositiveInteger(
          maxArtifactBytes,
          "Artifact storage bytes",
          MAX_EXACT_ARTIFACT_BYTES,
        ),
      };
      setInputError(null);
      update.mutate(input);
    } catch (error) {
      setInputError(error instanceof Error ? error.message : "The quota values are invalid.");
    }
  };

  return (
    <SettingsRow
      title="Edit tenant quotas"
      description="Leave a value blank for unlimited. Limits take effect on the next execution or Artifact confirmation."
    >
      <form className={CONTROL_PLANE_FORM_GRID_CLASS_NAME} onSubmit={submit}>
        <ControlPlaneFormField label="Concurrent executions">
          <Input
            inputMode="numeric"
            max={MAX_CONCURRENT_EXECUTIONS}
            min={1}
            placeholder="Unlimited"
            type="number"
            value={maxConcurrentExecutions}
            onChange={(event) => setMaxConcurrentExecutions(event.target.value)}
          />
        </ControlPlaneFormField>
        <ControlPlaneFormField label="Artifact storage bytes">
          <Input
            inputMode="numeric"
            max={MAX_EXACT_ARTIFACT_BYTES}
            min={1}
            placeholder="Unlimited"
            type="number"
            value={maxArtifactBytes}
            onChange={(event) => setMaxArtifactBytes(event.target.value)}
          />
        </ControlPlaneFormField>
        <div className="sm:col-span-2">
          <Button disabled={update.isPending} size="sm" type="submit">
            {update.isPending ? "Saving quotas…" : "Save tenant quotas"}
          </Button>
          <ControlPlaneInlineError error={inputError ?? update.error} />
        </div>
      </form>
    </SettingsRow>
  );
}
