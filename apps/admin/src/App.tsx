// FILE: App.tsx
// Purpose: Enforce authentication, Platform authority, and support_readonly boundaries before mounting Admin workflows.
// Layer: Admin application boundary

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  ControlPlaneError,
  type ControlPlaneSessionState,
  type ControlPlaneTenantAccess,
} from "@synara/control-plane-client";
import { IconExternalLink, IconLock, IconShieldOff, IconUserShield } from "@tabler/icons-react";
import { useState, type ReactNode } from "react";

import { SynaraLogo } from "./components/SynaraLogo";
import {
  Button,
  Field,
  InlineError,
  Input,
  LoadingState,
  Select,
  StatusPill,
} from "./components/ui";
import { navigateToTenantApp } from "./platform/navigation";
import { PlatformAdminShell } from "./platform/PlatformAdminShell";
import { platformQueryKeys } from "./platform/platformQueries";

export const adminSessionQueryKey = ["control-plane", "admin", "session"] as const;

function isUnauthenticated(error: unknown): boolean {
  return error instanceof ControlPlaneError && error.status === 401;
}

function isAuthorityDenied(error: unknown): boolean {
  return error instanceof ControlPlaneError && error.status === 403;
}

function authorityCandidates(
  session: ControlPlaneSessionState,
): ReadonlyArray<ControlPlaneTenantAccess> {
  return session.tenants.filter(
    (tenant) =>
      tenant.status === "active" &&
      tenant.role !== "support_readonly" &&
      (tenant.role === "owner" || tenant.role === "admin" || tenant.role === "security_admin"),
  );
}

export function AdminApp() {
  const session = useQuery({
    queryKey: adminSessionQueryKey,
    queryFn: controlPlaneClient.getSession,
    retry: false,
  });

  if (session.isPending)
    return (
      <FullPageState>
        <LoadingState label="Checking Platform session…" />
      </FullPageState>
    );
  if (session.error) {
    return isUnauthenticated(session.error) ? (
      <AdminLogin />
    ) : (
      <FullPageState
        title="Platform Admin is unavailable"
        description="The authenticated Control Plane session could not be loaded."
      >
        <InlineError error={session.error} />
        <Button onClick={() => void session.refetch()}>Retry</Button>
      </FullPageState>
    );
  }
  if (session.data.user.supportAccessGrantId) {
    return <SupportReadonlyBoundary session={session.data} />;
  }
  return <PlatformAuthorityGate session={session.data} />;
}

function PlatformAuthorityGate(props: { readonly session: ControlPlaneSessionState }) {
  const overview = useQuery({
    queryKey: platformQueryKeys.tenants,
    queryFn: controlPlaneClient.listPlatformTenants,
    retry: false,
    refetchInterval: 30_000,
  });
  if (overview.isPending)
    return (
      <FullPageState>
        <LoadingState label="Confirming Platform authority…" />
      </FullPageState>
    );
  if (overview.error) {
    return isAuthorityDenied(overview.error) ? (
      <AuthorityTenantChooser session={props.session} error={overview.error} />
    ) : (
      <FullPageState
        title="Platform operations are unavailable"
        description="This deployment has not exposed an authorized Platform operations surface."
      >
        <InlineError error={overview.error} />
        <Button onClick={() => void overview.refetch()}>Retry</Button>
      </FullPageState>
    );
  }
  return <PlatformAdminShell session={props.session} overview={overview.data} />;
}

function AdminLogin() {
  const queryClient = useQueryClient();
  const [tenantSlug, setTenantSlug] = useState("");
  const [connections, setConnections] = useState<ReadonlyArray<{ id: string; name: string }>>([]);
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const discover = useMutation({
    mutationFn: () => controlPlaneClient.listPublicIdentityConnections(tenantSlug),
    onSuccess: (result) => setConnections(result.items),
  });
  const startSSO = useMutation({
    mutationFn: controlPlaneClient.startPlatformAdminSSO,
    onSuccess: ({ authorizationUrl }) => window.location.assign(authorizationUrl),
  });
  const devLogin = useMutation({
    mutationFn: () => controlPlaneClient.devLogin({ email, displayName }),
    onSuccess: (result) => queryClient.setQueryData(adminSessionQueryKey, result),
  });
  const devLoginEnabled = import.meta.env.VITE_ENABLE_DEV_LOGIN?.toLowerCase() === "true";

  return (
    <div className="auth-shell">
      <div className="auth-brand">
        <SynaraLogo aria-label="Synara" />
        <div>
          <strong>Synara</strong>
          <span>Platform Admin</span>
        </div>
      </div>
      <main className="auth-panel">
        <IconLock aria-hidden size={24} />
        <h1>Sign in to Platform authority</h1>
        <p>This application has a separate authentication and role gate from Tenant Settings.</p>
        <form
          className="auth-form"
          onSubmit={(event) => {
            event.preventDefault();
            discover.mutate();
          }}
        >
          <Field label="Operator Tenant slug">
            <Input
              autoCapitalize="none"
              autoComplete="organization"
              onChange={(event) => setTenantSlug(event.target.value.toLowerCase())}
              required
              value={tenantSlug}
            />
          </Field>
          <Button disabled={discover.isPending} type="submit" variant="primary">
            {discover.isPending ? "Looking up identity…" : "Continue with enterprise identity"}
          </Button>
          <InlineError error={discover.error ?? startSSO.error} />
        </form>
        {connections.length > 0 ? (
          <div className="identity-connections">
            {connections.map((connection) => (
              <Button
                disabled={startSSO.isPending}
                key={connection.id}
                onClick={() => startSSO.mutate(connection.id)}
              >
                {connection.name}
                <IconExternalLink aria-hidden size={15} />
              </Button>
            ))}
          </div>
        ) : null}
        {devLoginEnabled ? (
          <form
            className="auth-form auth-form--dev"
            onSubmit={(event) => {
              event.preventDefault();
              devLogin.mutate();
            }}
          >
            <p>Development bootstrap</p>
            <Field label="Email">
              <Input
                onChange={(event) => setEmail(event.target.value)}
                required
                type="email"
                value={email}
              />
            </Field>
            <Field label="Display name">
              <Input
                onChange={(event) => setDisplayName(event.target.value)}
                required
                value={displayName}
              />
            </Field>
            <Button disabled={devLogin.isPending} type="submit">
              {devLogin.isPending ? "Signing in…" : "Development sign in"}
            </Button>
            <InlineError error={devLogin.error} />
          </form>
        ) : null}
      </main>
    </div>
  );
}

