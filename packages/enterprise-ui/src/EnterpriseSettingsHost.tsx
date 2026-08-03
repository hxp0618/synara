// FILE: EnterpriseSettingsHost.tsx
// Purpose: Inject Web-owned project/session and execution-target extensions into Tenant Overview.
// Layer: Public enterprise host boundary

import { createContext, useContext, type ComponentType, type ReactNode } from "react";

import type {
  ControlPlaneCredential,
  ControlPlaneExecutionTarget,
  ControlPlaneOrganization,
  ControlPlaneResourceLifecycleConfig,
  ControlPlaneWorker,
  ControlPlaneWorkerManifest,
} from "@synara/control-plane-client";

export type EnterpriseExecutionTargetsSectionProps = {
  tenantId: string;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  targets: ReadonlyArray<ControlPlaneExecutionTarget>;
  workerManifests: ReadonlyArray<ControlPlaneWorkerManifest>;
  workerManifestsLoading: boolean;
  workerManifestsError: unknown;
  workers: ReadonlyArray<ControlPlaneWorker>;
  workersLoading: boolean;
  workersError: unknown;
  canManage: boolean;
  canManageCredentialBindings: boolean;
  credentials: ReadonlyArray<ControlPlaneCredential>;
  requireOrganizationScope: boolean;
  isLoading: boolean;
  error: unknown;
  onCreated: (target: ControlPlaneExecutionTarget) => void;
  onUpdated: (targetId: string, status: ControlPlaneExecutionTarget["status"]) => void;
  onProviderPolicyUpdated: (target: ControlPlaneExecutionTarget) => void;
  onWorkersChanged?: () => void;
};

export type EnterpriseProjectSessionsSectionProps = {
  tenantId: string;
  userId: string;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  executionTargets: ReadonlyArray<ControlPlaneExecutionTarget>;
  credentials: ReadonlyArray<ControlPlaneCredential>;
  canReadProjects: boolean;
  canManageProjectLifecycle: boolean;
  resourceLifecycleConfig: ControlPlaneResourceLifecycleConfig | null;
};

export type EnterpriseSettingsHostComponents = {
  ExecutionTargetsSection: ComponentType<EnterpriseExecutionTargetsSectionProps>;
  ProjectSessionsSection: ComponentType<EnterpriseProjectSessionsSectionProps>;
};

const EnterpriseSettingsHostContext = createContext<EnterpriseSettingsHostComponents | null>(null);

export function EnterpriseSettingsHostProvider(props: {
  components: EnterpriseSettingsHostComponents;
  children: ReactNode;
}) {
  return (
    <EnterpriseSettingsHostContext.Provider value={props.components}>
      {props.children}
    </EnterpriseSettingsHostContext.Provider>
  );
}

export function useEnterpriseSettingsHost(): EnterpriseSettingsHostComponents {
  const host = useContext(EnterpriseSettingsHostContext);
  if (!host) {
    throw new Error(
      "Enterprise Tenant settings must be rendered inside EnterpriseSettingsHostProvider.",
    );
  }
  return host;
}
