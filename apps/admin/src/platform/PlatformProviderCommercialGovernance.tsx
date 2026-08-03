// FILE: PlatformProviderCommercialGovernance.tsx
// Purpose: Govern exact hosted Provider commercial permission and separated role decisions.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneProviderCommercialApprovalRole,
  type ControlPlaneProviderCommercialAuthorization,
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

const approvalRoles: ReadonlyArray<ControlPlaneProviderCommercialApprovalRole> = [
  "legal",
  "privacy",
  "security",
  "product",
];

function toISO(value: string): string {
  return new Date(value).toISOString();
}

export function PlatformProviderCommercialGovernance(props: { readonly canManage: boolean }) {
  const queryClient = useQueryClient();
  const authorizations = useQuery({
    queryKey: platformQueryKeys.providerCommercialAuthorizations,
    queryFn: controlPlaneClient.listPlatformProviderCommercialAuthorizations,
  });
  const [selectedId, setSelectedId] = useState("");
  useEffect(() => {
    if (!selectedId && authorizations.data?.items[0])
      setSelectedId(authorizations.data.items[0].id);
  }, [authorizations.data, selectedId]);
  const effectiveSelectedId = selectedId || authorizations.data?.items[0]?.id || "";
  const selected = useMemo(
    () => authorizations.data?.items.find((item) => item.id === effectiveSelectedId) ?? null,
    [authorizations.data, effectiveSelectedId],
  );
  const replace = (authorization: ControlPlaneProviderCommercialAuthorization) => {
    queryClient.setQueryData(
      platformQueryKeys.providerCommercialAuthorizations,
      (current: typeof authorizations.data) => ({
        items: current?.items.map((item) =>
          item.id === authorization.id ? authorization : item,
        ) ?? [authorization],
      }),
    );
  };

  if (authorizations.isPending)
    return <LoadingState label="Loading Provider commercial governance…" />;
  if (authorizations.error) return <InlineError error={authorizations.error} />;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Provider commercial use</h1>
          <p>
            Exact products, account types, contracting entities, credential scopes, Regions and
            data-use terms for hosted Provider execution. External contract authority remains a
            Legal gate.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <CreateAuthorization
          onCreated={(authorization) => {
            replace(authorization);
            setSelectedId(authorization.id);
          }}
        />
      ) : null}
      {authorizations.data.items.length === 0 ? (
        <EmptyState
          title="No hosted Provider authorization"
          description="Enterprise remote Provider claims remain blocked until an exact authorization is approved and active."
        />
      ) : (
        <section className="form-panel">
          <Field label="Authorization">
            <Select
              value={effectiveSelectedId}
              onChange={(event) => setSelectedId(event.target.value)}
            >
              {authorizations.data.items.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.authorizationKey} · {item.state.replaceAll("_", " ")}
                </option>
              ))}
            </Select>
          </Field>
        </section>
      )}
      {selected ? (
        <>
          <AuthorizationSummary authorization={selected} />
          {selected.state === "ready_for_review" ? (
            <ApprovalForm authorization={selected} onChanged={replace} />
          ) : null}
          {props.canManage && ["draft", "ready_for_review", "active"].includes(selected.state) ? (
            <TransitionForm authorization={selected} onChanged={replace} />
          ) : null}
        </>
      ) : null}
    </div>
  );
}

