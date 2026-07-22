import { useMutation } from "@tanstack/react-query";
import { useMemo, useRef, useState } from "react";

import {
  ControlPlaneInlineError,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { Button } from "~/components/ui/button";
import { DisclosureChevron } from "~/components/ui/DisclosureChevron";
import { DisclosureRegion } from "~/components/ui/DisclosureRegion";
import { Textarea } from "~/components/ui/textarea";
import {
  ControlPlaneError,
  controlPlaneClient,
  type ControlPlaneExecutionTarget,
  type ControlPlaneWorker,
  type ControlPlaneWorkerRevocationResult,
} from "~/lib/controlPlaneClient";
import { randomUUID } from "~/lib/utils";

const EMPTY_WORKERS: ReadonlyArray<ControlPlaneWorker> = [];
const MAX_REVOCATION_REASON_LENGTH = 2_000;
const timestampFormatter = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
});

export const tenantWorkersQueryKey = (tenantId: string | null) =>
  ["control-plane", "tenants", tenantId, "workers"] as const;

export function ExecutionTargetWorkerManagement(props: {
  tenantId: string;
  target: ControlPlaneExecutionTarget;
  workers?: ReadonlyArray<ControlPlaneWorker>;
  workersLoading?: boolean;
  workersError?: unknown;
  canManage?: boolean;
  onWorkersChanged?: () => void;
}) {
  const workers = useMemo(() => sortWorkers(props.workers ?? EMPTY_WORKERS), [props.workers]);

  return (
    <section className="space-y-2 border-t border-border/70 pt-2" aria-label="Workers">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
        <p className="font-medium text-foreground">Workers</p>
        <span className="text-muted-foreground">
          {workers.length === 0
            ? "no registered Workers"
            : `${workers.length} ${workers.length === 1 ? "registration" : "registrations"}`}
        </span>
      </div>
      {workers.length > 0 ? (
        <div className="space-y-2">
          {workers.map((worker) => (
            <WorkerCard
              key={`${worker.id}:${worker.incarnation}`}
              canManage={props.canManage ?? false}
              target={props.target}
              tenantId={props.tenantId}
              worker={worker}
              {...(props.onWorkersChanged ? { onWorkersChanged: props.onWorkersChanged } : {})}
            />
          ))}
        </div>
      ) : props.workersLoading ? (
        <WorkerNotice title="Loading Workers…">
          Worker registrations for this Execution Target are being loaded.
        </WorkerNotice>
      ) : props.workersError ? (
        <WorkerNotice title="Workers could not be loaded">
          {errorMessage(props.workersError)}
        </WorkerNotice>
      ) : (
        <WorkerNotice title="No Worker registered">
          No Worker has registered for this Execution Target yet.
        </WorkerNotice>
      )}
    </section>
  );
}

