// FILE: PlatformIncidentExerciseGovernance.tsx
// Purpose: Bind exact paging/public-communication exercise receipts and separated decisions to a release.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneStage6IncidentExercise,
  type ControlPlaneStage6IncidentExerciseApprovalRole,
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

const approvalRoles: ReadonlyArray<ControlPlaneStage6IncidentExerciseApprovalRole> = [
  "operations",
  "communications",
];

function tone(state: ControlPlaneStage6IncidentExercise["state"]): "active" | "neutral" | "danger" {
  if (state === "approved") return "active";
  if (state === "rejected") return "danger";
  return "neutral";
}

export function PlatformIncidentExerciseGovernance(props: { readonly canManage: boolean }) {
  const queryClient = useQueryClient();
  const exercises = useQuery({
    queryKey: platformQueryKeys.incidentExercises,
    queryFn: controlPlaneClient.listPlatformStage6IncidentExercises,
  });
  const candidates = useQuery({
    queryKey: platformQueryKeys.releaseCandidates,
    queryFn: controlPlaneClient.listPlatformStage6ReleaseCandidates,
  });
  const [selectedId, setSelectedId] = useState("");
  const effectiveId = selectedId || exercises.data?.items[0]?.id || "";
  const selected = useMemo(
    () => exercises.data?.items.find((item) => item.id === effectiveId) ?? null,
    [effectiveId, exercises.data],
  );
  const replace = (item: ControlPlaneStage6IncidentExercise) => {
    queryClient.setQueryData(
      platformQueryKeys.incidentExercises,
      (current: typeof exercises.data) => ({
        items: current?.items.some((existing) => existing.id === item.id)
          ? current.items.map((existing) => (existing.id === item.id ? item : existing))
          : [item, ...(current?.items ?? [])],
      }),
    );
  };

  if (exercises.isPending || candidates.isPending)
    return <LoadingState label="Loading Incident exercise governance…" />;
  if (exercises.error || candidates.error)
    return <InlineError error={exercises.error ?? candidates.error} />;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Incident exercises</h1>
          <p>
            Exact candidate-bound paging and internal Status Board exercise receipts with separated
            Operations/Communications decisions. Internal approval does not authenticate deployed
            delivery, signatures, execution or human authority.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <ImportExercise
          candidates={candidates.data.items}
          onImported={(item) => {
            replace(item);
            setSelectedId(item.id);
          }}
        />
      ) : null}
      {exercises.data.items.length === 0 ? (
        <EmptyState
          title="No Incident exercise reviews"
          description="Import the exact validator receipt after live paging, employee communication, notification delivery and recovery observation complete."
        />
      ) : (
        <section className="form-panel">
          <Field label="Incident exercise">
            <Select value={effectiveId} onChange={(event) => setSelectedId(event.target.value)}>
              {exercises.data.items.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.candidateId} · {item.state} · {item.exerciseId}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? <ExerciseDetail item={selected} onChanged={replace} /> : null}
    </div>
  );
}

function ImportExercise(props: {
  readonly candidates: ReadonlyArray<{ readonly id: string; readonly candidateId: string }>;
  readonly onImported: (item: ControlPlaneStage6IncidentExercise) => void;
}) {
  const [candidateRecordId, setCandidateRecordId] = useState(props.candidates[0]?.id ?? "");
  const [receipt, setReceipt] = useState<{ base64: string; sha256: string; name: string } | null>(
    null,
  );
  const [readError, setReadError] = useState<unknown>(null);
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.importPlatformStage6IncidentExercise({
        candidateRecordId,
        receiptBase64: receipt?.base64 ?? "",
        receiptSha256: receipt?.sha256 ?? "",
      }),
    onSuccess: props.onImported,
  });
  return (
    <section className="form-panel">
      <h2>Import exact Incident exercise receipt</h2>
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
            Import immutable exercise
          </Button>
        </div>
        <InlineError error={readError ?? mutation.error} />
      </form>
    </section>
  );
}

function ExerciseDetail(props: {
  readonly item: ControlPlaneStage6IncidentExercise;
  readonly onChanged: (item: ControlPlaneStage6IncidentExercise) => void;
}) {
  const item = props.item;
  const machineChecks = [
    ["Failure-independent internal Status Board", item.independentInternalStatusBoardDeclared],
    ["Role separation", item.roleSeparationComplete],
    ["Paging and escalation", item.pagingExerciseComplete],
    ["Internal service components", item.internalStatusBoardComponentsComplete],
    ["Internal update targets", item.internalTimelineWithinTargets],
    ["Employee notification delivery", item.employeeNotificationDeliveryComplete],
    ["Recovery observation", item.recoveryVerificationComplete],
    ["Exercise review", item.reviewComplete],
  ] as const;
  return (
    <>
      <section className="detail-card">
        <div className="detail-card__header">
          <div>
            <span className="eyebrow">{item.candidateId}</span>
            <h2>{item.exerciseId}</h2>
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
            <dt>Severity / mode</dt>
            <dd>
              {item.severity} · {item.exerciseMode}
            </dd>
          </div>
          <div>
            <dt>Exercise window</dt>
            <dd>
              {new Date(item.startedAt).toLocaleString()} –{" "}
              {new Date(item.completedAt).toLocaleString()}
            </dd>
          </div>
          <div>
            <dt>Internal delivery</dt>
            <dd>
              {item.serviceOrigin} → {item.internalStatusBoardOrigin}
            </dd>
          </div>
          <div>
            <dt>Exact receipt</dt>
            <dd>
              {item.receiptSha256} · {item.receiptSizeBytes.toLocaleString()} bytes
            </dd>
          </div>
          <div>
            <dt>Deployment verification</dt>
            <dd>
              {item.deploymentAuthorityVerificationRequired
                ? "Internal Status Board, paging, employee notifications, signatures, execution and authority still require deployment review"
                : "invalid boundary"}
            </dd>
          </div>
        </dl>
      </section>
      <section className="detail-card">
        <h2>Recomputed machine gate</h2>
        <div className="table-wrap">
          <table>
            <tbody>
              {machineChecks.map(([label, passed]) => (
                <tr key={label}>
                  <td>{label}</td>
                  <td>{passed ? "passed" : "failed"}</td>
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
  readonly item: ControlPlaneStage6IncidentExercise;
  readonly onChanged: (item: ControlPlaneStage6IncidentExercise) => void;
}) {
  const [role, setRole] = useState<ControlPlaneStage6IncidentExerciseApprovalRole>("operations");
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformStage6IncidentExerciseApproval(props.item.id, {
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
      <h2>Record separated Incident exercise decision</h2>
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
              setRole(event.target.value as ControlPlaneStage6IncidentExerciseApprovalRole)
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
        Requires the matching active `incident_exercise.*` Governance role. Approval preserves the
        deployed delivery, signature, execution and approver-authority boundary.
      </p>
    </section>
  );
}

async function readReceipt(file: File): Promise<{ base64: string; sha256: string; name: string }> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  if (bytes.byteLength < 1 || bytes.byteLength > 512 * 1024)
    throw new Error("Incident exercise receipt must be between 1 byte and 512 KiB.");
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return {
    base64: btoa(binary),
    sha256: `sha256:${Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("")}`,
    name: file.name,
  };
}
