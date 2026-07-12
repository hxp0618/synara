import { useState } from "react";

import { useControlPlane } from "../controlPlaneContext";
import { toastManager } from "./ui/toast";

export function ControlPlaneContextSwitcher() {
  const controlPlane = useControlPlane();
  const [switchingTenant, setSwitchingTenant] = useState(false);

  if (!controlPlane.isAuthoritative || !controlPlane.activeTenant) return null;

  return (
    <div className="mx-2 mb-1.5 rounded-xl border border-border/80 bg-foreground/[0.025] p-2">
      <div className="mb-1.5 flex items-center justify-between gap-2 px-0.5">
        <span className="text-[10px] font-semibold uppercase tracking-[0.12em] text-muted-foreground/70">
          SaaS context
        </span>
        <span className="text-[10px] text-muted-foreground/60">
          {controlPlane.profile?.profile}
        </span>
      </div>
      <div className="grid grid-cols-2 gap-1.5">
        <select
          aria-label="Active Tenant"
          className="h-7 min-w-0 rounded-md border border-border bg-background px-1.5 text-[11px] text-foreground outline-none focus:border-ring"
          disabled={switchingTenant}
          onChange={(event) => {
            setSwitchingTenant(true);
            void controlPlane
              .setActiveTenant(event.target.value)
              .catch((error: unknown) => {
                toastManager.add({
                  type: "error",
                  title: "Could not switch Tenant",
                  description: error instanceof Error ? error.message : "The request failed.",
                });
              })
              .finally(() => {
                setSwitchingTenant(false);
              });
          }}
          value={controlPlane.activeTenant.id}
        >
          {controlPlane.session?.tenants.map((tenant) => (
            <option key={tenant.id} disabled={tenant.status !== "active"} value={tenant.id}>
              {tenant.name}
            </option>
          ))}
        </select>
        <select
          aria-label="Active Organization"
          className="h-7 min-w-0 rounded-md border border-border bg-background px-1.5 text-[11px] text-foreground outline-none focus:border-ring"
          onChange={(event) => controlPlane.setActiveOrganization(event.target.value)}
          value={controlPlane.activeOrganization?.id ?? ""}
        >
          {controlPlane.organizations.map((organization) => (
            <option key={organization.id} value={organization.id}>
              {organization.name}
            </option>
          ))}
        </select>
      </div>
      {!controlPlane.capabilities.canCreateTurn ? (
        <p className="mt-1.5 px-0.5 text-[10px] text-amber-600 dark:text-amber-300">
          Read-only context
        </p>
      ) : null}
    </div>
  );
}
