// FILE: PlatformSupportAccess.tsx
// Purpose: Run the four-eyes, time-bounded Support Access workflow from Platform authority.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlanePlatformTenantOverview,
  type ControlPlaneSessionState,
  type ControlPlaneSupportAccessGrant,
} from "@synara/control-plane-client";
import { useEffect, useState } from "react";

import {
  Button,
  EmptyState,
  Field,
  InlineError,
  Input,
  Select,
  StatusPill,
} from "../components/ui";
import { navigateToTenantApp, type AdminDestination } from "./navigation";
import { canManagePlatform, platformQueryKeys } from "./platformQueries";

function grantTone(status: ControlPlaneSupportAccessGrant["status"]) {
  if (status === "active") return "active" as const;
  if (status === "pending") return "warning" as const;
  if (status === "denied" || status === "revoked") return "danger" as const;
  return "neutral" as const;
}

export function PlatformSupportAccess(props: {
  readonly overview: ControlPlanePlatformTenantOverview;
  readonly session: ControlPlaneSessionState;
  readonly onNavigate: (destination: AdminDestination) => void;
}) {
  const queryClient = useQueryClient();
  const canDecide = canManagePlatform(props.overview.operatorRole);
  const grants = useQuery({
    queryKey: platformQueryKeys.supportAccess,
    queryFn: controlPlaneClient.listPlatformSupportAccess,
    refetchInterval: 30_000,
  });
  const [tenantId, setTenantId] = useState(props.overview.items[0]?.id ?? "");
  const [durationSeconds, setDurationSeconds] = useState(30 * 60);
  const [requestReason, setRequestReason] = useState("");
  const [grantId, setGrantId] = useState("");
  const [decisionReason, setDecisionReason] = useState("");

  const actionable =
    grants.data?.items.filter((grant) => grant.status === "pending" || grant.status === "active") ??
    [];
  useEffect(() => {
    if (grantId.length === 0 && actionable[0]) setGrantId(actionable[0].id);
  }, [actionable, grantId]);

  const request = useMutation({
    mutationFn: () =>
      controlPlaneClient.requestPlatformSupportAccess({
        tenantId,
        reason: requestReason,
        requestedDurationSeconds: durationSeconds,
      }),
    onSuccess: async () => {
      setRequestReason("");
      await queryClient.invalidateQueries({ queryKey: platformQueryKeys.supportAccess });
    },
  });
  const decision = useMutation({
    mutationFn: async (action: "approve" | "deny" | "revoke") => {
      const grant = actionable.find((item) => item.id === grantId);
      if (!grant) throw new Error("Select a Support Access request.");
      const input = { expectedVersion: grant.version, reason: decisionReason };
      if (action === "approve") {
        return controlPlaneClient.approvePlatformSupportAccess(grant.id, input);
      }
      if (action === "deny") {
        return controlPlaneClient.denyPlatformSupportAccess(grant.id, input);
      }
      return controlPlaneClient.revokePlatformSupportAccess(grant.id, input);
    },
    onSuccess: async () => {
      setDecisionReason("");
      await queryClient.invalidateQueries({ queryKey: platformQueryKeys.supportAccess });
    },
  });
  const switchTenant = useMutation({
    mutationFn: (targetTenantId: string) => controlPlaneClient.setActiveTenant(targetTenantId),
    onSuccess: (session) => {
      queryClient.setQueryData(["control-plane", "admin", "session"], session);
      void navigateToTenantApp();
    },
  });
  const selected = actionable.find((grant) => grant.id === grantId) ?? null;
  const selfRequested = selected?.requesterUserId === props.session.user.userId;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <p className="page-heading__back">
            <button onClick={() => props.onNavigate("tenants")}>Tenant operations</button> / Support
            Access
          </p>
          <h1>Platform Support Access</h1>
          <p>
            Request one internal Tenant and a bounded duration; a different Platform Admin approves.
          </p>
        </div>
      </header>

      <section className="form-panel" aria-labelledby="support-request-heading">
        <header className="section-heading">
          <div>
            <h2 id="support-request-heading">Request Support Access</h2>
            <p>
              The reason is visible to the Tenant administrator. Never include secrets or Tenant
              content.
            </p>
          </div>
        </header>
        {props.overview.items.length === 0 ? (
          <EmptyState title="No internal Tenants" />
        ) : (
          <form
            className="form-grid"
            onSubmit={(event) => {
              event.preventDefault();
              request.mutate();
            }}
          >
            <Field label="Internal Tenant">
              <Select onChange={(event) => setTenantId(event.target.value)} value={tenantId}>
                {props.overview.items.map((tenant) => (
                  <option key={tenant.id} value={tenant.id}>
                    {tenant.name} · {tenant.status}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Duration">
              <Select
                onChange={(event) => setDurationSeconds(Number(event.target.value))}
                value={durationSeconds}
              >
                <option value={30 * 60}>30 minutes</option>
                <option value={60 * 60}>1 hour</option>
                <option value={2 * 60 * 60}>2 hours</option>
                <option value={4 * 60 * 60}>4 hours</option>
              </Select>
            </Field>
            <Field label="Incident or support reason">
              <Input
                maxLength={1000}
                minLength={10}
                onChange={(event) => setRequestReason(event.target.value)}
                required
                value={requestReason}
              />
            </Field>
            <div className="form-actions form-grid__wide">
              <Button disabled={request.isPending} type="submit" variant="primary">
                {request.isPending ? "Requesting…" : "Submit for approval"}
              </Button>
            </div>
            <div className="form-grid__wide">
              <InlineError error={request.error} />
            </div>
          </form>
        )}
      </section>

      <section className="support-list" aria-labelledby="support-requests-heading">
        <header className="section-heading">
          <div>
            <h2 id="support-requests-heading">Requests and grants</h2>
            <p>
              Entering a grant changes the session to an audited support_readonly Tenant context.
            </p>
          </div>
        </header>
        {grants.isPending ? (
          <p className="loading-state">Loading requests…</p>
        ) : grants.data?.items.length ? (
          <div className="request-rows">
            {grants.data.items.map((grant) => (
              <article className="request-row" key={grant.id}>
                <div>
                  <strong>
                    {grant.requesterDisplayName || grant.requesterEmail} → {grant.tenantName}
                  </strong>
                  <p>{grant.reason}</p>
                  <small>
                    {Math.round(grant.requestedDurationSeconds / 60)} minutes · requested{" "}
                    {new Date(grant.requestedAt).toLocaleString()}
                  </small>
                </div>
                <div className="request-row__actions">
                  <StatusPill value={grant.status} tone={grantTone(grant.status)} />
                  {grant.status === "active" &&
                  grant.requesterUserId === props.session.user.userId ? (
                    <Button
                      disabled={switchTenant.isPending}
                      size="sm"
                      onClick={() => switchTenant.mutate(grant.tenantId)}
                    >
                      Enter read-only view
                    </Button>
                  ) : null}
                </div>
              </article>
            ))}
          </div>
        ) : (
          <EmptyState title="No Support Access requests" />
        )}
        <InlineError error={grants.error ?? switchTenant.error} />
      </section>

      {canDecide && actionable.length > 0 ? (
        <section className="form-panel" aria-labelledby="support-decision-heading">
          <header className="section-heading">
            <div>
              <h2 id="support-decision-heading">Decide or revoke Support Access</h2>
              <p>Every decision requires the current grant version and an auditable reason.</p>
            </div>
          </header>
          <form className="form-grid" onSubmit={(event) => event.preventDefault()}>
            <Field label="Request">
              <Select onChange={(event) => setGrantId(event.target.value)} value={grantId}>
                {actionable.map((grant) => (
                  <option key={grant.id} value={grant.id}>
                    {grant.requesterDisplayName || grant.requesterEmail} → {grant.tenantName} ·{" "}
                    {grant.status}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Decision reason">
              <Input
                maxLength={1000}
                minLength={10}
                onChange={(event) => setDecisionReason(event.target.value)}
                required
                value={decisionReason}
              />
            </Field>
            {selfRequested && selected?.status === "pending" ? (
              <div className="boundary-callout form-grid__wide">
                Four-eyes policy blocks the requester from approving their own request.
              </div>
            ) : null}
            <div className="form-actions form-grid__wide">
              {selected?.status === "pending" ? (
                <>
                  <Button
                    disabled={
                      decision.isPending || selfRequested || decisionReason.trim().length < 10
                    }
                    onClick={() => decision.mutate("approve")}
                    variant="primary"
                  >
                    Approve
                  </Button>
                  <Button
                    disabled={decision.isPending || decisionReason.trim().length < 10}
                    onClick={() => decision.mutate("deny")}
                  >
                    Deny
                  </Button>
                </>
              ) : (
                <Button
                  disabled={decision.isPending || decisionReason.trim().length < 10}
                  onClick={() => decision.mutate("revoke")}
                  variant="danger"
                >
                  Revoke
                </Button>
              )}
            </div>
            <div className="form-grid__wide">
              <InlineError error={decision.error} />
            </div>
          </form>
        </section>
      ) : null}
    </div>
  );
}
