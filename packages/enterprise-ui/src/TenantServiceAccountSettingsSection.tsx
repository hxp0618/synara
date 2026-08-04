import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { controlPlaneClient, type ControlPlaneServiceAccount } from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

const SCOPES = [
  ["api.access", "Call public-beta Polaris resource APIs"],
  ["scim.read", "Read SCIM users and groups"],
  ["scim.write", "Provision SCIM users and groups"],
  ["identity.read", "Read identity configuration"],
  ["identity.manage", "Manage identity configuration"],
] as const;

export function serviceAccountQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "service-accounts"] as const;
}

export function serviceAccountUsageQueryKey(tenantId: string, serviceAccountId: string) {
  return [
    "control-plane",
    "tenants",
    tenantId,
    "service-accounts",
    serviceAccountId,
    "usage",
  ] as const;
}

export function TenantServiceAccountSettingsSection(props: {
  tenantId: string;
  canManage: boolean;
}) {
  const { Button, InlineError, SettingsListRow, SettingsRow, SettingsSection } =
    useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const accounts = useQuery({
    queryKey: serviceAccountQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listServiceAccounts(props.tenantId),
    retry: false,
  });
  const [issuedToken, setIssuedToken] = useState("");
  return (
    <SettingsSection title="Service Accounts and API Keys">
      <SettingsRow
        title="Machine credentials"
        description="Service Accounts are fixed-role machine identities for the Polaris API, SCIM, and identity automation. Tokens are shown once, stored only as SHA-256 hashes, and can be rotated or revoked without impersonating a User."
      >
        {props.canManage ? (
          <ServiceAccountForm
            tenantId={props.tenantId}
            onCreated={(account, token) => {
              queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneServiceAccount> }>(
                serviceAccountQueryKey(props.tenantId),
                (current) => ({ items: [...(current?.items ?? []), account] }),
              );
              setIssuedToken(token);
            }}
          />
        ) : null}
      </SettingsRow>
      {issuedToken ? (
        <SettingsRow
          title="Copy the new token now"
          description="Synara cannot display this token again. Store it in the identity provider or secret manager, then clear it from this screen."
          control={
            <Button size="sm" variant="outline" onClick={() => setIssuedToken("")}>
              Clear token
            </Button>
          }
        >
          <code className="block break-all rounded-md border border-border bg-muted/40 p-3 text-xs text-foreground">
            {issuedToken}
          </code>
        </SettingsRow>
      ) : null}
      {accounts.isPending ? <SettingsListRow title="Loading Service Accounts…" /> : null}
      {accounts.data?.items.map((account) => (
        <ServiceAccountRow
          key={account.id}
          account={account}
          canManage={props.canManage}
          tenantId={props.tenantId}
          onToken={setIssuedToken}
        />
      ))}
      {accounts.data?.items.length === 0 ? (
        <SettingsListRow
          title="No Service Accounts"
          description="Create an API Key or add SCIM scopes before connecting a directory provider."
        />
      ) : null}
      <InlineError error={accounts.error} />
    </SettingsSection>
  );
}