function CreateAuthorization(props: {
  readonly onCreated: (authorization: ControlPlaneProviderCommercialAuthorization) => void;
}) {
  const [authorizationKey, setAuthorizationKey] = useState("");
  const [provider, setProvider] = useState<"codex" | "claudeAgent">("codex");
  const [providerProduct, setProviderProduct] = useState("");
  const [accountType, setAccountType] = useState("");
  const [contractingEntity, setContractingEntity] = useState("");
  const [credentialMode, setCredentialMode] = useState<"customer_byok" | "platform_managed">(
    "customer_byok",
  );
  const [allowedCredentialScopes, setAllowedCredentialScopes] = useState("organization,tenant");
  const [allowedRegions, setAllowedRegions] = useState("");
  const [dataUsePolicy, setDataUsePolicy] = useState<"no_training" | "tenant_explicit_opt_in">(
    "no_training",
  );
  const [retentionPolicy, setRetentionPolicy] = useState("");
  const [termsEffectiveAt, setTermsEffectiveAt] = useState("");
  const [termsReference, setTermsReference] = useState("");
  const [termsSha256, setTermsSha256] = useState("");
  const [agreementReference, setAgreementReference] = useState("");
  const [agreementSha256, setAgreementSha256] = useState("");
  const [dpaReference, setDPAReference] = useState("");
  const [dpaSha256, setDPASHA256] = useState("");
  const [prohibitedUseSummary, setProhibitedUseSummary] = useState("");
  const [terminationRunbookReference, setTerminationRunbookReference] = useState("");
  const [terminationRunbookSha256, setTerminationRunbookSHA256] = useState("");
  const [reviewExpiresAt, setReviewExpiresAt] = useState("");
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createPlatformProviderCommercialAuthorization({
        authorizationKey,
        provider,
        providerProduct,
        accountType,
        contractingEntity,
        credentialMode,
        allowedCredentialScopes: allowedCredentialScopes
          .split(",")
          .map((value) => value.trim()) as Array<"user" | "organization" | "tenant" | "platform">,
        allowedRegions: allowedRegions.split(",").map((value) => value.trim()),
        dataUsePolicy,
        retentionPolicy,
        termsEffectiveAt: toISO(termsEffectiveAt),
        termsReference,
        termsSha256,
        agreementReference,
        agreementSha256,
        dpaReference,
        dpaSha256,
        prohibitedUseSummary,
        terminationRunbookReference,
        terminationRunbookSha256,
        reviewExpiresAt: toISO(reviewExpiresAt),
      }),
    onSuccess: props.onCreated,
  });
  return (
    <section className="form-panel">
      <h2>Create immutable commercial authorization</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Authorization key">
          <Input
            required
            value={authorizationKey}
            onChange={(event) => setAuthorizationKey(event.target.value)}
            placeholder="openai-api-2026"
          />
        </Field>
        <Field label="Provider">
          <Select
            value={provider}
            onChange={(event) => setProvider(event.target.value as typeof provider)}
          >
            <option value="codex">Codex / OpenAI</option>
            <option value="claudeAgent">Claude Agent / Anthropic</option>
          </Select>
        </Field>
        <Field label="Provider product">
          <Input
            required
            value={providerProduct}
            onChange={(event) => setProviderProduct(event.target.value)}
          />
        </Field>
        <Field label="Account type">
          <Input
            required
            value={accountType}
            onChange={(event) => setAccountType(event.target.value)}
          />
        </Field>
        <Field label="Contracting entity">
          <Input
            required
            value={contractingEntity}
            onChange={(event) => setContractingEntity(event.target.value)}
          />
        </Field>
        <Field label="Credential mode">
          <Select
            value={credentialMode}
            onChange={(event) => setCredentialMode(event.target.value as typeof credentialMode)}
          >
            <option value="customer_byok">Tenant BYOK</option>
            <option value="platform_managed">platform managed</option>
          </Select>
        </Field>
        <Field label="Credential scopes (comma separated)">
          <Input
            required
            value={allowedCredentialScopes}
            onChange={(event) => setAllowedCredentialScopes(event.target.value)}
          />
        </Field>
        <Field label="Regions (comma separated)">
          <Input
            required
            value={allowedRegions}
            onChange={(event) => setAllowedRegions(event.target.value)}
            placeholder="us-east-1,eu-west-1"
          />
        </Field>
        <Field label="Data use">
          <Select
            value={dataUsePolicy}
            onChange={(event) => setDataUsePolicy(event.target.value as typeof dataUsePolicy)}
          >
            <option value="no_training">no training</option>
            <option value="tenant_explicit_opt_in">Tenant explicit opt-in</option>
          </Select>
        </Field>
        <Field label="Terms effective at">
          <Input
            required
            type="datetime-local"
            value={termsEffectiveAt}
            onChange={(event) => setTermsEffectiveAt(event.target.value)}
          />
        </Field>
        <Field label="Review expires at (≤180 days)">
          <Input
            required
            type="datetime-local"
            value={reviewExpiresAt}
            onChange={(event) => setReviewExpiresAt(event.target.value)}
          />
        </Field>
        <Field label="Terms HTTPS reference">
          <Input
            required
            type="url"
            value={termsReference}
            onChange={(event) => setTermsReference(event.target.value)}
          />
        </Field>
        <Field label="Terms SHA-256">
          <Input
            required
            value={termsSha256}
            onChange={(event) => setTermsSha256(event.target.value)}
            placeholder="sha256:…"
          />
        </Field>
        <Field label="Agreement HTTPS reference">
          <Input
            required
            type="url"
            value={agreementReference}
            onChange={(event) => setAgreementReference(event.target.value)}
          />
        </Field>
        <Field label="Agreement SHA-256">
          <Input
            required
            value={agreementSha256}
            onChange={(event) => setAgreementSha256(event.target.value)}
            placeholder="sha256:…"
          />
        </Field>
        <Field label="DPA HTTPS reference">
          <Input
            required
            type="url"
            value={dpaReference}
            onChange={(event) => setDPAReference(event.target.value)}
          />
        </Field>
        <Field label="DPA SHA-256">
          <Input
            required
            value={dpaSha256}
            onChange={(event) => setDPASHA256(event.target.value)}
            placeholder="sha256:…"
          />
        </Field>
        <Field label="Termination runbook HTTPS reference">
          <Input
            required
            type="url"
            value={terminationRunbookReference}
            onChange={(event) => setTerminationRunbookReference(event.target.value)}
          />
        </Field>
        <Field label="Termination runbook SHA-256">
          <Input
            required
            value={terminationRunbookSha256}
            onChange={(event) => setTerminationRunbookSHA256(event.target.value)}
            placeholder="sha256:…"
          />
        </Field>
        <div className="form-grid__wide">
          <Field label="Retention policy">
            <textarea
              required
              minLength={3}
              value={retentionPolicy}
              onChange={(event) => setRetentionPolicy(event.target.value)}
            />
          </Field>
        </div>
        <div className="form-grid__wide">
          <Field label="Prohibited use">
            <textarea
              required
              minLength={20}
              value={prohibitedUseSummary}
              onChange={(event) => setProhibitedUseSummary(event.target.value)}
            />
          </Field>
        </div>
        <div className="boundary-callout form-grid__wide">
          Only Codex/OpenAI and Claude/Anthropic experimental adapters can be authorized. Local-only
          Providers remain blocked on remote Targets.
        </div>
        <div className="form-actions form-grid__wide">
          <Button disabled={create.isPending} type="submit" variant="primary">
            Create commercial authorization
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={create.error} />
        </div>
      </form>
    </section>
  );
}

