package authorization

type Permission string

const SupportReadOnlyRole = "support_readonly"

const (
	TenantRead             Permission = "tenant.read"
	TenantUpdate           Permission = "tenant.update"
	TenantDelete           Permission = "tenant.delete"
	TenantMembersRead      Permission = "tenant.members.read"
	TenantMembersInvite    Permission = "tenant.members.invite"
	TenantMembersUpdate    Permission = "tenant.members.update"
	TenantMembersRemove    Permission = "tenant.members.remove"
	OrganizationRead       Permission = "organization.read"
	OrganizationUpdate     Permission = "organization.update"
	OrganizationMembers    Permission = "organization.members.manage"
	ProjectCreate          Permission = "project.create"
	ProjectRead            Permission = "project.read"
	ProjectUpdate          Permission = "project.update"
	ProjectDelete          Permission = "project.delete"
	SessionCreate          Permission = "session.create"
	SessionRead            Permission = "session.read"
	SessionShare           Permission = "session.share"
	SessionArchive         Permission = "session.archive"
	SessionDelete          Permission = "session.delete"
	ExecutionCreate        Permission = "execution.create"
	ExecutionCancel        Permission = "execution.cancel"
	ExecutionApprove       Permission = "execution.approve"
	ExecutionReadLogs      Permission = "execution.read_logs"
	ArtifactRead           Permission = "artifact.read"
	ArtifactWrite          Permission = "artifact.write"
	ArtifactDelete         Permission = "artifact.delete"
	CredentialsRead        Permission = "credentials.read"
	CredentialsUse         Permission = "credentials.use"
	CredentialsManage      Permission = "credentials.manage"
	WorkerRead             Permission = "worker.read"
	WorkerManage           Permission = "worker.manage"
	AuditRead              Permission = "audit.read"
	CostManage             Permission = "cost.manage"
	QuotaRead              Permission = "quota.read"
	QuotaManage            Permission = "quota.manage"
	RetentionRead          Permission = "retention.read"
	RetentionManage        Permission = "retention.manage"
	LifecycleRead          Permission = "lifecycle.read"
	LifecycleManage        Permission = "lifecycle.manage"
	SchedulingPolicyRead   Permission = "scheduling_policy.read"
	SchedulingPolicyManage Permission = "scheduling_policy.manage"
	IdentityRead           Permission = "identity.read"
	IdentityManage         Permission = "identity.manage"
	IdentitySessionsRevoke Permission = "identity.sessions.revoke"
	ServiceAccountsRead    Permission = "service_accounts.read"
	ServiceAccountsManage  Permission = "service_accounts.manage"
	OutboxRead             Permission = "outbox.read"
	OutboxManage           Permission = "outbox.manage"
)

