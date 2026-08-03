// FILE: PlatformComplianceGovernance.tsx
// Purpose: Operate the Stage 6 compliance control, evidence and start-gate register.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneStage6ComplianceControl,
  type ControlPlaneStage6ComplianceDecisionRole,
  type ControlPlaneStage6ComplianceFamily,
  type ControlPlaneStage6ComplianceProgram,
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

const controlFamilies: ReadonlyArray<ControlPlaneStage6ComplianceFamily> = [
  "logical_access",
  "change_release",
  "operations",
  "data_governance",
  "resilience",
  "vendor_provider",
  "security_testing",
];
const decisionRoles: ReadonlyArray<ControlPlaneStage6ComplianceDecisionRole> = [
  "security",
  "operations",
  "legal_privacy",
  "executive",
];

function toISO(value: string): string {
  return new Date(value).toISOString();
}

function shortDigest(value: string): string {
  return value.length > 24 ? `${value.slice(0, 12)}…${value.slice(-8)}` : value;
}

export function PlatformComplianceGovernance(props: {
  readonly canManage: boolean;
  readonly currentUserId: string;
}) {
  const queryClient = useQueryClient();
  const programs = useQuery({
    queryKey: platformQueryKeys.compliancePrograms,
    queryFn: controlPlaneClient.listPlatformStage6CompliancePrograms,
  });
  const [selectedId, setSelectedId] = useState("");
  useEffect(() => {
    if (!selectedId && programs.data?.items[0]) setSelectedId(programs.data.items[0].id);
  }, [programs.data, selectedId]);
  const effectiveSelectedId = selectedId || programs.data?.items[0]?.id || "";
  const selected = useMemo(
    () => programs.data?.items.find((program) => program.id === effectiveSelectedId) ?? null,
    [effectiveSelectedId, programs.data],
  );
  const replaceProgram = (program: ControlPlaneStage6ComplianceProgram) => {
    queryClient.setQueryData(
      platformQueryKeys.compliancePrograms,
      (current: typeof programs.data) => ({
        items: current?.items.map((item) => (item.id === program.id ? program : item)) ?? [program],
      }),
    );
  };

  if (programs.isPending) return <LoadingState label="Loading compliance governance…" />;
  if (programs.error) return <InlineError error={programs.error} />;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Compliance governance</h1>
          <p>
            Named control ownership, append-only evidence metadata, independent reviews and the SOC
            2 start-gate record. Record completeness never means an audit is active or a report has
            been issued.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <CreateProgram
          currentUserId={props.currentUserId}
          onCreated={(program) => {
            replaceProgram(program);
            setSelectedId(program.id);
          }}
        />
      ) : null}
      {programs.data.items.length === 0 ? (
        <EmptyState
          title="No compliance programs"
          description="Create a program only after scope, auditor engagement, observation window and evidence repository policy are approved."
        />
      ) : (
        <section className="form-panel">
          <Field label="Compliance program">
            <Select
              value={effectiveSelectedId}
              onChange={(event) => setSelectedId(event.target.value)}
            >
              {programs.data.items.map((program) => (
                <option key={program.id} value={program.id}>
                  {program.programKey} · {program.state.replaceAll("_", " ")}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? (
        <>
          <ProgramSummary program={selected} />
          {selected.state !== "record_complete" && props.canManage ? (
            <CreateControl
              program={selected}
              currentUserId={props.currentUserId}
              onChanged={replaceProgram}
            />
          ) : null}
          {selected.state !== "record_complete" && selected.controls.length > 0 ? (
            <SubmitEvidence program={selected} onChanged={replaceProgram} />
          ) : null}
          {selected.state !== "record_complete" ? (
            <ReviewEvidence program={selected} onChanged={replaceProgram} />
          ) : null}
          {selected.state === "ready_for_review" ? (
            <RecordDecision program={selected} onChanged={replaceProgram} />
          ) : null}
          {props.canManage && selected.state !== "record_complete" ? (
            <TransitionProgram program={selected} onChanged={replaceProgram} />
          ) : null}
        </>
      ) : null}
    </div>
  );
}

function CreateProgram(props: {
  readonly currentUserId: string;
  readonly onCreated: (program: ControlPlaneStage6ComplianceProgram) => void;
}) {
  const [programKey, setProgramKey] = useState("");
  const [scopeVersion, setScopeVersion] = useState("");
  const [scopeSummary, setScopeSummary] = useState("");
  const [executiveSponsorUserId, setExecutiveSponsorUserId] = useState(props.currentUserId);
  const [auditorOrganization, setAuditorOrganization] = useState("");
  const [auditorEngagementReference, setAuditorEngagementReference] = useState("");
  const [observationStart, setObservationStart] = useState("");
  const [observationEnd, setObservationEnd] = useState("");
  const [evidenceRepositoryReference, setEvidenceRepositoryReference] = useState("");
  const [evidenceAccessPolicyReference, setEvidenceAccessPolicyReference] = useState("");
  const [evidenceRetentionDays, setEvidenceRetentionDays] = useState("365");
  const [vendorRegisterReference, setVendorRegisterReference] = useState("");
  const [riskRegisterReference, setRiskRegisterReference] = useState("");
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createPlatformStage6ComplianceProgram({
        programKey,
        framework: "soc2_type2",
        scopeVersion,
        scopeSummary,
        executiveSponsorUserId,
        auditorOrganization,
        auditorEngagementReference,
        observationStart: toISO(observationStart),
        observationEnd: toISO(observationEnd),
        evidenceRepositoryReference,
        evidenceAccessPolicyReference,
        evidenceRetentionDays: Number(evidenceRetentionDays),
        vendorRegisterReference,
        riskRegisterReference,
      }),
    onSuccess: props.onCreated,
  });
  return (
    <section className="form-panel">
      <h2>Create immutable program scope</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Program key">
          <Input
            required
            value={programKey}
            onChange={(event) => setProgramKey(event.target.value)}
            placeholder="soc2-2026"
          />
        </Field>
        <Field label="Scope version">
          <Input
            required
            value={scopeVersion}
            onChange={(event) => setScopeVersion(event.target.value)}
            placeholder="2026.1"
          />
        </Field>
        <Field label="Executive sponsor user ID">
          <Input
            required
            value={executiveSponsorUserId}
            onChange={(event) => setExecutiveSponsorUserId(event.target.value)}
          />
        </Field>
        <Field label="Auditor organization">
          <Input
            required
            value={auditorOrganization}
            onChange={(event) => setAuditorOrganization(event.target.value)}
          />
        </Field>
        <Field label="Observation starts">
          <Input
            required
            type="datetime-local"
            value={observationStart}
            onChange={(event) => setObservationStart(event.target.value)}
          />
        </Field>
        <Field label="Observation ends">
          <Input
            required
            type="datetime-local"
            value={observationEnd}
            onChange={(event) => setObservationEnd(event.target.value)}
          />
        </Field>
        <Field label="Auditor engagement HTTPS reference">
          <Input
            required
            type="url"
            value={auditorEngagementReference}
            onChange={(event) => setAuditorEngagementReference(event.target.value)}
          />
        </Field>
        <Field label="Evidence repository HTTPS reference">
          <Input
            required
            type="url"
            value={evidenceRepositoryReference}
            onChange={(event) => setEvidenceRepositoryReference(event.target.value)}
          />
        </Field>
        <Field label="Evidence access policy HTTPS reference">
          <Input
            required
            type="url"
            value={evidenceAccessPolicyReference}
            onChange={(event) => setEvidenceAccessPolicyReference(event.target.value)}
          />
        </Field>
        <Field label="Retention days">
          <Input
            required
            min={365}
            max={3650}
            type="number"
            value={evidenceRetentionDays}
            onChange={(event) => setEvidenceRetentionDays(event.target.value)}
          />
        </Field>
        <Field label="Vendor register HTTPS reference">
          <Input
            required
            type="url"
            value={vendorRegisterReference}
            onChange={(event) => setVendorRegisterReference(event.target.value)}
          />
        </Field>
        <Field label="Risk register HTTPS reference">
          <Input
            required
            type="url"
            value={riskRegisterReference}
            onChange={(event) => setRiskRegisterReference(event.target.value)}
          />
        </Field>
        <div className="form-grid__wide">
          <Field label="Approved scope summary">
            <textarea
              required
              minLength={20}
              maxLength={4000}
              value={scopeSummary}
              onChange={(event) => setScopeSummary(event.target.value)}
            />
          </Field>
        </div>
        <div className="boundary-callout form-grid__wide">
          Scope, auditor, observation window and repository policy are immutable. Corrections
          require a new program record.
        </div>
        <div className="form-actions form-grid__wide">
          <Button disabled={create.isPending} type="submit" variant="primary">
            {create.isPending ? "Creating…" : "Create compliance program"}
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={create.error} />
        </div>
      </form>
    </section>
  );
}

