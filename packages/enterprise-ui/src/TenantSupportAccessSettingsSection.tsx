// FILE: TenantSupportAccessSettingsSection.tsx
// Purpose: Let Tenant administrators control and audit time-bounded Support Access.
// Layer: Settings UI component
// Exports: TenantSupportAccessSettingsSection

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  controlPlaneClient,
  type ControlPlaneSupportAccessGrant,
  type ControlPlaneTenantSupportPolicy,
} from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

export function supportPolicyQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "support-policy"] as const;
}

export function supportGrantsQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "support-access"] as const;
}

function formatSupportExpiry(value: string | null): string {
  if (!value) return "No expiry";
  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  }).format(new Date(value));
}

export function TenantSupportAccessSettingsSection(props: {
  tenantId: string;
  canManage: boolean;
  canReadQuota: boolean;
}) {
  const { Button, InlineError, SettingsListRow, SettingsSection, StatusPill, downloadJsonFile } =
    useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const policy = useQuery({
    queryKey: supportPolicyQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.getTenantSupportPolicy(props.tenantId),
    retry: false,
  });
  const grants = useQuery({
    queryKey: supportGrantsQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listTenantSupportAccess(props.tenantId),
    retry: false,
    refetchInterval: 30_000,
  });
  const exportDiagnostic = useMutation({
    mutationFn: () => controlPlaneClient.getTenantSupportDiagnosticExport(props.tenantId),
    onSuccess: (result) => {
      downloadJsonFile(
        `synara-support-diagnostic-${props.tenantId}-${result.diagnostic.generatedAt.slice(0, 10)}.json`,
        result.body,
      );
    },
  });

  if (policy.isPending || grants.isPending) {
    return (
      <SettingsSection title="Support Access">
        <SettingsListRow title="Loading Support Access…" />
      </SettingsSection>
    );
  }
  if (policy.error || grants.error) {
    const error = policy.error ?? grants.error;
    return (
      <SettingsSection title="Support Access">
        <SettingsListRow
          title="Could not load Support Access"
          description={error instanceof Error ? error.message : "The request failed."}
          actions={
            <Button
              size="sm"
              variant="outline"
              onClick={() => void Promise.all([policy.refetch(), grants.refetch()])}
            >
              Retry
            </Button>
          }
        />
      </SettingsSection>
    );
  }

  const policyData = policy.data;
  const grantItems = grants.data.items;
  const activeGrants = grantItems.filter((grant) => grant.status === "active");
  return (
    <SettingsSection title="Support Access">
      <SettingsListRow
        title={
          policyData.supportAccessEnabled ? "Controlled access enabled" : "Support access disabled"
        }
        description="Support engineers need a reason, a separate Platform Admin approval, and an unexpired grant. Every request is read-only and audited for this Tenant."
        actions={
          <StatusPill
            value={policyData.supportAccessEnabled ? "enabled" : "disabled"}
            active={policyData.supportAccessEnabled}
          />
        }
      />
      {props.canReadQuota ? (
        <>
          <SettingsListRow
            title="Support diagnostic snapshot"
            description="Download a redacted, read-only snapshot of Tenant health, execution aggregates, Token usage, and internal cost coverage for troubleshooting. Secrets and payloads are excluded."
            actions={
              <Button
                disabled={exportDiagnostic.isPending}
                onClick={() => exportDiagnostic.mutate()}
                size="xs"
                variant="outline"
              >
                {exportDiagnostic.isPending ? "Preparing…" : "Download JSON"}
              </Button>
            }
          />
          <InlineError error={exportDiagnostic.error} />
        </>
      ) : null}
      {props.canManage ? (
        <SupportPolicyForm
          key={`${policyData.tenantId}:${policyData.version}`}
          policy={policyData}
          onSaved={(updated) =>
            queryClient.setQueryData(supportPolicyQueryKey(props.tenantId), updated)
          }
        />
      ) : null}
      {grantItems.length === 0 ? (
        <SettingsListRow
          title="No Support Access history"
          description="Requests and every time-bounded access window will appear here."
        />
      ) : (
        grantItems.map((grant) => (
          <SettingsListRow
            key={grant.id}
            title={grant.requesterDisplayName || grant.requesterEmail}
            description={`${grant.reason} · ${formatSupportExpiry(grant.expiresAt)}`}
            actions={<StatusPill value={grant.status} active={grant.status === "active"} />}
          />
        ))
      )}
      {props.canManage && activeGrants.length > 0 ? (
        <RevokeSupportGrantForm
          tenantId={props.tenantId}
          grants={activeGrants}
          onSaved={() =>
            void queryClient.invalidateQueries({ queryKey: supportGrantsQueryKey(props.tenantId) })
          }
        />
      ) : null}
    </SettingsSection>
  );
}

