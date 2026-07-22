import { PROVIDER_CAPABILITY_CATALOG, type ProviderHostProviderKind } from "@synara/contracts";
import { useMutation } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { ExecutionTargetCredentialBindings } from "~/components/settings/ExecutionTargetCredentialBindings";
import { ExecutionTargetWorkerManagement } from "~/components/settings/ExecutionTargetWorkerManagement";
import { WorkerReleaseControls } from "~/components/settings/WorkerReleaseControls";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { DisclosureChevron } from "~/components/ui/DisclosureChevron";
import { DisclosureRegion } from "~/components/ui/DisclosureRegion";
import { Input } from "~/components/ui/input";
import { Textarea } from "~/components/ui/textarea";
import {
  controlPlaneClient,
  type ControlPlaneCredential,
  type ControlPlaneExecutionTarget,
  type ControlPlaneExecutionTargetKind,
  type ControlPlaneOrganization,
  type ControlPlaneWorker,
  type ControlPlaneWorkerManifest,
  type ControlPlaneWorkerProviderManifest,
} from "~/lib/controlPlaneClient";

export const EXECUTION_TARGET_CAPABILITIES_PLACEHOLDER =
  '{"workspaceModes":["local","worktree"],"providerPolicy":{"experimentalProviders":["codex","claudeAgent"]}}';

