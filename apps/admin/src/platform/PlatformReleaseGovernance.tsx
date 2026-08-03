// FILE: PlatformReleaseGovernance.tsx
// Purpose: Record exact Stage 6 candidates, separated approvals, observation, decisions and residual risks in Audit.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneStage6ReleaseApprovalRole,
  type ControlPlaneStage6ReleaseCandidate,
  type ControlPlaneStage6ReleaseImpactDomain,
  type ControlPlaneStage6ReleaseReadiness,
  type ControlPlaneStage6ReleaseState,
} from "@synara/control-plane-client";
import { useEffect, useMemo, useState } from "react";

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
import { readCandidateEvidenceReceipt, readFinalReviewReceipt } from "./releaseEvidenceReceipt";

const impactDomainOptions: ReadonlyArray<{
  readonly value: ControlPlaneStage6ReleaseImpactDomain;
  readonly label: string;
}> = [
  { value: "code_change", label: "Code change" },
  { value: "data_migration", label: "Data migration" },
  { value: "runtime_isolation", label: "Runtime isolation" },
  { value: "provider_commercial", label: "Provider commercial terms" },
  { value: "internal_cost", label: "Internal usage and cost" },
  { value: "personal_data", label: "Personal data" },
  { value: "retention_legal_hold", label: "Retention or legal hold" },
  { value: "data_residency", label: "Data residency" },
  { value: "regulated_customer", label: "Regulated data scope" },
  { value: "desktop_distribution", label: "Desktop distribution" },
  { value: "security_incident", label: "Security incident" },
];

const nextStates: Partial<
  Record<
    ControlPlaneStage6ReleaseState,
    ReadonlyArray<Exclude<ControlPlaneStage6ReleaseState, "draft" | "rejected">>
  >
> = {
  draft: ["ready_for_review"],
  ready_for_review: ["approved"],
  approved: ["deploying"],
  deploying: ["observing", "rolled_back"],
  observing: ["released", "rolled_back"],
};

function abbreviated(value: string): string {
  return value.length > 30 ? `${value.slice(0, 18)}…${value.slice(-8)}` : value;
}

function tone(state: ControlPlaneStage6ReleaseState): "active" | "warning" | "neutral" | "danger" {
  if (state === "released" || state === "approved") return "active";
  if (state === "rejected" || state === "rolled_back") return "danger";
  if (state === "deploying" || state === "observing") return "warning";
  return "neutral";
}

