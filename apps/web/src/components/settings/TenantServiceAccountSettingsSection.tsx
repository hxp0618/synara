import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME as formGridClassName,
  ControlPlaneFormField as FormField,
  ControlPlaneInlineError as InlineError,
  ControlPlaneStatusPill as StatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { SettingsListRow, SettingsRow, SettingsSection } from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { controlPlaneClient, type ControlPlaneServiceAccount } from "~/lib/controlPlaneClient";

const SCOPES = [
  ["scim.read", "Read SCIM users and groups"],
  ["scim.write", "Provision SCIM users and groups"],
  ["identity.read", "Read identity configuration"],
  ["identity.manage", "Manage identity configuration"],
] as const;

function queryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "service-accounts"] as const;
}

export function TenantServiceAccountSettingsSection(props: { tenantId: string; canManage: boolean }) {
  const queryClient = useQueryClient();
  const accounts = useQuery({
    queryKey: queryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listServiceAccounts(props.tenantId),
    retry: false,
  });
  const [issuedToken, setIssuedToken] = useState("");
  return (
    <SettingsSection title="Service Accounts and SCIM">
      <SettingsRow
        title="Directory provisioning credentials"
        description="Service Accounts are Tenant-scoped machine identities. Tokens are shown once, stored only as SHA-256 hashes, and can be rotated or revoked without impersonating a User."
      >
        {props.canManage ? (
          <ServiceAccountForm
            tenantId={props.tenantId}
            onCreated={(account, token) => {
              queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneServiceAccount> }>(
                queryKey(props.tenantId),
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
          control={<Button size="sm" variant="outline" onClick={() => setIssuedToken("")}>Clear token</Button>}
        >
          <code className="block break-all rounded-md border border-border bg-muted/40 p-3 text-xs text-foreground">{issuedToken}</code>
        </SettingsRow>
      ) : null}
      {accounts.isPending ? <SettingsListRow title="Loading Service Accounts…" /> : null}
      {accounts.data?.items.map((account) => (
        <ServiceAccountRow key={account.id} account={account} canManage={props.canManage} tenantId={props.tenantId} onToken={setIssuedToken} />
      ))}
      {accounts.data?.items.length === 0 ? <SettingsListRow title="No Service Accounts" description="Create one with SCIM scopes before connecting a directory provider." /> : null}
      <InlineError error={accounts.error} />
    </SettingsSection>
  );
}

function ServiceAccountForm(props: {
  tenantId: string;
  onCreated: (account: ControlPlaneServiceAccount, token: string) => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [scopes, setScopes] = useState<ReadonlyArray<string>>(["scim.read", "scim.write"]);
  const create = useMutation({
    mutationFn: () => controlPlaneClient.createServiceAccount(props.tenantId, { name, description, scopes }),
    onSuccess: (issued) => {
      props.onCreated(issued.account, issued.token);
      setName("");
      setDescription("");
    },
  });
  return (
    <form data-testid="service-account-form" className={formGridClassName} onSubmit={(event: FormEvent) => { event.preventDefault(); create.mutate(); }}>
      <FormField label="Name"><Input data-testid="service-account-name" required value={name} onChange={(event) => setName(event.target.value)} /></FormField>
      <FormField label="Description"><Input data-testid="service-account-description" value={description} onChange={(event) => setDescription(event.target.value)} /></FormField>
      <div className="space-y-2 sm:col-span-2">
        {SCOPES.map(([scope, label]) => (
          <label key={scope} className="flex items-center gap-2 text-xs text-muted-foreground">
            <input
              checked={scopes.includes(scope)}
              type="checkbox"
              onChange={(event) => setScopes((current) => event.target.checked ? [...current, scope] : current.filter((item) => item !== scope))}
            />
            <span><span className="font-medium text-foreground">{scope}</span> · {label}</span>
          </label>
        ))}
      </div>
      <div className="sm:col-span-2">
        <Button data-testid="service-account-submit" disabled={create.isPending || scopes.length === 0} size="sm" type="submit">{create.isPending ? "Creating Service Account…" : "Create Service Account"}</Button>
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
  const queryClient = useQueryClient();
  const rotate = useMutation({
    mutationFn: () => controlPlaneClient.rotateServiceAccountToken(props.tenantId, props.account.id),
    onSuccess: (issued) => props.onToken(issued.token),
  });
  const revoke = useMutation({
    mutationFn: () => controlPlaneClient.revokeServiceAccount(props.tenantId, props.account.id),
    onSuccess: () => queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneServiceAccount> }>(
      queryKey(props.tenantId),
      (current) => ({ items: (current?.items ?? []).map((item) => item.id === props.account.id ? { ...item, status: "revoked", revokedAt: new Date().toISOString() } : item) }),
    ),
  });
  return (
    <SettingsListRow
      title={props.account.name}
      description={`${props.account.description || "No description"} · ${props.account.scopes.join(", ")}`}
      actions={<span className="flex items-center gap-2"><StatusPill value={props.account.status} />{props.canManage && props.account.status === "active" ? <><Button disabled={rotate.isPending} size="sm" variant="outline" onClick={() => rotate.mutate()}>Rotate token</Button><Button disabled={revoke.isPending} size="sm" variant="outline" onClick={() => revoke.mutate()}>Revoke</Button></> : null}</span>}
    />
  );
}
