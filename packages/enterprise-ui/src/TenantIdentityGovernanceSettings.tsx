// FILE: TenantIdentityGovernanceSettings.tsx
// Purpose: Manage verified Tenant domains and versioned SSO enforcement without database access.
// Layer: Settings UI component
// Exports: TenantIdentityGovernanceSettings, identityDomainsQueryKey, identityPolicyQueryKey

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import {
  controlPlaneClient,
  type ControlPlaneIdentityDomain,
  type ControlPlaneIdentityDomainChallenge,
  type ControlPlaneTenantMember,
} from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

export function identityDomainsQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "identity-domains"] as const;
}

export function identityPolicyQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "identity-policy"] as const;
}

function formatTimestamp(value: string | null): string {
  if (!value) return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

export function TenantIdentityGovernanceSettings(props: {
  tenantId: string;
  members: ReadonlyArray<ControlPlaneTenantMember>;
  canManage: boolean;
}) {
  const {
    Button,
    InlineError,
    Input,
    SettingsListRow,
    SettingsRow,
    StatusPill,
    confirmAction,
    nativeSelectClassName,
  } = useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const domains = useQuery({
    queryKey: identityDomainsQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listIdentityDomains(props.tenantId),
    retry: false,
  });
  const policy = useQuery({
    queryKey: identityPolicyQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.getTenantIdentityPolicy(props.tenantId),
    retry: false,
  });
  const [domain, setDomain] = useState("");
  const [challenge, setChallenge] = useState<ControlPlaneIdentityDomainChallenge | null>(null);
  const createDomain = useMutation({
    mutationFn: () => controlPlaneClient.createIdentityDomain(props.tenantId, domain.trim()),
    onSuccess: async (created) => {
      setDomain("");
      setChallenge(created);
      await queryClient.invalidateQueries({ queryKey: identityDomainsQueryKey(props.tenantId) });
    },
  });
  const verifyDomain = useMutation({
    mutationFn: (item: ControlPlaneIdentityDomain) =>
      controlPlaneClient.verifyIdentityDomain(props.tenantId, item.id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: identityDomainsQueryKey(props.tenantId) });
    },
  });
  const revokeDomain = useMutation({
    mutationFn: (item: ControlPlaneIdentityDomain) =>
      controlPlaneClient.revokeIdentityDomain(props.tenantId, item.id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: identityDomainsQueryKey(props.tenantId) });
    },
  });

  return (
    <>
      <SettingsRow
        title="Domain verification"
        description="Prove control with the one-time DNS TXT challenge before enabling required SSO. Active domain claims are globally unique and every verify/revoke action is audited."
      >
        {props.canManage ? (
          <form
            className="mt-3 flex flex-wrap gap-2"
            onSubmit={(event) => {
              event.preventDefault();
              createDomain.mutate();
            }}
          >
            <Input
              aria-label="Tenant domain"
              autoCapitalize="none"
              className="min-w-56 flex-1"
              placeholder="example.com"
              required
              value={domain}
              onChange={(event) => setDomain(event.target.value.toLowerCase())}
            />
            <Button disabled={createDomain.isPending} size="sm" type="submit">
              {createDomain.isPending ? "Creating challenge…" : "Create DNS challenge"}
            </Button>
          </form>
        ) : null}
        {challenge ? (
          <div className="mt-3 rounded-lg border border-border bg-muted/30 p-3 text-xs">
            <p className="font-medium text-foreground">Publish this DNS TXT record</p>
            <code className="mt-1 block break-all text-muted-foreground">
              {challenge.verificationRecordName} = {challenge.verificationRecordValue}
            </code>
            <p className="mt-1 text-muted-foreground">
              Expires {formatTimestamp(challenge.verificationExpiresAt)}. Clear this value after
              copying; Synara persists only its hash.
            </p>
            <Button className="mt-2" size="xs" variant="outline" onClick={() => setChallenge(null)}>
              Clear challenge
            </Button>
          </div>
        ) : null}
        <InlineError
          error={createDomain.error ?? verifyDomain.error ?? revokeDomain.error ?? domains.error}
        />
      </SettingsRow>
      {domains.isPending ? <SettingsListRow title="Loading verified domains…" /> : null}
      {domains.data?.items.map((item) => (
        <SettingsListRow
          key={item.id}
          title={item.domain}
          description={
            item.status === "pending"
              ? `${item.verificationRecordName} · challenge expires ${formatTimestamp(item.verificationExpiresAt)}`
              : item.status === "verified"
                ? `verified ${formatTimestamp(item.verifiedAt)}`
                : `revoked ${formatTimestamp(item.revokedAt)}`
          }
          actions={
            <div className="flex flex-wrap items-center justify-end gap-1.5">
              <StatusPill value={item.status} active={item.status === "verified"} />
              {props.canManage && item.status === "pending" ? (
                <Button
                  disabled={verifyDomain.isPending}
                  size="xs"
                  variant="outline"
                  onClick={() => verifyDomain.mutate(item)}
                >
                  Verify DNS
                </Button>
              ) : null}
              {props.canManage && item.status !== "revoked" ? (
                <Button
                  disabled={revokeDomain.isPending}
                  size="xs"
                  variant="outline"
                  onClick={() => {
                    if (confirmAction(`Revoke the ${item.domain} Tenant domain claim?`)) {
                      revokeDomain.mutate(item);
                    }
                  }}
                >
                  Revoke
                </Button>
              ) : null}
            </div>
          }
        />
      ))}
      {domains.data?.items.length === 0 ? (
        <SettingsListRow
          title="No Tenant domains"
          description="Create and verify a DNS claim before requiring SSO."
        />
      ) : null}
      <SSOEnforcementSettings
        canManage={props.canManage}
        members={props.members}
        policy={policy.data ?? null}
        policyError={policy.error}
        tenantId={props.tenantId}
      />
    </>
  );
}

