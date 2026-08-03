// FILE: PlatformGovernanceAuthorities.tsx
// Purpose: Assign explicit, time-bounded functional authority for Stage 6 governance decisions.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneGovernanceAuthorityGrant,
  type ControlPlaneGovernanceAuthorityKey,
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

function toISO(value: string): string {
  return new Date(value).toISOString();
}

function authorityLabel(value: string): string {
  return value.replaceAll("_", " ").replaceAll(".", " · ");
}

export function PlatformGovernanceAuthorities(props: {
  readonly canManage: boolean;
  readonly currentUserId: string;
}) {
  const queryClient = useQueryClient();
  const authorities = useQuery({
    queryKey: platformQueryKeys.governanceAuthorities,
    queryFn: controlPlaneClient.listPlatformGovernanceAuthorities,
  });
  const replace = (grant: ControlPlaneGovernanceAuthorityGrant) => {
    queryClient.setQueryData(
      platformQueryKeys.governanceAuthorities,
      (current: typeof authorities.data) => ({
        authorityKeys: current?.authorityKeys ?? [],
        operators: current?.operators ?? [],
        items: current?.items.map((item) => (item.id === grant.id ? grant : item)) ?? [grant],
      }),
    );
  };

  if (authorities.isPending) return <LoadingState label="Loading governance authorities…" />;
  if (authorities.error) return <InlineError error={authorities.error} />;

  const active = authorities.data.items.filter((item) => item.status === "active");
  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Governance authority</h1>
          <p>
            Functional roles are assigned by an Operator Tenant Owner and checked by the service and
            database before a Release, Compliance or Provider decision can be recorded.
          </p>
        </div>
      </header>
      {props.canManage ? (
        <CreateAuthority
          authorityKeys={authorities.data.authorityKeys}
          currentUserId={props.currentUserId}
          onCreated={(grant) => {
            queryClient.setQueryData(
              platformQueryKeys.governanceAuthorities,
              (current: typeof authorities.data) => ({
                authorityKeys: current?.authorityKeys ?? [],
                operators: current?.operators ?? [],
                items: [grant, ...(current?.items ?? [])],
              }),
            );
          }}
          operators={authorities.data.operators}
        />
      ) : (
        <div className="boundary-callout">
          Only an Operator Tenant Owner can grant or revoke functional authority.
        </div>
      )}
      {active.length === 0 ? (
        <EmptyState
          title="No active functional authorities"
          description="Governance decisions remain blocked until an Owner assigns exact, time-bounded authority."
        />
      ) : (
        <section className="form-panel">
          <h2>Active assignments</h2>
          <div className="page-stack">
            {active.map((grant) => (
              <AuthorityRow
                canManage={props.canManage}
                grant={grant}
                key={grant.id}
                onChanged={replace}
              />
            ))}
          </div>
        </section>
      )}
      {authorities.data.items.some((item) => item.status === "revoked") ? (
        <section className="form-panel">
          <h2>Revoked history</h2>
          {authorities.data.items
            .filter((item) => item.status === "revoked")
            .map((grant) => (
              <p key={grant.id}>
                {grant.userDisplayName} · {authorityLabel(grant.authorityKey)} ·{" "}
                {grant.revocationReason}
              </p>
            ))}
        </section>
      ) : null}
    </div>
  );
}

