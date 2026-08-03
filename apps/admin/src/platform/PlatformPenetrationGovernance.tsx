// FILE: PlatformPenetrationGovernance.tsx
// Purpose: Bind exact third-party penetration receipts and separated internal decisions to a release.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneStage6PenetrationApprovalRole,
  type ControlPlaneStage6PenetrationEngagement,
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

const approvalRoles: ReadonlyArray<ControlPlaneStage6PenetrationApprovalRole> = [
  "engineering",
  "product",
  "security",
];

function tone(
  state: ControlPlaneStage6PenetrationEngagement["state"],
): "active" | "neutral" | "danger" {
  if (state === "approved") return "active";
  if (state === "rejected") return "danger";
  return "neutral";
}

export function PlatformPenetrationGovernance(props: { readonly canManage: boolean }) {
  const queryClient = useQueryClient();
  const engagements = useQuery({
    queryKey: platformQueryKeys.penetrationEngagements,
    queryFn: controlPlaneClient.listPlatformStage6PenetrationEngagements,
  });
  const candidates = useQuery({
    queryKey: platformQueryKeys.releaseCandidates,
    queryFn: controlPlaneClient.listPlatformStage6ReleaseCandidates,
  });
  const [selectedId, setSelectedId] = useState("");
  const effectiveId = selectedId || engagements.data?.items[0]?.id || "";
  const selected = useMemo(
    () => engagements.data?.items.find((item) => item.id === effectiveId) ?? null,
    [engagements.data, effectiveId],
  );
  const replace = (item: ControlPlaneStage6PenetrationEngagement) => {
    queryClient.setQueryData(
      platformQueryKeys.penetrationEngagements,
      (current: typeof engagements.data) => ({
        items: current?.items.some((existing) => existing.id === item.id)
          ? current.items.map((existing) => (existing.id === item.id ? item : existing))
          : [item, ...(current?.items ?? [])],
      }),
    );
  };

  if (engagements.isPending || candidates.isPending)
    return <LoadingState label="Loading Penetration governance…" />;
  if (engagements.error || candidates.error)
    return <InlineError error={engagements.error ?? candidates.error} />;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Penetration reviews</h1>
          <p>
            Exact candidate-bound third-party receipts and separated Engineering, Product and
            Security decisions. Internal approval does not authenticate the assessor, report,
            signature or real test execution.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <ImportEngagement
          candidates={candidates.data.items}
          onImported={(item) => {
            replace(item);
            setSelectedId(item.id);
          }}
        />
      ) : null}
      {engagements.data.items.length === 0 ? (
        <EmptyState
          title="No Penetration reviews"
          description="Import the exact validator receipt after the third-party engagement and protected report review complete."
        />
      ) : (
        <section className="form-panel">
          <Field label="Penetration engagement">
            <Select value={effectiveId} onChange={(event) => setSelectedId(event.target.value)}>
              {engagements.data.items.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.candidateId} · {item.state} · {item.engagementId}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? <EngagementDetail item={selected} onChanged={replace} /> : null}
    </div>
  );
}

function ImportEngagement(props: {
  readonly candidates: ReadonlyArray<{ readonly id: string; readonly candidateId: string }>;
  readonly onImported: (item: ControlPlaneStage6PenetrationEngagement) => void;
}) {
  const [candidateRecordId, setCandidateRecordId] = useState(props.candidates[0]?.id ?? "");
  const [receipt, setReceipt] = useState<{ base64: string; sha256: string; name: string } | null>(
    null,
  );
  const [readError, setReadError] = useState<unknown>(null);
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.importPlatformStage6PenetrationEngagement({
        candidateRecordId,
        receiptBase64: receipt?.base64 ?? "",
        receiptSha256: receipt?.sha256 ?? "",
      }),
    onSuccess: props.onImported,
  });
  return (
    <section className="form-panel">
      <h2>Import exact Penetration receipt</h2>
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
            Import immutable engagement
          </Button>
        </div>
        <InlineError error={readError ?? mutation.error} />
      </form>
    </section>
  );
}

function EngagementDetail(props: {
  readonly item: ControlPlaneStage6PenetrationEngagement;
  readonly onChanged: (item: ControlPlaneStage6PenetrationEngagement) => void;
}) {
  const item = props.item;
  return (
    <>
      <section className="detail-card">
        <div className="detail-card__header">
          <div>
            <span className="eyebrow">{item.candidateId}</span>
            <h2>{item.engagementId}</h2>
          </div>
          <StatusPill value={item.state} tone={tone(item.state)} />
        </div>
        <dl className="detail-grid">
          <div>
            <dt>Environment</dt>
            <dd>
              {item.environmentClass} · {item.environmentId} · {item.deploymentProfile}
            </dd>
          </div>
          <div>
            <dt>Engagement window</dt>
            <dd>
              {new Date(item.startedAt).toLocaleString()} –{" "}
              {new Date(item.completedAt).toLocaleString()}
            </dd>
          </div>
          <div>
            <dt>Machine gate</dt>
            <dd>
              {item.eligibleForHumanGateReview &&
              item.noUnacceptedHighOrCriticalFindings &&
              item.scopeCoverageComplete
                ? "eligible evidence"
                : "failed / incomplete"}
            </dd>
          </div>
          <div>
            <dt>External verification</dt>
            <dd>
              {item.externalAuthorityVerificationRequired
                ? "assessor identity, report, signatures and execution still require external review"
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
        <h2>Candidate asset projections</h2>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Asset</th>
                <th>Candidate digest</th>
                <th>Tested</th>
              </tr>
            </thead>
            <tbody>
              {item.assets.map((asset) => (
                <tr key={asset.assetType}>
                  <td>{asset.assetType}</td>
                  <td>{asset.artifactSha256}</td>
                  <td>{asset.tested ? "yes" : "no"}</td>
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
                <span>
                  {approval.evidenceSha256 ?? "legacy-unbound"} ·{" "}
                  {approval.supersededAt === null ? "active" : "superseded"}
                </span>
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
  readonly item: ControlPlaneStage6PenetrationEngagement;
  readonly onChanged: (item: ControlPlaneStage6PenetrationEngagement) => void;
}) {
  const [role, setRole] = useState<ControlPlaneStage6PenetrationApprovalRole>("security");
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformStage6PenetrationApproval(props.item.id, {
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
      <h2>Record separated Penetration decision</h2>
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
              setRole(event.target.value as ControlPlaneStage6PenetrationApprovalRole)
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
        Requires the matching active `penetration.*` Governance role. Approval preserves the
        external assessor, report, signature and execution verification boundary.
      </p>
    </section>
  );
}

async function readReceipt(file: File): Promise<{ base64: string; sha256: string; name: string }> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  if (bytes.byteLength < 1 || bytes.byteLength > 512 * 1024)
    throw new Error("Penetration receipt must be between 1 byte and 512 KiB.");
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return {
    base64: btoa(binary),
    sha256: `sha256:${Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("")}`,
    name: file.name,
  };
}
