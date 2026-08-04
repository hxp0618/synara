package authorization

import "testing"

func TestTenantPermissionsAreRoleBased(t *testing.T) {
	if !TenantAllows("owner", TenantDelete) || !TenantAllows("owner", CostManage) {
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
	if !OrganizationAllows("member", SessionSettle) || !OrganizationAllows("member", SessionArchive) {
		t.Fatal("organization members must be able to settle and archive their Sessions")
	}
	if OrganizationAllows("viewer", SessionCreate) {
		t.Fatal("viewer must remain read-only")
	}
	if OrganizationAllows("viewer", SessionSettle) || OrganizationAllows("viewer", SessionArchive) {
		t.Fatal("viewers must not mutate Session lifecycle state")
	}
}

func TestSchedulingPolicyPermissionsAreIndependentFromWorkerManagement(t *testing.T) {
	if SchedulingPolicyRead == WorkerRead || SchedulingPolicyManage == WorkerManage {
		t.Fatal("scheduling policy permissions must be independent authorities")
	}
	for _, role := range []string{"owner", "admin", "security_admin"} {
		if !TenantAllows(role, SchedulingPolicyRead) || !TenantAllows(role, SchedulingPolicyManage) {
			t.Fatalf("tenant %s must read and manage scheduling policy", role)
		}
	}
	if !TenantAllows("auditor", SchedulingPolicyRead) || TenantAllows("auditor", SchedulingPolicyManage) {
		t.Fatal("tenant auditor scheduling policy authority must remain read-only")
	}
	if TenantAllows("cost_admin", SchedulingPolicyRead) || TenantAllows("member", SchedulingPolicyRead) {
		t.Fatal("unrelated tenant roles must not inherit scheduling policy authority")
	}

	for _, role := range []string{"owner", "admin"} {
		if !OrganizationAllows(role, SchedulingPolicyRead) || !OrganizationAllows(role, SchedulingPolicyManage) {
			t.Fatalf("organization %s must read and manage scheduling policy", role)
		}
	}
	for _, role := range []string{"agent_operator", "member", "viewer"} {
		if !OrganizationAllows(role, SchedulingPolicyRead) || OrganizationAllows(role, SchedulingPolicyManage) {
			t.Fatalf("organization %s scheduling policy authority must remain read-only", role)
		}
	}
}
