// FILE: controlPlanePermissions.ts
// Purpose: Derive presentation capabilities from authoritative Tenant and Organization roles/state.

import type {
  ControlPlaneOrganization,
  ControlPlaneTenantAccess,
} from "@synara/control-plane-client";

export type ControlPlaneCapabilities = {
  canReadOrganizations: boolean;
  canManageOrganizations: boolean;
  canReadProjects: boolean;
  canCreateProject: boolean;
  canUpdateProject: boolean;
  canCreateSession: boolean;
  canCreateTurn: boolean;
  canSteerExecution: boolean;
  canInterruptExecution: boolean;
  canApproveExecution: boolean;
  canReadMembers: boolean;
  canManageMembers: boolean;
  canReadExecutionTargets: boolean;
  canManageExecutionTargets: boolean;
  canReadQuota: boolean;
  canManageQuota: boolean;
  canManageCost: boolean;
  canReadRetention: boolean;
  canManageRetention: boolean;
  canReadLifecycle: boolean;
  canManageLifecycle: boolean;
  canReadSchedulingPolicy: boolean;
  canManageSchedulingPolicy: boolean;
  canReadAudit: boolean;
  canReadOutbox: boolean;
  canManageOutbox: boolean;
  canReadCredentials: boolean;
  canManageCredentials: boolean;
  canReadIdentity: boolean;
  canManageIdentity: boolean;
  canReadServiceAccounts: boolean;
  canManageServiceAccounts: boolean;
};

const NO_CAPABILITIES: ControlPlaneCapabilities = {
  canReadOrganizations: false,
  canManageOrganizations: false,
  canReadProjects: false,
  canCreateProject: false,
  canUpdateProject: false,
  canCreateSession: false,
  canCreateTurn: false,
  canSteerExecution: false,
  canInterruptExecution: false,
  canApproveExecution: false,
  canReadMembers: false,
  canManageMembers: false,
  canReadExecutionTargets: false,
  canManageExecutionTargets: false,
  canReadQuota: false,
  canManageQuota: false,
  canManageCost: false,
  canReadRetention: false,
  canManageRetention: false,
  canReadLifecycle: false,
  canManageLifecycle: false,
  canReadSchedulingPolicy: false,
  canManageSchedulingPolicy: false,
  canReadAudit: false,
  canReadOutbox: false,
  canManageOutbox: false,
  canReadCredentials: false,
  canManageCredentials: false,
  canReadIdentity: false,
  canManageIdentity: false,
  canReadServiceAccounts: false,
  canManageServiceAccounts: false,
};

export function isControlPlaneTenantOperational(
  tenant: Pick<ControlPlaneTenantAccess, "status" | "evaluationExpiresAt">,
  now = Date.now(),
): boolean {
  if (tenant.status === "active") return true;
  if (
    tenant.status !== "evaluation" ||
    tenant.evaluationExpiresAt === null ||
    tenant.evaluationExpiresAt === undefined
  )
    return false;
  const evaluationExpiresAt = Date.parse(tenant.evaluationExpiresAt);
  return Number.isFinite(evaluationExpiresAt) && evaluationExpiresAt > now;
}

export function resolveControlPlaneCapabilities(input: {
  tenant: ControlPlaneTenantAccess | null;
  organization: ControlPlaneOrganization | null;
}): ControlPlaneCapabilities {
  const { tenant, organization } = input;
  if (!tenant) return NO_CAPABILITIES;

  const tenantRole = tenant.role;
  const organizationRole = organization?.currentUserRole ?? null;
  const tenantOwner = tenantRole === "owner";
  const tenantAdmin = tenantOwner || tenantRole === "admin";
  const tenantSecurity = tenantRole === "security_admin";
  const tenantCost = tenantRole === "cost_admin";
  const tenantAuditor = tenantRole === "auditor";
  const tenantSupport = tenantRole === "support_readonly";
  const organizationManager = organizationRole === "owner" || organizationRole === "admin";
  const organizationOperator = organizationManager || organizationRole === "agent_operator";
  const organizationMember = organizationOperator || organizationRole === "member";
  const organizationReader = organizationMember || organizationRole === "viewer";
  const tenantOperational = isControlPlaneTenantOperational(tenant);
  const mutationScopeActive = tenantOperational && organization?.status === "active";
  const tenantProjectReader = tenantAdmin || tenantSecurity || tenantAuditor || tenantSupport;
  const tenantProjectOperator = tenantAdmin;
  const canReadProjects = tenantProjectReader || organizationReader;
  const canCreateProject = mutationScopeActive && (tenantProjectOperator || organizationManager);
  const canUpdateProject = mutationScopeActive && (tenantProjectOperator || organizationManager);
  const canCreateSession = mutationScopeActive && (tenantProjectOperator || organizationMember);
  const canCreateTurn = canCreateSession;

  return {
    canReadOrganizations: true,
    canManageOrganizations: tenantOperational && tenantAdmin,
    canReadProjects,
    canCreateProject,
    canUpdateProject,
    canCreateSession,
    canCreateTurn,
    canSteerExecution: canCreateTurn,
    canInterruptExecution: canCreateTurn,
    canApproveExecution: mutationScopeActive && (tenantProjectOperator || organizationOperator),
    canReadMembers: tenantRole !== "member",
    canManageMembers: tenantOperational && tenantAdmin,
    canReadExecutionTargets: tenantAdmin || tenantSecurity || tenantSupport,
    canManageExecutionTargets: tenantOperational && tenantAdmin,
    canReadQuota: tenantAdmin || tenantCost || tenantAuditor || tenantSupport,
    canManageQuota: tenantOperational && (tenantAdmin || tenantCost),
    canManageCost: tenantOperational && (tenantOwner || tenantCost),
    canReadRetention: tenantAdmin || tenantSecurity || tenantAuditor || tenantSupport,
    canManageRetention: tenantOperational && (tenantAdmin || tenantSecurity),
    canReadLifecycle: tenantAdmin || tenantSecurity || tenantAuditor || tenantSupport,
    canManageLifecycle: tenantOperational && tenantAdmin,
    canReadSchedulingPolicy: tenantAdmin || tenantSecurity || tenantAuditor || tenantSupport,
    canManageSchedulingPolicy: tenantOperational && (tenantAdmin || tenantSecurity),
    canReadAudit: tenantAdmin || tenantSecurity || tenantAuditor || tenantSupport,
    canReadOutbox: tenantAdmin || tenantSecurity || tenantAuditor || tenantSupport,
    canManageOutbox: tenantOperational && tenantAdmin,
    canReadCredentials: tenantOwner || tenantSecurity || tenantSupport,
    canManageCredentials: tenantOperational && (tenantOwner || tenantSecurity),
    canReadIdentity: tenantAdmin || tenantSecurity || tenantSupport,
    canManageIdentity: tenantOperational && (tenantOwner || tenantSecurity),
    canReadServiceAccounts: tenantAdmin || tenantSecurity || tenantSupport,
    canManageServiceAccounts: tenantOperational && (tenantAdmin || tenantSecurity),
  };
}
