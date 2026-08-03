// FILE: PlatformCapacityGovernance.tsx
// Purpose: Bind exact capacity/soak receipts and separated internal decisions to a release.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneStage6CapacityApprovalRole,
  type ControlPlaneStage6CapacityRun,
} from "@synara/control-plane-client";
import { useMemo, useState } from "react";

import {
  Button,
  EmptyState,
  Field,
  InlineError,
  Input,
  LoadingState,
  Select,
  StatusPill,
} from "../components/ui";
import { platformQueryKeys } from "./platformQueries";

const approvalRoles: ReadonlyArray<ControlPlaneStage6CapacityApprovalRole> = [
  "engineering",
  "operations",
];

function tone(state: ControlPlaneStage6CapacityRun["state"]): "active" | "neutral" | "danger" {
  if (state === "approved") return "active";
  if (state === "rejected") return "danger";
  return "neutral";
}

export function PlatformCapacityGovernance(props: { readonly canManage: boolean }) {
  const queryClient = useQueryClient();
  const runs = useQuery({
    queryKey: platformQueryKeys.capacityRuns,
    queryFn: controlPlaneClient.listPlatformStage6CapacityRuns,
  });
  const candidates = useQuery({
    queryKey: platformQueryKeys.releaseCandidates,
    queryFn: controlPlaneClient.listPlatformStage6ReleaseCandidates,
  });
  const [selectedId, setSelectedId] = useState("");
  const effectiveId = selectedId || runs.data?.items[0]?.id || "";
  const selected = useMemo(
    () => runs.data?.items.find((item) => item.id === effectiveId) ?? null,
    [effectiveId, runs.data],
  );
  const replace = (item: ControlPlaneStage6CapacityRun) => {
    queryClient.setQueryData(platformQueryKeys.capacityRuns, (current: typeof runs.data) => ({
      items: current?.items.some((existing) => existing.id === item.id)
        ? current.items.map((existing) => (existing.id === item.id ? item : existing))
        : [item, ...(current?.items ?? [])],
    }));
  };

  if (runs.isPending || candidates.isPending)
    return <LoadingState label="Loading Capacity governance…" />;
  if (runs.error || candidates.error) return <InlineError error={runs.error ?? candidates.error} />;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Capacity reviews</h1>
          <p>
            Exact candidate-bound production or production-like soak receipts and separated
            Engineering/Operations decisions. Internal approval does not authenticate the
            environment, telemetry, signatures or real execution.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <ImportRun
          candidates={candidates.data.items}
          onImported={(item) => {
            replace(item);
            setSelectedId(item.id);
          }}
        />
      ) : null}
      {runs.data.items.length === 0 ? (
        <EmptyState
          title="No Capacity reviews"
          description="Import the exact validator receipt after the uninterrupted soak run and protected evidence review complete."
        />
      ) : (
        <section className="form-panel">
          <Field label="Capacity run">
            <Select value={effectiveId} onChange={(event) => setSelectedId(event.target.value)}>
              {runs.data.items.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.candidateId} · {item.state} · {item.runId}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? <RunDetail item={selected} onChanged={replace} /> : null}
    </div>
  );
}

