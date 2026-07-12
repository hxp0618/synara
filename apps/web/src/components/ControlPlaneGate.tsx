import { useState, type FormEvent, type ReactNode } from "react";

import { APP_DISPLAY_NAME } from "../branding";
import { useControlPlane } from "../controlPlaneContext";
import { controlPlaneClient } from "../lib/controlPlaneClient";
import { Button } from "./ui/button";
import { Input } from "./ui/input";

export function ControlPlaneGate({ children }: { children: ReactNode }) {
  const controlPlane = useControlPlane();
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [tenantSlug, setTenantSlug] = useState("");
  const [pendingAction, setPendingAction] = useState<string | null>(null);
  const [actionError, setActionError] = useState<Error | null>(null);

  if (controlPlane.availability === "local") return children;
  if (controlPlane.isAuthoritative && controlPlane.activeTenant) return children;

  const run = async (key: string, action: () => Promise<void>) => {
    setPendingAction(key);
    setActionError(null);
    try {
      await action();
    } catch (error) {
      setActionError(error instanceof Error ? error : new Error("The request failed."));
    } finally {
      setPendingAction(null);
    }
  };
  const login = (event: FormEvent) => {
    event.preventDefault();
    void run("dev-login", () => controlPlane.devLogin({ email, displayName }));
  };
  const startSSO = (event: FormEvent) => {
    event.preventDefault();
    void run("sso", async () => {
      const connections = await controlPlaneClient.listPublicIdentityConnections(
        tenantSlug.trim(),
      );
      const connection = connections.items[0];
      if (!connection) throw new Error("No active SSO connection was found for this Tenant.");
      const result = await controlPlaneClient.startSSO(connection.id, window.location.pathname);
      window.location.assign(result.authorizationUrl);
    });
  };
  const error = actionError ?? controlPlane.error ?? controlPlane.projectionError;

  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-5 text-foreground">
      <div className="w-full max-w-lg rounded-2xl border border-border bg-card p-6 shadow-xl">
        <p className="text-xs font-medium uppercase tracking-[0.18em] text-muted-foreground">
          {APP_DISPLAY_NAME} Control Plane
        </p>
        {controlPlane.availability === "detecting" ||
        controlPlane.authentication === "unknown" ? (
          <div className="mt-4">
            <h1 className="text-lg font-semibold">Connecting to the Control Plane…</h1>
            <p className="mt-2 text-sm text-muted-foreground">
              Checking platform availability and authentication state.
            </p>
          </div>
        ) : controlPlane.availability === "unavailable" ||
          controlPlane.authentication === "error" ? (
          <div className="mt-4">
            <h1 className="text-lg font-semibold">Control Plane unavailable</h1>
            <p className="mt-2 text-sm text-muted-foreground">
              Remote Sessions stay read-only until the authoritative service is reachable. No local
              fallback Session will be created.
            </p>
            {error ? <p className="mt-3 text-sm text-destructive">{error.message}</p> : null}
            <Button
              className="mt-5"
              disabled={pendingAction === "retry"}
              onClick={() => void run("retry", controlPlane.retry)}
            >
              {pendingAction === "retry" ? "Retrying…" : "Retry"}
            </Button>
          </div>
        ) : controlPlane.authentication === "unauthenticated" ? (
          <div className="mt-4 space-y-6">
            <div>
              <h1 className="text-lg font-semibold">Sign in to continue</h1>
              <p className="mt-2 text-sm text-muted-foreground">
                This deployment uses the Control Plane as the authoritative Project and Session
                store.
              </p>
            </div>
            {controlPlane.profile?.profile !== "enterprise" ? (
              <form className="grid gap-3" onSubmit={login}>
                <Input
                  autoComplete="email"
                  onChange={(event) => setEmail(event.target.value)}
                  placeholder="you@company.com"
                  required
                  type="email"
                  value={email}
                />
                <Input
                  autoComplete="name"
                  onChange={(event) => setDisplayName(event.target.value)}
                  placeholder="Display name"
                  required
                  value={displayName}
                />
                <Button disabled={pendingAction !== null} type="submit">
                  {pendingAction === "dev-login" ? "Signing in…" : "Sign in for local SaaS"}
                </Button>
              </form>
            ) : null}
            <form className="grid gap-3 border-t border-border pt-5" onSubmit={startSSO}>
              <Input
                autoComplete="organization"
                onChange={(event) => setTenantSlug(event.target.value.toLowerCase())}
                placeholder="Tenant slug"
                required
                value={tenantSlug}
              />
              <Button disabled={pendingAction !== null} type="submit" variant="outline">
                {pendingAction === "sso" ? "Opening SSO…" : "Continue with enterprise SSO"}
              </Button>
            </form>
            {error ? <p className="text-sm text-destructive">{error.message}</p> : null}
          </div>
        ) : (
          <div className="mt-4">
            <h1 className="text-lg font-semibold">Choose a Tenant</h1>
            <p className="mt-2 text-sm text-muted-foreground">
              A Tenant must be active before Projects and Sessions can be loaded.
            </p>
            <div className="mt-4 grid gap-2">
              {controlPlane.session?.tenants.map((tenant) => (
                <Button
                  key={tenant.id}
                  disabled={pendingAction !== null || tenant.status !== "active"}
                  onClick={() =>
                    void run(`tenant-${tenant.id}`, () => controlPlane.setActiveTenant(tenant.id))
                  }
                  variant="outline"
                >
                  {tenant.name} · {tenant.role}
                </Button>
              ))}
            </div>
            {controlPlane.session?.tenants.length === 0 ? (
              <p className="mt-4 text-sm text-muted-foreground">
                This account has no active Tenant membership.
              </p>
            ) : null}
            {error ? <p className="mt-3 text-sm text-destructive">{error.message}</p> : null}
          </div>
        )}
      </div>
    </div>
  );
}
