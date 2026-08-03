// FILE: PlatformIncidentGovernance.tsx
// Purpose: Coordinate durable incident roles, internal Status Board evidence, cadence and resolution.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneSessionState,
  type ControlPlaneStage6Incident,
  type ControlPlaneStage6IncidentComponent,
  type ControlPlaneStage6IncidentSeverity,
  type ControlPlaneStage6IncidentState,
  type ControlPlaneTenantMember,
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

const componentOptions: ReadonlyArray<{
  readonly value: ControlPlaneStage6IncidentComponent;
  readonly label: string;
}> = [
  { value: "control-plane-api", label: "Control Plane / API" },
  { value: "authentication-sso", label: "Authentication / SSO" },
  { value: "execution-scheduling", label: "Execution scheduling" },
  { value: "worker-runtime", label: "Worker runtime" },
  { value: "artifact-service", label: "Artifact service" },
  { value: "web-application", label: "Web application" },
];

const transitions: Partial<
  Record<
    ControlPlaneStage6IncidentState,
    ReadonlyArray<Exclude<ControlPlaneStage6IncidentState, "investigating">>
  >
> = {
  investigating: ["identified", "cancelled"],
  identified: ["monitoring", "cancelled"],
  monitoring: ["resolved"],
};

function toLocalInput(date: Date): string {
  const offset = date.getTimezoneOffset() * 60_000;
  return new Date(date.getTime() - offset).toISOString().slice(0, 16);
}

function toUTC(local: string): string {
  return new Date(local).toISOString();
}

function tone(value: string): "active" | "warning" | "neutral" | "danger" {
  if (value === "resolved" || value === "on-time") return "active";
  if (value === "overdue" || value === "SEV-0" || value === "SEV-1") return "danger";
  if (value === "monitoring" || value.startsWith("awaiting")) return "warning";
  return "neutral";
}

function operatorMembers(items: ReadonlyArray<ControlPlaneTenantMember>) {
  return items.filter(
    (member) =>
      member.status === "active" &&
      (member.role === "owner" || member.role === "admin" || member.role === "security_admin"),
  );
}

