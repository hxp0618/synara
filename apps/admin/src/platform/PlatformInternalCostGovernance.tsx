// FILE: PlatformInternalCostGovernance.tsx
// Purpose: Review exact internal usage, Token, Provider cost and platform allocation receipts.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneStage6InternalCostApprovalRole,
  type ControlPlaneStage6InternalCostReview,
} from "@synara/control-plane-client";
import { useState } from "react";

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

const approvalRoles: ReadonlyArray<ControlPlaneStage6InternalCostApprovalRole> = [
  "operations",
  "owner",
];

export function PlatformInternalCostGovernance(props: { readonly canManage: boolean }) {
  const queryClient = useQueryClient();
  const reviews = useQuery({
    queryKey: platformQueryKeys.internalCostReviews,
    queryFn: controlPlaneClient.listPlatformStage6InternalCostReviews,
  });
  const candidates = useQuery({
    queryKey: platformQueryKeys.releaseCandidates,
    queryFn: controlPlaneClient.listPlatformStage6ReleaseCandidates,
  });
  const [selectedId, setSelectedId] = useState("");
  if (reviews.isPending || candidates.isPending)
    return <LoadingState label="Loading internal cost reviews…" />;
  if (reviews.error || candidates.error)
    return <InlineError error={reviews.error ?? candidates.error} />;
  const items = reviews.data?.items ?? [];
  const selected = items.find((item) => item.id === selectedId) ?? items[0] ?? null;
  const replace = (item: ControlPlaneStage6InternalCostReview) => {
    queryClient.setQueryData(platformQueryKeys.internalCostReviews, {
      items: items.map((current) => (current.id === item.id ? item : current)),
    });
    setSelectedId(item.id);
  };
  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <p className="page-heading__eyebrow">Release evidence</p>
          <h1>Internal usage &amp; cost reviews</h1>
          <p>
            Candidate-bound Token totals, Provider cost coverage and actual-over-estimated platform
            allocation. This workflow is limited to internal cost evidence and separated approval.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <ImportReview
          candidates={(candidates.data?.items ?? []).map(({ id, candidateId }) => ({
            id,
            candidateId,
          }))}
          onImported={(item) => {
            queryClient.setQueryData(platformQueryKeys.internalCostReviews, {
              items: [item, ...items],
            });
            setSelectedId(item.id);
          }}
        />
      ) : null}
      {items.length === 0 ? (
        <EmptyState
          title="No internal cost reviews"
          description="Import the exact v1 internal cost receipt for a v4 candidate."
        />
      ) : (
        <section className="form-panel">
          <Field label="Internal cost review">
            <Select
              value={selected?.id ?? ""}
              onChange={(event) => setSelectedId(event.target.value)}
            >
              {items.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.candidateId} · {item.state}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? <ReviewDetail item={selected} onChanged={replace} /> : null}
    </div>
  );
}

function ImportReview(props: {
  readonly candidates: ReadonlyArray<{ readonly id: string; readonly candidateId: string }>;
  readonly onImported: (item: ControlPlaneStage6InternalCostReview) => void;
}) {
  const [candidateRecordId, setCandidateRecordId] = useState(props.candidates[0]?.id ?? "");
  const [receipt, setReceipt] = useState<{ base64: string; sha256: string; name: string } | null>(
    null,
  );
  const [readError, setReadError] = useState<unknown>(null);
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.importPlatformStage6InternalCostReview({
        candidateRecordId,
        receiptBase64: receipt?.base64 ?? "",
        receiptSha256: receipt?.sha256 ?? "",
      }),
    onSuccess: props.onImported,
  });
  return (
    <section className="form-panel">
      <h2>Import exact internal usage and cost receipt</h2>
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
              if (file) void readReceipt(file).then(setReceipt).catch(setReadError);
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
            Import immutable review
          </Button>
        </div>
        <InlineError error={readError ?? mutation.error} />
      </form>
    </section>
  );
}

function ReviewDetail(props: {
  readonly item: ControlPlaneStage6InternalCostReview;
  readonly onChanged: (item: ControlPlaneStage6InternalCostReview) => void;
}) {
  const { item } = props;
  return (
    <>
      <section className="detail-card">
        <header>
          <div>
            <h2>{item.candidateId}</h2>
            <p>
              {item.periodStart} → {item.periodEnd}
            </p>
          </div>
          <StatusPill value={item.state} />
        </header>
        <dl className="detail-grid">
          <div>
            <dt>Executions</dt>
            <dd>{item.executionCount}</dd>
          </div>
          <div>
            <dt>Input / output Tokens</dt>
            <dd>
              {item.inputTokens} / {item.outputTokens}
            </dd>
          </div>
          <div>
            <dt>Provider coverage</dt>
            <dd>
              {item.providerCostReportedExecutionCount} reported ·{" "}
              {item.providerCostUnavailableExecutionCount} unavailable
            </dd>
          </div>
          <div>
            <dt>Platform allocation</dt>
            <dd>
              {item.actualPlatformAllocationCount} actual · {item.estimatedPlatformAllocationCount}{" "}
              estimated
            </dd>
          </div>
          <div>
            <dt>Known internal cost</dt>
            <dd>{formatCosts(item.knownCostByCurrency)}</dd>
          </div>
          <div>
            <dt>Receipt</dt>
            <dd>{item.receiptSha256}</dd>
          </div>
        </dl>
        <p className="form-hint">
          Actual allocation replaces estimate for the same execution; unavailable Provider cost
          remains explicit and never becomes zero.
        </p>
      </section>
      {item.state === "recorded" ? <ApprovalForm item={item} onChanged={props.onChanged} /> : null}
    </>
  );
}

function ApprovalForm(props: {
  readonly item: ControlPlaneStage6InternalCostReview;
  readonly onChanged: (item: ControlPlaneStage6InternalCostReview) => void;
}) {
  const [role, setRole] = useState<ControlPlaneStage6InternalCostApprovalRole>("operations");
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSha256, setEvidenceSha256] = useState("");
  const recordedRoles = new Set(
    props.item.approvals.filter((item) => item.supersededAt === null).map((item) => item.role),
  );
  const mutation = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformStage6InternalCostApproval(props.item.id, {
        role,
        decision,
        reason,
        evidenceReference,
        evidenceSha256,
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Record separated internal cost decision</h2>
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
              setRole(event.target.value as ControlPlaneStage6InternalCostApprovalRole)
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
        <Field label="Exact evidence SHA-256">
          <Input
            value={evidenceSha256}
            pattern="sha256:[0-9a-f]{64}"
            placeholder="sha256:…"
            onChange={(event) => setEvidenceSha256(event.target.value)}
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
      <p className="form-hint">Requires the matching active `internal_cost.*` Governance role.</p>
    </section>
  );
}

function formatCosts(values: Readonly<Record<string, number>>): string {
  return Object.entries(values)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([currency, amount]) => `${currency} ${amount} minor`)
    .join(" · ");
}

async function readReceipt(file: File): Promise<{ base64: string; sha256: string; name: string }> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  if (bytes.byteLength < 1 || bytes.byteLength > 2 * 1024 * 1024)
    throw new Error("Internal cost receipt must be between 1 byte and 2 MiB.");
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return {
    base64: btoa(binary),
    sha256: `sha256:${Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("")}`,
    name: file.name,
  };
}