function AuthorityTenantChooser(props: {
  readonly session: ControlPlaneSessionState;
  readonly error: unknown;
}) {
  const queryClient = useQueryClient();
  const candidates = authorityCandidates(props.session);
  const [tenantId, setTenantId] = useState(candidates[0]?.id ?? "");
  const switchTenant = useMutation({
    mutationFn: () => controlPlaneClient.setActiveTenant(tenantId),
    onSuccess: async (nextSession) => {
      queryClient.setQueryData(adminSessionQueryKey, nextSession);
      await queryClient.invalidateQueries({ queryKey: platformQueryKeys.tenants });
    },
  });
  const logout = useMutation({
    mutationFn: controlPlaneClient.logout,
    onSuccess: () => queryClient.removeQueries(),
  });
  return (
    <FullPageState
      icon={<IconShieldOff aria-hidden size={28} />}
      title="Platform authority is not active"
      description="Switch to the dedicated Operator Tenant. Internal Tenant roles never become Platform authority."
    >
      {candidates.length > 0 ? (
        <form
          className="authority-switcher"
          onSubmit={(event) => {
            event.preventDefault();
            switchTenant.mutate();
          }}
        >
          <Field label="Eligible Tenant membership">
            <Select onChange={(event) => setTenantId(event.target.value)} value={tenantId}>
              {candidates.map((tenant) => (
                <option key={tenant.id} value={tenant.id}>
                  {tenant.name} · {tenant.role}
                </option>
              ))}
            </Select>
          </Field>
          <Button disabled={switchTenant.isPending} type="submit" variant="primary">
            Activate authority Tenant
          </Button>
          <InlineError error={switchTenant.error} />
        </form>
      ) : (
        <p className="state-copy">
          No active owner, admin, or security membership is available in this session.
        </p>
      )}
      <InlineError error={props.error} />
      <Button disabled={logout.isPending} onClick={() => logout.mutate()} variant="ghost">
        Sign out
      </Button>
    </FullPageState>
  );
}

function SupportReadonlyBoundary(props: { readonly session: ControlPlaneSessionState }) {
  const queryClient = useQueryClient();
  const candidates = authorityCandidates(props.session);
  const [tenantId, setTenantId] = useState(candidates[0]?.id ?? "");
  const exitSupport = useMutation({
    mutationFn: () => controlPlaneClient.setActiveTenant(tenantId),
    onSuccess: async (nextSession) => {
      queryClient.setQueryData(adminSessionQueryKey, nextSession);
      await queryClient.invalidateQueries({ queryKey: platformQueryKeys.tenants });
    },
  });
  const activeTenant = props.session.tenants.find(
    (tenant) => tenant.id === props.session.user.activeTenantId,
  );
  return (
    <div className="support-boundary">
      <header className="authority-strip authority-strip--blocked">
        <div>
          <IconUserShield aria-hidden size={18} />
          <span>Platform authority</span>
          <StatusPill value="paused" tone="warning" />
        </div>
        <span>
          Tenant context: <strong>support_readonly</strong>
        </span>
      </header>
      <FullPageState
        title="Tenant Support View is active"
        description={`This session is scoped read-only to ${activeTenant?.name ?? "the selected internal Tenant"}. Global Platform queries and writes are not mounted.`}
      >
        <Button onClick={() => void navigateToTenantApp()} variant="primary">
          Open internal Tenant Web
        </Button>
        {candidates.length > 0 ? (
          <form
            className="authority-switcher"
            onSubmit={(event) => {
              event.preventDefault();
              exitSupport.mutate();
            }}
          >
            <Field label="Return to authority Tenant">
              <Select onChange={(event) => setTenantId(event.target.value)} value={tenantId}>
                {candidates.map((tenant) => (
                  <option key={tenant.id} value={tenant.id}>
                    {tenant.name} · {tenant.role}
                  </option>
                ))}
              </Select>
            </Field>
            <Button disabled={exitSupport.isPending} type="submit">
              Exit Tenant context
            </Button>
            <InlineError error={exitSupport.error} />
          </form>
        ) : null}
      </FullPageState>
    </div>
  );
}

function FullPageState(props: {
  readonly title?: string;
  readonly description?: string;
  readonly icon?: ReactNode;
  readonly children: ReactNode;
}) {
  return (
    <div className="full-page-state">
      <div className="full-page-state__brand">
        <SynaraLogo aria-hidden />
        <span>Platform Admin</span>
      </div>
      <main>
        {props.icon}
        {props.title ? <h1>{props.title}</h1> : null}
        {props.description ? <p>{props.description}</p> : null}
        <div className="full-page-state__actions">{props.children}</div>
      </main>
    </div>
  );
}
