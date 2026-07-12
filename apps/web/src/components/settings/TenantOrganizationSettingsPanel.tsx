import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME as formGridClassName,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME as nativeSelectClassName,
  ControlPlaneFormField as FormField,
  ControlPlaneInlineError as InlineError,
  ControlPlaneStatusPill as StatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { ProjectSessionSettingsSection } from "~/components/settings/ProjectSessionSettingsSection";
import { ExecutionTargetSettingsSection } from "~/components/settings/ExecutionTargetSettingsSection";
import { TenantQuotaSettingsSection } from "~/components/settings/TenantQuotaSettingsSection";
import { TenantRetentionSettingsSection } from "~/components/settings/TenantRetentionSettingsSection";
import { TenantAuditSettingsSection } from "~/components/settings/TenantAuditSettingsSection";
import { TenantIdentitySettingsSection } from "~/components/settings/TenantIdentitySettingsSection";
import { TenantServiceAccountSettingsSection } from "~/components/settings/TenantServiceAccountSettingsSection";
import {
  credentialsQueryKey,
  TenantCredentialSettingsSection,
} from "~/components/settings/TenantCredentialSettingsSection";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import {
  controlPlaneClient,
  ControlPlaneError,
  type ControlPlaneExecutionTarget,
  type ControlPlaneProviderCredential,
  type ControlPlaneSessionState,
  type TenantInvitation,
} from "~/lib/controlPlaneClient";
import { cn } from "~/lib/utils";

const controlPlaneQueryKeys = {
  session: ["control-plane", "session"] as const,
  organizations: (tenantId: string | null) =>
    ["control-plane", "tenants", tenantId, "organizations"] as const,
  members: (tenantId: string | null) =>
    ["control-plane", "tenants", tenantId, "members"] as const,
  executionTargets: (tenantId: string | null) =>
    ["control-plane", "tenants", tenantId, "execution-targets"] as const,
};

