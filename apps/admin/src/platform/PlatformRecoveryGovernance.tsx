// FILE: PlatformRecoveryGovernance.tsx
// Purpose: Import exact Recovery v2 receipts and record separated internal decisions.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneStage6RecoveryApprovalRole,
  type ControlPlaneStage6RecoveryDrill,
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

const approvalRoles: ReadonlyArray<ControlPlaneStage6RecoveryApprovalRole> = [
  "database",
  "kms",
  "operations",
  "security",
  "storage",
];

function tone(state: ControlPlaneStage6RecoveryDrill["state"]): "active" | "neutral" | "danger" {
  if (state === "approved") return "active";
  if (state === "rejected") return "danger";
  return "neutral";
}

export function PlatformRecoveryGovernance(props: { readonly canManage: boolean }) {
  const queryClient = useQueryClient();
  const drills = useQuery({
    queryKey: platformQueryKeys.recoveryDrills,
    queryFn: controlPlaneClient.listPlatformStage6RecoveryDrills,
  });
  const candidates = useQuery({
    queryKey: platformQueryKeys.releaseCandidates,
    queryFn: controlPlaneClient.listPlatformStage6ReleaseCandidates,
  });
  const [selectedId, setSelectedId] = useState("");
  const effectiveId = selectedId || drills.data?.items[0]?.id || "";
  const selected = useMemo(
    () => drills.data?.items.find((item) => item.id === effectiveId) ?? null,
    [drills.data, effectiveId],
  );
  const replace = (item: ControlPlaneStage6RecoveryDrill) => {
    queryClient.setQueryData(platformQueryKeys.recoveryDrills, (current: typeof drills.data) => ({
      items: current?.items.some((existing) => existing.id === item.id)
        ? current.items.map((existing) => (existing.id === item.id ? item : existing))
        : [item, ...(current?.items ?? [])],
    }));
  };

  if (drills.isPending || candidates.isPending)
    return <LoadingState label="Loading Recovery governance…" />;
  if (drills.error || candidates.error)
    return <InlineError error={drills.error ?? candidates.error} />;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Recovery drills</h1>
          <p>
            Exact candidate-bound Recovery v2 receipts and separated Database, KMS, Operations,
            Security and Storage decisions. Internal approval does not authenticate an external
            backup authority or cryptographic signature.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <ImportDrill
          candidates={candidates.data.items}
          onImported={(item) => {
            replace(item);
            setSelectedId(item.id);
          }}
        />
      ) : null}
      {drills.data.items.length === 0 ? (
        <EmptyState
          title="No Recovery drills"
          description="Import the exact validator receipt after the isolated restore drill and source evidence reviews complete."
        />
      ) : (
        <section className="form-panel">
          <Field label="Recovery drill">
            <Select value={effectiveId} onChange={(event) => setSelectedId(event.target.value)}>
              {drills.data.items.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.candidateId} · {item.state} · {item.drillId}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? <DrillDetail item={selected} onChanged={replace} /> : null}
    </div>
  );
}

function ImportDrill(props: {
  readonly candidates: ReadonlyArray<{ readonly id: string; readonly candidateId: string }>;
  readonly onImported: (item: ControlPlaneStage6RecoveryDrill) => void;
}) {
  const [candidateRecordId, setCandidateRecordId] = useState(props.candidates[0]?.id ?? "");
  const [receipt, setReceipt] = useState<{ base64: string; sha256: string; name: string } | null>(
    null,
  );
  const [readError, setReadError] = useState<unknown>(null);
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.importPlatformStage6RecoveryDrill({
        candidateRecordId,
        receiptBase64: receipt?.base64 ?? "",
        receiptSha256: receipt?.sha256 ?? "",
      }),
    onSuccess: props.onImported,
  });
  return (
    <section className="form-panel">
      <h2>Import exact Recovery v2 receipt</h2>
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
              void readRecoveryReceipt(file).then(setReceipt).catch(setReadError);
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
            Import immutable drill
          </Button>
        </div>
        <InlineError error={readError ?? mutation.error} />
      </form>
    </section>
  );
}