function SupportPolicyForm(props: {
  policy: ControlPlaneTenantSupportPolicy;
  onSaved: (policy: ControlPlaneTenantSupportPolicy) => void;
}) {
  const { Button, InlineError, Input, SettingsRow, Switch } = useEnterpriseUiHost();
  const [enabled, setEnabled] = useState(props.policy.supportAccessEnabled);
  const [reason, setReason] = useState("");
  const update = useMutation({
    mutationFn: () =>
      controlPlaneClient.updateTenantSupportPolicy(props.policy.tenantId, {
        supportAccessEnabled: enabled,
        version: props.policy.version,
        reason,
      }),
    onSuccess: props.onSaved,
  });

  return (
    <SettingsRow
      title="Tenant Support Access policy"
      description="Disabling the policy immediately revokes every active grant. Enter a reason for the Tenant audit trail."
      control={
        <Switch
          checked={enabled}
          onCheckedChange={(checked) => setEnabled(Boolean(checked))}
          aria-label="Allow controlled Support Access"
        />
      }
    >
      <form
        className="mt-3 flex flex-col gap-2 sm:flex-row"
        onSubmit={(event) => {
          event.preventDefault();
          update.mutate();
        }}
      >
        <Input
          minLength={10}
          maxLength={1000}
          required
          placeholder="Reason for changing Support Access"
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
        <Button disabled={update.isPending} size="sm" type="submit">
          {update.isPending ? "Saving…" : "Save policy"}
        </Button>
      </form>
      <InlineError error={update.error} />
    </SettingsRow>
  );
}

function RevokeSupportGrantForm(props: {
  tenantId: string;
  grants: ReadonlyArray<ControlPlaneSupportAccessGrant>;
  onSaved: () => void;
}) {
  const { Button, InlineError, Input, SettingsRow, nativeSelectClassName } = useEnterpriseUiHost();
  const [grantId, setGrantId] = useState(props.grants[0]?.id ?? "");
  const [reason, setReason] = useState("");
  const revoke = useMutation({
    mutationFn: () => {
      const grant = props.grants.find((item) => item.id === grantId);
      if (!grant) throw new Error("Select an active Support Access grant.");
      return controlPlaneClient.revokeTenantSupportAccess(props.tenantId, grant.id, {
        expectedVersion: grant.version,
        reason,
      });
    },
    onSuccess: props.onSaved,
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    revoke.mutate();
  };

  return (
    <SettingsRow
      title="Revoke active Support Access"
      description="Revocation takes effect on the next request and clears the support engineer's active Tenant context."
    >
      <form
        className="mt-3 grid gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)_auto]"
        onSubmit={submit}
      >
        <select
          aria-label="Active Support Access grant"
          className={nativeSelectClassName}
          value={grantId}
          onChange={(event) => setGrantId(event.target.value)}
        >
          {props.grants.map((grant) => (
            <option key={grant.id} value={grant.id}>
              {grant.requesterDisplayName || grant.requesterEmail}
            </option>
          ))}
        </select>
        <Input
          minLength={10}
          maxLength={1000}
          required
          placeholder="Reason for revocation"
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
        <Button disabled={revoke.isPending} size="sm" type="submit" variant="destructive">
          {revoke.isPending ? "Revoking…" : "Revoke"}
        </Button>
      </form>
      <InlineError error={revoke.error} />
    </SettingsRow>
  );
}