const workerHeartbeatFormatter = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
});
const EMPTY_WORKER_MANIFESTS: ReadonlyArray<ControlPlaneWorkerManifest> = [];
const EXPERIMENTAL_PROVIDER_OPTIONS = PROVIDER_CAPABILITY_CATALOG.providers.filter(
  (provider) => provider.supportTier === "experimental",
);

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
  workerManifests: ReadonlyArray<ControlPlaneWorkerManifest>;
  workerManifestsLoading: boolean;
  workerManifestsError: unknown;
  workers: ReadonlyArray<ControlPlaneWorker>;
  workersLoading: boolean;
  workersError: unknown;
  canManage: boolean;
  canManageCredentialBindings: boolean;
  credentials: ReadonlyArray<ControlPlaneCredential>;
  requireOrganizationScope: boolean;
  isLoading: boolean;
  error: unknown;
  onCreated: (target: ControlPlaneExecutionTarget) => void;
  onUpdated: (targetId: string, status: ControlPlaneExecutionTarget["status"]) => void;
  onProviderPolicyUpdated: (target: ControlPlaneExecutionTarget) => void;
  onWorkersChanged?: () => void;
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
        ...(resolvedOrganizationId ? { organizationId: resolvedOrganizationId } : {}),
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
    requestError instanceof Error
      ? requestError.message
      : requestError
        ? "The request failed."
        : null;
  const workerManifestsByTarget = useMemo(
    () => groupWorkerManifestsByTarget(props.workerManifests),
    [props.workerManifests],
  );
  const workersByTarget = useMemo(() => groupWorkersByTarget(props.workers), [props.workers]);
  const organizationByID = useMemo(
    () => new Map(props.organizations.map((organization) => [organization.id, organization])),
    [props.organizations],
  );

  return (
    <SettingsSection title="Execution targets">
      {props.targets.map((target) => {
        const organization = target.organizationId
          ? organizationByID.get(target.organizationId)
          : undefined;
        const scope =
          organization?.name ?? (target.tenantId === null ? "Platform shared" : "Tenant wide");
        return (
          <ExecutionTargetRow
            key={target.id}
            canManage={props.canManage}
            onUpdated={props.onUpdated}
            onProviderPolicyUpdated={props.onProviderPolicyUpdated}
            scope={scope}
            target={target}
            tenantId={props.tenantId}
            releaseManifests={props.workerManifests}
            workerManifests={workerManifestsByTarget.get(target.id) ?? EMPTY_WORKER_MANIFESTS}
            workerManifestsError={props.workerManifestsError}
            workerManifestsLoading={props.workerManifestsLoading}
            workers={workersByTarget.get(target.id) ?? []}
            workersError={props.workersError}
            workersLoading={props.workersLoading}
            {...(props.onWorkersChanged ? { onWorkersChanged: props.onWorkersChanged } : {})}
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
      {props.canManageCredentialBindings ? (
        <ExecutionTargetCredentialBindings
          credentials={props.credentials}
          targets={props.targets}
          tenantId={props.tenantId}
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
                onChange={(event) => setKind(event.target.value as ControlPlaneExecutionTargetKind)}
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
              <span className="grid gap-1.5">
                <Textarea
                  placeholder={EXECUTION_TARGET_CAPABILITIES_PLACEHOLDER}
                  size="sm"
                  value={capabilities}
                  onChange={(event) => setCapabilities(event.target.value)}
                />
                <span className="text-[10px] leading-relaxed text-muted-foreground">
                  Experimental Providers are disabled unless explicitly listed in
                  providerPolicy.experimentalProviders.
                </span>
              </span>
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

function ExecutionTargetRow(props: {
  tenantId: string;
  target: ControlPlaneExecutionTarget;
  scope: string;
  canManage: boolean;
  releaseManifests: ReadonlyArray<ControlPlaneWorkerManifest>;
  workerManifests: ReadonlyArray<ControlPlaneWorkerManifest>;
  workerManifestsLoading: boolean;
  workerManifestsError: unknown;
  workers: ReadonlyArray<ControlPlaneWorker>;
  workersLoading: boolean;
  workersError: unknown;
  onUpdated: (targetId: string, status: ControlPlaneExecutionTarget["status"]) => void;
  onProviderPolicyUpdated: (target: ControlPlaneExecutionTarget) => void;
  onWorkersChanged?: () => void;
}) {
  return (
    <SettingsListRow
      align="start"
      title={props.target.name}
      description={
        <div className="space-y-1.5">
          <p>{`${props.scope} · ${props.target.kind}`}</p>
          <ExecutionTargetPolicyDisclosure
            canManage={props.canManage && props.target.tenantId !== null}
            canManageWorkers={props.canManage}
            manifestError={props.workerManifestsError}
            manifestLoading={props.workerManifestsLoading}
            manifests={props.workerManifests}
            releaseManifests={props.releaseManifests}
            onProviderPolicyUpdated={props.onProviderPolicyUpdated}
            tenantId={props.tenantId}
            target={props.target}
            workers={props.workers}
            workersError={props.workersError}
            workersLoading={props.workersLoading}
            {...(props.onWorkersChanged ? { onWorkersChanged: props.onWorkersChanged } : {})}
          />
        </div>
      }
      actions={
        <span className="flex flex-wrap justify-end gap-1.5">
          <ControlPlaneStatusPill value={props.target.kind} active={false} />
          <ControlPlaneStatusPill value={props.target.status} />
          {props.canManage && props.target.kind === "ssh" ? (
            <SSHProvisioningActions
              onUpdated={props.onUpdated}
              target={props.target}
              tenantId={props.tenantId}
            />
          ) : null}
        </span>
      }
    />
  );
}

export function ExecutionTargetPolicyDisclosure(props: {
  target: ControlPlaneExecutionTarget;
  manifests?: ReadonlyArray<ControlPlaneWorkerManifest>;
  releaseManifests?: ReadonlyArray<ControlPlaneWorkerManifest>;
  manifestLoading?: boolean;
  manifestError?: unknown;
  canManage?: boolean;
  canManageWorkers?: boolean;
  tenantId?: string;
  onProviderPolicyUpdated?: (target: ControlPlaneExecutionTarget) => void;
  workers?: ReadonlyArray<ControlPlaneWorker>;
  workersLoading?: boolean;
  workersError?: unknown;
  onWorkersChanged?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const experimentalProviders = readExperimentalProviders(props.target.capabilities);
  const regionId = `execution-target-observed-manifest-${props.target.id}`;
  const manifests = props.manifests ?? EMPTY_WORKER_MANIFESTS;
  const totalOnlineWorkers = manifests.reduce(
    (total, manifest) => total + manifest.workerStatusCounts.online,
    0,
  );
  const observedState =
    manifests.length > 0
      ? `${manifests.length} ${manifests.length === 1 ? "variant" : "variants"} · ${totalOnlineWorkers} online`
      : props.manifestLoading
        ? "loading"
        : props.manifestError
          ? "load failed"
          : props.target.tenantId === null
            ? "shared target"
            : "not observed";

  return (
    <div>
      <button
        type="button"
        aria-controls={regionId}
        aria-expanded={open}
        className="inline-flex min-h-6 items-center gap-1 text-[11px] font-medium text-foreground/75 hover:text-foreground"
        onClick={() => setOpen((value) => !value)}
      >
        <DisclosureChevron className="size-3" open={open} />
        Observed manifest
        <span className="font-normal text-muted-foreground">· {observedState}</span>
      </button>
      <DisclosureRegion open={open}>
        <div
          id={regionId}
          className="mt-1.5 space-y-3 rounded-lg border border-border bg-foreground/[0.025] p-2.5 text-[10px] leading-relaxed"
        >
          <dl className="grid gap-1.5">
            <div className="grid gap-0.5 sm:grid-cols-[10rem_1fr] sm:gap-2">
              <dt className="font-medium text-foreground">Experimental Providers</dt>
              <dd className="text-muted-foreground">
                {experimentalProviders.length > 0
                  ? experimentalProviders.join(", ")
                  : "None explicitly enabled"}
              </dd>
            </div>
          </dl>
          {props.canManage && props.tenantId && props.onProviderPolicyUpdated ? (
            <ProviderPolicyControls
              enabledProviders={experimentalProviders}
              onUpdated={props.onProviderPolicyUpdated}
              target={props.target}
              tenantId={props.tenantId}
            />
          ) : null}
          {props.tenantId && props.target.tenantId !== null ? (
            <WorkerReleaseControls
              canManage={props.canManage ?? false}
              enabled={open}
              manifests={props.releaseManifests ?? manifests}
              target={props.target}
              tenantId={props.tenantId}
            />
          ) : null}
          {props.tenantId ? (
            <ExecutionTargetWorkerManagement
              canManage={props.canManageWorkers ?? false}
              target={props.target}
              tenantId={props.tenantId}
              {...(props.workers ? { workers: props.workers } : {})}
              {...(props.workersError !== undefined ? { workersError: props.workersError } : {})}
              {...(props.workersLoading !== undefined
                ? { workersLoading: props.workersLoading }
                : {})}
              {...(props.onWorkersChanged ? { onWorkersChanged: props.onWorkersChanged } : {})}
            />
          ) : null}
          {manifests.length > 0 ? (
            <div className="space-y-2">
              {manifests.map((manifest, index) => (
                <ObservedWorkerManifestDetails
                  key={manifest.manifestId}
                  manifest={manifest}
                  variantCount={manifests.length}
                  variantIndex={index}
                />
              ))}
            </div>
          ) : props.manifestLoading ? (
            <ObservedManifestNotice title="Loading observed manifest…">
              Worker compatibility data is being loaded. The execution target status remains
              authoritative.
            </ObservedManifestNotice>
          ) : props.manifestError ? (
            <ObservedManifestNotice title="Observed manifest could not be loaded">
              {`${manifestErrorMessage(props.manifestError)} The execution target status is unchanged.`}
            </ObservedManifestNotice>
          ) : props.target.tenantId === null ? (
            <ObservedManifestNotice title="Shared target observation is not available">
              This Tenant view has no Worker Manifest projection for the platform-shared target.
              Absence here is not a target health or availability signal.
            </ObservedManifestNotice>
          ) : (
            <ObservedManifestNotice title="Not observed yet">
              {props.target.kind === "kubernetes"
                ? "No active Worker Pod has registered a Manifest yet. This is expected before the first Worker startup and does not make the Kubernetes target unavailable."
                : "No active Worker has registered a Manifest yet. This observation state does not change the execution target status."}
            </ObservedManifestNotice>
          )}
        </div>
      </DisclosureRegion>
    </div>
  );
}

function ProviderPolicyControls(props: {
  tenantId: string;
  target: ControlPlaneExecutionTarget;
  enabledProviders: ReadonlyArray<string>;
  onUpdated: (target: ControlPlaneExecutionTarget) => void;
}) {
  const updatePolicy = useMutation({
    mutationFn: (experimentalProviders: ReadonlyArray<ProviderHostProviderKind>) =>
      controlPlaneClient.updateExecutionTargetProviderPolicy(
        props.tenantId,
        props.target.id,
        experimentalProviders,
      ),
    onSuccess: props.onUpdated,
  });
  const enabled = new Set(props.enabledProviders);
  const toggle = (provider: ProviderHostProviderKind) => {
    const next = new Set(props.enabledProviders);
    if (next.has(provider)) next.delete(provider);
    else next.add(provider);
    updatePolicy.mutate(
      PROVIDER_CAPABILITY_CATALOG.providers
        .map((entry) => entry.provider)
        .filter((candidate) => next.has(candidate)),
    );
  };

  return (
    <div className="space-y-1.5 border-t border-border/70 pt-2">
      <p className="font-medium text-foreground">Provider Policy</p>
      <div className="flex flex-wrap gap-1.5">
        {EXPERIMENTAL_PROVIDER_OPTIONS.map((entry) => {
          const active = enabled.has(entry.provider);
          return (
            <Button
              key={entry.provider}
              aria-pressed={active}
              disabled={updatePolicy.isPending}
              onClick={() => toggle(entry.provider)}
              size="xs"
              variant={active ? "secondary" : "outline"}
            >
              {active
                ? `Disable ${providerLabel(entry.provider)}`
                : `Enable ${providerLabel(entry.provider)}`}
            </Button>
          );
        })}
      </div>
      {updatePolicy.error ? (
        <p className="text-destructive">{manifestErrorMessage(updatePolicy.error)}</p>
      ) : (
        <p className="text-muted-foreground">
          A change fences current Worker Manifests until the Worker re-registers with the new
          policy.
        </p>
      )}
    </div>
  );
}

function providerLabel(provider: ProviderHostProviderKind): string {
  return provider === "claudeAgent"
    ? "Claude"
    : provider.charAt(0).toUpperCase() + provider.slice(1);
}

function ObservedManifestNotice(props: { title: string; children: string }) {
  return (
    <div className="rounded-md border border-border/80 bg-background/70 px-2.5 py-2">
      <p className="font-medium text-foreground">{props.title}</p>
      <p className="mt-0.5 text-muted-foreground">{props.children}</p>
    </div>
  );
}

function ObservedWorkerManifestDetails(props: {
  manifest: ControlPlaneWorkerManifest;
  variantCount: number;
  variantIndex: number;
}) {
  const { manifest } = props;
  const workers = manifest.workerStatusCounts;

  return (
    <article className="space-y-3 rounded-md border border-border/80 bg-background/70 p-2.5">
      <header className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
        <p className="font-medium text-foreground">
          {props.variantCount === 1
            ? "Manifest variant"
            : `Manifest variant ${props.variantIndex + 1}`}
        </p>
        <code className="break-all font-mono text-[9px] text-muted-foreground">
          {manifest.manifestId}
        </code>
      </header>
      <dl className="grid gap-x-5 gap-y-1.5 sm:grid-cols-2">
        <ManifestFact
          label="Workers"
          value={`${workers.online} online · ${workers.draining} draining · ${workers.offline} offline`}
        />
        <ManifestFact
          label="Last heartbeat"
          value={formatWorkerHeartbeat(manifest.lastHeartbeatAt)}
          dateTime={manifest.lastHeartbeatAt}
        />
        <ManifestFact
          label="Worker build"
          value={`${manifest.workerBuild.version} · ${manifest.workerBuild.operatingSystem}/${manifest.workerBuild.architecture}`}
        />
        {manifest.workerBuild.gitSha ? (
          <ManifestFact label="Git SHA" value={manifest.workerBuild.gitSha} mono />
        ) : null}
        {manifest.workerBuild.imageDigest ? (
          <ManifestFact label="Image digest" value={manifest.workerBuild.imageDigest} mono />
        ) : null}
        <ManifestFact
          label="Worker Protocol"
          value={formatVersionRange(
            manifest.workerProtocol.minimum,
            manifest.workerProtocol.maximum,
          )}
        />
        <ManifestFact
          label="Runtime Event"
          value={formatVersionRange(manifest.runtimeEvent.minimum, manifest.runtimeEvent.maximum)}
        />
      </dl>
      <div className="space-y-2">
        <p className="font-medium text-foreground">Providers</p>
        {manifest.providers.map((provider) => (
          <ObservedProviderManifest key={provider.provider} provider={provider} />
        ))}
      </div>
    </article>
  );
}

function ManifestFact(props: { label: string; value: string; mono?: boolean; dateTime?: string }) {
  return (
    <div className="min-w-0">
      <dt className="font-medium text-foreground">{props.label}</dt>
      <dd
        className={
          props.mono ? "break-all font-mono text-muted-foreground" : "text-muted-foreground"
        }
      >
        {props.dateTime ? <time dateTime={props.dateTime}>{props.value}</time> : props.value}
      </dd>
    </div>
  );
}

function ObservedProviderManifest(props: { provider: ControlPlaneWorkerProviderManifest }) {
  const { provider } = props;
  const capabilityGroups = groupProviderCapabilities(provider);
  const runtimeRange = provider.runtime.compatibleRange.maximumExclusive
    ? `${provider.runtime.compatibleRange.minimumInclusive} ≤ version < ${provider.runtime.compatibleRange.maximumExclusive}`
    : `version ≥ ${provider.runtime.compatibleRange.minimumInclusive}`;

  return (
    <article className="space-y-2 rounded-md border border-border/80 bg-background/70 p-2.5">
      <header className="flex flex-wrap items-center gap-1.5">
        <code className="mr-auto font-mono text-[10px] font-semibold text-foreground">
          {provider.provider}
        </code>
        <ControlPlaneStatusPill value={provider.supportTier} active={false} />
        <ControlPlaneStatusPill
          value={provider.compatibilityStatus}
          active={provider.compatibilityStatus === "compatible"}
        />
      </header>
      <dl className="grid gap-x-5 gap-y-1.5 sm:grid-cols-2">
        <ManifestFact
          label="Runtime"
          value={`${provider.runtime.kind} · ${provider.runtime.name} · ${provider.runtime.version ?? "version not reported"}`}
        />
        <ManifestFact
          label="Runtime check"
          value={`${provider.runtime.available ? "available" : "unavailable"} · ${provider.runtime.compatible ? "compatible" : "incompatible"} · ${provider.runtime.versionSource}`}
        />
        <ManifestFact label="Compatible range" value={runtimeRange} />
        <ManifestFact label="Release policy" value={formatReleasePolicy(provider)} />
      </dl>
      {provider.incompatibilityCode || provider.incompatibilityMessage ? (
        <p className="rounded-md border border-border bg-foreground/[0.025] px-2 py-1.5 text-muted-foreground">
          {provider.incompatibilityCode ? (
            <code className="mr-1 font-mono text-foreground">{provider.incompatibilityCode}</code>
          ) : null}
          {provider.incompatibilityMessage}
        </p>
      ) : null}
      <dl className="grid gap-1.5 border-t border-border/70 pt-2">
        {capabilityGroups.map((group) => (
          <div key={group.support} className="grid gap-0.5 sm:grid-cols-[6rem_1fr] sm:gap-2">
            <dt>
              <ControlPlaneStatusPill value={group.support} active={group.support === "native"} />
            </dt>
            <dd className="break-words font-mono text-[9px] text-muted-foreground">
              {group.capabilities.length > 0 ? group.capabilities.join(", ") : "None"}
            </dd>
          </div>
        ))}
      </dl>
    </article>
  );
}

function groupProviderCapabilities(provider: ControlPlaneWorkerProviderManifest) {
  const groups = {
    native: [] as Array<string>,
    emulated: [] as Array<string>,
    unsupported: [] as Array<string>,
  };
  for (const [capability, support] of Object.entries(provider.capabilities)) {
    groups[support].push(capability);
  }
  return (Object.keys(groups) as Array<keyof typeof groups>).map((support) => ({
    support,
    capabilities: groups[support],
  }));
}

function groupWorkerManifestsByTarget(
  manifests: ReadonlyArray<ControlPlaneWorkerManifest>,
): ReadonlyMap<string, ReadonlyArray<ControlPlaneWorkerManifest>> {
  const grouped = new Map<string, Array<ControlPlaneWorkerManifest>>();
  for (const manifest of manifests) {
    const targetManifests = grouped.get(manifest.executionTargetId);
    if (targetManifests) targetManifests.push(manifest);
    else grouped.set(manifest.executionTargetId, [manifest]);
  }
  for (const [targetId, targetManifests] of grouped) {
    grouped.set(targetId, targetManifests.toSorted(compareWorkerManifestVariants));
  }
  return grouped;
}

function groupWorkersByTarget(
  workers: ReadonlyArray<ControlPlaneWorker>,
): ReadonlyMap<string, ReadonlyArray<ControlPlaneWorker>> {
  const grouped = new Map<string, Array<ControlPlaneWorker>>();
  for (const worker of workers) {
    if (typeof worker.executionTargetId !== "string" || worker.executionTargetId.length === 0) {
      continue;
    }
    const targetWorkers = grouped.get(worker.executionTargetId);
    if (targetWorkers) targetWorkers.push(worker);
    else grouped.set(worker.executionTargetId, [worker]);
  }
  return grouped;
}

function compareWorkerManifestVariants(
  left: ControlPlaneWorkerManifest,
  right: ControlPlaneWorkerManifest,
): number {
  const onlineDifference = right.workerStatusCounts.online - left.workerStatusCounts.online;
  if (onlineDifference !== 0) return onlineDifference;
  const heartbeatDifference = right.lastHeartbeatAt.localeCompare(left.lastHeartbeatAt);
  if (heartbeatDifference !== 0) return heartbeatDifference;
  return left.manifestId.localeCompare(right.manifestId);
}

function formatReleasePolicy(provider: ControlPlaneWorkerProviderManifest): string {
  if (!provider.releasePolicy.requiresExplicitEnablement) return "No explicit enablement required";
  return provider.releasePolicy.enabled
    ? "Explicitly enabled"
    : "Explicit enablement required · disabled";
}

function formatVersionRange(minimum: number, maximum: number): string {
  return minimum === maximum ? `v${minimum}` : `v${minimum}–v${maximum}`;
}

function formatWorkerHeartbeat(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : workerHeartbeatFormatter.format(date);
}

function manifestErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The Worker Manifest request failed.";
}

function readExperimentalProviders(capabilities: Record<string, unknown>): ReadonlyArray<string> {
  const policy = capabilities.providerPolicy;
  if (typeof policy !== "object" || policy === null || Array.isArray(policy)) return [];
  const providers = (policy as Record<string, unknown>).experimentalProviders;
  if (!Array.isArray(providers)) return [];
  return providers.filter((provider): provider is string => typeof provider === "string");
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