function ProgramSummary(props: { readonly program: ControlPlaneStage6ComplianceProgram }) {
  const program = props.program;
  return (
    <section className="form-panel">
      <div className="form-actions">
        <h2>{program.programKey}</h2>
        <StatusPill
          value={program.state}
          tone={program.state === "record_complete" ? "active" : "warning"}
        />
      </div>
      <div className="boundary-callout">
        Assessment: <strong>{program.readiness.assessment}</strong>. External auditor authority,
        object immutability and certification remain human/external gates.
      </div>
      <dl className="summary-list">
        <div>
          <dt>Framework</dt>
          <dd>{program.framework}</dd>
        </div>
        <div>
          <dt>Scope version</dt>
          <dd>{program.scopeVersion}</dd>
        </div>
        <div>
          <dt>Auditor</dt>
          <dd>{program.auditorOrganization}</dd>
        </div>
        <div>
          <dt>Observation</dt>
          <dd>
            {new Date(program.observationStart).toLocaleDateString()} –{" "}
            {new Date(program.observationEnd).toLocaleDateString()}
          </dd>
        </div>
        <div>
          <dt>Control families</dt>
          <dd>{7 - program.readiness.missingControlFamilies.length}/7</dd>
        </div>
        <div>
          <dt>Start-gate decisions</dt>
          <dd>{4 - program.readiness.missingDecisionRoles.length}/4</dd>
        </div>
        <div>
          <dt>Accepted release manifest</dt>
          <dd>{program.readiness.hasAcceptedReleaseManifest ? "yes" : "no"}</dd>
        </div>
        <div>
          <dt>Retention</dt>
          <dd>{program.evidenceRetentionDays} days</dd>
        </div>
      </dl>
      {program.readiness.missingControlFamilies.length > 0 ? (
        <p>Missing controls: {program.readiness.missingControlFamilies.join(", ")}</p>
      ) : null}
      {program.readiness.missingDecisionRoles.length > 0 ? (
        <p>Missing decisions: {program.readiness.missingDecisionRoles.join(", ")}</p>
      ) : null}
      {program.decisions.map((decision) => (
        <div className="boundary-callout" key={decision.id}>
          <strong>{decision.decisionRole}</strong> · {decision.decision} ·{" "}
          {decision.evidenceSha256 ?? "legacy-unbound"} ·{" "}
          {decision.supersededAt === null ? "active" : "superseded"}
        </div>
      ))}
      {program.controls.map((control) => (
        <ControlSummary key={control.id} control={control} />
      ))}
    </section>
  );
}

