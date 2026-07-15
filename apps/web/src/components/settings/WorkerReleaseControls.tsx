import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useRef, useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Textarea } from "~/components/ui/textarea";
import {
  ControlPlaneError,
  controlPlaneClient,
  type ControlPlaneExecutionTarget,
  type ControlPlaneWorkerManifest,
  type ControlPlaneWorkerReleaseOverview,
  type ControlPlaneWorkerReleaseRevision,
} from "~/lib/controlPlaneClient";
import { randomUUID } from "~/lib/utils";

export const workerReleasesQueryKey = (tenantId: string, targetId: string) =>
  ["control-plane", "tenants", tenantId, "execution-targets", targetId, "worker-releases"] as const;

type ReleaseAction = "canary" | "promote" | "rollback";
type IdempotencyState = { signature: string; key: string };

type CreateReleaseMutationInput = {
  workerManifestId: string;
  description: string;
  idempotencyKey: string;
};

type TransitionReleaseMutationInput = {
  revisionId: string;
  action: ReleaseAction;
  expectedPolicyVersion: number;
  reason: string;
  canaryPercent?: number;
  idempotencyKey: string;
};

const WORKER_RELEASE_TEXT_LIMIT = 2_000;

export function WorkerReleaseControls(props: {
  tenantId: string;
  target: ControlPlaneExecutionTarget;
  manifests: ReadonlyArray<ControlPlaneWorkerManifest>;
  canManage: boolean;
  enabled: boolean;
}) {
  const queryClient = useQueryClient();
  const [manifestId, setManifestId] = useState("");
  const [description, setDescription] = useState("");
  const [reason, setReason] = useState("");
  const [canaryPercent, setCanaryPercent] = useState(10);
  const [transitionNotice, setTransitionNotice] = useState<string | null>(null);
  const createIdempotency = useRef<IdempotencyState | null>(null);
  const transitionIdempotency = useRef<IdempotencyState | null>(null);
  const queryKey = useMemo(
    () => workerReleasesQueryKey(props.tenantId, props.target.id),
    [props.target.id, props.tenantId],
  );
  const releaseManagementDisabled = props.target.status !== "active";
  const canMutate = props.canManage && !releaseManagementDisabled;
  const managedImageTarget = isManagedImageTarget(props.target);
  const releases = useQuery({
    queryKey,
    queryFn: () => controlPlaneClient.listWorkerReleases(props.tenantId, props.target.id),
    enabled: props.enabled,
    staleTime: 15_000,
  });
  const registeredManifestIds = useMemo(
    () => new Set(releases.data?.revisions.map((revision) => revision.workerManifestId) ?? []),
    [releases.data?.revisions],
  );
  const availableManifests = useMemo(() => {
    const byID = new Map<string, ControlPlaneWorkerManifest>();
    for (const manifest of props.manifests) {
      if (
        registeredManifestIds.has(manifest.manifestId) ||
        (managedImageTarget && !manifest.workerBuild.imageDigest)
      ) {
        continue;
      }
      const current = byID.get(manifest.manifestId);
      if (!current || manifest.executionTargetId === props.target.id) {
        byID.set(manifest.manifestId, manifest);
      }
    }
    return [...byID.values()];
  }, [managedImageTarget, props.manifests, props.target.id, registeredManifestIds]);
  const selectedManifestId = availableManifests.some(
    (manifest) => manifest.manifestId === manifestId,
  )
    ? manifestId
    : (availableManifests[0]?.manifestId ?? "");

  const createRelease = useMutation({
    mutationFn: (input: CreateReleaseMutationInput) =>
      controlPlaneClient.createWorkerRelease(
        props.tenantId,
        props.target.id,
        { workerManifestId: input.workerManifestId, description: input.description },
        { idempotencyKey: input.idempotencyKey },
      ),
    onSuccess: async (_revision, input) => {
      createIdempotency.current = null;
      setDescription((current) => (current.trim() === input.description ? "" : current));
      await queryClient.invalidateQueries({ queryKey });
    },
  });
  const transitionRelease = useMutation({
    mutationFn: (input: TransitionReleaseMutationInput) =>
      controlPlaneClient.transitionWorkerRelease(
        props.tenantId,
        props.target.id,
        input.revisionId,
        input.action,
        {
          expectedPolicyVersion: input.expectedPolicyVersion,
          reason: input.reason,
          ...(input.action === "canary" ? { canaryPercent: input.canaryPercent } : {}),
        },
        { idempotencyKey: input.idempotencyKey },
      ),
    onMutate: () => setTransitionNotice(null),
    onError: async (error) => {
      if (!isPolicyVersionConflict(error)) return;
      transitionIdempotency.current = null;
      await queryClient.invalidateQueries({ queryKey });
      setTransitionNotice(policyConflictNotice(error));
    },
    onSuccess: async (_policy, input) => {
      transitionIdempotency.current = null;
      setReason((current) => (current.trim() === input.reason ? "" : current));
      await queryClient.invalidateQueries({ queryKey });
    },
  });

  const submitRelease = (event: FormEvent) => {
    event.preventDefault();
    if (!canMutate || !selectedManifestId) return;
    const trimmedDescription = description.trim();
    const signature = JSON.stringify([selectedManifestId, trimmedDescription]);
    const idempotencyKey = resolveIdempotencyKey(
      createIdempotency,
      signature,
      "web-worker-release-create",
    );
    createRelease.mutate({
      workerManifestId: selectedManifestId,
      description: trimmedDescription,
      idempotencyKey,
    });
  };
  const requestTransition = (revisionId: string, action: ReleaseAction) => {
    if (!canMutate) return;
    const expectedPolicyVersion = releases.data?.policy?.policyVersion ?? 0;
    const trimmedReason = reason.trim();
    const requestedCanaryPercent = action === "canary" ? canaryPercent : undefined;
    const signature = JSON.stringify([
      revisionId,
      action,
      expectedPolicyVersion,
      trimmedReason,
      requestedCanaryPercent ?? 0,
    ]);
    const idempotencyKey = resolveIdempotencyKey(
      transitionIdempotency,
      signature,
      "web-worker-release-policy",
    );
    transitionRelease.mutate({
      revisionId,
      action,
      expectedPolicyVersion,
      reason: trimmedReason,
      ...(requestedCanaryPercent === undefined ? {} : { canaryPercent: requestedCanaryPercent }),
      idempotencyKey,
    });
  };
  const queryErrorMessage = errorMessage(
    releases.error,
    "Worker release policy could not be loaded.",
  );
  const createErrorMessage = errorMessage(
    createRelease.error,
    "Worker release registration failed.",
  );
  const transitionErrorMessage = isPolicyVersionConflict(transitionRelease.error)
    ? null
    : errorMessage(transitionRelease.error, "Worker release transition failed.");

  return (
    <div className="space-y-2 border-t border-border pt-2.5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <p className="font-medium text-foreground">Worker release policy</p>
          <p className="text-muted-foreground">
            Immutable revisions fence scheduling and drive managed Docker/Kubernetes image rollout.
          </p>
        </div>
        {releases.data?.policy ? (
          <ControlPlaneStatusPill
            active={false}
            value={`policy v${releases.data.policy.policyVersion}`}
          />
        ) : null}
      </div>

      {props.canManage && releaseManagementDisabled ? (
        <p className="rounded-md border border-border bg-foreground/[0.025] px-2.5 py-2 text-muted-foreground">
          Release changes are disabled while this Execution Target is {props.target.status}.
          Reactivate the target before changing its Worker release policy.
        </p>
      ) : null}

      {releases.isPending ? (
        <p aria-live="polite" className="text-muted-foreground">
          Loading Worker release policy…
        </p>
      ) : releases.data ? (
        <WorkerReleaseRevisionList
          canManage={props.canManage}
          canaryPercent={canaryPercent}
          isPending={transitionRelease.isPending}
          managementDisabled={releaseManagementDisabled}
          overview={releases.data}
          reason={reason.trim()}
          onTransition={requestTransition}
        />
      ) : null}

      {props.canManage && releases.data ? (
        <div className="grid gap-2 rounded-md border border-border/80 bg-background/60 p-2.5">
          <ControlPlaneFormField label="Release reason">
            <Input
              disabled={releaseManagementDisabled || transitionRelease.isPending}
              maxLength={WORKER_RELEASE_TEXT_LIMIT}
              placeholder="Why this rollout or rollback is required"
              value={reason}
              onChange={(event) => {
                setReason(event.target.value);
                transitionIdempotency.current = null;
                transitionRelease.reset();
                setTransitionNotice(null);
              }}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Canary traffic percent">
            <Input
              disabled={releaseManagementDisabled || transitionRelease.isPending}
              max={100}
              min={1}
              type="number"
              value={canaryPercent}
              onChange={(event) => {
                setCanaryPercent(Math.max(1, Math.min(100, Number(event.target.value) || 1)));
                transitionIdempotency.current = null;
                transitionRelease.reset();
                setTransitionNotice(null);
              }}
            />
          </ControlPlaneFormField>
          <p className="text-[10px] leading-relaxed text-muted-foreground sm:col-span-2">
            Canary Workers are rounded up to a whole Worker. Small pools can exceed the requested
            percentage; a one-Worker pool switches entirely to canary.
          </p>
        </div>
      ) : null}

      {props.canManage && releases.data ? (
        <form className={CONTROL_PLANE_FORM_GRID_CLASS_NAME} onSubmit={submitRelease}>
          <ControlPlaneFormField label="Observed Worker manifest">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              disabled={
                releaseManagementDisabled ||
                createRelease.isPending ||
                availableManifests.length === 0
              }
              value={selectedManifestId}
              onChange={(event) => {
                setManifestId(event.target.value);
                createIdempotency.current = null;
                createRelease.reset();
              }}
            >
              {availableManifests.length === 0 ? (
                <option value="">No unregistered manifest</option>
              ) : null}
              {availableManifests.map((manifest) => (
                <option key={manifest.manifestId} value={manifest.manifestId}>
                  {manifest.workerBuild.version} · {shortIdentifier(manifest.manifestId)}
                  {manifest.executionTargetId === props.target.id ? "" : " · observed elsewhere"}
                </option>
              ))}
            </select>
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Revision description">
            <Textarea
              disabled={releaseManagementDisabled || createRelease.isPending}
              maxLength={WORKER_RELEASE_TEXT_LIMIT}
              placeholder="Optional immutable release note"
              size="sm"
              value={description}
              onChange={(event) => {
                setDescription(event.target.value);
                createIdempotency.current = null;
                createRelease.reset();
              }}
            />
          </ControlPlaneFormField>
          <div className="sm:col-span-2">
            <Button
              disabled={!canMutate || availableManifests.length === 0 || createRelease.isPending}
              size="sm"
              type="submit"
              variant="outline"
            >
              {createRelease.isPending ? "Registering revision…" : "Register immutable revision"}
            </Button>
          </div>
        </form>
      ) : null}

      {releases.data ? <WorkerReleaseTransitionHistory overview={releases.data} /> : null}

      <div aria-live="polite" className="space-y-1">
        {transitionNotice ? (
          <p className="text-xs leading-relaxed text-muted-foreground" role="status">
            {transitionNotice}
          </p>
        ) : null}
        {queryErrorMessage ? <ReleaseRequestError message={queryErrorMessage} /> : null}
        {createErrorMessage ? <ReleaseRequestError message={createErrorMessage} /> : null}
        {transitionErrorMessage ? <ReleaseRequestError message={transitionErrorMessage} /> : null}
      </div>
    </div>
  );
}

