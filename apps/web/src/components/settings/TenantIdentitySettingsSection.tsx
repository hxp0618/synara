import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME as formGridClassName,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME as nativeSelectClassName,
  ControlPlaneFormField as FormField,
  ControlPlaneInlineError as InlineError,
  ControlPlaneStatusPill as StatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { SettingsListRow, SettingsRow, SettingsSection } from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import {
  controlPlaneClient,
  type ControlPlaneIdentityConnection,
  type ControlPlaneIdentityGroupMapping,
  type ControlPlaneOrganization,
} from "~/lib/controlPlaneClient";

const TENANT_ROLES = ["member", "auditor", "billing_admin", "security_admin", "admin"] as const;
const ORGANIZATION_ROLES = ["viewer", "member", "agent_operator", "admin", "owner"] as const;

function connectionsQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "identity-connections"] as const;
}

function mappingsQueryKey(tenantId: string, connectionId: string) {
  return ["control-plane", "tenants", tenantId, "identity-connections", connectionId, "group-mappings"] as const;
}

export function TenantIdentitySettingsSection(props: {
  tenantId: string;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
  const connections = useQuery({
    queryKey: connectionsQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listIdentityConnections(props.tenantId),
    retry: false,
  });
  const activeConnections = useMemo(
    () => connections.data?.items.filter((connection) => connection.status === "active") ?? [],
    [connections.data?.items],
  );
  const [selectedConnectionId, setSelectedConnectionId] = useState("");
  const effectiveConnectionId = activeConnections.some(
    (connection) => connection.id === selectedConnectionId,
  )
    ? selectedConnectionId
    : (activeConnections[0]?.id ?? "");
  const onCreated = (connection: ControlPlaneIdentityConnection) => {
    queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneIdentityConnection> }>(
      connectionsQueryKey(props.tenantId),
      (current) => ({ items: [...(current?.items ?? []), connection] }),
    );
    setSelectedConnectionId(connection.id);
  };

  return (
    <SettingsSection title="Enterprise identity">
      <SettingsRow
        title="OIDC single sign-on"
        description="OIDC uses discovery, signed ID-token verification, nonce validation, PKCE, allowed email domains, and explicit Group-to-role mappings. Client secrets are KMS envelope encrypted."
      >
        {props.canManage ? (
          <OIDCConnectionForm tenantId={props.tenantId} onCreated={onCreated} />
        ) : null}
      </SettingsRow>
      <SettingsRow
        title="SAML single sign-on"
        description="SAML validates IdP metadata, signed assertions, request correlation, destination, audience, issuer, and allowed email domains. Each connection receives a KMS-encrypted service-provider signing key."
      >
        {props.canManage ? (
          <SAMLConnectionForm tenantId={props.tenantId} onCreated={onCreated} />
        ) : null}
      </SettingsRow>
      {connections.isPending ? <SettingsListRow title="Loading identity connections…" /> : null}
      {connections.data?.items.map((connection) => (
        <IdentityConnectionRow
          key={connection.id}
          canManage={props.canManage}
          connection={connection}
          tenantId={props.tenantId}
        />
      ))}
      {connections.data?.items.length === 0 ? (
        <SettingsListRow
          title="No enterprise identity connection"
          description="Create an OIDC or SAML connection to enable Tenant-scoped enterprise SSO."
        />
      ) : null}
      {props.canManage && effectiveConnectionId ? (
        <IdentityGroupMappingsForm
          connectionId={effectiveConnectionId}
          connections={activeConnections}
          organizations={props.organizations}
          onConnectionChange={setSelectedConnectionId}
          tenantId={props.tenantId}
        />
      ) : null}
      <InlineError error={connections.error} />
    </SettingsSection>
  );
}