function ControlSummary(props: { readonly control: ControlPlaneStage6ComplianceControl }) {
  return (
    <article className="resource-card">
      <div className="form-actions">
        <strong>
          {props.control.controlId} · {props.control.title}
        </strong>
        <StatusPill value={props.control.family} tone="neutral" />
      </div>
      <p>{props.control.description}</p>
      <p>
        Owner {props.control.ownerUserId} · {props.control.cadence} ·{" "}
        {props.control.evidence.length} evidence item(s)
      </p>
      {props.control.evidence.map((evidence) => (
        <div className="boundary-callout" key={evidence.id}>
          <strong>{evidence.evidenceId}</strong> · {evidence.evidenceType} ·{" "}
          {shortDigest(evidence.sha256)} · review {evidence.review?.decision ?? "pending"} · review
          digest {evidence.review?.evidenceSha256 ?? "unbound"}
        </div>
      ))}
    </article>
  );
}

function CreateControl(props: {
  readonly program: ControlPlaneStage6ComplianceProgram;
  readonly currentUserId: string;
  readonly onChanged: (program: ControlPlaneStage6ComplianceProgram) => void;
}) {
  const [controlId, setControlId] = useState("");
  const [family, setFamily] = useState<ControlPlaneStage6ComplianceFamily>("logical_access");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [ownerUserId, setOwnerUserId] = useState(props.currentUserId);
  const [cadence, setCadence] =
    useState<ControlPlaneStage6ComplianceControl["cadence"]>("quarterly");
  const [evidenceRequirement, setEvidenceRequirement] = useState("");
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createPlatformStage6ComplianceControl(props.program.id, {
        controlId,
        family,
        title,
        description,
        ownerUserId,
        cadence,
        evidenceRequirement,
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Add append-only control</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Control ID">
          <Input
            required
            value={controlId}
            onChange={(event) => setControlId(event.target.value)}
            placeholder="CC6.1"
          />
        </Field>
        <Field label="Family">
          <Select
            value={family}
            onChange={(event) =>
              setFamily(event.target.value as ControlPlaneStage6ComplianceFamily)
            }
          >
            {controlFamilies.map((value) => (
              <option key={value}>{value}</option>
            ))}
          </Select>
        </Field>
        <Field label="Title">
          <Input required value={title} onChange={(event) => setTitle(event.target.value)} />
        </Field>
        <Field label="Owner user ID">
          <Input
            required
            value={ownerUserId}
            onChange={(event) => setOwnerUserId(event.target.value)}
          />
        </Field>
        <Field label="Cadence">
          <Select
            value={cadence}
            onChange={(event) => setCadence(event.target.value as typeof cadence)}
          >
            {[
              "continuous",
              "daily",
              "monthly",
              "quarterly",
              "annual",
              "per_release",
              "per_incident",
            ].map((value) => (
              <option key={value}>{value}</option>
            ))}
          </Select>
        </Field>
        <div className="form-grid__wide">
          <Field label="Description">
            <textarea
              required
              minLength={20}
              value={description}
              onChange={(event) => setDescription(event.target.value)}
            />
          </Field>
        </div>
        <div className="form-grid__wide">
          <Field label="Evidence requirement">
            <textarea
              required
              minLength={20}
              value={evidenceRequirement}
              onChange={(event) => setEvidenceRequirement(event.target.value)}
            />
          </Field>
        </div>
        <div className="form-actions form-grid__wide">
          <Button disabled={create.isPending} type="submit" variant="primary">
            Add control
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={create.error} />
        </div>
      </form>
    </section>
  );
}

function SubmitEvidence(props: {
  readonly program: ControlPlaneStage6ComplianceProgram;
  readonly onChanged: (program: ControlPlaneStage6ComplianceProgram) => void;
}) {
  const [controlRecordId, setControlRecordId] = useState(props.program.controls[0]?.id ?? "");
  const [evidenceId, setEvidenceId] = useState("");
  const [evidenceType, setEvidenceType] = useState<"release_manifest" | "other">(
    "release_manifest",
  );
  const [periodStart, setPeriodStart] = useState("");
  const [periodEnd, setPeriodEnd] = useState("");
  const [sourceReference, setSourceReference] = useState("");
  const [sha256, setSha256] = useState("");
  const [collectedAt, setCollectedAt] = useState("");
  const [retentionUntil, setRetentionUntil] = useState("");
  const submit = useMutation({
    mutationFn: () =>
      controlPlaneClient.submitPlatformStage6ComplianceEvidence(props.program.id, {
        controlRecordId,
        evidenceId,
        evidenceType,
        periodStart: toISO(periodStart),
        periodEnd: toISO(periodEnd),
        sourceReference,
        sha256,
        mediaType: "application/json",
        classification: "confidential",
        collectedAt: toISO(collectedAt),
        retentionUntil: toISO(retentionUntil),
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Submit immutable evidence metadata</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          submit.mutate();
        }}
      >
        <Field label="Control">
          <Select
            value={controlRecordId}
            onChange={(event) => setControlRecordId(event.target.value)}
          >
            {props.program.controls.map((control) => (
              <option key={control.id} value={control.id}>
                {control.controlId}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Evidence ID">
          <Input
            required
            value={evidenceId}
            onChange={(event) => setEvidenceId(event.target.value)}
          />
        </Field>
        <Field label="Evidence type">
          <Select
            value={evidenceType}
            onChange={(event) => setEvidenceType(event.target.value as typeof evidenceType)}
          >
            <option value="release_manifest">release_manifest</option>
            <option value="other">other</option>
          </Select>
        </Field>
        <Field label="Source HTTPS reference">
          <Input
            required
            type="url"
            value={sourceReference}
            onChange={(event) => setSourceReference(event.target.value)}
          />
        </Field>
        <Field label="SHA-256">
          <Input required value={sha256} onChange={(event) => setSha256(event.target.value)} />
        </Field>
        <Field label="Period starts">
          <Input
            required
            type="datetime-local"
            value={periodStart}
            onChange={(event) => setPeriodStart(event.target.value)}
          />
        </Field>
        <Field label="Period ends">
          <Input
            required
            type="datetime-local"
            value={periodEnd}
            onChange={(event) => setPeriodEnd(event.target.value)}
          />
        </Field>
        <Field label="Collected at">
          <Input
            required
            type="datetime-local"
            value={collectedAt}
            onChange={(event) => setCollectedAt(event.target.value)}
          />
        </Field>
        <Field label="Retain until">
          <Input
            required
            type="datetime-local"
            value={retentionUntil}
            onChange={(event) => setRetentionUntil(event.target.value)}
          />
        </Field>
        <div className="boundary-callout form-grid__wide">
          Only metadata and digest are stored here. Do not submit secrets, prompts, Tenant content
          or raw evidence.
        </div>
        <div className="form-actions form-grid__wide">
          <Button disabled={submit.isPending} type="submit" variant="primary">
            Submit evidence metadata
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={submit.error} />
        </div>
      </form>
    </section>
  );
}

function ReviewEvidence(props: {
  readonly program: ControlPlaneStage6ComplianceProgram;
  readonly onChanged: (program: ControlPlaneStage6ComplianceProgram) => void;
}) {
  const pending = props.program.controls
    .flatMap((control) => control.evidence)
    .filter((evidence) => !evidence.review);
  const [evidenceRecordId, setEvidenceRecordId] = useState(pending[0]?.id ?? "");
  const [decision, setDecision] = useState<"accepted" | "rejected">("accepted");
  const [reviewRole, setReviewRole] = useState<
    "security" | "operations" | "legal_privacy" | "auditor"
  >("security");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  const review = useMutation({
    mutationFn: () =>
      controlPlaneClient.reviewPlatformStage6ComplianceEvidence(
        props.program.id,
        evidenceRecordId,
        { decision, reviewRole, reason, evidenceReference, evidenceSha256: evidenceSHA256 },
      ),
    onSuccess: props.onChanged,
  });
  if (pending.length === 0) return null;
  return (
    <section className="form-panel">
      <h2>Independently review evidence</h2>
      <div className="boundary-callout">
        Requires the matching active `compliance.evidence.*` Governance roles assignment.
      </div>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          review.mutate();
        }}
      >
        <Field label="Evidence">
          <Select
            value={evidenceRecordId}
            onChange={(event) => setEvidenceRecordId(event.target.value)}
          >
            {pending.map((evidence) => (
              <option key={evidence.id} value={evidence.id}>
                {evidence.evidenceId}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Review role">
          <Select
            value={reviewRole}
            onChange={(event) => setReviewRole(event.target.value as typeof reviewRole)}
          >
            {["security", "operations", "legal_privacy", "auditor"].map((value) => (
              <option key={value}>{value}</option>
            ))}
          </Select>
        </Field>
        <Field label="Decision">
          <Select
            value={decision}
            onChange={(event) => setDecision(event.target.value as typeof decision)}
          >
            <option value="accepted">accepted</option>
            <option value="rejected">rejected</option>
          </Select>
        </Field>
        <Field label="Review evidence HTTPS reference">
          <Input
            required
            type="url"
            value={evidenceReference}
            onChange={(event) => setEvidenceReference(event.target.value)}
          />
        </Field>
        <Field label="Exact review evidence SHA-256">
          <Input
            required
            pattern="sha256:[0-9a-f]{64}"
            placeholder="sha256:…"
            value={evidenceSHA256}
            onChange={(event) => setEvidenceSHA256(event.target.value)}
          />
        </Field>
        <div className="form-grid__wide">
          <Field label="Reason">
            <textarea
              required
              minLength={10}
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
          </Field>
        </div>
        <div className="form-actions form-grid__wide">
          <Button disabled={review.isPending} type="submit" variant="primary">
            Record immutable review
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={review.error} />
        </div>
      </form>
    </section>
  );
}

function RecordDecision(props: {
  readonly program: ControlPlaneStage6ComplianceProgram;
  readonly onChanged: (program: ControlPlaneStage6ComplianceProgram) => void;
}) {
  const missing = decisionRoles.filter(
    (role) =>
      !props.program.decisions.some(
        (decision) => decision.decisionRole === role && decision.supersededAt === null,
      ),
  );
  const [decisionRole, setDecisionRole] = useState<ControlPlaneStage6ComplianceDecisionRole>(
    missing[0] ?? "security",
  );
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  const record = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformStage6ComplianceDecision(props.program.id, {
        decisionRole,
        decision,
        reason,
        evidenceReference,
        evidenceSha256: evidenceSHA256,
      }),
    onSuccess: props.onChanged,
  });
  if (missing.length === 0) return null;
  return (
    <section className="form-panel">
      <h2>Record separated start-gate decision</h2>
      <div className="boundary-callout">
        Requires the matching active `compliance.*` Governance roles assignment; this role selector
        is not authority.
      </div>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          record.mutate();
        }}
      >
        <Field label="Decision role">
          <Select
            value={decisionRole}
            onChange={(event) => setDecisionRole(event.target.value as typeof decisionRole)}
          >
            {missing.map((value) => (
              <option key={value}>{value}</option>
            ))}
          </Select>
        </Field>
        <Field label="Decision">
          <Select
            value={decision}
            onChange={(event) => setDecision(event.target.value as typeof decision)}
          >
            <option value="approved">approved</option>
            <option value="rejected">rejected</option>
          </Select>
        </Field>
        <Field label="Decision evidence HTTPS reference">
          <Input
            required
            type="url"
            value={evidenceReference}
            onChange={(event) => setEvidenceReference(event.target.value)}
          />
        </Field>
        <Field label="Exact decision evidence SHA-256">
          <Input
            required
            pattern="sha256:[0-9a-f]{64}"
            placeholder="sha256:…"
            value={evidenceSHA256}
            onChange={(event) => setEvidenceSHA256(event.target.value)}
          />
        </Field>
        <div className="form-grid__wide">
          <Field label="Reason">
            <textarea
              required
              minLength={10}
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
          </Field>
        </div>
        <div className="form-actions form-grid__wide">
          <Button disabled={record.isPending} type="submit" variant="primary">
            Record immutable decision
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={record.error} />
        </div>
      </form>
    </section>
  );
}

function TransitionProgram(props: {
  readonly program: ControlPlaneStage6ComplianceProgram;
  readonly onChanged: (program: ControlPlaneStage6ComplianceProgram) => void;
}) {
  const targetState = props.program.state === "draft" ? "ready_for_review" : "record_complete";
  const [reason, setReason] = useState("");
  const transition = useMutation({
    mutationFn: () =>
      controlPlaneClient.transitionPlatformStage6ComplianceProgram(props.program.id, {
        expectedVersion: props.program.version,
        targetState,
        reason,
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Advance compliance record</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          transition.mutate();
        }}
      >
        <Field label="Target state">
          <Input readOnly value={targetState} />
        </Field>
        <div className="form-grid__wide">
          <Field label="Reason">
            <textarea
              required
              minLength={10}
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
          </Field>
        </div>
        <div className="form-actions form-grid__wide">
          <Button
            disabled={
              transition.isPending ||
              (targetState === "record_complete" &&
                !props.program.readiness.eligibleForRecordCompleteReview)
            }
            type="submit"
            variant="primary"
          >
            Advance compliance record
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={transition.error} />
        </div>
      </form>
    </section>
  );
}
