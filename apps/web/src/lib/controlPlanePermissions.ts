import type {
  ControlPlaneOrganization,
  ControlPlaneTenantAccess,
} from "./controlPlaneClient";

export type ControlPlaneCapabilities = {
  canReadOrganizations: boolean;
  canManageOrganizations: boolean;
  canReadProjects: boolean;
  canCreateProject: boolean;
  canCreateSession: boolean;
  canCreateTurn: boolean;
  canInterruptExecution: boolean;
  canApproveExecution: boolean;
  canReadMembers: boolean;
  canManageMembers: boolean;
  canReadExecutionTargets: boolean;
  canManageExecutionTargets: boolean;
  canReadQuota: boolean;
  canManageQuota: boolean;
  canReadRetention: boolean;
  canManageRetention: boolean;
  canReadAudit: boolean;
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
  canCreateSession: false,
  canCreateTurn: false,
  canInterruptExecution: false,
  canApproveExecution: false,
  canReadMembers: false,
  canManageMembers: false,
  canReadExecutionTargets: false,
  canManageExecutionTargets: false,
  canReadQuota: false,
  canManageQuota: false,
  canReadRetention: false,
  canManageRetention: false,
  canReadAudit: false,
  canManageCredentials: false,
  canReadIdentity: false,
  canManageIdentity: false,
  canReadServiceAccounts: false,
  canManageServiceAccounts: false,
};

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
  const tenantBilling = tenantRole === "billing_admin";
  const tenantAuditor = tenantRole === "auditor";
  const organizationManager = organizationRole === "owner" || organizationRole === "admin";
  const organizationOperator = organizationManager || organizationRole === "agent_operator";
  const organizationMember = organizationOperator || organizationRole === "member";
  const organizationReader = organizationMember || organizationRole === "viewer";
  const mutationScopeActive = tenant.status === "active" && organization?.status === "active";
  const tenantProjectReader = tenantAdmin || tenantSecurity || tenantAuditor;
  const tenantProjectOperator = tenantAdmin;
  const canReadProjects = tenantProjectReader || organizationReader;
  const canCreateProject = mutationScopeActive && (tenantProjectOperator || organizationManager);
  const canCreateSession = mutationScopeActive && (tenantProjectOperator || organizationMember);
  const canCreateTurn = canCreateSession;

  return {
    canReadOrganizations: true,
    canManageOrganizations: tenant.status === "active" && tenantAdmin,
    canReadProjects,
    canCreateProject,
    canCreateSession,
    canCreateTurn,
    canInterruptExecution: canCreateTurn,
    canApproveExecution: mutationScopeActive && (tenantProjectOperator || organizationOperator),
    canReadMembers: tenantRole !== "member",
    canManageMembers: tenant.status === "active" && tenantAdmin,
    canReadExecutionTargets: tenantAdmin || tenantSecurity || tenantAuditor,
    canManageExecutionTargets: tenant.status === "active" && tenantAdmin,
    canReadQuota: tenantAdmin || tenantBilling || tenantAuditor,
    canManageQuota: tenant.status === "active" && (tenantAdmin || tenantBilling),
    canReadRetention: tenantAdmin || tenantSecurity || tenantAuditor,
    canManageRetention: tenant.status === "active" && (tenantAdmin || tenantSecurity),
    canReadAudit: tenantAdmin || tenantSecurity || tenantAuditor,
    canManageCredentials: tenant.status === "active" && (tenantOwner || tenantSecurity),
    canReadIdentity: tenantAdmin || tenantSecurity,
    canManageIdentity: tenant.status === "active" && (tenantOwner || tenantSecurity),
    canReadServiceAccounts: tenantAdmin || tenantSecurity,
    canManageServiceAccounts: tenant.status === "active" && (tenantAdmin || tenantSecurity),
  };
}
