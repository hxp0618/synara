import { useMutation } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Textarea } from "~/components/ui/textarea";
import {
  controlPlaneClient,
  type ControlPlaneExecutionTarget,
  type ControlPlaneExecutionTargetKind,
  type ControlPlaneOrganization,
} from "~/lib/controlPlaneClient";

function parseJSONObject(value: string, label: string): Record<string, unknown> {
  const trimmed = value.trim();
  if (trimmed === "") return {};
  const parsed: unknown = JSON.parse(trimmed);
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new Error(`${label} must be a JSON object.`);
  }
  return parsed as Record<string, unknown>;
}

export function ExecutionTargetSettingsSection(props: {
  tenantId: string;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  targets: ReadonlyArray<ControlPlaneExecutionTarget>;
  canManage: boolean;
  requireOrganizationScope: boolean;
  isLoading: boolean;
  error: unknown;
  onCreated: (target: ControlPlaneExecutionTarget) => void;
  onUpdated: (targetId: string, status: ControlPlaneExecutionTarget["status"]) => void;
}) {
  const [name, setName] = useState("");
  const [kind, setKind] = useState<ControlPlaneExecutionTargetKind>("local");
  const [organizationId, setOrganizationId] = useState(props.organizations[0]?.id ?? "");
  const [configuration, setConfiguration] = useState("");
  const [capabilities, setCapabilities] = useState("");
  const [inputError, setInputError] = useState<string | null>(null);
  const resolvedOrganizationId = props.organizations.some(
    (organization) => organization.id === organizationId,
  )
    ? organizationId
    : props.requireOrganizationScope
      ? (props.organizations[0]?.id ?? "")
      : "";

  const createTarget = useMutation({
    mutationFn: () =>
      controlPlaneClient.createExecutionTarget(props.tenantId, {
        organizationId: resolvedOrganizationId || undefined,
        kind,
        name,
        configuration: parseJSONObject(configuration, "Configuration"),
        capabilities: parseJSONObject(capabilities, "Capabilities"),
      }),
    onSuccess: (target) => {
      setName("");
      setConfiguration("");
      setCapabilities("");
      setInputError(null);
      props.onCreated(target);
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    try {
      parseJSONObject(configuration, "Configuration");
      parseJSONObject(capabilities, "Capabilities");
      setInputError(null);
      createTarget.mutate();
    } catch (error) {
      setInputError(error instanceof Error ? error.message : "Target JSON is invalid.");
    }
  };

  const requestError = createTarget.error ?? props.error;
  const requestErrorMessage =
    requestError instanceof Error ? requestError.message : requestError ? "The request failed." : null;

  return (
    <SettingsSection title="Execution targets">
      {props.targets.map((target) => {
        const organization = props.organizations.find(
          (candidate) => candidate.id === target.organizationId,
        );
        const scope = organization?.name ?? (target.tenantId === null ? "Platform shared" : "Tenant wide");
        return (
          <SettingsListRow
            key={target.id}
            title={target.name}
            description={`${scope} · ${target.kind}`}
            actions={
              <span className="flex flex-wrap justify-end gap-1.5">
                <ControlPlaneStatusPill value={target.kind} active={false} />
                <ControlPlaneStatusPill value={target.status} />
                {props.canManage && target.kind === "ssh" ? (
                  <SSHProvisioningActions
                    onUpdated={props.onUpdated}
                    target={target}
                    tenantId={props.tenantId}
                  />
                ) : null}
              </span>
            }
          />
        );
      })}
      {props.isLoading ? (
        <SettingsListRow title="Loading execution targets…" />
      ) : props.targets.length === 0 ? (
        <SettingsListRow
          title="No execution target available"
          description="Create a target before starting an agent session in this tenant."
        />
      ) : null}
      {props.canManage ? (
        <SettingsRow
          title="Create execution target"
          description="Connection values are encrypted by the control plane and are never returned to the browser."
        >
          <form className={CONTROL_PLANE_FORM_GRID_CLASS_NAME} onSubmit={submit}>
            <ControlPlaneFormField label="Name">
              <Input required value={name} onChange={(event) => setName(event.target.value)} />
            </ControlPlaneFormField>
            <ControlPlaneFormField label="Kind">
              <select
                className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
                value={kind}
                onChange={(event) =>
                  setKind(event.target.value as ControlPlaneExecutionTargetKind)
                }
              >
                <option value="local">Local</option>
                <option value="ssh">SSH</option>
                <option value="docker">Docker</option>
                <option value="kubernetes">Kubernetes</option>
              </select>
            </ControlPlaneFormField>
            <ControlPlaneFormField label="Organization scope">
              <select
                className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
                value={resolvedOrganizationId}
                onChange={(event) => setOrganizationId(event.target.value)}
              >
                {props.requireOrganizationScope ? null : <option value="">Tenant wide</option>}
                {props.organizations.map((organization) => (
                  <option key={organization.id} value={organization.id}>
                    {organization.name}
                  </option>
                ))}
              </select>
            </ControlPlaneFormField>
            <div />
            <ControlPlaneFormField label="Encrypted configuration JSON">
              <Textarea
                placeholder={configurationPlaceholder(kind)}
                size="sm"
                value={configuration}
                onChange={(event) => setConfiguration(event.target.value)}
              />
            </ControlPlaneFormField>
            <ControlPlaneFormField label="Public capabilities JSON">
              <Textarea
                placeholder='{"workspaceModes":["local","worktree"]}'
                size="sm"
                value={capabilities}
                onChange={(event) => setCapabilities(event.target.value)}
              />
            </ControlPlaneFormField>
            <div className="sm:col-span-2">
              <Button disabled={createTarget.isPending} size="sm" type="submit">
                {createTarget.isPending ? "Creating target…" : "Create execution target"}
              </Button>
              {inputError || requestErrorMessage ? (
                <p className="mt-2 text-xs leading-relaxed text-destructive">
                  {inputError ?? requestErrorMessage}
                </p>
              ) : null}
            </div>
          </form>
        </SettingsRow>
      ) : null}
    </SettingsSection>
  );
}

function configurationPlaceholder(kind: ControlPlaneExecutionTargetKind): string {
  switch (kind) {
    case "ssh":
      return '{"host":"agent.example.com","user":"root","privateKey":"…","hostKey":"ssh-ed25519 …","runnerCommand":["provider-host","run","--jsonl"]}';
    case "docker":
      return '{"image":"synara-worker:latest","desiredWorkers":2,"runnerCommand":["provider-host","run","--jsonl"],"memoryBytes":1073741824,"nanoCpus":1000000000}';
    case "kubernetes":
      return '{"namespace":"synara-workers","image":"synara-worker:latest","runnerCommand":["provider-host","run","--jsonl"]}';
    default:
      return "{}";
  }
}

function SSHProvisioningActions(props: {
  tenantId: string;
  target: ControlPlaneExecutionTarget;
  onUpdated: (targetId: string, status: ControlPlaneExecutionTarget["status"]) => void;
}) {
  const provision = useMutation({
    mutationFn: (operation: "install" | "upgrade" | "revoke") =>
      controlPlaneClient.provisionSSHExecutionTarget(props.tenantId, props.target.id, operation),
    onSuccess: (result) => props.onUpdated(result.targetId, result.status),
  });
  const primaryOperation = props.target.status === "active" ? "upgrade" : "install";
  return (
    <span className="flex flex-wrap gap-1.5">
      <Button
        disabled={provision.isPending}
        size="sm"
        variant="outline"
        onClick={() => provision.mutate(primaryOperation)}
      >
        {provision.isPending && provision.variables === primaryOperation
          ? primaryOperation === "install"
            ? "Installing…"
            : "Upgrading…"
          : primaryOperation === "install"
            ? "Install Agentd"
            : "Upgrade Agentd"}
      </Button>
      {props.target.status !== "disabled" ? (
        <Button
          className="text-destructive hover:text-destructive"
          disabled={provision.isPending}
          size="sm"
          variant="outline"
          onClick={() => provision.mutate("revoke")}
        >
          {provision.isPending && provision.variables === "revoke" ? "Revoking…" : "Revoke"}
        </Button>
      ) : null}
      {provision.error ? (
        <span className="basis-full text-right text-[10px] text-destructive">
          {provision.error.message}
        </span>
      ) : null}
    </span>
  );
}