function LoginPanel() {
  const queryClient = useQueryClient();
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [tenantSlug, setTenantSlug] = useState("");
  const login = useMutation({
    mutationFn: controlPlaneClient.devLogin,
    onSuccess: (session) => {
      queryClient.setQueryData(controlPlaneQueryKeys.session, session);
    },
  });
  const discoverSSO = useMutation({
    mutationFn: () => controlPlaneClient.listPublicIdentityConnections(tenantSlug.trim()),
  });
  const startSSO = useMutation({
    mutationFn: (connectionId: string) => controlPlaneClient.startSSO(connectionId),
    onSuccess: ({ authorizationUrl }) => window.location.assign(authorizationUrl),
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    login.mutate({ email, displayName });
  };

  return (
    <SettingsSection title="SaaS control plane">
      <SettingsRow
        title="Sign in to the local control plane"
        description="Development bootstrap creates a local SaaS identity, personal tenant, and root organization. Production deployments should replace this with enterprise SSO."
      >
        <form className={formGridClassName} onSubmit={submit}>
          <FormField label="Email">
            <Input
              autoComplete="email"
              onChange={(event) => setEmail(event.target.value)}
              placeholder="you@company.com"
              required
              type="email"
              value={email}
            />
          </FormField>
          <FormField label="Display name">
            <Input
              autoComplete="name"
              onChange={(event) => setDisplayName(event.target.value)}
              placeholder="Your name"
              required
              value={displayName}
            />
          </FormField>
          <div className="sm:col-span-2">
            <Button disabled={login.isPending} size="sm" type="submit">
              {login.isPending ? "Signing in…" : "Create local SaaS identity"}
            </Button>
            <InlineError error={login.error} />
          </div>
        </form>
      </SettingsRow>
      <SettingsRow
        title="Sign in with enterprise SSO"
        description="Enter the Tenant slug to discover active OIDC or SAML connections without exposing their configuration."
      >
        <form
          className={formGridClassName}
          onSubmit={(event) => {
            event.preventDefault();
            discoverSSO.mutate();
          }}
        >
          <FormField label="Tenant slug">
            <Input
              autoComplete="organization"
              placeholder="acme"
              required
              value={tenantSlug}
              onChange={(event) => setTenantSlug(event.target.value.toLowerCase())}
            />
          </FormField>
          <div className="flex items-end">
            <Button disabled={discoverSSO.isPending} size="sm" type="submit" variant="outline">
              {discoverSSO.isPending ? "Finding SSO…" : "Find SSO connections"}
            </Button>
          </div>
          {discoverSSO.data?.items.map((connection) => (
            <div key={connection.id} className="flex items-center justify-between gap-3 sm:col-span-2">
              <span className="text-xs text-muted-foreground">
                {connection.name} · {connection.kind.toUpperCase()}
              </span>
              <Button
                disabled={startSSO.isPending}
                size="sm"
                onClick={() => startSSO.mutate(connection.id)}
                type="button"
              >
                Continue with {connection.name}
              </Button>
            </div>
          ))}
          {discoverSSO.data?.items.length === 0 ? (
            <p className="text-xs text-muted-foreground sm:col-span-2">
              No active SSO connection was found for this Tenant.
            </p>
          ) : null}
          <div className="sm:col-span-2">
            <InlineError error={discoverSSO.error ?? startSSO.error} />
          </div>
        </form>
      </SettingsRow>
    </SettingsSection>
  );
}

export function TenantOrganizationSettingsPanel() {
  const queryClient = useQueryClient();
  const sessionQuery = useQuery({
    queryKey: controlPlaneQueryKeys.session,
    queryFn: controlPlaneClient.getSession,
    retry: false,
    staleTime: 30_000,
  });

  if (sessionQuery.isPending) {
    return (
      <SettingsSection title="SaaS control plane">
        <SettingsListRow title="Connecting…" description="Loading tenant and organization context." />
      </SettingsSection>
    );
  }

  if (sessionQuery.error) {
    if (sessionQuery.error instanceof ControlPlaneError && sessionQuery.error.status === 401) {
      return <LoginPanel />;
    }
    const unavailable =
      sessionQuery.error instanceof ControlPlaneError &&
      (sessionQuery.error.status === 502 || sessionQuery.error.status === 503);
    return (
      <SettingsSection title="SaaS control plane">
        <SettingsListRow
          title={unavailable ? "Control plane unavailable" : "Could not load tenant context"}
          description={sessionQuery.error.message}
          actions={
            <Button size="sm" variant="outline" onClick={() => void sessionQuery.refetch()}>
              Retry
            </Button>
          }
        />
      </SettingsSection>
    );
  }

  return (
    <AuthenticatedTenantPanel
      session={sessionQuery.data}
      onSessionChange={(session) =>
        queryClient.setQueryData(controlPlaneQueryKeys.session, session)
      }
    />
  );
}

function AuthenticatedTenantPanel(props: {
  session: ControlPlaneSessionState;
  onSessionChange: (session: ControlPlaneSessionState) => void;
}) {
  const queryClient = useQueryClient();
  const activeTenant = useMemo(
    () =>
      props.session.tenants.find((tenant) => tenant.id === props.session.user.activeTenantId) ??
      props.session.tenants[0] ??
      null,
    [props.session.tenants, props.session.user.activeTenantId],
  );
  const activeTenantId = activeTenant?.id ?? null;
  const [organizationName, setOrganizationName] = useState("");
  const [organizationSlug, setOrganizationSlug] = useState("");
  const [organizationKind, setOrganizationKind] =
    useState<"team" | "department" | "personal">("team");
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState("member");
  const [createdInvitation, setCreatedInvitation] = useState<TenantInvitation | null>(null);

  const organizationsQuery = useQuery({
    queryKey: controlPlaneQueryKeys.organizations(activeTenantId),
    queryFn: () => controlPlaneClient.listOrganizations(activeTenantId!),
    enabled: activeTenantId !== null,
    retry: false,
  });
  const canReadMembers = activeTenant?.role !== "member";
  const canManageMembers = activeTenant?.role === "owner" || activeTenant?.role === "admin";
  const canReadExecutionTargets = ["owner", "admin", "security_admin", "auditor"].includes(
    activeTenant?.role ?? "",
  );
  const canManageExecutionTargets = activeTenant?.role === "owner" || activeTenant?.role === "admin";
  const canReadQuota = ["owner", "admin", "billing_admin", "auditor"].includes(
    activeTenant?.role ?? "",
  );
  const canManageQuota = ["owner", "admin", "billing_admin"].includes(activeTenant?.role ?? "");
	const canReadRetention = ["owner", "admin", "security_admin", "auditor"].includes(
		activeTenant?.role ?? "",
	);
	const canManageRetention = ["owner", "admin", "security_admin"].includes(
		activeTenant?.role ?? "",
	);
  const canReadAudit = ["owner", "admin", "security_admin", "auditor"].includes(
    activeTenant?.role ?? "",
  );
  const canManageCredentials = ["owner", "security_admin"].includes(activeTenant?.role ?? "");
	const canReadIdentity = ["owner", "admin", "security_admin"].includes(activeTenant?.role ?? "");
	const canManageIdentity = ["owner", "security_admin"].includes(activeTenant?.role ?? "");
	const canReadServiceAccounts = ["owner", "admin", "security_admin"].includes(
		activeTenant?.role ?? "",
	);
	const canManageServiceAccounts = canReadServiceAccounts;
	const credentialsQuery = useQuery({
		queryKey: credentialsQueryKey(activeTenantId ?? ""),
		queryFn: () => controlPlaneClient.listCredentials(activeTenantId!),
		enabled: activeTenantId !== null && canManageCredentials,
		retry: false,
	});
  const membersQuery = useQuery({
    queryKey: controlPlaneQueryKeys.members(activeTenantId),
    queryFn: () => controlPlaneClient.listTenantMembers(activeTenantId!),
    enabled: activeTenantId !== null && canReadMembers,
    retry: false,
  });
  const executionTargetsQuery = useQuery({
    queryKey: controlPlaneQueryKeys.executionTargets(activeTenantId),
    queryFn: () => controlPlaneClient.listExecutionTargets(activeTenantId!),
    enabled: activeTenantId !== null && canReadExecutionTargets,
    retry: false,
  });
  const setActiveTenant = useMutation({
    mutationFn: controlPlaneClient.setActiveTenant,
    onSuccess: (session) => {
      props.onSessionChange(session);
      void queryClient.invalidateQueries({ queryKey: ["control-plane", "tenants"] });
    },
  });
  const createOrganization = useMutation({
    mutationFn: () =>
      controlPlaneClient.createOrganization(activeTenantId!, {
        name: organizationName,
        slug: organizationSlug,
        kind: organizationKind,
      }),
    onSuccess: () => {
      setOrganizationName("");
      setOrganizationSlug("");
      void queryClient.invalidateQueries({
        queryKey: controlPlaneQueryKeys.organizations(activeTenantId),
      });
    },
  });
  const inviteMember = useMutation({
    mutationFn: () =>
      controlPlaneClient.inviteTenantMember(activeTenantId!, {
        email: inviteEmail,
        role: inviteRole,
      }),
    onSuccess: (invitation) => {
      setCreatedInvitation(invitation);
      setInviteEmail("");
    },
  });
  const logout = useMutation({
    mutationFn: controlPlaneClient.logout,
    onSuccess: () => {
      queryClient.removeQueries({ queryKey: ["control-plane"] });
      void queryClient.invalidateQueries({ queryKey: controlPlaneQueryKeys.session });
    },
  });

  if (!activeTenant) {
    return (
      <SettingsSection title="Tenant access">
        <SettingsListRow
          title="No active tenant"
          description="This account has no active tenant membership. Ask an administrator for an invitation."
        />
      </SettingsSection>
    );
  }

  return (
    <div className="space-y-7">
      <SettingsSection title="Tenant context">
        <SettingsRow
          title={activeTenant.name}
          description={`${activeTenant.slug} · ${activeTenant.region} · ${activeTenant.planCode} plan`}
          status={
            <span className="flex flex-wrap gap-1.5">
              <StatusPill value={activeTenant.role} />
              <StatusPill value={activeTenant.status} />
            </span>
          }
          control={
            props.session.tenants.length > 1 ? (
              <select
                aria-label="Active tenant"
                className={cn(nativeSelectClassName, "min-w-44")}
                disabled={setActiveTenant.isPending}
                onChange={(event) => setActiveTenant.mutate(event.target.value)}
                value={activeTenant.id}
              >
                {props.session.tenants.map((tenant) => (
                  <option key={tenant.id} value={tenant.id}>
                    {tenant.name}
                  </option>
                ))}
              </select>
            ) : undefined
          }
        />
        <SettingsRow
          title={props.session.user.displayName}
          description={props.session.user.email}
          control={
            <Button disabled={logout.isPending} size="sm" variant="outline" onClick={() => logout.mutate()}>
              Sign out
            </Button>
          }
        />
      </SettingsSection>

      {canReadQuota ? (
        <TenantQuotaSettingsSection
          key={`quota-${activeTenant.id}`}
          canManage={canManageQuota}
          tenantId={activeTenant.id}
        />
      ) : null}

		{canReadRetention ? (
			<TenantRetentionSettingsSection
				key={`retention-${activeTenant.id}`}
				canManage={canManageRetention}
				tenantId={activeTenant.id}
			/>
		) : null}

      {canReadAudit ? (
        <TenantAuditSettingsSection
          key={`audit-${activeTenant.id}`}
          tenantId={activeTenant.id}
        />
      ) : null}

      {canManageCredentials && organizationsQuery.data ? (
        <TenantCredentialSettingsSection
          key={`credentials-${activeTenant.id}`}
          organizations={organizationsQuery.data.items.filter(
            (organization) => organization.status === "active",
          )}
          tenantId={activeTenant.id}
        />
      ) : null}

		{canReadIdentity && organizationsQuery.data ? (
			<TenantIdentitySettingsSection
				key={`identity-${activeTenant.id}`}
				canManage={canManageIdentity}
				organizations={organizationsQuery.data.items.filter(
					(organization) => organization.status === "active",
				)}
				tenantId={activeTenant.id}
			/>
		) : null}

		{canReadServiceAccounts ? (
			<TenantServiceAccountSettingsSection
				key={`service-accounts-${activeTenant.id}`}
				canManage={canManageServiceAccounts}
				tenantId={activeTenant.id}
			/>
		) : null}

      <SettingsSection title="Organizations">
        {organizationsQuery.data?.items.map((organization) => (
          <SettingsListRow
            key={organization.id}
            title={organization.name}
            description={`${organization.slug} · ${organization.kind}`}
            actions={<StatusPill value={organization.status} />}
          />
        ))}
        {organizationsQuery.isPending ? (
          <SettingsListRow title="Loading organizations…" />
        ) : organizationsQuery.data?.items.length === 0 ? (
          <SettingsListRow
            title="No organization access"
            description="You are a tenant member but have not been assigned to an organization."
          />
        ) : null}
        {authorizationCanCreateOrganization(activeTenant.role) ? (
          <SettingsRow
            title="Create organization"
            description="Organizations own projects and define the collaboration boundary inside a tenant."
          >
            <form
              className={formGridClassName}
              onSubmit={(event) => {
                event.preventDefault();
                createOrganization.mutate();
              }}
            >
              <FormField label="Name">
                <Input required value={organizationName} onChange={(event) => setOrganizationName(event.target.value)} />
              </FormField>
              <FormField label="Slug">
                <Input
                  pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]"
                  placeholder="engineering"
                  required
                  value={organizationSlug}
                  onChange={(event) => setOrganizationSlug(event.target.value.toLowerCase())}
                />
              </FormField>
              <FormField label="Kind">
                <select
                  className={nativeSelectClassName}
                  value={organizationKind}
                  onChange={(event) =>
                    setOrganizationKind(event.target.value as "team" | "department" | "personal")
                  }
                >
                  <option value="team">Team</option>
                  <option value="department">Department</option>
                  <option value="personal">Personal</option>
                </select>
              </FormField>
              <div className="flex items-end">
                <Button disabled={createOrganization.isPending} size="sm" type="submit">
                  {createOrganization.isPending ? "Creating…" : "Create organization"}
                </Button>
              </div>
              <div className="sm:col-span-2">
                <InlineError error={createOrganization.error ?? organizationsQuery.error} />
              </div>
            </form>
          </SettingsRow>
        ) : null}
      </SettingsSection>

      {organizationsQuery.data ? (
        <>
          {canReadExecutionTargets ? (
            <ExecutionTargetSettingsSection
              canManage={canManageExecutionTargets}
              error={executionTargetsQuery.error}
              isLoading={executionTargetsQuery.isPending}
              onCreated={(target) => {
                queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneExecutionTarget> }>(
                  controlPlaneQueryKeys.executionTargets(activeTenantId),
                  (current) => ({ items: [...(current?.items ?? []), target] }),
                );
              }}
              onUpdated={(targetId, status) => {
                queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneExecutionTarget> }>(
                  controlPlaneQueryKeys.executionTargets(activeTenantId),
                  (current) => ({
                    items: (current?.items ?? []).map((target) =>
                      target.id === targetId ? { ...target, status } : target,
                    ),
                  }),
                );
              }}
              organizations={organizationsQuery.data.items.filter(
                (organization) => organization.status === "active",
              )}
              requireOrganizationScope={activeTenant.planCode === "personal"}
              targets={executionTargetsQuery.data?.items ?? []}
              tenantId={activeTenant.id}
            />
          ) : null}
          <ProjectSessionSettingsSection
			credentials={(credentialsQuery.data?.items ?? []) as ReadonlyArray<ControlPlaneProviderCredential>}
            executionTargets={executionTargetsQuery.data?.items ?? []}
            tenantId={activeTenant.id}
            organizations={organizationsQuery.data.items.filter(
              (organization) => organization.status === "active",
            )}
          />
        </>
      ) : null}

      {canReadMembers ? (
        <SettingsSection title="Tenant members">
          {membersQuery.data?.items.map((member) => (
            <SettingsListRow
              key={member.userId}
              title={member.displayName}
              description={member.email}
              actions={
                <span className="flex gap-1.5">
                  <StatusPill value={member.role} />
                  <StatusPill value={member.status} />
                </span>
              }
            />
          ))}
          {membersQuery.isPending ? <SettingsListRow title="Loading members…" /> : null}
          {canManageMembers ? (
            <SettingsRow
              title="Invite tenant member"
              description="The invitation token is displayed once until email delivery is connected."
            >
              <form
                className={formGridClassName}
                onSubmit={(event) => {
                  event.preventDefault();
                  setCreatedInvitation(null);
                  inviteMember.mutate();
                }}
              >
                <FormField label="Email">
                  <Input
                    required
                    type="email"
                    value={inviteEmail}
                    onChange={(event) => setInviteEmail(event.target.value)}
                  />
                </FormField>
                <FormField label="Tenant role">
                  <select
                    className={nativeSelectClassName}
                    value={inviteRole}
                    onChange={(event) => setInviteRole(event.target.value)}
                  >
                    <option value="member">Member</option>
                    <option value="auditor">Auditor</option>
                    <option value="billing_admin">Billing admin</option>
                    <option value="security_admin">Security admin</option>
                    <option value="admin">Admin</option>
                    {activeTenant.role === "owner" ? <option value="owner">Owner</option> : null}
                  </select>
                </FormField>
                <div className="sm:col-span-2">
                  <Button disabled={inviteMember.isPending} size="sm" type="submit">
                    {inviteMember.isPending ? "Creating invitation…" : "Create invitation"}
                  </Button>
                  <InlineError error={inviteMember.error ?? membersQuery.error} />
                </div>
              </form>
              {createdInvitation?.token ? (
                <div className="mt-3 rounded-lg border border-border bg-foreground/3 p-3">
                  <p className="text-[11px] font-medium text-foreground">One-time invitation token</p>
                  <code className="mt-1 block break-all text-[11px] text-muted-foreground">
                    {createdInvitation.token}
                  </code>
                </div>
              ) : null}
            </SettingsRow>
          ) : null}
        </SettingsSection>
      ) : null}
    </div>
  );
}

function authorizationCanCreateOrganization(role: string) {
  return role === "owner" || role === "admin";
}
