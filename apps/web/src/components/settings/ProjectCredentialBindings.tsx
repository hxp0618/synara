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
  type ControlPlaneCredentialBindingKind,
  type ControlPlaneCredentialPurpose,
  type ControlPlaneProject,
} from "~/lib/controlPlaneClient";
import { listUsableControlPlaneCredentials } from "~/lib/controlPlaneCredentials";

const PROJECT_BINDING_KIND_OPTIONS: ReadonlyArray<{
  kind: Exclude<ControlPlaneCredentialBindingKind, "worker_image_pull">;
  label: string;
}> = [
  { kind: "git_fetch", label: "Git clone/fetch" },
  { kind: "git_push", label: "Git push" },
  { kind: "registry_pull", label: "OCI Registry pull" },
  { kind: "registry_push", label: "OCI Registry push" },
  { kind: "package_read", label: "Package install/read" },
  { kind: "package_publish", label: "Package publish" },
];

type ProjectBindingKind = (typeof PROJECT_BINDING_KIND_OPTIONS)[number]["kind"];

export function projectCredentialBindingsQueryKey(tenantId: string, projectId: string) {
  return [
    "control-plane",
    "tenants",
    tenantId,
    "projects",
    projectId,
    "credential-bindings",
  ] as const;
}

export function ProjectCredentialBindings(props: {
  tenantId: string;
  project: ControlPlaneProject;
  credentials: ReadonlyArray<ControlPlaneCredential>;
}) {
  const queryClient = useQueryClient();
  const queryKey = projectCredentialBindingsQueryKey(props.tenantId, props.project.id);
  const bindings = useQuery({
    queryKey,
    queryFn: () =>
      controlPlaneClient.listCredentialBindings(props.tenantId, {
        projectId: props.project.id,
      }),
    retry: false,
  });
  const [bindingKind, setBindingKind] = useState<ProjectBindingKind>("git_fetch");
  const [credentialId, setCredentialId] = useState("");
  const purpose = bindingPurpose(bindingKind);
  const repositoryCredentialType = gitCredentialTypeForRepository(props.project.repositoryUrl);
  const compatibleCredentials = listUsableControlPlaneCredentials(props.credentials, {
    purpose,
    organizationId: props.project.organizationId,
  }).filter((credential) => {
    if (credential.scope !== "organization" && credential.scope !== "tenant") return false;
    return (
      purpose !== "git" ||
      (repositoryCredentialType !== null && credential.credentialType === repositoryCredentialType)
    );
  });
  const selectedCredentialId = compatibleCredentials.some(
    (credential) => credential.id === credentialId,
  )
    ? credentialId
    : (compatibleCredentials[0]?.id ?? "");
  const createBinding = useMutation({
    mutationFn: () =>
      controlPlaneClient.createCredentialBinding(props.tenantId, {
        projectId: props.project.id,
        credentialId: selectedCredentialId,
        bindingKind,
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

  return (
    <>
      <SettingsRow
        title="Workspace Credential Bindings"
        description="Bindings grant one Project stage access to one immutable Credential selector. Disable and replace a Binding instead of mutating it."
      >
        <form
          className={CONTROL_PLANE_FORM_GRID_CLASS_NAME}
          onSubmit={(event: FormEvent) => {
            event.preventDefault();
            createBinding.mutate();
          }}
        >
          <ControlPlaneFormField label="Stage">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              value={bindingKind}
              onChange={(event) => {
                setBindingKind(event.target.value as ProjectBindingKind);
                setCredentialId("");
              }}
            >
              {PROJECT_BINDING_KIND_OPTIONS.map((option) => (
                <option key={option.kind} value={option.kind}>
                  {option.label}
                </option>
              ))}
            </select>
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Credential">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              disabled={compatibleCredentials.length === 0}
              value={selectedCredentialId}
              onChange={(event) => setCredentialId(event.target.value)}
            >
              {compatibleCredentials.length === 0 ? (
                <option value="">No compatible Credential</option>
              ) : null}
              {compatibleCredentials.map((credential) => (
                <option key={credential.id} value={credential.id}>
                  {credential.name} · {credentialScopeLabel(credential)}
                </option>
              ))}
            </select>
          </ControlPlaneFormField>
          {purpose === "git" && repositoryCredentialType === null ? (
            <p className="text-xs text-muted-foreground sm:col-span-2">
              Configure a credential-free HTTPS or ssh:// repository URL before creating a Git
              Binding.
            </p>
          ) : null}
          <div className="sm:col-span-2">
            <Button
              disabled={createBinding.isPending || selectedCredentialId === ""}
              size="sm"
              type="submit"
            >
              {createBinding.isPending ? "Creating Binding…" : "Create Binding"}
            </Button>
            <ControlPlaneInlineError error={createBinding.error ?? bindings.error} />
          </div>
        </form>
      </SettingsRow>
      {bindings.isPending ? <SettingsListRow title="Loading Credential Bindings…" /> : null}
      {bindings.data?.items.map((binding) => (
        <SettingsListRow
          key={binding.id}
          title={bindingKindLabel(binding.bindingKind)}
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
                      window.confirm(
                        `Disable the ${bindingKindLabel(binding.bindingKind)} Binding for ${credentialNames.get(binding.credentialId) ?? "this Credential"}?`,
                      )
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
      {bindings.data?.items.length === 0 ? (
        <SettingsListRow
          title="No Workspace Credential Bindings"
          description="Public Git and dependency sources need no Binding. Private stages fail closed until a matching Binding exists."
        />
      ) : null}
      <ControlPlaneInlineError error={disableBinding.error} />
    </>
  );
}

function bindingPurpose(
  kind: ProjectBindingKind,
): Exclude<ControlPlaneCredentialPurpose, "provider"> {
  if (kind.startsWith("git_")) return "git";
  if (kind.startsWith("registry_")) return "registry";
  return "package";
}

function bindingKindLabel(kind: ControlPlaneCredentialBindingKind): string {
  return (
    PROJECT_BINDING_KIND_OPTIONS.find((option) => option.kind === kind)?.label ??
    kind.replaceAll("_", " ")
  );
}

function credentialScopeLabel(credential: ControlPlaneCredential): string {
  return credential.scope === "organization" ? "Organization scope" : "Tenant scope";
}

export function gitCredentialTypeForRepository(repositoryUrl: string | null): string | null {
  if (!repositoryUrl) return null;
  try {
    const parsed = new URL(repositoryUrl);
    if (
      parsed.protocol === "https:" &&
      parsed.username === "" &&
      parsed.password === "" &&
      (parsed.port === "" || parsed.port === "443")
    ) {
      return "https_token";
    }
    if (
      parsed.protocol === "ssh:" &&
      parsed.username !== "" &&
      parsed.password === "" &&
      (parsed.port === "" || Number(parsed.port) > 0)
    ) {
      return "ssh_key";
    }
    return null;
  } catch {
    return null;
  }
}
