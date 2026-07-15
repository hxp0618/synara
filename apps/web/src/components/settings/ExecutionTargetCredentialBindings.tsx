import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneInlineError,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { SettingsListRow, SettingsRow } from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import {
  controlPlaneClient,
  type ControlPlaneCredential,
  type ControlPlaneCredentialBinding,
  type ControlPlaneExecutionTarget,
} from "~/lib/controlPlaneClient";
import { listUsableControlPlaneCredentials } from "~/lib/controlPlaneCredentials";

function targetCredentialBindingsQueryKey(tenantId: string, targetId: string) {
  return [
    "control-plane",
    "tenants",
    tenantId,
    "execution-targets",
    targetId,
    "credential-bindings",
  ] as const;
}

export function ExecutionTargetCredentialBindings(props: {
  tenantId: string;
  targets: ReadonlyArray<ControlPlaneExecutionTarget>;
  credentials: ReadonlyArray<ControlPlaneCredential>;
}) {
  const queryClient = useQueryClient();
  const eligibleTargets = props.targets.filter((target) => target.tenantId !== null);
  const [targetSelection, setTargetSelection] = useState("");
  const selectedTarget =
    eligibleTargets.find((target) => target.id === targetSelection) ?? eligibleTargets[0] ?? null;
  const queryKey = targetCredentialBindingsQueryKey(props.tenantId, selectedTarget?.id ?? "none");
  const bindings = useQuery({
    queryKey,
    queryFn: () =>
      controlPlaneClient.listCredentialBindings(props.tenantId, {
        executionTargetId: selectedTarget!.id,
      }),
    enabled: selectedTarget !== null,
    retry: false,
  });
  const compatibleCredentials = selectedTarget
    ? listUsableControlPlaneCredentials(props.credentials, {
        purpose: "registry",
        organizationId: selectedTarget.organizationId,
      }).filter(
        (credential) =>
          credential.scope === "tenant" ||
          (credential.scope === "organization" &&
            selectedTarget.organizationId !== null &&
            credential.organizationId === selectedTarget.organizationId),
      )
    : [];
  const [credentialSelection, setCredentialSelection] = useState("");
  const selectedCredentialId = compatibleCredentials.some(
    (credential) => credential.id === credentialSelection,
  )
    ? credentialSelection
    : (compatibleCredentials[0]?.id ?? "");
  const createBinding = useMutation({
    mutationFn: () =>
      controlPlaneClient.createCredentialBinding(props.tenantId, {
        executionTargetId: selectedTarget!.id,
        credentialId: selectedCredentialId,
        bindingKind: "worker_image_pull",
      }),
    onSuccess: (created) => {
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneCredentialBinding> }>(
        queryKey,
        (current) => ({ items: [created, ...(current?.items ?? [])] }),
      );
    },
  });
  const disableBinding = useMutation({
    mutationFn: (bindingId: string) =>
      controlPlaneClient.disableCredentialBinding(props.tenantId, bindingId),
    onSuccess: (disabled) => {
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneCredentialBinding> }>(
        queryKey,
        (current) => ({
          items: (current?.items ?? []).map((binding) =>
            binding.id === disabled.id ? disabled : binding,
          ),
        }),
      );
    },
  });
  const credentialNames = new Map(
    props.credentials.map((credential) => [credential.id, credential.name]),
  );

  if (selectedTarget === null) return null;

  return (
    <>
      <SettingsRow
        title="Worker image Registry Binding"
        description="Pins one OCI Registry Credential to image pulls for a tenant-owned Execution Target. The running Worker never receives this infrastructure Credential."
      >
        <form
          className={CONTROL_PLANE_FORM_GRID_CLASS_NAME}
          onSubmit={(event: FormEvent) => {
            event.preventDefault();
            createBinding.mutate();
          }}
        >
          <ControlPlaneFormField label="Execution target">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              value={selectedTarget.id}
              onChange={(event) => {
                setTargetSelection(event.target.value);
                setCredentialSelection("");
              }}
            >
              {eligibleTargets.map((target) => (
                <option key={target.id} value={target.id}>
                  {target.name} · {target.kind}
                </option>
              ))}
            </select>
          </ControlPlaneFormField>
          <ControlPlaneFormField label="OCI Registry Credential">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              disabled={compatibleCredentials.length === 0}
              value={selectedCredentialId}
              onChange={(event) => setCredentialSelection(event.target.value)}
            >
              {compatibleCredentials.length === 0 ? (
                <option value="">No compatible Registry Credential</option>
              ) : null}
              {compatibleCredentials.map((credential) => (
                <option key={credential.id} value={credential.id}>
                  {credential.name} · {credential.credentialType.replaceAll("_", " ")}
                </option>
              ))}
            </select>
          </ControlPlaneFormField>
          <div className="sm:col-span-2">
            <Button
              disabled={createBinding.isPending || selectedCredentialId === ""}
              size="sm"
              type="submit"
            >
              {createBinding.isPending ? "Creating Binding…" : "Create image-pull Binding"}
            </Button>
            <ControlPlaneInlineError error={createBinding.error ?? bindings.error} />
          </div>
        </form>
      </SettingsRow>
      {bindings.data?.items.map((binding) => (
        <SettingsListRow
          key={binding.id}
          title={`${selectedTarget.name} image pull`}
          description={`${credentialNames.get(binding.credentialId) ?? `Credential ${binding.credentialId.slice(0, 8)}`} · ${binding.selector}`}
          actions={
            <span className="flex flex-wrap items-center justify-end gap-1.5">
              <ControlPlaneStatusPill value={binding.disabledAt ? "disabled" : "active"} />
              {binding.disabledAt === null ? (
                <Button
                  className="text-destructive hover:text-destructive"
                  disabled={disableBinding.isPending}
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    if (
                      window.confirm(`Disable the image-pull Binding for ${selectedTarget.name}?`)
                    ) {
                      disableBinding.mutate(binding.id);
                    }
                  }}
                >
                  Disable
                </Button>
              ) : null}
            </span>
          }
        />
      ))}
      {bindings.isPending ? <SettingsListRow title="Loading image-pull Bindings…" /> : null}
      {bindings.data?.items.length === 0 ? (
        <SettingsListRow
          title="No image-pull Binding"
          description="Public images need no Registry Credential. Private image pulls fail closed until a matching Binding exists."
        />
      ) : null}
      <ControlPlaneInlineError error={disableBinding.error} />
    </>
  );
}
