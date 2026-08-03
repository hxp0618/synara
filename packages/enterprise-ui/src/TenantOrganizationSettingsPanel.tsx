// FILE: TenantOrganizationSettingsPanel.tsx
// Purpose: Compose one capability-filtered Stage 6 Tenant settings destination at a time.
// Layer: Enterprise feature implementation (Phase A migration source)
// Exports: TenantOrganizationSettingsPanel

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  controlPlaneClient,
  type ControlPlaneExecutionTarget,
  type TenantInvitation,
} from "@synara/control-plane-client";

import { useEnterpriseControlPlaneRuntime } from "./EnterpriseControlPlaneRuntime";
import { useEnterpriseSettingsHost } from "./EnterpriseSettingsHost";
import { useEnterpriseUiHost } from "./EnterpriseUiHost";
import { TenantAuditSettingsSection } from "./TenantAuditSettingsSection";
import {
  credentialsQueryKey,
  TenantCredentialSettingsSection,
} from "./TenantCredentialSettingsSection";
import { TenantDataResidencySettingsSection } from "./TenantDataResidencySettingsSection";
import { TenantDeletionRecoverySettingsSection } from "./TenantDeletionRecoverySettingsSection";
import { TenantIdentitySettingsSection } from "./TenantIdentitySettingsSection";
import { TenantLegalHoldsSettingsSection } from "./TenantLegalHoldsSettingsSection";
import { TenantLifecyclePolicySettingsSection } from "./TenantLifecyclePolicySettingsSection";
import { TenantMemberLifecycleControls } from "./TenantMemberLifecycleControls";
import { TenantOutboxSettingsSection } from "./TenantOutboxSettingsSection";
import { TenantPrivacyRequestsSettingsSection } from "./TenantPrivacyRequestsSettingsSection";
import { TenantQuotaSettingsSection } from "./TenantQuotaSettingsSection";
import { TenantRetentionSettingsSection } from "./TenantRetentionSettingsSection";
import { TenantServiceAccountSettingsSection } from "./TenantServiceAccountSettingsSection";
import { TenantStatusLifecycleSettingsSection } from "./TenantStatusLifecycleSettingsSection";
import { tenantLifecycleDisplayLabel } from "./TenantStatusLifecycleSettingsSection";
import { TenantSupportAccessSettingsSection } from "./TenantSupportAccessSettingsSection";
import { TenantUsageSettingsSection } from "./TenantUsageSettingsSection";
import {
  resolveEnterpriseSettingsNavItems,
  type EnterpriseSettingsSectionId,
} from "./settingsRegistration";

export const enterpriseTenantWorkersQueryKey = (tenantId: string | null) =>
  ["control-plane", "tenants", tenantId, "workers"] as const;

const settingsQueryKeys = {
  members: (tenantId: string | null) => ["control-plane", "tenants", tenantId, "members"] as const,
  executionTargets: (tenantId: string | null) =>
    ["control-plane", "tenants", tenantId, "execution-targets"] as const,
  workerManifests: (tenantId: string | null) =>
    ["control-plane", "tenants", tenantId, "worker-manifests"] as const,
  organizations: (tenantId: string | null) =>
    ["control-plane", "tenants", tenantId, "organizations"] as const,
  workers: enterpriseTenantWorkersQueryKey,
};