function WorkerCard(props: {
  tenantId: string;
  target: ControlPlaneExecutionTarget;
  worker: ControlPlaneWorker;
  canManage: boolean;
  onWorkersChanged?: () => void;
}) {
  const [revokeOpen, setRevokeOpen] = useState(false);
  const [reason, setReason] = useState("");
  const [result, setResult] = useState<ControlPlaneWorkerRevocationResult | null>(null);
  const pendingOperation = useRef<{ signature: string; idempotencyKey: string } | null>(null);
  const worker = result?.worker ?? props.worker;
  const canRevoke =
    props.canManage &&
    (worker.administrativeStatus === "active" || worker.administrativeStatus === "draining");
  const revoke = useMutation({
    mutationFn: () => {
      const trimmedReason = reason.trim();
      const signature = JSON.stringify([worker.id, worker.incarnation, trimmedReason]);
      const operation =
        pendingOperation.current?.signature === signature
          ? pendingOperation.current
          : { signature, idempotencyKey: `web-worker-revoke-${randomUUID()}` };
      pendingOperation.current = operation;
      return controlPlaneClient.revokeWorker(
        props.tenantId,
        worker.id,
        { expectedIncarnation: worker.incarnation, reason: trimmedReason },
        { idempotencyKey: operation.idempotencyKey },
      );
    },
    retry: (failureCount, error) => failureCount < 2 && isRetryableUnknownOutcome(error),
    onSuccess: (nextResult) => {
      pendingOperation.current = null;
      setResult(nextResult);
      setReason("");
      setRevokeOpen(false);
      props.onWorkersChanged?.();
    },
    onError: (error) => {
      if (isDefinitiveRevocationFailure(error)) {
        pendingOperation.current = null;
      }
      if (isWorkerStateConflict(error)) {
        props.onWorkersChanged?.();
      }
    },
  });

  return (
    <article className="space-y-3 rounded-md border border-border/80 bg-background/70 p-2.5">
      <header className="flex flex-wrap items-center gap-1.5">
        <div className="mr-auto min-w-0">
          <p className="break-all font-medium text-foreground">{worker.podName}</p>
          <code className="break-all font-mono text-[10px] text-muted-foreground">{worker.id}</code>
        </div>
        <ControlPlaneStatusPill value={worker.status} />
        <ControlPlaneStatusPill value={worker.administrativeStatus} />
        <ControlPlaneStatusPill value={worker.compatibilityStatus} />
      </header>

      <dl className="grid gap-x-5 gap-y-1.5 sm:grid-cols-2">
        <WorkerFact label="Target" value={`${props.target.name} · ${worker.targetKind}`} />
        <WorkerFact label="Incarnation" mono value={String(worker.incarnation)} />
        <WorkerFact label="Instance UID" mono value={worker.instanceUid} />
        <WorkerFact label="Cluster" mono value={worker.clusterId} />
        <WorkerFact label="Namespace" mono value={worker.namespace} />
        <WorkerFact
          label="Version"
          value={`${worker.version} · protocol ${worker.protocolVersion}`}
        />
        <WorkerFact label="Release" value={formatRelease(worker)} />
        <WorkerFact label="Manifest" mono value={worker.currentManifestId ?? "not reported"} />
        <WorkerFact
          label="Last heartbeat"
          value={formatTimestamp(worker.lastHeartbeatAt)}
          dateTime={worker.lastHeartbeatAt}
        />
        <WorkerFact
          label="Lease / fencing"
          value={`${worker.leaseSupported ? "lease" : "no lease"} · ${worker.fencingSupported ? "fencing" : "no fencing"}`}
        />
      </dl>

      {worker.compatibilityReason ? (
        <p className="text-muted-foreground">{worker.compatibilityReason}</p>
      ) : null}
      {worker.revocationReason ? (
        <WorkerNotice title="Revocation reason">{worker.revocationReason}</WorkerNotice>
      ) : null}

      {canRevoke ? (
        <div className="space-y-2 border-t border-border/70 pt-2">
          <button
            type="button"
            aria-controls={`worker-revoke-${worker.id}`}
            aria-expanded={revokeOpen}
            aria-label={`Open revoke controls for worker ${worker.id}`}
            className="inline-flex min-h-6 items-center gap-1 text-[11px] font-medium text-foreground/75 hover:text-foreground"
            onClick={() => setRevokeOpen((open) => !open)}
          >
            <DisclosureChevron className="size-3" open={revokeOpen} />
            Revoke worker
            <span className="font-normal text-muted-foreground">
              {`· current incarnation ${worker.incarnation}`}
            </span>
          </button>
          <DisclosureRegion open={revokeOpen}>
            <div
              id={`worker-revoke-${worker.id}`}
              className="mt-1.5 space-y-2 rounded-md border border-border/80 bg-background/70 p-2.5"
            >
              <label className="grid gap-1">
                <span className="font-medium text-foreground">Revocation reason</span>
                <Textarea
                  aria-label={`Revocation reason for worker ${worker.id}`}
                  disabled={revoke.isPending}
                  maxLength={MAX_REVOCATION_REASON_LENGTH}
                  placeholder="Confirmed Worker identity compromise"
                  value={reason}
                  onChange={(event) => setReason(event.target.value)}
                />
              </label>
              <p className="text-muted-foreground">
                {`This permanently revokes incarnation ${worker.incarnation}; use Drain for planned replacement.`}
              </p>
              <div className="flex flex-wrap gap-1.5">
                <Button
                  aria-label={`Revoke worker ${worker.id} at incarnation ${worker.incarnation}`}
                  disabled={revoke.isPending || reason.trim().length === 0}
                  onClick={() => revoke.mutate()}
                  size="xs"
                  variant="destructive"
                >
                  {revoke.isPending ? "Revoking…" : "Revoke worker"}
                </Button>
                <Button
                  disabled={revoke.isPending}
                  onClick={() => setRevokeOpen(false)}
                  size="xs"
                  variant="outline"
                >
                  Cancel
                </Button>
              </div>
              <ControlPlaneInlineError error={revoke.error} />
            </div>
          </DisclosureRegion>
        </div>
      ) : worker.administrativeStatus === "revoked" ? null : (
        <p className="border-t border-border/70 pt-2 text-muted-foreground">
          {props.canManage
            ? "This Worker cannot be revoked from its current administrative state."
            : "Worker revocation requires Worker manage permission."}
        </p>
      )}

      {result ? (
        <WorkerNotice title="Worker revoked">{formatRevocationCounts(result)}</WorkerNotice>
      ) : null}
    </article>
  );
}