function WorkerReleaseTransitionHistory(props: { overview: ControlPlaneWorkerReleaseOverview }) {
  const recent = props.overview.transitions.slice(0, 3);
  if (recent.length === 0) return null;
  return (
    <section
      aria-label="Recent Worker release changes"
      className="rounded-md border border-border/80 bg-background/60 px-2.5 py-2"
    >
      <p className="font-medium text-foreground">Recent policy changes</p>
      <ol className="mt-1 space-y-1 text-muted-foreground">
        {recent.map((transition) => (
          <li key={transition.id} className="flex flex-wrap gap-x-1.5">
            <span className="font-medium text-foreground/80">
              policy v{transition.policyVersion}
            </span>
            <span>· {formatReleaseAction(transition.action)}</span>
            <span className="min-w-0 truncate">· {transition.reason}</span>
          </li>
        ))}
      </ol>
    </section>
  );
}

function WorkerReleaseRevisionList(props: {
  overview: ControlPlaneWorkerReleaseOverview;
  canManage: boolean;
  isPending: boolean;
  managementDisabled: boolean;
  reason: string;
  canaryPercent: number;
  onTransition: (revisionId: string, action: ReleaseAction) => void;
}) {
  if (props.overview.revisions.length === 0) {
    return (
      <p className="rounded-md border border-dashed border-border p-2.5 text-muted-foreground">
        No immutable release revision has been registered for this target.
      </p>
    );
  }
  const policy = props.overview.policy;
  const promoted = policy
    ? props.overview.revisions.find((revision) => revision.id === policy.promotedRevisionId)
    : undefined;
  return (
    <div className="space-y-1.5">
      {props.overview.revisions.map((revision) => {
        const isPromoted = policy?.promotedRevisionId === revision.id;
        const isCanary = policy?.canaryRevisionId === revision.id;
        return (
          <div
            key={revision.id}
            className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border/80 bg-background/60 px-2.5 py-2"
          >
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="font-medium text-foreground">Revision {revision.revision}</span>
                <span className="text-muted-foreground">· {revision.workerBuildVersion}</span>
                {isPromoted ? <ControlPlaneStatusPill value="promoted" /> : null}
                {isCanary ? (
                  <ControlPlaneStatusPill value={`canary ${policy?.canaryPercent ?? 0}%`} />
                ) : null}
              </div>
              <p className="truncate text-muted-foreground">
                {revision.imageDigest
                  ? `Image ${shortIdentifier(revision.imageDigest)}`
                  : "No image digest reported"}
                {revision.description ? ` · ${revision.description}` : ""}
              </p>
            </div>
            {props.canManage ? (
              <span className="flex flex-wrap justify-end gap-1.5">
                {!policy ? (
                  <ReleaseActionButton
                    ariaLabel={`Promote revision ${revision.revision} as baseline`}
                    disabled={props.managementDisabled || !props.reason || props.isPending}
                    label="Promote baseline"
                    onClick={() => props.onTransition(revision.id, "promote")}
                  />
                ) : null}
                {policy && promoted && revision.revision > promoted.revision && !isCanary ? (
                  <ReleaseActionButton
                    ariaLabel={`Start canary with revision ${revision.revision}`}
                    disabled={
                      props.managementDisabled ||
                      !props.reason ||
                      props.isPending ||
                      props.canaryPercent < 1
                    }
                    label="Start canary"
                    onClick={() => props.onTransition(revision.id, "canary")}
                  />
                ) : null}
                {isCanary ? (
                  <>
                    <ReleaseActionButton
                      ariaLabel={`Promote canary revision ${revision.revision}`}
                      disabled={props.managementDisabled || !props.reason || props.isPending}
                      label="Promote canary"
                      onClick={() => props.onTransition(revision.id, "promote")}
                    />
                    {promoted ? (
                      <ReleaseActionButton
                        ariaLabel={`Abort canary revision ${revision.revision} and keep promoted revision ${promoted.revision}`}
                        disabled={props.managementDisabled || !props.reason || props.isPending}
                        label="Abort canary"
                        onClick={() => props.onTransition(promoted.id, "rollback")}
                      />
                    ) : null}
                  </>
                ) : null}
                {policy && promoted && revision.revision < promoted.revision ? (
                  <ReleaseActionButton
                    ariaLabel={`Roll back to revision ${revision.revision}`}
                    disabled={props.managementDisabled || !props.reason || props.isPending}
                    label="Roll back"
                    onClick={() => props.onTransition(revision.id, "rollback")}
                  />
                ) : null}
              </span>
            ) : null}
          </div>
        );
      })}
    </div>
  );
}