var tenantRolePermissions = map[string]map[Permission]struct{}{
	"owner": permissionSet(
		TenantRead, TenantUpdate, TenantDelete, TenantMembersRead, TenantMembersInvite,
		TenantMembersUpdate, TenantMembersRemove, OrganizationRead, OrganizationUpdate,
		OrganizationMembers, ProjectCreate, ProjectRead, ProjectUpdate, ProjectDelete,
		SessionCreate, SessionRead, SessionShare, SessionArchive, SessionDelete,
		ExecutionCreate, ExecutionCancel, ExecutionApprove, ExecutionReadLogs,
		ArtifactRead, ArtifactWrite, ArtifactDelete,
		CredentialsRead, CredentialsUse, CredentialsManage, WorkerRead, WorkerManage, AuditRead, CostManage,
		QuotaRead, QuotaManage, RetentionRead, RetentionManage, LifecycleRead, LifecycleManage,
		SchedulingPolicyRead, SchedulingPolicyManage,
		IdentityRead, IdentityManage, IdentitySessionsRevoke, ServiceAccountsRead, ServiceAccountsManage,
		OutboxRead, OutboxManage,
	),
	"admin": permissionSet(
		TenantRead, TenantUpdate, TenantMembersRead, TenantMembersInvite, TenantMembersUpdate,
		TenantMembersRemove, OrganizationRead, OrganizationUpdate, OrganizationMembers,
		ProjectCreate, ProjectRead, ProjectUpdate, ProjectDelete, SessionCreate, SessionRead,
		SessionShare, SessionArchive, SessionDelete, ExecutionCreate, ExecutionCancel,
		ExecutionApprove, ExecutionReadLogs, WorkerRead, WorkerManage, AuditRead,
		ArtifactRead, ArtifactWrite, ArtifactDelete, CredentialsUse, QuotaRead, QuotaManage,
		RetentionRead, RetentionManage, LifecycleRead, LifecycleManage,
		SchedulingPolicyRead, SchedulingPolicyManage,
		IdentityRead, IdentitySessionsRevoke, ServiceAccountsRead, ServiceAccountsManage,
		OutboxRead, OutboxManage,
	),
	"security_admin": permissionSet(
		TenantRead, TenantMembersRead, OrganizationRead, ProjectRead, SessionRead,
		ExecutionReadLogs, ArtifactRead, CredentialsRead, CredentialsUse, CredentialsManage, WorkerRead, AuditRead,
		RetentionRead, RetentionManage, LifecycleRead,
		SchedulingPolicyRead, SchedulingPolicyManage,
		IdentityRead, IdentityManage, IdentitySessionsRevoke, ServiceAccountsRead, ServiceAccountsManage,
		OutboxRead,
	),
	"cost_admin": permissionSet(TenantRead, TenantMembersRead, CostManage, QuotaRead, QuotaManage),
	"auditor":    permissionSet(TenantRead, TenantMembersRead, OrganizationRead, ProjectRead, SessionRead, ExecutionReadLogs, ArtifactRead, AuditRead, QuotaRead, RetentionRead, LifecycleRead, SchedulingPolicyRead, OutboxRead),
	"member":     permissionSet(TenantRead),
	SupportReadOnlyRole: permissionSet(
		TenantRead, TenantMembersRead, OrganizationRead, ProjectRead, SessionRead,
		ExecutionReadLogs, ArtifactRead, CredentialsRead, WorkerRead, AuditRead,
		QuotaRead, RetentionRead, LifecycleRead, SchedulingPolicyRead, IdentityRead,
		ServiceAccountsRead, OutboxRead,
	),
}

var organizationRolePermissions = map[string]map[Permission]struct{}{
	"owner": permissionSet(
		OrganizationRead, OrganizationUpdate, OrganizationMembers, ProjectCreate, ProjectRead,
		ProjectUpdate, ProjectDelete, SessionCreate, SessionRead, SessionShare, SessionArchive,
		SessionDelete, ExecutionCreate, ExecutionCancel, ExecutionApprove, ExecutionReadLogs,
		ArtifactRead, ArtifactWrite, ArtifactDelete, CredentialsUse,
		SchedulingPolicyRead, SchedulingPolicyManage,
	),
	"admin": permissionSet(
		OrganizationRead, OrganizationUpdate, OrganizationMembers, ProjectCreate, ProjectRead,
		ProjectUpdate, ProjectDelete, SessionCreate, SessionRead, SessionShare, SessionArchive,
		SessionDelete, ExecutionCreate, ExecutionCancel, ExecutionApprove, ExecutionReadLogs,
		ArtifactRead, ArtifactWrite, ArtifactDelete, CredentialsUse,
		SchedulingPolicyRead, SchedulingPolicyManage,
	),
	"agent_operator": permissionSet(
		OrganizationRead, ProjectRead, SessionCreate, SessionRead, SessionShare, SessionArchive,
		ExecutionCreate, ExecutionCancel, ExecutionApprove, ExecutionReadLogs,
		ArtifactRead, ArtifactWrite, ArtifactDelete, CredentialsUse,
		SchedulingPolicyRead,
	),
	"member": permissionSet(
		OrganizationRead, ProjectRead, SessionCreate, SessionRead, SessionArchive,
		ExecutionCreate, ExecutionCancel, ExecutionReadLogs, ArtifactRead, ArtifactWrite, CredentialsUse,
		SchedulingPolicyRead,
	),
	"viewer": permissionSet(OrganizationRead, ProjectRead, SessionRead, ExecutionReadLogs, ArtifactRead, SchedulingPolicyRead),
}

func TenantAllows(role string, permission Permission) bool {
	_, allowed := tenantRolePermissions[role][permission]
	return allowed
}

func OrganizationAllows(role string, permission Permission) bool {
	_, allowed := organizationRolePermissions[role][permission]
	return allowed
}

func permissionSet(values ...Permission) map[Permission]struct{} {
	result := make(map[Permission]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