function ServiceAccountForm(props: {
  tenantId: string;
  onCreated: (account: ControlPlaneServiceAccount, token: string) => void;
}) {
  const { Button, FormField, InlineError, Input, formGridClassName, nativeSelectClassName } =
    useEnterpriseUiHost();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [scopes, setScopes] = useState<ReadonlyArray<string>>(["scim.read", "scim.write"]);
  const [role, setRole] = useState<ControlPlaneServiceAccount["role"]>("member");
  const [rateLimitPerMinute, setRateLimitPerMinute] = useState(600);
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createServiceAccount(props.tenantId, {
        name,
        description,
        role,
        scopes,
        rateLimitPerMinute,
      }),
    onSuccess: (issued) => {
      props.onCreated(issued.account, issued.token);
      setName("");
      setDescription("");
    },
  });
  return (
    <form
      data-testid="service-account-form"
      className={formGridClassName}
      onSubmit={(event: FormEvent) => {
        event.preventDefault();
        create.mutate();
      }}
    >
      <FormField label="Name">
        <Input
          data-testid="service-account-name"
          required
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
      </FormField>
      <FormField label="Description">
        <Input
          data-testid="service-account-description"
          value={description}
          onChange={(event) => setDescription(event.target.value)}
        />
      </FormField>
      <FormField label="Fixed Tenant role">
        <select
          data-testid="service-account-role"
          className={nativeSelectClassName}
          value={role}
          onChange={(event) => setRole(event.target.value as ControlPlaneServiceAccount["role"])}
        >
          <option value="member">Member</option>
          <option value="auditor">Auditor</option>
          <option value="cost_admin">Cost admin</option>
          <option value="security_admin">Security admin</option>
          <option value="admin">Admin</option>
          <option value="owner">Owner</option>
        </select>
      </FormField>
      <FormField label="Requests per minute">
        <Input
          data-testid="service-account-rate-limit"
          type="number"
          min={1}
          max={60000}
          required
          value={rateLimitPerMinute}
          onChange={(event) => setRateLimitPerMinute(event.target.valueAsNumber)}
        />
      </FormField>
      <div className="space-y-2 sm:col-span-2">
        {SCOPES.map(([scope, label]) => (
          <label key={scope} className="flex items-center gap-2 text-xs text-muted-foreground">
            <input
              checked={scopes.includes(scope)}
              type="checkbox"
              onChange={(event) =>
                setScopes((current) =>
                  event.target.checked
                    ? [...current, scope]
                    : current.filter((item) => item !== scope),
                )
              }
            />
            <span>
              <span className="font-medium text-foreground">{scope}</span> · {label}
            </span>
          </label>
        ))}
      </div>
      <div className="sm:col-span-2">
        <Button
          data-testid="service-account-submit"
          disabled={
            create.isPending ||
            scopes.length === 0 ||
            !Number.isSafeInteger(rateLimitPerMinute) ||
            rateLimitPerMinute < 1 ||
            rateLimitPerMinute > 60000
          }
          size="sm"
          type="submit"
        >
          {create.isPending ? "Creating Service Account…" : "Create Service Account"}
        </Button>
        <InlineError error={create.error} />
      </div>
    </form>
  );
}

function ServiceAccountRow(props: {
  tenantId: string;
  account: ControlPlaneServiceAccount;
  canManage: boolean;
  onToken: (token: string) => void;
}) {
  const { Button, SettingsListRow, StatusPill } = useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const usage = useQuery({
    queryKey: serviceAccountUsageQueryKey(props.tenantId, props.account.id),
    queryFn: () => controlPlaneClient.getServiceAccountAPIUsage(props.tenantId, props.account.id),
    enabled: props.account.status === "active",
    retry: false,
  });
  const aggregateUsage = usage.data?.items.find((item) => item.routePattern === "*");
  const rotate = useMutation({
    mutationFn: () =>
      controlPlaneClient.rotateServiceAccountToken(props.tenantId, props.account.id),
    onSuccess: (issued) => props.onToken(issued.token),
  });
  const revoke = useMutation({
    mutationFn: () => controlPlaneClient.revokeServiceAccount(props.tenantId, props.account.id),
    onSuccess: () =>
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneServiceAccount> }>(
        serviceAccountQueryKey(props.tenantId),
        (current) => ({
          items: (current?.items ?? []).map((item) =>
            item.id === props.account.id
              ? { ...item, status: "revoked", revokedAt: new Date().toISOString() }
              : item,
          ),
        }),
      ),
  });
  return (
    <SettingsListRow
      title={props.account.name}
      description={`${props.account.description || "No description"} · ${props.account.role} · ${props.account.rateLimitPerMinute}/min · ${props.account.scopes.join(", ")} · last used ${props.account.lastUsedAt ? new Date(props.account.lastUsedAt).toLocaleString() : "never"}${aggregateUsage ? ` · 24h ${aggregateUsage.requestCount} admitted / ${aggregateUsage.rateLimitedCount} limited` : ""}`}
      actions={
        <span className="flex items-center gap-2">
          <StatusPill value={props.account.status} />
          {props.canManage && props.account.status === "active" ? (
            <>
              <Button
                disabled={rotate.isPending}
                size="sm"
                variant="outline"
                onClick={() => rotate.mutate()}
              >
                Rotate token
              </Button>
              <Button
                disabled={revoke.isPending}
                size="sm"
                variant="outline"
                onClick={() => revoke.mutate()}
              >
                Revoke
              </Button>
            </>
          ) : null}
        </span>
      }
    />
  );
}