function LoginPanel() {
  const controlPlane = useEnterpriseControlPlaneRuntime();
  const {
    Button,
    FormField,
    InlineError,
    Input,
    SettingsRow,
    SettingsSection,
    formGridClassName,
    navigateToUrl,
  } = useEnterpriseUiHost();
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
    onSuccess: ({ authorizationUrl }) => navigateToUrl(authorizationUrl),
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    login.mutate({ email, displayName });
  };

  return (
    <SettingsSection title="Self-hosted control plane">
      <SettingsRow
        title="Sign in to the local control plane"
        description="Development bootstrap creates a local self-hosted identity, personal tenant, and root organization. Production deployments should replace this with enterprise SSO."
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
              {login.isPending ? "Signing in…" : "Create local identity"}
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

export function TenantOrganizationSettingsPanel(props: {
  destination: EnterpriseSettingsSectionId;
}) {
  const controlPlane = useEnterpriseControlPlaneRuntime();
  const { Button, SettingsListRow, SettingsSection } = useEnterpriseUiHost();

  if (
    controlPlane.availability === "detecting" ||
    (controlPlane.availability === "available" && controlPlane.authentication === "unknown")
  ) {
    return (
      <SettingsSection title="Self-hosted control plane">
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
      <SettingsSection title="Self-hosted control plane">
        <SettingsListRow
          title="Local mode"
          description="This Synara instance has no enterprise Control Plane configured. Local Projects and chats remain authoritative."
        />
      </SettingsSection>
    );
  }

  if (controlPlane.error || controlPlane.availability === "unavailable") {
    return (
      <SettingsSection title="Self-hosted control plane">
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

  if (!controlPlane.session) return null;

  const destinationAvailable = resolveEnterpriseSettingsNavItems({
    availability: controlPlane.availability,
    authentication: controlPlane.authentication,
    hasSession: true,
    hasActiveTenant: controlPlane.activeTenant !== null,
    capabilities: controlPlane.capabilities,
  }).some((item) => item.id === props.destination);
  if (!destinationAvailable) {
    return (
      <SettingsSection title="Tenant settings">
        <SettingsListRow
          title="This destination is unavailable"
          description="The authenticated Tenant context does not expose the capability required for this page. Server authorization remains authoritative for every API request."
        />
      </SettingsSection>
    );
  }

  return <AuthenticatedTenantPanel destination={props.destination} />;
}

function AuthenticatedTenantPanel(props: { destination: EnterpriseSettingsSectionId }) {
  const controlPlane = useEnterpriseControlPlaneRuntime();
  const {
    Button,
    FormField,
    InlineError,
    Input,
    SettingsListRow,
    SettingsRow,
    SettingsSection,
    StatusPill,
    formGridClassName,
    nativeSelectClassName,
  } = useEnterpriseUiHost();
  const {
    ExecutionTargetsSection: ExecutionTargetSettingsSection,
    ProjectSessionsSection: ProjectSessionSettingsSection,
  } = useEnterpriseSettingsHost();
  const queryClient = useQueryClient();
  const activeTenant = controlPlane.activeTenant;
  const activeTenantId = activeTenant?.id ?? null;
  const isOverview = props.destination === "organization-overview";
  const isMembers = props.destination === "organization-members";
  const isIdentity = props.destination === "organization-identity";
  const isCredentials = props.destination === "organization-credentials";
  const isUsage = props.destination === "organization-usage";
  const isData = props.destination === "organization-data";
  const isSupport = props.destination === "organization-support";
  const [organizationName, setOrganizationName] = useState("");
  const [organizationSlug, setOrganizationSlug] = useState("");
  const [organizationKind, setOrganizationKind] = useState<"team" | "department" | "personal">(
    "team",
  );
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState("member");
  const [createdInvitation, setCreatedInvitation] = useState<TenantInvitation | null>(null);

  const {
    canReadProjects,
    canUpdateProject,
    canReadMembers,
    canManageMembers,
    canReadExecutionTargets,
    canManageExecutionTargets,
    canReadQuota,
    canManageQuota,
    canManageCost,
    canReadRetention,
    canManageRetention,
    canReadLifecycle,
    canManageLifecycle,
    canReadSchedulingPolicy,
    canManageSchedulingPolicy,
    canReadAudit,
    canReadOutbox,
    canManageOutbox,
    canReadCredentials,
    canManageCredentials,
    canReadIdentity,
    canManageIdentity,
    canReadServiceAccounts,
    canManageServiceAccounts,
  } = controlPlane.capabilities;
  const credentialsQuery = useQuery({
    queryKey: credentialsQueryKey(activeTenantId ?? ""),
    queryFn: () => controlPlaneClient.listCredentials(activeTenantId!),
    enabled: activeTenantId !== null && canReadCredentials && (isOverview || isCredentials),
    retry: false,
  });
  const membersQuery = useQuery({
    queryKey: settingsQueryKeys.members(activeTenantId),
    queryFn: () => controlPlaneClient.listTenantMembers(activeTenantId!),
    enabled:
      activeTenantId !== null &&
      canReadMembers &&
      (isMembers || isIdentity || isCredentials || isData),
    retry: false,
  });
  const executionTargetsQuery = useQuery({
    queryKey: settingsQueryKeys.executionTargets(activeTenantId),
    queryFn: () => controlPlaneClient.listExecutionTargets(activeTenantId!),
    enabled: activeTenantId !== null && canReadExecutionTargets && isOverview,
    retry: false,
  });
  const workerManifestsQuery = useQuery({
    queryKey: settingsQueryKeys.workerManifests(activeTenantId),
    queryFn: () => controlPlaneClient.listWorkerManifests(activeTenantId!),
    enabled: activeTenantId !== null && canReadExecutionTargets && isOverview,
    retry: false,
  });
  const workersQuery = useQuery({
    queryKey: settingsQueryKeys.workers(activeTenantId),
    queryFn: () => controlPlaneClient.listWorkers(activeTenantId!),
    enabled: activeTenantId !== null && canReadExecutionTargets && isOverview,
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
        queryKey: settingsQueryKeys.organizations(activeTenantId),
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
      <div className="space-y-7">
        <SettingsSection title="Tenant access">
          <SettingsListRow
            title="No active tenant"
            description="This account has no active Tenant. Restore an owned deletion request below or ask an administrator for an invitation."
          />
        </SettingsSection>
        <TenantDeletionRecoverySettingsSection onRestored={controlPlane.retry} />
      </div>
    );
  }

  return (
    <div className="space-y-7">
      <SettingsSection title="Tenant context">
        <SettingsRow
          title={activeTenant.name}
          description={`${activeTenant.slug} · ${activeTenant.region} · ${activeTenant.entitlementProfileCode === "enterprise" ? "Enterprise" : activeTenant.entitlementProfileCode === "standard" ? "Standard" : activeTenant.entitlementProfileCode} profile`}
          status={
            <span className="flex flex-wrap gap-1.5">
              <StatusPill value={activeTenant.role} />
              <StatusPill value={tenantLifecycleDisplayLabel(activeTenant.status)} />
            </span>
          }
          control={
            (controlPlane.session?.tenants.length ?? 0) > 1 ? (
              <select
                aria-label="Active tenant"
                className={`${nativeSelectClassName} min-w-44`}
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
        {isOverview ? (
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
        ) : null}
      </SettingsSection>

      {isOverview ? (
        <TenantStatusLifecycleSettingsSection
          key={`tenant-status-${activeTenant.id}-${activeTenant.lifecycleVersion ?? "unknown"}`}
          tenant={activeTenant}
          onChanged={controlPlane.retry}
        />
      ) : null}

      {isOverview ? (
        <TenantDeletionRecoverySettingsSection onRestored={controlPlane.retry} />
      ) : null}

      {isSupport && canReadAudit ? (
        <TenantSupportAccessSettingsSection
          key={`support-access-${activeTenant.id}`}
          canManage={activeTenant.role === "owner" || activeTenant.role === "admin"}
          canReadQuota={canReadQuota}
          tenantId={activeTenant.id}
        />
      ) : null}

      {isUsage && canReadQuota ? (
        <>
          <TenantUsageSettingsSection
            key={`usage-${activeTenant.id}`}
            canManageCost={canManageCost}
            tenantId={activeTenant.id}
          />
          <TenantQuotaSettingsSection
            key={`quota-${activeTenant.id}`}
            canManage={canManageQuota}
            tenantId={activeTenant.id}
          />
        </>
      ) : null}

      {isData && canReadRetention ? (
        <>
          <TenantRetentionSettingsSection
            key={`retention-${activeTenant.id}`}
            canManage={canManageRetention}
            tenantId={activeTenant.id}
          />
          <TenantLegalHoldsSettingsSection
            key={`legal-holds-${activeTenant.id}`}
            canManage={canManageRetention}
            tenantId={activeTenant.id}
          />
        </>
      ) : null}

      {isData ? (
        <TenantPrivacyRequestsSettingsSection
          key={`privacy-requests-${activeTenant.id}`}
          canManage={canManageRetention}
          currentUserId={controlPlane.session?.user.userId ?? ""}
          members={membersQuery.data?.items ?? []}
          tenantId={activeTenant.id}
        />
      ) : null}

      {isData && canReadSchedulingPolicy ? (
        <TenantDataResidencySettingsSection
          key={`data-residency-${activeTenant.id}`}
          canManage={canManageSchedulingPolicy}
          homeRegion={activeTenant.region}
          tenantId={activeTenant.id}
          tenantName={activeTenant.name}
        />
      ) : null}

      {isData && canReadLifecycle && controlPlane.profile?.resourceLifecyclePolicy ? (
        <TenantLifecyclePolicySettingsSection
          key={`lifecycle-${activeTenant.id}`}
          canManage={canManageLifecycle}
          config={controlPlane.profile.resourceLifecyclePolicy}
          tenantId={activeTenant.id}
        />
      ) : null}

      {isSupport && canReadAudit ? (
        <TenantAuditSettingsSection key={`audit-${activeTenant.id}`} tenantId={activeTenant.id} />
      ) : null}

      {isSupport && canReadOutbox ? (
        <TenantOutboxSettingsSection
          key={`outbox-${activeTenant.id}`}
          canManage={canManageOutbox}
          tenantId={activeTenant.id}
        />
      ) : null}

      {isCredentials && canReadCredentials ? (
        <TenantCredentialSettingsSection
          key={`credentials-${activeTenant.id}`}
          canManage={canManageCredentials}
          members={membersQuery.data?.items ?? []}
          organizations={controlPlane.organizations.filter(
            (organization) => organization.status === "active",
          )}
          tenantId={activeTenant.id}
        />
      ) : null}

      {isIdentity && canReadIdentity ? (
        <TenantIdentitySettingsSection
          key={`identity-${activeTenant.id}`}
          canManage={canManageIdentity}
          members={membersQuery.data?.items ?? []}
          organizations={controlPlane.organizations.filter(
            (organization) => organization.status === "active",
          )}
          tenantId={activeTenant.id}
        />
      ) : null}

      {isIdentity && canReadServiceAccounts ? (
        <TenantServiceAccountSettingsSection
          key={`service-accounts-${activeTenant.id}`}
          canManage={canManageServiceAccounts}
          tenantId={activeTenant.id}
        />
      ) : null}

      {isOverview ? (
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
      ) : null}

      {isOverview && controlPlane.organizations.length > 0 ? (
        <>
          {canReadExecutionTargets ? (
            <ExecutionTargetSettingsSection
              canManage={canManageExecutionTargets}
              canManageCredentialBindings={canManageExecutionTargets && canManageCredentials}
              credentials={credentialsQuery.data?.items ?? []}
              error={executionTargetsQuery.error}
              isLoading={executionTargetsQuery.isPending}
              onCreated={(target) => {
                queryClient.setQueryData<{
                  items: ReadonlyArray<ControlPlaneExecutionTarget>;
                }>(settingsQueryKeys.executionTargets(activeTenantId), (current) => ({
                  items: [...(current?.items ?? []), target],
                }));
              }}
              onUpdated={(targetId, status) => {
                queryClient.setQueryData<{
                  items: ReadonlyArray<ControlPlaneExecutionTarget>;
                }>(settingsQueryKeys.executionTargets(activeTenantId), (current) => ({
                  items: (current?.items ?? []).map((target) =>
                    target.id === targetId ? { ...target, status } : target,
                  ),
                }));
              }}
              onProviderPolicyUpdated={(target) => {
                queryClient.setQueryData<{
                  items: ReadonlyArray<ControlPlaneExecutionTarget>;
                }>(settingsQueryKeys.executionTargets(activeTenantId), (current) => ({
                  items: (current?.items ?? []).map((candidate) =>
                    candidate.id === target.id ? target : candidate,
                  ),
                }));
              }}
              organizations={controlPlane.organizations.filter(
                (organization) => organization.status === "active",
              )}
              requireOrganizationScope={activeTenant.entitlementProfileCode === "personal"}
              targets={executionTargetsQuery.data?.items ?? []}
              tenantId={activeTenant.id}
              workerManifests={workerManifestsQuery.data?.items ?? []}
              workerManifestsError={workerManifestsQuery.error}
              workerManifestsLoading={workerManifestsQuery.isPending}
              workers={workersQuery.data?.items ?? []}
              workersError={workersQuery.error}
              workersLoading={workersQuery.isPending}
              onWorkersChanged={() =>
                void Promise.all([
                  queryClient.invalidateQueries({
                    queryKey: settingsQueryKeys.workers(activeTenantId),
                  }),
                  queryClient.invalidateQueries({
                    queryKey: settingsQueryKeys.workerManifests(activeTenantId),
                  }),
                ])
              }
            />
          ) : null}
          <ProjectSessionSettingsSection
            canManageProjectLifecycle={canUpdateProject}
            canReadProjects={canReadProjects}
            credentials={credentialsQuery.data?.items ?? []}
            executionTargets={executionTargetsQuery.data?.items ?? []}
            resourceLifecycleConfig={controlPlane.profile?.resourceLifecyclePolicy ?? null}
            tenantId={activeTenant.id}
            userId={controlPlane.session!.user.userId}
            organizations={controlPlane.organizations.filter(
              (organization) => organization.status === "active",
            )}
          />
        </>
      ) : null}

      {isMembers && canReadMembers ? (
        <SettingsSection title="Tenant members">
          {canManageMembers ? (
            <SettingsListRow
              title="Audited joiner, mover, and leaver controls"
              description="Suspension atomically revokes this Tenant's Login Sessions and user Credentials. Reactivation restores only the Tenant Membership; Organization access and Credentials must be assigned again deliberately."
            />
          ) : null}
          {membersQuery.data?.items.map((member) => (
            <SettingsListRow
              key={member.userId}
              title={member.displayName}
              description={member.email}
              actions={
                <div className="flex max-w-full flex-wrap items-center justify-end gap-1.5">
                  <StatusPill value={member.role} />
                  <StatusPill value={member.status} />
                  {canManageMembers ? (
                    <TenantMemberLifecycleControls
                      key={`${member.userId}-${member.updatedAt}`}
                      actorRole={activeTenant.role}
                      actorUserId={controlPlane.session!.user.userId}
                      member={member}
                      tenantId={activeTenant.id}
                      onChanged={async () => {
                        await queryClient.invalidateQueries({
                          queryKey: settingsQueryKeys.members(activeTenantId),
                        });
                      }}
                    />
                  ) : null}
                </div>
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
                    <option value="cost_admin">Usage & cost admin</option>
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