function ImportRun(props: {
  readonly candidates: ReadonlyArray<{ readonly id: string; readonly candidateId: string }>;
  readonly onImported: (item: ControlPlaneStage6CapacityRun) => void;
}) {
  const [candidateRecordId, setCandidateRecordId] = useState(props.candidates[0]?.id ?? "");
  const [receipt, setReceipt] = useState<{ base64: string; sha256: string; name: string } | null>(
    null,
  );
  const [readError, setReadError] = useState<unknown>(null);
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.importPlatformStage6CapacityRun({
        candidateRecordId,
        receiptBase64: receipt?.base64 ?? "",
        receiptSha256: receipt?.sha256 ?? "",
      }),
    onSuccess: props.onImported,
  });
  return (
    <section className="form-panel">
      <h2>Import exact Capacity receipt</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          mutation.mutate();
        }}
      >
        <Field label="Release candidate">
          <Select
            value={candidateRecordId}
            onChange={(event) => setCandidateRecordId(event.target.value)}
            required
          >
            <option value="">Select candidate</option>
            {props.candidates.map((candidate) => (
              <option key={candidate.id} value={candidate.id}>
                {candidate.candidateId}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Validated receipt JSON">
          <Input
            type="file"
            accept="application/json,.json"
            required
            onChange={(event) => {
              const file = event.target.files?.[0];
              setReceipt(null);
              setReadError(null);
              if (!file) return;
              void readReceipt(file).then(setReceipt).catch(setReadError);
            }}
          />
        </Field>
        {receipt ? (
          <p className="form-grid__wide">
            {receipt.name} · {receipt.sha256}
          </p>
        ) : null}
        <div className="form-actions form-grid__wide">
          <Button
            type="submit"
            variant="primary"
            disabled={!candidateRecordId || !receipt || mutation.isPending}
          >
            Import immutable run
          </Button>
        </div>
        <InlineError error={readError ?? mutation.error} />
      </form>
    </section>
  );
}

function RunDetail(props: {
  readonly item: ControlPlaneStage6CapacityRun;
  readonly onChanged: (item: ControlPlaneStage6CapacityRun) => void;
}) {
  const item = props.item;
  return (
    <>
      <section className="detail-card">
        <div className="detail-card__header">
          <div>
            <span className="eyebrow">{item.candidateId}</span>
            <h2>{item.runId}</h2>
          </div>
          <StatusPill value={item.state} tone={tone(item.state)} />
        </div>
        <dl className="detail-grid">
          <div>
            <dt>Environment</dt>
            <dd>
              {item.environmentClass} · {item.environmentId}
            </dd>
          </div>
          <div>
            <dt>Run window</dt>
            <dd>
              {new Date(item.startedAt).toLocaleString()} –{" "}
              {new Date(item.completedAt).toLocaleString()}
            </dd>
          </div>
          <div>
            <dt>Duration</dt>
            <dd>
              {(item.durationSeconds / 3600).toFixed(1)} hours · minimum{" "}
              {(item.minimumDurationSeconds / 3600).toFixed(0)}
            </dd>
          </div>
          <div>
            <dt>External probes</dt>
            <dd>
              {item.externalProbeRegions} Regions ·{" "}
              {(item.externalProbeCoverageRatio * 100).toFixed(2)}% coverage
            </dd>
          </div>
          <div>
            <dt>Machine gate</dt>
            <dd>
              {item.eligibleForHumanGateReview &&
              item.forecastHeadroomCovered &&
              item.measurementsWithinObjectives
                ? "eligible evidence"
                : "failed / incomplete"}
            </dd>
          </div>
          <div>
            <dt>External verification</dt>
            <dd>
              {item.externalAuthorityVerificationRequired
                ? "environment, telemetry, signatures, execution and approver authority still require external review"
                : "invalid boundary"}
            </dd>
          </div>
          <div>
            <dt>Exact receipt</dt>
            <dd>
              {item.receiptSha256} · {item.receiptSizeBytes.toLocaleString()} bytes
            </dd>
          </div>
        </dl>
      </section>
      <section className="detail-card">
        <h2>Uninterrupted phase projections</h2>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Phase</th>
                <th>Duration</th>
                <th>Load</th>
              </tr>
            </thead>
            <tbody>
              {item.phases.map((phase) => (
                <tr key={phase.name}>
                  <td>{phase.name}</td>
                  <td>{(phase.durationSeconds / 3600).toFixed(1)}h</td>
                  <td>{phase.loadMultiplier.toFixed(2)}×</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
      <section className="detail-card">
        <h2>Separated decisions</h2>
        {item.approvals.length === 0 ? (
          <p>No internal decisions recorded.</p>
        ) : (
          <ul className="timeline-list">
            {item.approvals.map((approval) => (
              <li key={approval.id}>
                <strong>
                  {approval.role} · {approval.decision}
                </strong>
                <span>
                  {approval.approverName || approval.approverEmail} ·{" "}
                  {new Date(approval.createdAt).toLocaleString()}
                </span>
                <p>{approval.reason}</p>
                <code>{approval.evidenceSha256 ?? "legacy URL-only evidence"}</code>
                <span>{approval.supersededAt ? "superseded" : "active"}</span>
              </li>
            ))}
          </ul>
        )}
      </section>
      {item.state === "recorded" ? <ApprovalForm item={item} onChanged={props.onChanged} /> : null}
    </>
  );
}

function ApprovalForm(props: {
  readonly item: ControlPlaneStage6CapacityRun;
  readonly onChanged: (item: ControlPlaneStage6CapacityRun) => void;
}) {
  const [role, setRole] = useState<ControlPlaneStage6CapacityApprovalRole>("operations");
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformStage6CapacityApproval(props.item.id, {
        role,
        decision,
        reason,
        evidenceReference,
        evidenceSha256: evidenceSHA256,
      }),
    onSuccess: props.onChanged,
  });
  const recordedRoles = new Set(
    props.item.approvals
      .filter((approval) => approval.supersededAt === null)
      .map((approval) => approval.role),
  );
  return (
    <section className="form-panel">
      <h2>Record separated Capacity decision</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          mutation.mutate();
        }}
      >
        <Field label="Authority role">
          <Select
            value={role}
            onChange={(event) =>
              setRole(event.target.value as ControlPlaneStage6CapacityApprovalRole)
            }
          >
            {approvalRoles.map((value) => (
              <option key={value} value={value} disabled={recordedRoles.has(value)}>
                {value}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Decision">
          <Select
            value={decision}
            onChange={(event) => setDecision(event.target.value as "approved" | "rejected")}
          >
            <option value="approved">Approve internal gate</option>
            <option value="rejected">Reject</option>
          </Select>
        </Field>
        <Field label="Protected evidence URL">
          <Input
            type="url"
            value={evidenceReference}
            onChange={(event) => setEvidenceReference(event.target.value)}
            required
          />
        </Field>
        <Field label="Exact approval evidence SHA-256">
          <Input
            value={evidenceSHA256}
            pattern="sha256:[0-9a-f]{64}"
            placeholder="sha256:…"
            onChange={(event) => setEvidenceSHA256(event.target.value)}
            required
          />
        </Field>
        <div className="form-grid__wide">
          <Field label="Reason">
            <textarea
              value={reason}
              minLength={20}
              maxLength={2000}
              onChange={(event) => setReason(event.target.value)}
              required
            />
          </Field>
        </div>
        <div className="form-actions form-grid__wide">
          <Button
            type="submit"
            variant="primary"
            disabled={reason.trim().length < 20 || mutation.isPending || recordedRoles.has(role)}
          >
            Record immutable decision
          </Button>
        </div>
        <InlineError error={mutation.error} />
      </form>
      <p className="form-hint">
        Requires the matching active `capacity.*` Governance role. Approval preserves the external
        environment, telemetry, signature, execution and approver-authority boundary.
      </p>
    </section>
  );
}

async function readReceipt(file: File): Promise<{ base64: string; sha256: string; name: string }> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  if (bytes.byteLength < 1 || bytes.byteLength > 512 * 1024)
    throw new Error("Capacity receipt must be between 1 byte and 512 KiB.");
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return {
    base64: btoa(binary),
    sha256: `sha256:${Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("")}`,
    name: file.name,
  };
}