function AuthorizationSummary(props: {
  readonly authorization: ControlPlaneProviderCommercialAuthorization;
}) {
  const item = props.authorization;
  const expired = item.state === "active" && new Date(item.reviewExpiresAt).getTime() <= Date.now();
  const displayState = expired ? "expired" : item.state;
  return (
    <section className="form-panel">
      <div className="form-actions">
        <h2>{item.authorizationKey}</h2>
        <StatusPill
          value={displayState}
          tone={
            displayState === "active"
              ? "active"
              : displayState === "rejected" ||
                  displayState === "revoked" ||
                  displayState === "expired"
                ? "danger"
                : "warning"
          }
        />
      </div>
      <dl className="summary-list">
        <div>
          <dt>Provider product</dt>
          <dd>
            {item.provider} · {item.providerProduct}
          </dd>
        </div>
        <div>
          <dt>Account</dt>
          <dd>{item.accountType}</dd>
        </div>
        <div>
          <dt>Contracting entity</dt>
          <dd>{item.contractingEntity}</dd>
        </div>
        <div>
          <dt>Credential boundary</dt>
          <dd>
            {item.credentialMode} · {item.allowedCredentialScopes.join(", ")}
          </dd>
        </div>
        <div>
          <dt>Regions</dt>
          <dd>{item.allowedRegions.join(", ")}</dd>
        </div>
        <div>
          <dt>Data use</dt>
          <dd>{item.dataUsePolicy}</dd>
        </div>
        <div>
          <dt>Review expires</dt>
          <dd>{new Date(item.reviewExpiresAt).toLocaleString()}</dd>
        </div>
        <div>
          <dt>Approvals</dt>
          <dd>{item.approvals.filter((approval) => approval.decision === "approved").length}/4</dd>
        </div>
        <div>
          <dt>Bound documents</dt>
          <dd>
            Terms {item.termsSha256 ?? "legacy-unbound"} · Agreement{" "}
            {item.agreementSha256 ?? "legacy-unbound"} · DPA {item.dpaSha256 ?? "legacy-unbound"} ·
            Termination {item.terminationRunbookSha256 ?? "legacy-unbound"}
          </dd>
        </div>
      </dl>
      {expired ? (
        <div className="boundary-callout">
          This authorization is expired. New Enterprise remote claims are blocked until a newly
          approved authorization becomes active.
        </div>
      ) : null}
      <div className="boundary-callout">
        Active enables only new Enterprise remote claims matching Provider, credential mode/scope
        and Region. SHA-256 binds the reviewed document bytes; external repository and counsel
        authority still require independent verification.
      </div>
      {item.approvals.map((approval) => (
        <p key={approval.id}>
          {approval.role}: <strong>{approval.decision}</strong> · {approval.reason} · evidence{" "}
          {approval.evidenceSha256 ?? "legacy-unbound"}
        </p>
      ))}
    </section>
  );
}

