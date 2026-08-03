// FILE: PlatformSLOGovernance.tsx
// Purpose: Import exact validated SLO windows and record separated functional decisions.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneStage6SLOApprovalRole,
  type ControlPlaneStage6SLOWindow,
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

const approvalRoles: ReadonlyArray<ControlPlaneStage6SLOApprovalRole> = [
  "engineering",
  "operations",
  "security",
  "product",
];

function percent(value: number): string {
  return new Intl.NumberFormat(undefined, { style: "percent", maximumFractionDigits: 3 }).format(
    value,
  );
}

function tone(state: ControlPlaneStage6SLOWindow["state"]): "active" | "neutral" | "danger" {
  if (state === "approved") return "active";
  if (state === "rejected") return "danger";
  return "neutral";
}

export function PlatformSLOGovernance(props: { readonly canManage: boolean }) {
  const queryClient = useQueryClient();
  const windows = useQuery({
    queryKey: platformQueryKeys.sloWindows,
    queryFn: controlPlaneClient.listPlatformStage6SLOWindows,
  });
  const candidates = useQuery({
    queryKey: platformQueryKeys.releaseCandidates,
    queryFn: controlPlaneClient.listPlatformStage6ReleaseCandidates,
  });
  const [selectedId, setSelectedId] = useState("");
  const effectiveId = selectedId || windows.data?.items[0]?.id || "";
  const selected = useMemo(
    () => windows.data?.items.find((item) => item.id === effectiveId) ?? null,
    [effectiveId, windows.data],
  );
  const replace = (item: ControlPlaneStage6SLOWindow) => {
    queryClient.setQueryData(platformQueryKeys.sloWindows, (current: typeof windows.data) => ({
      items: current?.items.some((existing) => existing.id === item.id)
        ? current.items.map((existing) => (existing.id === item.id ? item : existing))
        : [item, ...(current?.items ?? [])],
    }));
  };

  if (windows.isPending || candidates.isPending)
    return <LoadingState label="Loading SLO governance…" />;
  if (windows.error || candidates.error)
    return <InlineError error={windows.error ?? candidates.error} />;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>SLO windows</h1>
          <p>
            Exact 30-day evidence, error-budget policy and separated Engineering, Operations,
            Security and Product decisions. Approval is an internal review record, not a production
            SLO claim.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <ImportWindow
          candidates={candidates.data.items}
          onImported={(item) => {
            replace(item);
            setSelectedId(item.id);
          }}
        />
      ) : null}
      {windows.data.items.length === 0 ? (
        <EmptyState
          title="No SLO windows"
          description="Import an exact validator receipt after its uninterrupted window completes."
        />
      ) : (
        <section className="form-panel">
          <Field label="SLO window">
            <Select value={effectiveId} onChange={(event) => setSelectedId(event.target.value)}>
              {windows.data.items.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.candidateId} · {item.state} · {item.environmentId}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? <WindowDetail item={selected} onChanged={replace} /> : null}
    </div>
  );
}

function ImportWindow(props: {
  readonly candidates: ReadonlyArray<{ readonly id: string; readonly candidateId: string }>;
  readonly onImported: (item: ControlPlaneStage6SLOWindow) => void;
}) {
  const [candidateRecordId, setCandidateRecordId] = useState(props.candidates[0]?.id ?? "");
  const [receipt, setReceipt] = useState<{ base64: string; sha256: string; name: string } | null>(
    null,
  );
  const [readError, setReadError] = useState<unknown>(null);
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.importPlatformStage6SLOWindow({
        candidateRecordId,
        receiptBase64: receipt?.base64 ?? "",
        receiptSha256: receipt?.sha256 ?? "",
      }),
    onSuccess: props.onImported,
  });
  return (
    <section className="form-panel">
      <h2>Import exact SLO validator receipt</h2>
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
              void readSLOReceipt(file).then(setReceipt).catch(setReadError);
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
            Import immutable window
          </Button>
        </div>
        <InlineError error={readError ?? mutation.error} />
      </form>
    </section>
  );
}

function WindowDetail(props: {
  readonly item: ControlPlaneStage6SLOWindow;
  readonly onChanged: (item: ControlPlaneStage6SLOWindow) => void;
}) {
  const item = props.item;
  return (
    <>
      <section className="detail-card">
        <div className="detail-card__header">
          <div>
            <span className="eyebrow">{item.candidateId}</span>
            <h2>{item.windowId}</h2>
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
            <dt>Public origin</dt>
            <dd>
              <a href={item.publicOrigin} target="_blank" rel="noreferrer">
                {item.publicOrigin}
              </a>
            </dd>
          </div>
          <div>
            <dt>Window</dt>
            <dd>
              {new Date(item.windowStartedAt).toLocaleString()} –{" "}
              {new Date(item.windowCompletedAt).toLocaleString()}
            </dd>
          </div>
          <div>
            <dt>Review eligibility</dt>
            <dd>
              {item.eligibleForHumanGateReview ? "eligible evidence" : "failed / not assessable"}
            </dd>
          </div>
          <div>
            <dt>Worst remaining budget</dt>
            <dd>
              {percent(item.worstBudgetRemainingRatio)} · {item.budgetPolicyState}
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
        <h2>Objective projections</h2>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Objective</th>
                <th>Good</th>
                <th>Target</th>
                <th>Samples</th>
                <th>Budget</th>
                <th>Result</th>
              </tr>
            </thead>
            <tbody>
              {item.objectives.map((objective) => (
                <tr key={objective.key}>
                  <td>{objective.key}</td>
                  <td>{percent(objective.goodRatio)}</td>
                  <td>{percent(objective.targetRatio)}</td>
                  <td>{objective.sampleCount.toLocaleString()}</td>
                  <td>
                    {percent(objective.errorBudgetRemainingRatio)} · {objective.policyState}
                  </td>
                  <td>
                    {objective.assessable
                      ? objective.objectiveMet
                        ? "met"
                        : "missed"
                      : "not assessable"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
      <section className="detail-card">
        <h2>Separated review decisions</h2>
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
  readonly item: ControlPlaneStage6SLOWindow;
  readonly onChanged: (item: ControlPlaneStage6SLOWindow) => void;
}) {
  const remainingRoles = approvalRoles.filter(
    (role) =>
      !props.item.approvals.some(
        (approval) => approval.role === role && approval.supersededAt === null,
      ),
  );
  const [role, setRole] = useState<ControlPlaneStage6SLOApprovalRole>(
    remainingRoles[0] ?? "engineering",
  );
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformStage6SLOApproval(props.item.id, {
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
      <h2>Record separated SLO decision</h2>
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
            onChange={(event) => setRole(event.target.value as ControlPlaneStage6SLOApprovalRole)}
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

async function readSLOReceipt(
  file: File,
): Promise<{ base64: string; sha256: string; name: string }> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  if (bytes.byteLength < 1 || bytes.byteLength > 256 * 1024)
    throw new Error("SLO receipt must be between 1 byte and 256 KiB.");
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let binary = "";
  for (const value of bytes) binary += String.fromCharCode(value);
  return {
    name: file.name,
    base64: btoa(binary),
    sha256: `sha256:${Array.from(digest, (value) => value.toString(16).padStart(2, "0")).join("")}`,
  };
}
