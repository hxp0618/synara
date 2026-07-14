import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

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
  type ControlPlaneExecutionTarget,
  type TenantInvitation,
} from "~/lib/controlPlaneClient";
import { cn } from "~/lib/utils";
import { controlPlaneQueryKeys, useControlPlane } from "~/controlPlaneContext";

const settingsQueryKeys = {
  members: (tenantId: string | null) => ["control-plane", "tenants", tenantId, "members"] as const,
  executionTargets: (tenantId: string | null) =>
    ["control-plane", "tenants", tenantId, "execution-targets"] as const,
  workerManifests: (tenantId: string | null) =>
    ["control-plane", "tenants", tenantId, "worker-manifests"] as const,
};

function LoginPanel() {
  const controlPlane = useControlPlane();
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [tenantSlug, setTenantSlug] = useState("");
  const login = useMutation({
    mutationFn: controlPlane.devLogin,
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
            <div
              key={connection.id}
              className="flex items-center justify-between gap-3 sm:col-span-2"
            >
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
  const controlPlane = useControlPlane();

  if (
    controlPlane.availability === "detecting" ||
    (controlPlane.availability === "available" && controlPlane.authentication === "unknown")
  ) {
    return (
      <SettingsSection title="SaaS control plane">
        <SettingsListRow
          title="Connecting…"
          description="Loading tenant and organization context."
        />
      </SettingsSection>
    );
  }

  if (controlPlane.authentication === "unauthenticated") return <LoginPanel />;

  if (controlPlane.availability === "local") {
    return (
      <SettingsSection title="SaaS control plane">
        <SettingsListRow
          title="Local mode"
          description="This Synara instance has no SaaS Control Plane configured. Local Projects and chats remain authoritative."
        />
      </SettingsSection>
    );
  }

  if (controlPlane.error || controlPlane.availability === "unavailable") {
    return (
      <SettingsSection title="SaaS control plane">
        <SettingsListRow
          title="Control plane unavailable"
          description={controlPlane.error?.message ?? "The Control Plane could not be reached."}
          actions={
            <Button size="sm" variant="outline" onClick={() => void controlPlane.retry()}>
              Retry
            </Button>
          }
        />
      </SettingsSection>
    );
  }

  return controlPlane.session ? <AuthenticatedTenantPanel /> : null;
}

function AuthenticatedTenantPanel() {
  const controlPlane = useControlPlane();
  const queryClient = useQueryClient();
  const activeTenant = controlPlane.activeTenant;
  const activeTenantId = activeTenant?.id ?? null;
  const [organizationName, setOrganizationName] = useState("");
  const [organizationSlug, setOrganizationSlug] = useState("");
  const [organizationKind, setOrganizationKind] = useState<"team" | "department" | "personal">(
    "team",
  );
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState("member");
  const [createdInvitation, setCreatedInvitation] = useState<TenantInvitation | null>(null);

  const {
    canReadMembers,
    canManageMembers,
    canReadExecutionTargets,
    canManageExecutionTargets,
    canReadQuota,
    canManageQuota,
    canReadRetention,
    canManageRetention,
    canReadAudit,
    canManageCredentials,
    canReadIdentity,
    canManageIdentity,
    canReadServiceAccounts,
    canManageServiceAccounts,
  } = controlPlane.capabilities;
  const credentialsQuery = useQuery({
    queryKey: credentialsQueryKey(activeTenantId ?? ""),
    queryFn: () => controlPlaneClient.listCredentials(activeTenantId!),
    enabled: activeTenantId !== null && canManageCredentials,
    retry: false,
  });
  const membersQuery = useQuery({
    queryKey: settingsQueryKeys.members(activeTenantId),
    queryFn: () => controlPlaneClient.listTenantMembers(activeTenantId!),
    enabled: activeTenantId !== null && canReadMembers,
    retry: false,
  });
  const executionTargetsQuery = useQuery({
    queryKey: settingsQueryKeys.executionTargets(activeTenantId),
    queryFn: () => controlPlaneClient.listExecutionTargets(activeTenantId!),
    enabled: activeTenantId !== null && canReadExecutionTargets,
    retry: false,
  });
  const workerManifestsQuery = useQuery({
    queryKey: settingsQueryKeys.workerManifests(activeTenantId),
    queryFn: () => controlPlaneClient.listWorkerManifests(activeTenantId!),
    enabled: activeTenantId !== null && canReadExecutionTargets,
    retry: false,
  });
  const setActiveTenant = useMutation({
    mutationFn: controlPlane.setActiveTenant,
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
    mutationFn: controlPlane.logout,
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
            (controlPlane.session?.tenants.length ?? 0) > 1 ? (
              <select
                aria-label="Active tenant"
                className={cn(nativeSelectClassName, "min-w-44")}
                disabled={setActiveTenant.isPending}
                onChange={(event) => setActiveTenant.mutate(event.target.value)}
                value={activeTenant.id}
              >
                {controlPlane.session?.tenants.map((tenant) => (
                  <option key={tenant.id} value={tenant.id}>
                    {tenant.name}
                  </option>
                ))}
              </select>
            ) : undefined
          }
        />
        <SettingsRow
          title={controlPlane.session?.user.displayName ?? "Signed-in user"}
          description={controlPlane.session?.user.email ?? ""}
          control={
            <Button
              disabled={logout.isPending}
              size="sm"
              variant="outline"
              onClick={() => logout.mutate()}
            >
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
        <TenantAuditSettingsSection key={`audit-${activeTenant.id}`} tenantId={activeTenant.id} />
      ) : null}

      {canManageCredentials ? (
        <TenantCredentialSettingsSection
          key={`credentials-${activeTenant.id}`}
          organizations={controlPlane.organizations.filter(
            (organization) => organization.status === "active",
          )}
          tenantId={activeTenant.id}
        />
      ) : null}

      {canReadIdentity ? (
        <TenantIdentitySettingsSection
          key={`identity-${activeTenant.id}`}
          canManage={canManageIdentity}
          organizations={controlPlane.organizations.filter(
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
        {controlPlane.organizations.map((organization) => (
          <SettingsListRow
            key={organization.id}
            title={organization.name}
            description={`${organization.slug} · ${organization.kind}`}
            actions={<StatusPill value={organization.status} />}
          />
        ))}
        {controlPlane.organizations.length === 0 ? (
          <SettingsListRow
            title="No organization access"
            description="You are a tenant member but have not been assigned to an organization."
          />
        ) : null}
        {controlPlane.capabilities.canManageOrganizations ? (
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
                <Input
                  required
                  value={organizationName}
                  onChange={(event) => setOrganizationName(event.target.value)}
                />
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
                <InlineError error={createOrganization.error ?? controlPlane.error} />
              </div>
            </form>
          </SettingsRow>
        ) : null}
      </SettingsSection>

      {controlPlane.organizations.length > 0 ? (
        <>
          {canReadExecutionTargets ? (
            <ExecutionTargetSettingsSection
              canManage={canManageExecutionTargets}
              error={executionTargetsQuery.error}
              isLoading={executionTargetsQuery.isPending}
              onCreated={(target) => {
                queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneExecutionTarget> }>(
                  settingsQueryKeys.executionTargets(activeTenantId),
                  (current) => ({ items: [...(current?.items ?? []), target] }),
                );
              }}
              onUpdated={(targetId, status) => {
                queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneExecutionTarget> }>(
                  settingsQueryKeys.executionTargets(activeTenantId),
                  (current) => ({
                    items: (current?.items ?? []).map((target) =>
                      target.id === targetId ? { ...target, status } : target,
                    ),
                  }),
                );
              }}
              onProviderPolicyUpdated={(target) => {
                queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneExecutionTarget> }>(
                  settingsQueryKeys.executionTargets(activeTenantId),
                  (current) => ({
                    items: (current?.items ?? []).map((candidate) =>
                      candidate.id === target.id ? target : candidate,
                    ),
                  }),
                );
              }}
              organizations={controlPlane.organizations.filter(
                (organization) => organization.status === "active",
              )}
              requireOrganizationScope={activeTenant.planCode === "personal"}
              targets={executionTargetsQuery.data?.items ?? []}
              tenantId={activeTenant.id}
              workerManifests={workerManifestsQuery.data?.items ?? []}
              workerManifestsError={workerManifestsQuery.error}
              workerManifestsLoading={workerManifestsQuery.isPending}
            />
          ) : null}
          <ProjectSessionSettingsSection
            credentials={credentialsQuery.data?.items ?? []}
            executionTargets={executionTargetsQuery.data?.items ?? []}
            tenantId={activeTenant.id}
            organizations={controlPlane.organizations.filter(
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
                  <p className="text-[11px] font-medium text-foreground">
                    One-time invitation token
                  </p>
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