function SAMLConnectionForm(props: {
  tenantId: string;
  onCreated: (connection: ControlPlaneIdentityConnection) => void;
}) {
  const [name, setName] = useState("");
  const [metadataUrl, setMetadataUrl] = useState("");
  const [issuer, setIssuer] = useState("");
  const [entityId, setEntityId] = useState("");
  const [emailAttribute, setEmailAttribute] = useState("email");
  const [displayNameAttribute, setDisplayNameAttribute] = useState("displayName");
  const [groupsAttribute, setGroupsAttribute] = useState("groups");
  const [allowedDomains, setAllowedDomains] = useState("");
  const [defaultTenantRole, setDefaultTenantRole] = useState("member");
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createIdentityConnection(props.tenantId, {
        kind: "saml",
        name,
        issuer,
        saml: {
          metadataUrl,
          entityId,
          emailAttribute,
          displayNameAttribute,
          groupsAttribute,
          allowedDomains: allowedDomains
            .split(",")
            .map((domain) => domain.trim())
            .filter(Boolean),
          defaultTenantRole,
        },
      }),
    onSuccess: (connection) => {
      setName("");
      setMetadataUrl("");
      setIssuer("");
      setEntityId("");
      setAllowedDomains("");
      props.onCreated(connection);
    },
  });
  return (
    <form
      data-testid="saml-connection-form"
      className={formGridClassName}
      onSubmit={(event: FormEvent) => {
        event.preventDefault();
        create.mutate();
      }}
    >
      <FormField label="Connection name">
        <Input data-testid="saml-connection-name" required value={name} onChange={(event) => setName(event.target.value)} />
      </FormField>
      <FormField label="IdP metadata URL">
        <Input data-testid="saml-metadata-url" placeholder="https://id.example.com/metadata" required type="url" value={metadataUrl} onChange={(event) => setMetadataUrl(event.target.value)} />
      </FormField>
      <FormField label="IdP issuer (optional)">
        <Input placeholder="Auto-detected from metadata" value={issuer} onChange={(event) => setIssuer(event.target.value)} />
      </FormField>
      <FormField label="SP entity ID (optional)">
        <Input placeholder="Generated when blank" value={entityId} onChange={(event) => setEntityId(event.target.value)} />
      </FormField>
      <FormField label="Email attribute">
        <Input value={emailAttribute} onChange={(event) => setEmailAttribute(event.target.value)} />
      </FormField>
      <FormField label="Display name attribute">
        <Input value={displayNameAttribute} onChange={(event) => setDisplayNameAttribute(event.target.value)} />
      </FormField>
      <FormField label="Groups attribute">
        <Input value={groupsAttribute} onChange={(event) => setGroupsAttribute(event.target.value)} />
      </FormField>
      <FormField label="Allowed email domains">
        <Input placeholder="example.com, subsidiary.com" value={allowedDomains} onChange={(event) => setAllowedDomains(event.target.value)} />
      </FormField>
      <FormField label="Default Tenant role">
        <select className={nativeSelectClassName} value={defaultTenantRole} onChange={(event) => setDefaultTenantRole(event.target.value)}>
          {TENANT_ROLES.map((role) => <option key={role} value={role}>{role}</option>)}
        </select>
      </FormField>
      <div className="flex items-end">
        <Button data-testid="saml-connection-submit" disabled={create.isPending} size="sm" type="submit">
          {create.isPending ? "Creating SAML connection…" : "Create SAML connection"}
        </Button>
      </div>
      <div className="sm:col-span-2"><InlineError error={create.error} /></div>
    </form>
  );
}