function WorkerFact(props: { label: string; value: string; mono?: boolean; dateTime?: string }) {
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

function WorkerNotice(props: { title: string; children: string }) {
  return (
    <div className="rounded-md border border-border/80 bg-background/70 px-2.5 py-2">
      <p className="font-medium text-foreground">{props.title}</p>
      <p className="mt-0.5 text-muted-foreground">{props.children}</p>
    </div>
  );
}

function sortWorkers(
  workers: ReadonlyArray<ControlPlaneWorker>,
): ReadonlyArray<ControlPlaneWorker> {
  return [...workers].sort((left, right) => {
    const stateDifference = workerStateRank(left) - workerStateRank(right);
    if (stateDifference !== 0) return stateDifference;
    const heartbeatDifference =
      Date.parse(right.lastHeartbeatAt) - Date.parse(left.lastHeartbeatAt);
    if (Number.isFinite(heartbeatDifference) && heartbeatDifference !== 0)
      return heartbeatDifference;
    return left.id.localeCompare(right.id);
  });
}

function workerStateRank(worker: ControlPlaneWorker): number {
  if (worker.administrativeStatus === "active" && worker.status === "online") return 0;
  if (worker.administrativeStatus === "draining") return 1;
  if (worker.administrativeStatus === "active") return 2;
  if (worker.administrativeStatus === "revoked") return 4;
  return 3;
}

function formatRelease(worker: ControlPlaneWorker): string {
  const details = [worker.workerReleaseChannel, worker.workerReleaseStatus].filter(Boolean);
  if (worker.workerReleaseRevisionId) details.push(worker.workerReleaseRevisionId);
  return details.join(" · ") || "unmanaged";
}

function formatTimestamp(value: string): string {
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp) ? timestampFormatter.format(new Date(timestamp)) : value;
}

function formatRevocationCounts(result: ControlPlaneWorkerRevocationResult): string {
  return `${result.releasedExecutionLeases} leases released · ${result.recoveringExecutions} recovering · ${result.outcomeUnknownExecutions} outcome unknown · ${result.checkpointUnconfirmedExecutions} checkpoint unconfirmed · ${result.requeuedWorkspaceCleanups} cleanups requeued`;
}

function isRetryableUnknownOutcome(error: unknown): boolean {
  return !(error instanceof ControlPlaneError) || error.status === 429 || error.status >= 500;
}

function isDefinitiveRevocationFailure(error: unknown): boolean {
  return error instanceof ControlPlaneError && error.status < 500 && error.status !== 429;
}

function isWorkerStateConflict(error: unknown): boolean {
  return (
    error instanceof ControlPlaneError &&
    (error.code === "worker_incarnation_conflict" ||
      error.code === "worker_revocation_conflict" ||
      error.code === "worker_administrative_state_invalid" ||
      error.code === "idempotency_conflict")
  );
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The Worker request failed.";
}
