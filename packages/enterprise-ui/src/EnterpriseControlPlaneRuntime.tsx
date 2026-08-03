// FILE: EnterpriseControlPlaneRuntime.tsx
// Purpose: Inject the authenticated Control Plane state/actions required by Tenant settings.
// Layer: Public enterprise host boundary

import { createContext, useContext, type ReactNode } from "react";

import type {
  ControlPlaneOrganization,
  ControlPlanePlatformProfile,
  ControlPlaneSessionState,
  ControlPlaneTenantAccess,
} from "@synara/control-plane-client";

import type { ControlPlaneCapabilities } from "./controlPlanePermissions";

export type EnterpriseControlPlaneAvailability =
  | "detecting"
  | "local"
  | "available"
  | "unavailable";

export type EnterpriseControlPlaneAuthentication =
  | "unknown"
  | "unauthenticated"
  | "authenticated"
  | "error";

export type EnterpriseControlPlaneRuntime = {
  availability: EnterpriseControlPlaneAvailability;
  authentication: EnterpriseControlPlaneAuthentication;
  profile: ControlPlanePlatformProfile | null;
  session: ControlPlaneSessionState | null;
  activeTenant: ControlPlaneTenantAccess | null;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  capabilities: ControlPlaneCapabilities;
  error: Error | null;
  retry: () => Promise<void>;
  devLogin: (input: { email: string; displayName: string }) => Promise<void>;
  logout: () => Promise<void>;
  setActiveTenant: (tenantId: string) => Promise<void>;
};

const EnterpriseControlPlaneRuntimeContext = createContext<EnterpriseControlPlaneRuntime | null>(
  null,
);

export function EnterpriseControlPlaneRuntimeProvider(props: {
  runtime: EnterpriseControlPlaneRuntime;
  children: ReactNode;
}) {
  return (
    <EnterpriseControlPlaneRuntimeContext.Provider value={props.runtime}>
      {props.children}
    </EnterpriseControlPlaneRuntimeContext.Provider>
  );
}

export function useEnterpriseControlPlaneRuntime(): EnterpriseControlPlaneRuntime {
  const runtime = useContext(EnterpriseControlPlaneRuntimeContext);
  if (!runtime) {
    throw new Error(
      "Enterprise Tenant settings must be rendered inside EnterpriseControlPlaneRuntimeProvider.",
    );
  }
  return runtime;
}