function OIDCConnectionForm(props: {
  tenantId: string;
  onCreated: (connection: ControlPlaneIdentityConnection) => void;
}) {
  const [name, setName] = useState("");
  const [issuer, setIssuer] = useState("");
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [allowedDomains, setAllowedDomains] = useState("");
  const [groupsClaim, setGroupsClaim] = useState("groups");
  const [defaultTenantRole, setDefaultTenantRole] = useState("member");
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createIdentityConnection(props.tenantId, {
        kind: "oidc",
        name,
        issuer,
        clientId,
        clientSecret,
        oidc: {
          allowedDomains: allowedDomains
            .split(",")
            .map((domain) => domain.trim())
            .filter(Boolean),
          groupsClaim,
          defaultTenantRole,
        },
      }),
    onSuccess: (connection) => {
      setName("");
      setIssuer("");
      setClientId("");
      setClientSecret("");
      setAllowedDomains("");
      props.onCreated(connection);
    },
  });
  return (
    <form
      data-testid="oidc-connection-form"
      className={formGridClassName}
      onSubmit={(event: FormEvent) => {
        event.preventDefault();
        create.mutate();
      }}
    >
      <FormField label="Connection name">
        <Input data-testid="oidc-connection-name" required value={name} onChange={(event) => setName(event.target.value)} />
      </FormField>
      <FormField label="Issuer URL">
        <Input data-testid="oidc-issuer-url" placeholder="https://id.example.com" required type="url" value={issuer} onChange={(event) => setIssuer(event.target.value)} />
      </FormField>
      <FormField label="Client ID">
        <Input data-testid="oidc-client-id" required value={clientId} onChange={(event) => setClientId(event.target.value)} />
      </FormField>
      <FormField label="Client secret">
        <Input data-testid="oidc-client-secret" autoComplete="new-password" type="password" value={clientSecret} onChange={(event) => setClientSecret(event.target.value)} />
      </FormField>
      <FormField label="Allowed email domains">
        <Input placeholder="example.com, subsidiary.com" value={allowedDomains} onChange={(event) => setAllowedDomains(event.target.value)} />
      </FormField>
      <FormField label="Groups claim">
        <Input value={groupsClaim} onChange={(event) => setGroupsClaim(event.target.value)} />
      </FormField>
      <FormField label="Default Tenant role">
        <select className={nativeSelectClassName} value={defaultTenantRole} onChange={(event) => setDefaultTenantRole(event.target.value)}>
          {TENANT_ROLES.map((role) => <option key={role} value={role}>{role}</option>)}
        </select>
      </FormField>
      <div className="flex items-end">
        <Button data-testid="oidc-connection-submit" disabled={create.isPending} size="sm" type="submit">
          {create.isPending ? "Creating OIDC connection…" : "Create OIDC connection"}
        </Button>
      </div>
      <div className="sm:col-span-2"><InlineError error={create.error} /></div>
    </form>
  );
}

function IdentityConnectionRow(props: {
  tenantId: string;
  connection: ControlPlaneIdentityConnection;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
  const disable = useMutation({
    mutationFn: () => controlPlaneClient.disableIdentityConnection(props.tenantId, props.connection.id),
    onSuccess: () => {
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneIdentityConnection> }>(
        connectionsQueryKey(props.tenantId),
        (current) => ({
          items: (current?.items ?? []).map((item) => item.id === props.connection.id ? { ...item, status: "disabled" } : item),
        }),
      );
    },
  });
  return (
    <SettingsListRow
      title={props.connection.name}
      description={
        <span className="space-y-1">
          <span className="block">
            {props.connection.kind.toUpperCase()} · {props.connection.issuer}
            {props.connection.clientId ? ` · ${props.connection.clientId}` : ""}
          </span>
          {props.connection.kind === "saml" ? (
            <span className="block">
              SP entity ID: {props.connection.configuration.entityId}{" "}
              <a
                className="text-foreground underline decoration-border underline-offset-2 hover:decoration-foreground"
                href={`/v1/auth/sso/${encodeURIComponent(props.connection.id)}/metadata`}
                rel="noreferrer"
                target="_blank"
              >
                View SP metadata
              </a>
            </span>
          ) : null}
        </span>
      }
      actions={
        <span className="flex items-center gap-2">
          <StatusPill value={props.connection.status} />
          {props.canManage && props.connection.status === "active" ? (
            <Button disabled={disable.isPending} size="sm" variant="outline" onClick={() => disable.mutate()}>
              Disable
            </Button>
          ) : null}
        </span>
      }
    />
  );
}

