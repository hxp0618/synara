package authorization

import "testing"

func TestTenantPermissionsAreRoleBased(t *testing.T) {
	if !TenantAllows("owner", TenantDelete) || !TenantAllows("owner", BillingManage) {
		t.Fatal("owner must have every tenant permission")
	}
	if !TenantAllows("member", TenantRead) {
		t.Fatal("member must be able to read its tenant")
	}
	if TenantAllows("member", TenantMembersRead) || TenantAllows("admin", TenantDelete) {
		t.Fatal("member and admin permissions exceeded their fixed role contract")
	}
	if !TenantAllows("admin", OutboxManage) || !TenantAllows("auditor", OutboxRead) || TenantAllows("auditor", OutboxManage) {
		t.Fatal("outbox operations must separate read-only audit from replay authority")
	}
}

func TestOrganizationPermissionsSeparateOperatorsAndViewers(t *testing.T) {
	if !OrganizationAllows("agent_operator", ExecutionApprove) {
		t.Fatal("agent operator must be able to approve executions")
	}
	if OrganizationAllows("viewer", SessionCreate) {
		t.Fatal("viewer must remain read-only")
	}
}