function CreateAuthority(props: {
  readonly authorityKeys: ReadonlyArray<ControlPlaneGovernanceAuthorityKey>;
  readonly currentUserId: string;
  readonly operators: ReadonlyArray<{
    userId: string;
    displayName: string;
    email: string;
    role: string;
  }>;
  readonly onCreated: (grant: ControlPlaneGovernanceAuthorityGrant) => void;
}) {
  const eligibleOperators = useMemo(
    () => props.operators.filter((operator) => operator.userId !== props.currentUserId),
    [props.currentUserId, props.operators],
  );
  const [userId, setUserId] = useState(eligibleOperators[0]?.userId ?? "");
  const [authorityKey, setAuthorityKey] = useState<ControlPlaneGovernanceAuthorityKey>(
    props.authorityKeys[0] ?? "release.engineering",
  );
  const [expiresAt, setExpiresAt] = useState("");
  const [reason, setReason] = useState("");
  const [evidenceReference, setEvidenceReference] = useState("");
  const [evidenceSHA256, setEvidenceSHA256] = useState("");
  useEffect(() => {
    if (!eligibleOperators.some((operator) => operator.userId === userId))
      setUserId(eligibleOperators[0]?.userId ?? "");
  }, [eligibleOperators, userId]);
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createPlatformGovernanceAuthority({
        userId,
        authorityKey,
        expiresAt: toISO(expiresAt),
        reason,
        evidenceReference,
        evidenceSha256: evidenceSHA256,
      }),
    onSuccess: props.onCreated,
  });
  return (
    <section className="form-panel">
      <h2>Assign exact functional authority</h2>
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Operator">
          <Select required value={userId} onChange={(event) => setUserId(event.target.value)}>
            {eligibleOperators.map((operator) => (
              <option key={operator.userId} value={operator.userId}>
                {operator.displayName} · {operator.email} · {operator.role}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Functional authority">
          <Select
            value={authorityKey}
            onChange={(event) =>
              setAuthorityKey(event.target.value as ControlPlaneGovernanceAuthorityKey)
            }
          >
            {props.authorityKeys.map((key) => (
              <option key={key} value={key}>
                {authorityLabel(key)}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Expires at (≤366 days)">
          <Input
            required
            type="datetime-local"
            value={expiresAt}
            onChange={(event) => setExpiresAt(event.target.value)}
          />
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
            value={evidenceSHA256}
            onChange={(event) => setEvidenceSHA256(event.target.value)}
            placeholder="sha256:…"
          />
        </Field>
        <div className="form-grid__wide">
          <Field label="Assignment reason">
            <textarea
              required
              minLength={10}
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
          </Field>
        </div>
        <div className="boundary-callout form-grid__wide">
          The Owner cannot assign authority to themselves. The reference and SHA-256 must identify
          the exact corporate delegation evidence bytes. Expiry, offboarding or revocation
          immediately blocks new decisions.
        </div>
        <div className="form-actions form-grid__wide">
          <Button disabled={create.isPending || !userId} type="submit" variant="primary">
            Assign authority
          </Button>
        </div>
        <div className="form-grid__wide">
          <InlineError error={create.error} />
        </div>
      </form>
    </section>
  );
}

function AuthorityRow(props: {
  readonly canManage: boolean;
  readonly grant: ControlPlaneGovernanceAuthorityGrant;
  readonly onChanged: (grant: ControlPlaneGovernanceAuthorityGrant) => void;
}) {
  const [reason, setReason] = useState("");
  const revoke = useMutation({
    mutationFn: () =>
      controlPlaneClient.revokePlatformGovernanceAuthority(props.grant.id, {
        expectedVersion: props.grant.version,
        reason,
      }),
    onSuccess: props.onChanged,
  });
  const expired = new Date(props.grant.expiresAt).getTime() <= Date.now();
  return (
    <div className="boundary-callout">
      <strong>
        {props.grant.userDisplayName} · {authorityLabel(props.grant.authorityKey)}
      </strong>
      <span>
        {props.grant.userEmail} · expires {new Date(props.grant.expiresAt).toLocaleString()}
      </span>
      <span>{props.grant.evidenceSha256 ?? "legacy-unbound"}</span>
      <StatusPill value={expired ? "expired" : "active"} tone={expired ? "danger" : "active"} />
      {props.canManage ? (
        <form
          className="form-grid"
          onSubmit={(event) => {
            event.preventDefault();
            revoke.mutate();
          }}
        >
          <div className="form-grid__wide">
            <Field label="Revocation reason">
              <textarea
                required
                minLength={10}
                value={reason}
                onChange={(event) => setReason(event.target.value)}
              />
            </Field>
          </div>
          <div className="form-actions form-grid__wide">
            <Button disabled={revoke.isPending} type="submit" variant="danger">
              Revoke authority
            </Button>
          </div>
          <div className="form-grid__wide">
            <InlineError error={revoke.error} />
          </div>
        </form>
      ) : null}
    </div>
  );
}