export function PlatformReleaseGovernance(props: { readonly canManage: boolean }) {
  const queryClient = useQueryClient();
  const candidates = useQuery({
    queryKey: platformQueryKeys.releaseCandidates,
    queryFn: controlPlaneClient.listPlatformStage6ReleaseCandidates,
  });
  const [selectedId, setSelectedId] = useState("");
  useEffect(() => {
    if (!selectedId && candidates.data?.items[0]) setSelectedId(candidates.data.items[0].id);
  }, [candidates.data, selectedId]);
  const effectiveSelectedId = selectedId || candidates.data?.items[0]?.id || "";
  const selected = useMemo(
    () => candidates.data?.items.find((candidate) => candidate.id === effectiveSelectedId) ?? null,
    [candidates.data, effectiveSelectedId],
  );
  const replaceCandidate = (candidate: ControlPlaneStage6ReleaseCandidate) => {
    queryClient.setQueryData(
      platformQueryKeys.releaseCandidates,
      (current: typeof candidates.data) => ({
        items: current?.items.map((item) => (item.id === candidate.id ? candidate : item)) ?? [
          candidate,
        ],
      }),
    );
    void queryClient.invalidateQueries({
      queryKey: platformQueryKeys.releaseReadiness(candidate.id),
    });
  };

  if (candidates.isPending) return <LoadingState label="Loading release governance…" />;
  if (candidates.error) return <InlineError error={candidates.error} />;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Release governance</h1>
          <p>
            Exact candidate identity, separated decisions and final residual-risk disposition. This
            record does not manufacture missing external evidence.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <CreateCandidate
          onCreated={(candidate) => {
            replaceCandidate(candidate);
            setSelectedId(candidate.id);
          }}
        />
      ) : null}
      {candidates.data.items.length === 0 ? (
        <EmptyState
          title="No release candidates"
          description="Create one only after the immutable evidence bundle and final asset set exist."
        />
      ) : (
        <section className="form-panel">
          <Field label="Candidate">
            <Select
              onChange={(event) => setSelectedId(event.target.value)}
              value={effectiveSelectedId}
            >
              {candidates.data.items.map((candidate) => (
                <option key={candidate.id} value={candidate.id}>
                  {candidate.candidateId} · {candidate.state.replaceAll("_", " ")}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? (
        <>
          <CandidateSummary candidate={selected} />
          <CandidateReadiness candidate={selected} />
          {selected.state === "ready_for_review" ? (
            <ApprovalForm candidate={selected} onChanged={replaceCandidate} />
          ) : null}
          {props.canManage && selected.state === "observing" && selected.finalReview === null ? (
            <FinalReviewForm candidate={selected} onChanged={replaceCandidate} />
          ) : null}
          {props.canManage && (nextStates[selected.state]?.length ?? 0) > 0 ? (
            <TransitionForm candidate={selected} onChanged={replaceCandidate} />
          ) : null}
        </>
      ) : null}
    </div>
  );
}

function CandidateReadiness(props: { readonly candidate: ControlPlaneStage6ReleaseCandidate }) {
  const readiness = useQuery({
    queryKey: platformQueryKeys.releaseReadiness(props.candidate.id),
    queryFn: () => controlPlaneClient.getPlatformStage6ReleaseReadiness(props.candidate.id),
  });
  if (readiness.isPending) return <LoadingState label="Loading exact-candidate readiness…" />;
  if (readiness.error) return <InlineError error={readiness.error} />;
  return <ReadinessSummary readiness={readiness.data} onRefresh={() => void readiness.refetch()} />;
}

function ReadinessSummary(props: {
  readonly readiness: ControlPlaneStage6ReleaseReadiness;
  readonly onRefresh: () => void;
}) {
  return (
    <section className="form-panel">
      <div className="form-actions">
        <div>
          <h2>Exact-candidate readiness</h2>
          <p>One shared server projection drives this view and the transition into approved.</p>
        </div>
        <StatusPill
          value={props.readiness.internalGatesSatisfied ? "internal gates satisfied" : "incomplete"}
          tone={props.readiness.internalGatesSatisfied ? "active" : "warning"}
        />
        <Button type="button" variant="outline" onClick={props.onRefresh}>
          Refresh gates
        </Button>
      </div>
      <div className="page-stack">
        {props.readiness.gates.map((gate) => (
          <div className="boundary-callout" key={gate.id}>
            <strong>
              {gate.label} · {gate.satisfied ? "satisfied internally" : "missing"}
            </strong>
            <span>{gate.internalAssessment}</span>
            <span>External boundary: {gate.externalBoundary}</span>
          </div>
        ))}
        <div className="boundary-callout">
          <strong>
            {props.readiness.finalReviewGate.label} ·{" "}
            {props.readiness.finalReviewGate.satisfied ? "satisfied internally" : "missing"}
          </strong>
          <span>{props.readiness.finalReviewGate.internalAssessment}</span>
          <span>External boundary: {props.readiness.finalReviewGate.externalBoundary}</span>
          <span>
            Released transition:{" "}
            {props.readiness.releaseTransitionEligible ? "eligible" : "not eligible"}
          </span>
        </div>
      </div>
      <div className="boundary-callout">
        <strong>External GA status · not verified by Synara</strong>
        <span>
          Internal readiness only enables the product state transition. It never proves production
          execution, evidence authority, signatures, organizational authority or final GA approval.
        </span>
      </div>
    </section>
  );
}

function CreateCandidate(props: {
  readonly onCreated: (candidate: ControlPlaneStage6ReleaseCandidate) => void;
}) {
  const providerAuthorizations = useQuery({
    queryKey: platformQueryKeys.providerCommercialAuthorizations,
    queryFn: controlPlaneClient.listPlatformProviderCommercialAuthorizations,
  });
  const [candidateId, setCandidateId] = useState("");
  const [sourceCommit, setSourceCommit] = useState("");
  const [lockfileSHA256, setLockfileSHA256] = useState("");
  const [evidenceReceipt, setEvidenceReceipt] = useState<{
    readonly name: string;
    readonly sha256: string;
    readonly base64: string;
  } | null>(null);
  const [evidenceReceiptError, setEvidenceReceiptError] = useState<string | null>(null);
  const [finalAssetSetSHA256, setFinalAssetSetSHA256] = useState("");
  const [environmentId, setEnvironmentId] = useState("");
  const [impactDomains, setImpactDomains] = useState<
    ReadonlyArray<ControlPlaneStage6ReleaseImpactDomain>
  >(["code_change"]);
  const [providerCommercialAuthorizationIds, setProviderCommercialAuthorizationIds] = useState<
    ReadonlyArray<string>
  >([]);
  const create = useMutation({
    mutationFn: () => {
      if (evidenceReceipt === null)
        throw new Error("Select an eligible candidate evidence receipt.");
      return controlPlaneClient.createPlatformStage6ReleaseCandidate({
        candidateId,
        sourceCommit,
        lockfileSha256: lockfileSHA256,
        evidenceBundleSha256: evidenceReceipt.sha256,
        evidenceBundleReceiptBase64: evidenceReceipt.base64,
        finalAssetSetSha256: finalAssetSetSHA256,
        environmentId,
        impactDomains,
        providerCommercialAuthorizationIds,
      });
    },
    onSuccess: props.onCreated,
  });
  return (
    <section className="form-panel">
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Candidate ID">
          <Input
            required
            value={candidateId}
            onChange={(event) => setCandidateId(event.target.value)}
            placeholder="v0.6.3-stage6.1"
          />
        </Field>
        <Field label="Environment ID">
          <Input
            required
            value={environmentId}
            onChange={(event) => setEnvironmentId(event.target.value)}
            placeholder="stage6/production"
          />
        </Field>
        <Field label="Source commit">
          <Input
            minLength={40}
            maxLength={40}
            required
            value={sourceCommit}
            onChange={(event) => setSourceCommit(event.target.value)}
          />
        </Field>
        <Field label="bun.lock SHA-256">
          <Input
            required
            value={lockfileSHA256}
            onChange={(event) => setLockfileSHA256(event.target.value)}
          />
        </Field>
        <Field label="Candidate evidence receipt">
          <input
            accept="application/json,.json"
            required
            type="file"
            onChange={(event) => {
              const file = event.target.files?.[0];
              setEvidenceReceipt(null);
              setEvidenceReceiptError(null);
              if (file === undefined) return;
              void readCandidateEvidenceReceipt(file)
                .then(setEvidenceReceipt)
                .catch((error: unknown) =>
                  setEvidenceReceiptError(
                    error instanceof Error ? error.message : "Evidence receipt could not be read.",
                  ),
                );
            }}
          />
          {evidenceReceipt === null ? null : (
            <small title={evidenceReceipt.sha256}>
              {evidenceReceipt.name} · {abbreviated(evidenceReceipt.sha256)}
            </small>
          )}
          {evidenceReceiptError === null ? null : (
            <small role="alert">{evidenceReceiptError}</small>
          )}
        </Field>
        <Field label="Final asset set SHA-256">
          <Input
            required
            value={finalAssetSetSHA256}
            onChange={(event) => setFinalAssetSetSHA256(event.target.value)}
          />
        </Field>
        <fieldset className="form-grid__wide">
          <legend>Immutable impact domains</legend>
          <div className="page-stack">
            {impactDomainOptions.map((option) => (
              <label key={option.value}>
                <input
                  checked={impactDomains.includes(option.value)}
                  onChange={(event) => {
                    setImpactDomains((current) =>
                      event.target.checked
                        ? [...current, option.value]
                        : current.filter((value) => value !== option.value),
                    );
                    if (option.value === "provider_commercial" && !event.target.checked) {
                      setProviderCommercialAuthorizationIds([]);
                    }
                  }}
                  type="checkbox"
                />{" "}
                {option.label}
              </label>
            ))}
          </div>
        </fieldset>
        {impactDomains.includes("provider_commercial") ? (
          <fieldset className="form-grid__wide">
            <legend>Exact active Provider commercial authorizations</legend>
            {providerAuthorizations.isPending ? (
              <LoadingState label="Loading Provider commercial authorizations…" />
            ) : providerAuthorizations.error ? (
              <InlineError error={providerAuthorizations.error} />
            ) : providerAuthorizations.data.items.filter(
                (authorization) => authorization.state === "active",
              ).length === 0 ? (
              <div className="boundary-callout">
                No active Provider commercial authorization is available. Create and approve the
                exact byte-bound authorization before creating this candidate.
              </div>
            ) : (
              <div className="page-stack">
                {providerAuthorizations.data.items
                  .filter((authorization) => authorization.state === "active")
                  .map((authorization) => (
                    <label key={authorization.id}>
                      <input
                        checked={providerCommercialAuthorizationIds.includes(authorization.id)}
                        onChange={(event) =>
                          setProviderCommercialAuthorizationIds((current) =>
                            event.target.checked
                              ? [...current, authorization.id]
                              : current.filter((id) => id !== authorization.id),
                          )
                        }
                        type="checkbox"
                      />{" "}
                      {authorization.provider} · {authorization.authorizationKey} · version{" "}
                      {authorization.version}
                    </label>
                  ))}
              </div>
            )}
          </fieldset>
        ) : null}
        <div className="boundary-callout form-grid__wide">
          Candidate identity and impact scope are immutable after creation. Privacy/Legal approval
          is derived server-side for provider authorization, privacy, residency, regulated-data and
          security incident impacts. A correction creates a new candidate record. The uploaded v3
          receipt is parsed again by the Control Plane, hashed from its exact bytes, and bound to
          this candidate before review can begin. Provider commercial impact also freezes each
          selected active authorization ID and version; revocation, expiry or version drift blocks
          later release transitions.
        </div>
        <div className="form-actions form-grid__wide">
          <Button
            disabled={
              create.isPending ||
              (impactDomains.includes("provider_commercial") &&
                providerCommercialAuthorizationIds.length === 0)
            }
            type="submit"
            variant="primary"
          >
            {create.isPending ? "Creating…" : "Create candidate"}
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={create.error} />
        </div>
      </form>
    </section>
  );
}

function CandidateSummary(props: { readonly candidate: ControlPlaneStage6ReleaseCandidate }) {
  const candidate = props.candidate;
  return (
    <section className="form-panel">
      <div className="form-actions">
        <h2>{candidate.candidateId}</h2>
        <StatusPill value={candidate.state} tone={tone(candidate.state)} />
      </div>
      <dl className="summary-list">
        <div>
          <dt>Source commit</dt>
          <dd title={candidate.sourceCommit}>{abbreviated(candidate.sourceCommit)}</dd>
        </div>
        <div>
          <dt>Environment</dt>
          <dd>{candidate.environmentId}</dd>
        </div>
        <div>
          <dt>Evidence receipt</dt>
          <dd title={candidate.evidenceBundleSha256}>
            {abbreviated(candidate.evidenceBundleSha256)}
          </dd>
        </div>
        <div>
          <dt>Evidence binding</dt>
          <dd>
            {candidate.evidenceReceiptBound
              ? `${candidate.evidenceBundleSchema} · ${candidate.evidenceBundleReceiptSizeBytes} bytes`
              : "Unbound — cannot enter review"}
          </dd>
        </div>
        <div>
          <dt>Desktop artifact set</dt>
          <dd title={candidate.desktopArtifactSetSha256}>
            {abbreviated(candidate.desktopArtifactSetSha256)}
          </dd>
        </div>
        <div>
          <dt>Final assets</dt>
          <dd title={candidate.finalAssetSetSha256}>
            {abbreviated(candidate.finalAssetSetSha256)}
          </dd>
        </div>
        <div>
          <dt>Impact domains</dt>
          <dd>{candidate.impactDomains.map((value) => value.replaceAll("_", " ")).join(", ")}</dd>
        </div>
        <div>
          <dt>Privacy/Legal</dt>
          <dd>{candidate.privacyLegalRequired ? "Required" : "Not required"}</dd>
        </div>
        <div>
          <dt>Provider commercial authorizations</dt>
          <dd>
            {candidate.providerCommercialAuthorizations.length === 0
              ? "Not bound"
              : candidate.providerCommercialAuthorizations
                  .map(
                    (binding) =>
                      `${binding.provider}/${binding.authorizationKey}@${binding.authorizationVersion} (${binding.bindingValid ? "valid" : "invalid"})`,
                  )
                  .join(", ")}
          </dd>
        </div>
        <div>
          <dt>Final Review</dt>
          <dd title={candidate.finalReview?.receiptSha256}>
            {candidate.finalReview === null
              ? "Unbound — cannot enter released"
              : `${abbreviated(candidate.finalReview.receiptSha256)} · ${candidate.finalReview.controlCount} controls`}
          </dd>
        </div>
      </dl>
      <h3>Role decisions</h3>
      {candidate.approvals.length === 0 ? (
        <EmptyState title="No decisions recorded" />
      ) : (
        <div className="page-stack">
          {candidate.approvals.map((approval) => (
            <div className="boundary-callout" key={approval.id}>
              <strong>
                {approval.role.replaceAll("_", " ")} · {approval.decision}
              </strong>
              <span>
                {approval.approverName} · {approval.reason} ·{" "}
                <a href={approval.evidenceReference} rel="noreferrer" target="_blank">
                  evidence
                </a>{" "}
                · {approval.evidenceSha256 ?? "legacy-unbound"}
              </span>
            </div>
          ))}
        </div>
      )}
      {candidate.decisionSummary ? (
        <div className="boundary-callout">
          <strong>Final decision</strong>
          <span>{candidate.decisionSummary}</span>
        </div>
      ) : null}
    </section>
  );
}

function FinalReviewForm(props: {
  readonly candidate: ControlPlaneStage6ReleaseCandidate;
  readonly onChanged: (candidate: ControlPlaneStage6ReleaseCandidate) => void;
}) {
  const [receipt, setReceipt] = useState<{
    readonly name: string;
    readonly sha256: string;
    readonly base64: string;
  } | null>(null);
  const [receiptError, setReceiptError] = useState<string | null>(null);
  const record = useMutation({
    mutationFn: () => {
      if (receipt === null)
        throw new Error("Select the eligible exact-candidate Final Review receipt.");
      return controlPlaneClient.recordPlatformStage6ReleaseFinalReview(props.candidate.id, {
        receiptSha256: receipt.sha256,
        receiptBase64: receipt.base64,
      });
    },
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Bind immutable Final Review</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          record.mutate();
        }}
      >
        <Field label="Final Review validation receipt">
          <input
            accept="application/json,.json"
            required
            type="file"
            onChange={(event) => {
              const file = event.target.files?.[0];
              setReceipt(null);
              setReceiptError(null);
              if (file === undefined) return;
              void readFinalReviewReceipt(file)
                .then(setReceipt)
                .catch((error: unknown) =>
                  setReceiptError(
                    error instanceof Error
                      ? error.message
                      : "Final Review receipt could not be read.",
                  ),
                );
            }}
          />
          {receipt === null ? null : (
            <small title={receipt.sha256}>
              {receipt.name} · {abbreviated(receipt.sha256)}
            </small>
          )}
          {receiptError === null ? null : <small role="alert">{receiptError}</small>}
        </Field>
        <div className="boundary-callout form-grid__wide">
          The Control Plane re-hashes and parses the exact v1 receipt, binds candidate/commit/
          environment/lockfile/artifact identity, rechecks 32 passed controls and separated final
          approvals, and preserves the external-authority boundary. The record is append-only and
          its decision summary and residual risks must match the released transition exactly.
        </div>
        <div className="form-actions form-grid__wide">
          <Button disabled={record.isPending || receipt === null} type="submit" variant="primary">
            {record.isPending ? "Binding…" : "Bind exact Final Review"}
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={record.error} />
        </div>
      </form>
    </section>
  );
}

function ApprovalForm(props: {
  readonly candidate: ControlPlaneStage6ReleaseCandidate;
  readonly onChanged: (candidate: ControlPlaneStage6ReleaseCandidate) => void;
}) {
  const [role, setRole] = useState<ControlPlaneStage6ReleaseApprovalRole>("engineering");
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  const applicableRoles = props.candidate.requiredApprovalRoles;
  useEffect(() => {
    if (!applicableRoles.includes(role)) setRole(applicableRoles[0] ?? "engineering");
  }, [applicableRoles, role]);
  const record = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformStage6ReleaseApproval(props.candidate.id, {
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
      <h2>Record separated decision</h2>
      <div className="boundary-callout">
        Governance roles must show an active exact `release.*` assignment for the selected function.
        Selecting a role here does not grant authority.
      </div>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          record.mutate();
        }}
      >
        <Field label="Approval role">
          <Select
            value={role}
            onChange={(event) =>
              setRole(event.target.value as ControlPlaneStage6ReleaseApprovalRole)
            }
          >
            {applicableRoles.map((item) => (
              <option key={item} value={item}>
                {item.replaceAll("_", " ")}
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
        <Field label="Evidence URL">
          <Input
            required
            type="url"
            value={evidenceReference}
            onChange={(event) => setEvidenceReference(event.target.value)}
          />
        </Field>
        <Field label="Evidence SHA-256">
          <Input
            required
            value={evidenceSHA256}
            onChange={(event) => setEvidenceSHA256(event.target.value)}
            placeholder="sha256:…"
          />
        </Field>
        <Field label="Reason">
          <Input
            minLength={10}
            required
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </Field>
        <div className="boundary-callout form-grid__wide">
          One person may fill only one role and the candidate creator cannot approve. The URL and
          SHA-256 must identify the exact reviewed evidence bytes. Rejection is terminal.
        </div>
        <div className="form-actions form-grid__wide">
          <Button
            disabled={record.isPending}
            type="submit"
            variant={decision === "rejected" ? "danger" : "primary"}
          >
            {record.isPending ? "Recording…" : "Record immutable decision"}
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={record.error} />
        </div>
      </form>
    </section>
  );
}

function TransitionForm(props: {
  readonly candidate: ControlPlaneStage6ReleaseCandidate;
  readonly onChanged: (candidate: ControlPlaneStage6ReleaseCandidate) => void;
}) {
  const options = (nextStates[props.candidate.state] ?? []).filter(
    (state) => state !== "released" || props.candidate.finalReview !== null,
  );
  const [targetState, setTargetState] = useState(options[0] ?? "ready_for_review");
  const [reason, setReason] = useState("");
  useEffect(() => {
    setTargetState(options[0] ?? "ready_for_review");
  }, [props.candidate.finalReview?.id, props.candidate.id, props.candidate.state]);
  const transition = useMutation({
    mutationFn: () => {
      const finalReview = props.candidate.finalReview;
      if (targetState === "released" && finalReview === null) {
        throw new Error("Bind the immutable Final Review before release.");
      }
      return controlPlaneClient.transitionPlatformStage6ReleaseCandidate(props.candidate.id, {
        expectedVersion: props.candidate.version,
        targetState,
        reason,
        ...(targetState === "released"
          ? {
              decisionSummary: finalReview!.decisionSummary,
              residualRiskDisposition: finalReview!.residualRiskDisposition,
              residualRisks: finalReview!.residualRisks,
            }
          : {}),
      });
    },
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Advance candidate state</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          transition.mutate();
        }}
      >
        <Field label="Target state">
          <Select
            value={targetState}
            onChange={(event) => setTargetState(event.target.value as typeof targetState)}
          >
            {options.map((state) => (
              <option key={state} value={state}>
                {state.replaceAll("_", " ")}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Audit reason">
          <Input
            minLength={10}
            required
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </Field>
        {targetState === "released" ? (
          <div className="boundary-callout form-grid__wide">
            <strong>Immutable Final Review decision</strong>
            <span>{props.candidate.finalReview?.decisionSummary}</span>
            <span>
              Residual-risk disposition: {props.candidate.finalReview?.residualRiskDisposition}
            </span>
            {props.candidate.finalReview?.residualRisks.map((risk) => (
              <span key={risk.id}>
                {risk.id} · {risk.owner} · due {new Date(risk.dueAt).toLocaleString()} ·{" "}
                {risk.summary}
              </span>
            ))}
            <span>
              These values come from the exact immutable receipt and cannot be edited during the
              released transition.
            </span>
          </div>
        ) : null}
        <div className="form-actions form-grid__wide">
          <Button
            disabled={transition.isPending}
            type="submit"
            variant={targetState === "rolled_back" ? "danger" : "primary"}
          >
            {transition.isPending ? "Advancing…" : `Move to ${targetState.replaceAll("_", " ")}`}
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={transition.error} />
        </div>
      </form>
    </section>
  );
}