function DrillDetail(props: {
  readonly item: ControlPlaneStage6RecoveryDrill;
  readonly onChanged: (item: ControlPlaneStage6RecoveryDrill) => void;
}) {
  const item = props.item;
  return (
    <>
      <section className="detail-card">
        <div className="detail-card__header">
          <div>
            <span className="eyebrow">{item.candidateId}</span>
            <h2>{item.drillId}</h2>
          </div>
          <StatusPill value={item.state} tone={tone(item.state)} />
        </div>
        <dl className="detail-grid">
          <div>
            <dt>Drill window</dt>
            <dd>
              {new Date(item.startedAt).toLocaleString()} –{" "}
              {new Date(item.completedAt).toLocaleString()}
            </dd>
          </div>
          <div>
            <dt>Review eligibility</dt>
            <dd>{item.eligibleForHumanGateReview ? "eligible evidence" : "failed / incomplete"}</dd>
          </div>
          <div>
            <dt>Exact receipt</dt>
            <dd>
              {item.receiptSha256} · {item.receiptSizeBytes.toLocaleString()} bytes
            </dd>
          </div>
          <div>
            <dt>External verification</dt>
            <dd>
              {item.externalAuthorityVerificationRequired
                ? "backup, approver and signature authority still required"
                : "invalid boundary"}
            </dd>
          </div>
        </dl>
      </section>
      <section className="detail-card">
        <h2>Recovery component projections</h2>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Component</th>
                <th>Profile</th>
                <th>Regions</th>
                <th>RPO</th>
                <th>RTO</th>
                <th>Canary</th>
              </tr>
            </thead>
            <tbody>
              {item.components.map((component) => (
                <tr key={component.key}>
                  <td>{component.key}</td>
                  <td>{component.profile}</td>
                  <td>
                    {component.sourceRegion} → {component.restoreRegion}
                  </td>
                  <td>
                    {component.measuredRpoSeconds}s / {component.rpoObjectiveSeconds}s
                  </td>
                  <td>
                    {component.measuredRtoSeconds}s / {component.rtoObjectiveSeconds}s
                  </td>
                  <td>{component.restoreServedCanary ? "served" : "failed"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
      <section className="detail-card">
        <h2>Separated internal decisions</h2>
        {item.approvals.length === 0 ? (
          <p>No decisions recorded.</p>
        ) : (
          <ul className="timeline-list">
            {item.approvals.map((approval) => (
              <li key={approval.id}>
                <strong>
                  {approval.role} · {approval.decision}
                </strong>
                <span>
                  {approval.approverName} · {approval.reason} ·{" "}
                  {approval.evidenceSha256 ?? "legacy-unbound"} ·{" "}
                  {approval.supersededAt === null ? "active" : "superseded"}
                </span>
                <a href={approval.evidenceReference} target="_blank" rel="noreferrer">
                  Review evidence
                </a>
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
  readonly item: ControlPlaneStage6RecoveryDrill;
  readonly onChanged: (item: ControlPlaneStage6RecoveryDrill) => void;
}) {
  const remainingRoles = approvalRoles.filter(
    (role) =>
      !props.item.approvals.some(
        (approval) => approval.role === role && approval.supersededAt === null,
      ),
  );
  const [role, setRole] = useState<ControlPlaneStage6RecoveryApprovalRole>(
    remainingRoles[0] ?? "database",
  );
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformStage6RecoveryApproval(props.item.id, {
        role,
        decision,
        reason,
        evidenceReference,
        evidenceSha256: evidenceSHA256,
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Record separated Recovery decision</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          mutation.mutate();
        }}
      >
        <Field label="Functional role">
          <Select
            value={role}
            onChange={(event) =>
              setRole(event.target.value as ControlPlaneStage6RecoveryApprovalRole)
            }
          >
            {remainingRoles.map((value) => (
              <option key={value} value={value}>
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
            <option value="approved">Approved</option>
            <option value="rejected">Rejected</option>
          </Select>
        </Field>
        <Field label="Reason">
          <Input
            value={reason}
            minLength={20}
            maxLength={2000}
            required
            onChange={(event) => setReason(event.target.value)}
          />
        </Field>
        <Field label="HTTPS evidence">
          <Input
            type="url"
            value={evidenceReference}
            required
            onChange={(event) => setEvidenceReference(event.target.value)}
          />
        </Field>
        <Field label="Exact approval evidence SHA-256">
          <Input
            value={evidenceSHA256}
            pattern="sha256:[0-9a-f]{64}"
            placeholder="sha256:…"
            required
            onChange={(event) => setEvidenceSHA256(event.target.value)}
          />
        </Field>
        <div className="form-actions form-grid__wide">
          <Button
            type="submit"
            variant="primary"
            disabled={remainingRoles.length === 0 || mutation.isPending}
          >
            Record immutable decision
          </Button>
        </div>
        <InlineError error={mutation.error} />
      </form>
    </section>
  );
}

async function readRecoveryReceipt(
  file: File,
): Promise<{ base64: string; sha256: string; name: string }> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  if (bytes.byteLength < 1 || bytes.byteLength > 512 * 1024)
    throw new Error("Recovery receipt must be between 1 byte and 512 KiB.");
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let binary = "";
  for (const value of bytes) binary += String.fromCharCode(value);
  return {
    name: file.name,
    base64: btoa(binary),
    sha256: `sha256:${Array.from(digest, (value) => value.toString(16).padStart(2, "0")).join("")}`,
  };
}