function ReleaseActionButton(props: {
  ariaLabel: string;
  label: string;
  disabled: boolean;
  onClick: () => void;
}) {
  return (
    <Button
      aria-label={props.ariaLabel}
      disabled={props.disabled}
      onClick={props.onClick}
      size="sm"
      type="button"
      variant="outline"
    >
      {props.label}
    </Button>
  );
}

function ReleaseRequestError(props: { message: string }) {
  return (
    <p className="text-xs leading-relaxed text-destructive" role="alert">
      {props.message}
    </p>
  );
}

function resolveIdempotencyKey(
  reference: { current: IdempotencyState | null },
  signature: string,
  prefix: string,
): string {
  if (reference.current?.signature !== signature) {
    reference.current = { signature, key: `${prefix}-${randomUUID()}` };
  }
  return reference.current.key;
}

function errorMessage(error: unknown, fallback: string): string | null {
  if (!error) return null;
  return error instanceof Error ? error.message : fallback;
}

function isPolicyVersionConflict(error: unknown): error is ControlPlaneError {
  return (
    error instanceof ControlPlaneError && error.code === "worker_release_policy_version_conflict"
  );
}

function policyConflictNotice(error: ControlPlaneError): string {
  const currentPolicyVersion = error.details?.currentPolicyVersion;
  const versionSuffix =
    typeof currentPolicyVersion === "number" ? ` to version ${currentPolicyVersion}` : "";
  return `The Worker release policy changed${versionSuffix} and has been refreshed. Review the current policy before retrying.`;
}

function shortIdentifier(value: string): string {
  return value.length <= 18 ? value : `${value.slice(0, 10)}…${value.slice(-6)}`;
}

function formatReleaseAction(
  action: ControlPlaneWorkerReleaseOverview["transitions"][number]["action"],
): string {
  switch (action) {
    case "abort-canary":
      return "canary aborted";
    case "canary":
      return "canary started";
    case "promote":
      return "promoted";
    case "rollback":
      return "rolled back";
  }
}

function isManagedImageTarget(target: ControlPlaneExecutionTarget): boolean {
  return target.kind === "docker" || target.kind === "kubernetes";
}
