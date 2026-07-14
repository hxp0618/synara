import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME as formGridClassName,
  ControlPlaneFormField as FormField,
  ControlPlaneInlineError as InlineError,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { controlPlaneClient } from "~/lib/controlPlaneClient";

function retentionQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "retention-policy"] as const;
}

function parseDays(value: string): number | null {
  if (value.trim() === "") return null;
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < 1 || parsed > 36_500) {
    throw new Error("Retention days must be a whole number between 1 and 36500.");
  }
  return parsed;
}

export function TenantRetentionSettingsSection(props: { tenantId: string; canManage: boolean }) {
  const queryClient = useQueryClient();
  const policy = useQuery({
    queryKey: retentionQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.getRetentionPolicy(props.tenantId),
    retry: false,
  });
  const [sessionDays, setSessionDays] = useState("");
  const [artifactDays, setArtifactDays] = useState("");
  const [inputError, setInputError] = useState<unknown>(null);

  useEffect(() => {
    if (!policy.data) return;
    setSessionDays(policy.data.sessionArchiveAfterDays?.toString() ?? "");
    setArtifactDays(policy.data.artifactDeleteAfterDays?.toString() ?? "");
  }, [policy.data]);

  const update = useMutation({
    mutationFn: () =>
      controlPlaneClient.updateRetentionPolicy(props.tenantId, {
        sessionArchiveAfterDays: parseDays(sessionDays),
        artifactDeleteAfterDays: parseDays(artifactDays),
      }),
    onSuccess: (next) => {
      setInputError(null);
      queryClient.setQueryData(retentionQueryKey(props.tenantId), next);
    },
  });

  if (policy.isPending) {
    return (
      <SettingsSection title="Data retention">
        <SettingsListRow title="Loading retention policy…" />
      </SettingsSection>
    );
  }

  return (
    <SettingsSection title="Data retention">
      <SettingsRow
        title="Archive inactive Agent Sessions"
        description="Sessions older than the configured age are archived only when they have no queued, leased, running, or recovering Execution. Leave blank to disable automatic archival."
      />
      <SettingsRow
        title="Delete expired Artifact payloads"
        description="Artifact objects and access tokens are deleted before metadata is marked deleted. Failed object-store deletions remain retryable. Explicit Artifact expiry can delete sooner."
      >
        {props.canManage ? (
          <form
            className={formGridClassName}
            onSubmit={(event: FormEvent) => {
              event.preventDefault();
              setInputError(null);
              try {
                parseDays(sessionDays);
                parseDays(artifactDays);
                update.mutate();
              } catch (error) {
                setInputError(error);
              }
            }}
          >
            <FormField label="Session archive after days">
              <Input
                inputMode="numeric"
                max={36_500}
                min={1}
                placeholder="Disabled"
                type="number"
                value={sessionDays}
                onChange={(event) => setSessionDays(event.target.value)}
              />
            </FormField>
            <FormField label="Artifact delete after days">
              <Input
                inputMode="numeric"
                max={36_500}
                min={1}
                placeholder="Disabled"
                type="number"
                value={artifactDays}
                onChange={(event) => setArtifactDays(event.target.value)}
              />
            </FormField>
            <div className="sm:col-span-2">
              <Button disabled={update.isPending} size="sm" type="submit">
                {update.isPending ? "Saving retention…" : "Save retention policy"}
              </Button>
              <InlineError error={inputError ?? update.error ?? policy.error} />
            </div>
          </form>
        ) : (
          <p className="text-xs text-muted-foreground">
            Session archive: {policy.data?.sessionArchiveAfterDays ?? "disabled"} days · Artifact
            deletion: {policy.data?.artifactDeleteAfterDays ?? "disabled"} days
          </p>
        )}
      </SettingsRow>
    </SettingsSection>
  );
}