function ApprovalForm(props: {
  readonly authorization: ControlPlaneProviderCommercialAuthorization;
  readonly onChanged: (authorization: ControlPlaneProviderCommercialAuthorization) => void;
}) {
  const missing = approvalRoles.filter(
    (role) => !props.authorization.approvals.some((approval) => approval.role === role),
  );
  const [role, setRole] = useState<ControlPlaneProviderCommercialApprovalRole>(
    missing[0] ?? "legal",
  );
  const [decision, setDecision] = useState<"approved" | "rejected">("approved");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSha256, setEvidenceSha256] = useState("");
  const record = useMutation({
    mutationFn: () =>
      controlPlaneClient.recordPlatformProviderCommercialApproval(props.authorization.id, {
        role,
        decision,
        reason,
        evidenceReference,
        evidenceSha256,
      }),
    onSuccess: props.onChanged,
  });
  if (missing.length === 0) return null;
  return (
    <section className="form-panel">
      <h2>Record separated commercial decision</h2>
      <div className="boundary-callout">
        Requires the matching active `provider_commercial.*` Governance roles assignment; this role
        selector is not authority.
      </div>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          record.mutate();
        }}
      >
        <Field label="Role">
          <Select value={role} onChange={(event) => setRole(event.target.value as typeof role)}>
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
        <Field label="Evidence HTTPS reference">
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
            value={evidenceSha256}
            onChange={(event) => setEvidenceSha256(event.target.value)}
            placeholder="sha256:…"
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
            Record immutable commercial decision
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
  readonly authorization: ControlPlaneProviderCommercialAuthorization;
  readonly onChanged: (authorization: ControlPlaneProviderCommercialAuthorization) => void;
}) {
  const targetState =
    props.authorization.state === "draft"
      ? "ready_for_review"
      : props.authorization.state === "ready_for_review"
        ? "active"
        : "revoked";
  const [reason, setReason] = useState("");
  const transition = useMutation({
    mutationFn: () =>
      controlPlaneClient.transitionPlatformProviderCommercialAuthorization(props.authorization.id, {
        expectedVersion: props.authorization.version,
        targetState,
        reason,
      }),
    onSuccess: props.onChanged,
  });
  return (
    <section className="form-panel">
      <h2>Advance or revoke authorization</h2>
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
            disabled={transition.isPending}
            type="submit"
            variant={targetState === "revoked" ? "danger" : "primary"}
          >
            {targetState === "revoked" ? "Revoke authorization" : "Advance authorization"}
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={transition.error} />
        </div>
      </form>
    </section>
  );
}