function IdentityGroupMappingsForm(props: {
  tenantId: string;
  connectionId: string;
  connections: ReadonlyArray<ControlPlaneIdentityConnection>;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  onConnectionChange: (connectionId: string) => void;
}) {
  const queryClient = useQueryClient();
  const mappings = useQuery({
    queryKey: mappingsQueryKey(props.tenantId, props.connectionId),
    queryFn: () => controlPlaneClient.listIdentityGroupMappings(props.tenantId, props.connectionId),
    retry: false,
  });
  const [externalGroup, setExternalGroup] = useState("");
  const [tenantRole, setTenantRole] = useState("");
  const [organizationId, setOrganizationId] = useState("");
  const [organizationRole, setOrganizationRole] = useState("member");
  const organizationNamesById = useMemo(
    () => new Map(props.organizations.map((organization) => [organization.id, organization.name])),
    [props.organizations],
  );
  const replace = useMutation({
    mutationFn: (items: ReadonlyArray<Omit<ControlPlaneIdentityGroupMapping, "id">>) =>
      controlPlaneClient.replaceIdentityGroupMappings(props.tenantId, props.connectionId, items),
    onSuccess: (next) => queryClient.setQueryData(mappingsQueryKey(props.tenantId, props.connectionId), next),
  });
  const currentItems = mappings.data?.items ?? [];
  return (
    <SettingsRow
      title="Identity Group mappings"
      description="Select an OIDC or SAML connection, then map an exact external Group value to a Tenant role, an Organization role, or both. Existing stronger Tenant roles are never downgraded during login."
    >
      <div className="space-y-3">
        <FormField label="Identity connection">
          <select
            className={nativeSelectClassName}
            value={props.connectionId}
            onChange={(event) => props.onConnectionChange(event.target.value)}
          >
            {props.connections.map((connection) => (
              <option key={connection.id} value={connection.id}>
                {connection.name} ({connection.kind.toUpperCase()})
              </option>
            ))}
          </select>
        </FormField>
        {currentItems.map((mapping) => (
          <div key={mapping.id} className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
            <span>{mapping.externalGroup} → {mapping.tenantRole ?? "no Tenant role"}{mapping.organizationId ? ` · ${organizationNamesById.get(mapping.organizationId) ?? mapping.organizationId} / ${mapping.organizationRole}` : ""}</span>
            <Button
              disabled={replace.isPending}
              size="sm"
              variant="ghost"
              onClick={() => replace.mutate(currentItems.filter((item) => item.id !== mapping.id).map(({ id: _id, ...item }) => item))}
            >
              Remove
            </Button>
          </div>
        ))}
        <form
          className={formGridClassName}
          onSubmit={(event) => {
            event.preventDefault();
            const next = [
              ...currentItems.map(({ id: _id, ...item }) => item),
              {
                externalGroup,
                tenantRole: tenantRole || null,
                organizationId: organizationId || null,
                organizationRole: organizationId ? organizationRole : null,
              },
            ];
            replace.mutate(next, { onSuccess: () => setExternalGroup("") });
          }}
        >
          <FormField label="External Group">
            <Input required value={externalGroup} onChange={(event) => setExternalGroup(event.target.value)} />
          </FormField>
          <FormField label="Tenant role">
            <select className={nativeSelectClassName} value={tenantRole} onChange={(event) => setTenantRole(event.target.value)}>
              <option value="">No Tenant role</option>
              {TENANT_ROLES.map((role) => <option key={role} value={role}>{role}</option>)}
            </select>
          </FormField>
          <FormField label="Organization">
            <select className={nativeSelectClassName} value={organizationId} onChange={(event) => setOrganizationId(event.target.value)}>
              <option value="">No Organization role</option>
              {props.organizations.map((organization) => <option key={organization.id} value={organization.id}>{organization.name}</option>)}
            </select>
          </FormField>
          <FormField label="Organization role">
            <select className={nativeSelectClassName} disabled={!organizationId} value={organizationRole} onChange={(event) => setOrganizationRole(event.target.value)}>
              {ORGANIZATION_ROLES.map((role) => <option key={role} value={role}>{role}</option>)}
            </select>
          </FormField>
          <div className="sm:col-span-2">
            <Button disabled={replace.isPending || (!tenantRole && !organizationId)} size="sm" type="submit">Add Group mapping</Button>
            <InlineError error={replace.error ?? mappings.error} />
          </div>
        </form>
      </div>
    </SettingsRow>
  );
}