function SSOEnforcementSettings(props: {
  tenantId: string;
  members: ReadonlyArray<ControlPlaneTenantMember>;
  canManage: boolean;
  policy: Awaited<ReturnType<typeof controlPlaneClient.getTenantIdentityPolicy>> | null;
  policyError: unknown;
}) {
  const { Button, InlineError, SettingsRow, StatusPill, nativeSelectClassName } =
    useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const [modeOverride, setModeOverride] = useState<"optional" | "required" | null>(null);
  const recoveryOwners = props.members.filter(
    (member) => member.role === "owner" && member.status === "active",
  );
  const [recoveryUserIdOverride, setRecoveryUserIdOverride] = useState<string | null>(null);
  const effectiveMode = modeOverride ?? props.policy?.ssoEnforcement ?? "optional";
  const recoveryUserId =
    recoveryUserIdOverride ?? props.policy?.recoveryUserId ?? recoveryOwners[0]?.userId ?? "";
  const update = useMutation({
    mutationFn: async () => {
      if (!props.policy) throw new Error("Load the SSO enforcement policy before changing it.");
      if (effectiveMode === "required" && !recoveryUserId) {
        throw new Error("Required SSO needs an active recovery Owner.");
      }
      return controlPlaneClient.updateTenantIdentityPolicy(props.tenantId, {
        ssoEnforcement: effectiveMode,
        recoveryUserId: effectiveMode === "required" ? recoveryUserId : null,
        expectedVersion: props.policy.version,
      });
    },
    onSuccess: (next) => {
      setModeOverride(null);
      setRecoveryUserIdOverride(null);
      queryClient.setQueryData(identityPolicyQueryKey(props.tenantId), next);
    },
  });

  return (
    <SettingsRow
      title="SSO enforcement"
      description="Required mode revokes non-SSO Sessions and needs a verified domain, an active Identity Connection, and an active recovery Owner. The server rechecks every prerequisite and rejects lockout."
    >
      {props.policy ? (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <StatusPill
            value={props.policy.ssoEnforcement}
            active={props.policy.ssoEnforcement === "required"}
          />
          <span className="text-[10px] text-muted-foreground">version {props.policy.version}</span>
        </div>
      ) : null}
      {props.canManage ? (
        <form
          className="mt-3 grid gap-2 sm:grid-cols-[minmax(0,10rem)_minmax(0,1fr)_auto]"
          onSubmit={(event) => {
            event.preventDefault();
            update.mutate();
          }}
        >
          <select
            aria-label="SSO enforcement mode"
            className={nativeSelectClassName}
            value={effectiveMode}
            onChange={(event) => setModeOverride(event.target.value as "optional" | "required")}
          >
            <option value="optional">Optional</option>
            <option value="required">Required</option>
          </select>
          <select
            aria-label="SSO recovery Owner"
            className={nativeSelectClassName}
            disabled={effectiveMode !== "required"}
            required={effectiveMode === "required"}
            value={effectiveMode === "required" ? recoveryUserId : ""}
            onChange={(event) => setRecoveryUserIdOverride(event.target.value)}
          >
            <option value="">Select an active recovery Owner</option>
            {recoveryOwners.map((member) => (
              <option key={member.userId} value={member.userId}>
                {member.displayName || member.email}
              </option>
            ))}
          </select>
          <Button
            disabled={
              !props.policy ||
              update.isPending ||
              (effectiveMode === "required" && recoveryUserId === "")
            }
            size="sm"
            type="submit"
          >
            {update.isPending ? "Saving…" : "Save SSO enforcement"}
          </Button>
        </form>
      ) : null}
      <InlineError error={update.error ?? props.policyError} />
    </SettingsRow>
  );
}