export function PlatformIncidentGovernance(props: { readonly session: ControlPlaneSessionState }) {
  const queryClient = useQueryClient();
  const incidents = useQuery({
    queryKey: platformQueryKeys.incidents,
    queryFn: controlPlaneClient.listPlatformStage6Incidents,
    refetchInterval: 30_000,
  });
  const members = useQuery({
    queryKey: [
      "control-plane",
      "platform",
      "incident-operators",
      props.session.user.activeTenantId,
    ],
    queryFn: () => {
      if (!props.session.user.activeTenantId)
        throw new Error("Platform Operator Tenant is unavailable.");
      return controlPlaneClient.listTenantMembers(props.session.user.activeTenantId);
    },
    enabled: props.session.user.activeTenantId !== null,
  });
  const [selectedId, setSelectedId] = useState("");
  useEffect(() => {
    if (!selectedId && incidents.data?.items[0]) setSelectedId(incidents.data.items[0].id);
  }, [incidents.data, selectedId]);
  const selected = useMemo(
    () =>
      incidents.data?.items.find((incident) => incident.id === selectedId) ??
      incidents.data?.items[0] ??
      null,
    [incidents.data, selectedId],
  );
  const replace = (incident: ControlPlaneStage6Incident) => {
    queryClient.setQueryData(platformQueryKeys.incidents, (current: typeof incidents.data) => ({
      items: current?.items.map((item) => (item.id === incident.id ? incident : item)) ?? [
        incident,
      ],
    }));
    setSelectedId(incident.id);
  };

  if (incidents.isPending || members.isPending)
    return <LoadingState label="Loading incident governance…" />;
  if (incidents.error || members.error)
    return <InlineError error={incidents.error ?? members.error} />;
  const availableMembers = operatorMembers(members.data.items);

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Incident governance</h1>
          <p>
            Durable role assignment, employee-safe internal Status Board evidence and update
            cadence. Paging remains a separate operational authority.
          </p>
        </div>
      </header>
      <CreateIncident
        currentUserId={props.session.user.userId}
        members={availableMembers}
        onCreated={replace}
      />
      {incidents.data.items.length === 0 ? (
        <EmptyState
          title="No incidents"
          description="Create an incident when user impact is confirmed; do not use this surface for private raw logs."
        />
      ) : (
        <section className="form-panel">
          <Field label="Incident">
            <Select
              value={selected?.id ?? ""}
              onChange={(event) => setSelectedId(event.target.value)}
            >
              {incidents.data.items.map((incident) => (
                <option key={incident.id} value={incident.id}>
                  {incident.incidentKey} · {incident.severity} · {incident.state}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? (
        <IncidentWorkspace
          currentUserId={props.session.user.userId}
          incident={selected}
          onChanged={replace}
        />
      ) : null}
    </div>
  );
}

function CreateIncident(props: {
  readonly currentUserId: string;
  readonly members: ReadonlyArray<ControlPlaneTenantMember>;
  readonly onCreated: (incident: ControlPlaneStage6Incident) => void;
}) {
  const now = new Date();
  const [incidentKey, setIncidentKey] = useState("");
  const [severity, setSeverity] = useState<ControlPlaneStage6IncidentSeverity>("SEV-2");
  const [title, setTitle] = useState("");
  const [summary, setSummary] = useState("");
  const [broadInternalImpact, setBroadInternalImpact] = useState(true);
  const [securityImpact, setSecurityImpact] = useState(false);
  const [components, setComponents] = useState<ReadonlyArray<ControlPlaneStage6IncidentComponent>>([
    "control-plane-api",
  ]);
  const [regions, setRegions] = useState("global");
  const separatedMembers = props.members.filter((member) => member.userId !== props.currentUserId);
  const [communicationsLead, setCommunicationsLead] = useState(separatedMembers[0]?.userId ?? "");
  const [securityLead, setSecurityLead] = useState(
    separatedMembers[1]?.userId ?? separatedMembers[0]?.userId ?? "",
  );
  const [startedAt, setStartedAt] = useState(toLocalInput(new Date(now.getTime() - 5 * 60_000)));
  const [confirmedAt, setConfirmedAt] = useState(toLocalInput(now));
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createPlatformStage6Incident({
        incidentKey,
        severity,
        title,
        internalImpactSummary: summary,
        broadInternalImpact,
        securityPrivacyImpact: securityImpact,
        affectedComponents: components,
        affectedRegions: regions
          .split(",")
          .map((value) => value.trim())
          .filter(Boolean),
        communicationsLeadUserId: communicationsLead,
        ...(securityImpact ? { securityPrivacyLeadUserId: securityLead } : {}),
        startedAt: toUTC(startedAt),
        impactConfirmedAt: toUTC(confirmedAt),
      }),
    onSuccess: props.onCreated,
  });

  return (
    <section className="form-panel">
      <h2>Open governed incident</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Incident ID">
          <Input
            required
            value={incidentKey}
            onChange={(event) => setIncidentKey(event.target.value)}
            placeholder="INC-2026-001"
          />
        </Field>
        <Field label="Severity">
          <Select
            value={severity}
            onChange={(event) =>
              setSeverity(event.target.value as ControlPlaneStage6IncidentSeverity)
            }
          >
            {(["SEV-0", "SEV-1", "SEV-2", "SEV-3"] as const).map((value) => (
              <option key={value}>{value}</option>
            ))}
          </Select>
        </Field>
        <Field label="Title">
          <Input
            required
            minLength={10}
            maxLength={200}
            value={title}
            onChange={(event) => setTitle(event.target.value)}
          />
        </Field>
        <Field label="Affected Regions">
          <Input
            required
            value={regions}
            onChange={(event) => setRegions(event.target.value)}
            placeholder="eu-west-1, us-east-1"
          />
        </Field>
        <Field label="Impact confirmed at">
          <Input
            required
            type="datetime-local"
            value={confirmedAt}
            onChange={(event) => setConfirmedAt(event.target.value)}
          />
        </Field>
        <Field label="Impact started at">
          <Input
            required
            type="datetime-local"
            value={startedAt}
            onChange={(event) => setStartedAt(event.target.value)}
          />
        </Field>
        <Field label="Communications lead">
          <Select
            required
            value={communicationsLead}
            onChange={(event) => setCommunicationsLead(event.target.value)}
          >
            <option value="">Select a separated operator</option>
            {separatedMembers.map((member) => (
              <option key={member.userId} value={member.userId}>
                {member.displayName} · {member.role}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Security / Privacy lead">
          <Select
            disabled={!securityImpact}
            required={securityImpact}
            value={securityLead}
            onChange={(event) => setSecurityLead(event.target.value)}
          >
            <option value="">Select a separated operator</option>
            {separatedMembers.map((member) => (
              <option key={member.userId} value={member.userId}>
                {member.displayName} · {member.role}
              </option>
            ))}
          </Select>
        </Field>
        <fieldset className="field form-grid__wide">
          <legend className="field__label">Affected internal components</legend>
          <div className="checkbox-grid">
            {componentOptions.map((option) => (
              <label key={option.value}>
                <input
                  checked={components.includes(option.value)}
                  type="checkbox"
                  onChange={(event) =>
                    setComponents((current) =>
                      event.target.checked
                        ? [...current, option.value]
                        : current.filter((value) => value !== option.value),
                    )
                  }
                />{" "}
                {option.label}
              </label>
            ))}
          </div>
        </fieldset>
        <Field label="User-safe impact summary">
          <textarea
            className="control"
            required
            minLength={20}
            maxLength={2000}
            value={summary}
            onChange={(event) => setSummary(event.target.value)}
          />
        </Field>
        <div className="field">
          <label>
            <input
              checked={broadInternalImpact}
              type="checkbox"
              onChange={(event) => setBroadInternalImpact(event.target.checked)}
            />{" "}
            Broad internal user impact
          </label>
          <label>
            <input
              checked={securityImpact}
              type="checkbox"
              onChange={(event) => setSecurityImpact(event.target.checked)}
            />{" "}
            Security / Privacy impact
          </label>
        </div>
        <div className="form-actions form-grid__wide">
          <Button
            disabled={create.isPending || components.length === 0}
            type="submit"
            variant="primary"
          >
            {create.isPending ? "Opening…" : "Open governed incident"}
          </Button>
        </div>
        <InlineError error={create.error} />
      </form>
    </section>
  );
}

function IncidentWorkspace(props: {
  readonly currentUserId: string;
  readonly incident: ControlPlaneStage6Incident;
  readonly onChanged: (incident: ControlPlaneStage6Incident) => void;
}) {
  const { incident } = props;
  const isCommander = incident.incidentCommander.userId === props.currentUserId;
  const isCommunicationsLead = incident.communicationsLead.userId === props.currentUserId;
  const isSecurityLead = incident.securityPrivacyLead?.userId === props.currentUserId;
  return (
    <>
      <section className="detail-panel">
        <div className="detail-panel__header">
          <div>
            <span className="eyebrow">{incident.incidentKey}</span>
            <h2>{incident.title}</h2>
          </div>
          <div className="status-row">
            <StatusPill value={incident.severity} tone={tone(incident.severity)} />
            <StatusPill value={incident.state} tone={tone(incident.state)} />
            <StatusPill value={incident.cadence.status} tone={tone(incident.cadence.status)} />
          </div>
        </div>
        <p>{incident.internalImpactSummary}</p>
        <dl className="definition-grid">
          <div>
            <dt>Incident commander</dt>
            <dd>{incident.incidentCommander.displayName}</dd>
          </div>
          <div>
            <dt>Communications lead</dt>
            <dd>{incident.communicationsLead.displayName}</dd>
          </div>
          <div>
            <dt>Security / Privacy</dt>
            <dd>{incident.securityPrivacyLead?.displayName ?? "Not declared"}</dd>
          </div>
          <div>
            <dt>Internal Status Board authority</dt>
            <dd>{incident.internalStatusBoardOrigin ?? "Not broad-impact"}</dd>
          </div>
          <div>
            <dt>Internal Status Board reference</dt>
            <dd>{incident.internalStatusBoardIncidentReference ?? "Not bound"}</dd>
          </div>
          <div>
            <dt>Components</dt>
            <dd>{incident.affectedComponents.join(", ")}</dd>
          </div>
          <div>
            <dt>Regions</dt>
            <dd>{incident.affectedRegions.join(", ")}</dd>
          </div>
          <div>
            <dt>Next internal update</dt>
            <dd>
              {incident.cadence.nextInternalUpdateTargetAt
                ? new Date(incident.cadence.nextInternalUpdateTargetAt).toLocaleString()
                : "—"}
            </dd>
          </div>
          <div>
            <dt>Version</dt>
            <dd>{incident.version}</dd>
          </div>
        </dl>
      </section>
      <InternalNotificationStatus incident={incident} />
      {incident.broadInternalImpact &&
      !incident.internalStatusBoardIncidentReference &&
      (isCommander || isCommunicationsLead) ? (
        <BindStatusBoard incident={incident} onChanged={props.onChanged} />
      ) : null}
      {incident.broadInternalImpact &&
      incident.internalStatusBoardIncidentReference &&
      isCommunicationsLead &&
      incident.state !== "resolved" &&
      incident.state !== "cancelled" ? (
        <InternalUpdateForm incident={incident} onChanged={props.onChanged} />
      ) : null}
      <InternalTimeline incident={incident} />
      {incident.resolutionApprovals.length > 0 ? (
        <section className="detail-card">
          <h2>Security / Privacy resolution decisions</h2>
          <ul className="timeline-list">
            {incident.resolutionApprovals.map((approval) => (
              <li key={approval.id}>
                <strong>{approval.decision}</strong>
                <span>
                  {approval.approver.displayName || approval.approver.email} ·{" "}
                  {new Date(approval.createdAt).toLocaleString()}
                </span>
                <p>{approval.reason}</p>
                <code>{approval.evidenceSha256 ?? "legacy URL-only evidence"}</code>
                <span>{approval.supersededAt ? "superseded" : "active"}</span>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
      {incident.securityPrivacyImpact &&
      incident.state === "monitoring" &&
      isSecurityLead &&
      !incident.resolutionApproval ? (
        <ResolutionApprovalForm incident={incident} onChanged={props.onChanged} />
      ) : null}
      {isCommander && (transitions[incident.state]?.length ?? 0) > 0 ? (
        <TransitionForm incident={incident} onChanged={props.onChanged} />
      ) : null}
    </>
  );
}

function BindStatusBoard(props: {
  readonly incident: ControlPlaneStage6Incident;
  readonly onChanged: (incident: ControlPlaneStage6Incident) => void;
}) {
  const [reference, setReference] = useState("");
  const [reason, setReason] = useState("");
  const bind = useMutation({
    mutationFn: () =>
      controlPlaneClient.bindPlatformStage6IncidentStatusBoard(props.incident.id, {
        expectedVersion: props.incident.version,
        internalStatusBoardIncidentReference: reference,
        reason,
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Bind internal Status Board incident</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          bind.mutate();
        }}
      >
        <Field label="Internal board incident reference">
          <Input
            required
            value={reference}
            onChange={(event) => setReference(event.target.value)}
          />
        </Field>
        <Field label="Reason">
          <Input
            required
            minLength={10}
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </Field>
        <div className="form-actions form-grid__wide">
          <Button type="submit" variant="primary" disabled={bind.isPending}>
            Bind immutable reference
          </Button>
        </div>
        <InlineError error={bind.error} />
      </form>
    </section>
  );
}

function InternalUpdateForm(props: {
  readonly incident: ControlPlaneStage6Incident;
  readonly onChanged: (incident: ControlPlaneStage6Incident) => void;
}) {
  const defaultKind = props.incident.internalUpdates.length === 0 ? "initial" : "progress";
  const [kind, setKind] = useState<"initial" | "progress" | "resolved">(defaultKind);
  const [summary, setSummary] = useState("");
  const [publishedAt, setPublishedAt] = useState(toLocalInput(new Date()));
  const [evidenceReference, setEvidenceReference] = useState("");
  const record = useMutation({
    mutationFn: () =>
      controlPlaneClient.addPlatformStage6IncidentInternalUpdate(props.incident.id, {
        expectedVersion: props.incident.version,
        kind,
        summary,
        publishedAt: toUTC(publishedAt),
        evidenceReference,
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Record internal incident update</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          record.mutate();
        }}
      >
        <Field label="Update kind">
          <Select value={kind} onChange={(event) => setKind(event.target.value as typeof kind)}>
            {props.incident.internalUpdates.length === 0 ? (
              <option value="initial">Initial</option>
            ) : (
              <>
                <option value="progress">Progress</option>
                {props.incident.state === "monitoring" ? (
                  <option value="resolved">Resolved</option>
                ) : null}
              </>
            )}
          </Select>
        </Field>
        <Field label="Published at">
          <Input
            required
            type="datetime-local"
            value={publishedAt}
            onChange={(event) => setPublishedAt(event.target.value)}
          />
        </Field>
        <Field label="Internal Status Board evidence URL">
          <Input
            required
            type="url"
            value={evidenceReference}
            onChange={(event) => setEvidenceReference(event.target.value)}
          />
        </Field>
        <Field label="Employee-safe summary">
          <textarea
            className="control"
            required
            minLength={20}
            maxLength={2000}
            value={summary}
            onChange={(event) => setSummary(event.target.value)}
          />
        </Field>
        <div className="form-actions form-grid__wide">
          <Button type="submit" variant="primary" disabled={record.isPending}>
            Record immutable internal update
          </Button>
        </div>
        <InlineError error={record.error} />
      </form>
    </section>
  );
}

function notificationTone(status: string): "active" | "warning" | "neutral" | "danger" {
  if (status === "published") return "active";
  if (status === "retrying") return "warning";
  if (status === "dead-letter") return "danger";
  return "neutral";
}

function InternalNotificationStatus(props: { readonly incident: ControlPlaneStage6Incident }) {
  const notifications = props.incident.internalNotifications ?? [];
  return (
    <section className="detail-panel">
      <h2>Employee notification intents</h2>
      <p>
        Durable Outbox state only; a published row is not proof that the internal Status Board or
        employee channel delivered the update.
      </p>
      {notifications.length === 0 ? (
        <EmptyState title="No notification intent recorded" />
      ) : (
        <div className="timeline-list">
          {notifications.map((notification) => (
            <article key={notification.id} className="timeline-item">
              <div>
                <StatusPill
                  value={`${notification.kind} · ${notification.status}`}
                  tone={notificationTone(notification.status)}
                />
                <time>{new Date(notification.availableAt).toLocaleString()}</time>
              </div>
              <p>
                Attempts: {notification.attempts}
                {notification.publishedAt
                  ? ` · published ${new Date(notification.publishedAt).toLocaleString()}`
                  : notification.deadLetteredAt
                    ? ` · dead-lettered ${new Date(notification.deadLetteredAt).toLocaleString()}`
                    : " · awaiting publisher acknowledgement"}
              </p>
            </article>
          ))}
        </div>
      )}
    </section>
  );
}

function InternalTimeline(props: { readonly incident: ControlPlaneStage6Incident }) {
  return (
    <section className="detail-panel">
      <h2>Internal communication timeline</h2>
      {props.incident.internalUpdates.length === 0 ? (
        <EmptyState title="No internal update recorded" />
      ) : (
        <div className="timeline-list">
          {props.incident.internalUpdates.map((update) => (
            <article key={update.id} className="timeline-item">
              <div>
                <StatusPill
                  value={update.kind}
                  tone={update.kind === "resolved" ? "active" : "neutral"}
                />
                <time>{new Date(update.publishedAt).toLocaleString()}</time>
              </div>
              <p>{update.summary}</p>
              <a href={update.evidenceReference} rel="noreferrer" target="_blank">
                Open immutable internal Status Board evidence
              </a>
            </article>
          ))}
        </div>
      )}
    </section>
  );
}

function ResolutionApprovalForm(props: {
  readonly incident: ControlPlaneStage6Incident;
  readonly onChanged: (incident: ControlPlaneStage6Incident) => void;
}) {
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  const approve = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformStage6IncidentResolutionApproval(props.incident.id, {
        decision,
        reason,
        evidenceReference,
        evidenceSha256: evidenceSHA256,
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Independent Security / Privacy resolution decision</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          approve.mutate();
        }}
      >
        <Field label="Decision">
          <Select
            value={decision}
            onChange={(event) => setDecision(event.target.value as typeof decision)}
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
        <Field label="Exact approval evidence SHA-256">
          <Input
            required
            value={evidenceSHA256}
            pattern="sha256:[0-9a-f]{64}"
            placeholder="sha256:…"
            onChange={(event) => setEvidenceSHA256(event.target.value)}
          />
        </Field>
        <Field label="Reason">
          <textarea
            className="control"
            required
            minLength={20}
            maxLength={2000}
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </Field>
        <div className="form-actions form-grid__wide">
          <Button type="submit" variant="primary" disabled={approve.isPending}>
            Record immutable decision
          </Button>
        </div>
        <InlineError error={approve.error} />
      </form>
    </section>
  );
}

function TransitionForm(props: {
  readonly incident: ControlPlaneStage6Incident;
  readonly onChanged: (incident: ControlPlaneStage6Incident) => void;
}) {
  const options = transitions[props.incident.state] ?? [];
  const [targetState, setTargetState] = useState(options[0] ?? "identified");
  const [reason, setReason] = useState("");
  const transition = useMutation({
    mutationFn: () =>
      controlPlaneClient.transitionPlatformStage6Incident(props.incident.id, {
        expectedVersion: props.incident.version,
        targetState,
        reason,
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Commander transition</h2>
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
            {options.map((value) => (
              <option key={value}>{value}</option>
            ))}
          </Select>
        </Field>
        <Field label="Reason">
          <Input
            required
            minLength={10}
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </Field>
        <div className="form-actions form-grid__wide">
          <Button
            type="submit"
            variant={targetState === "cancelled" ? "danger" : "primary"}
            disabled={transition.isPending}
          >
            Advance incident
          </Button>
        </div>
        <InlineError error={transition.error} />
      </form>
    </section>
  );
}
