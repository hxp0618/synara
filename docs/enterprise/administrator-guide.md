# Enterprise administrator guide

This guide covers Tenant owners, administrators, security administrators, cost administrators and auditors. The exact
permission boundary is defined in the [role-permission matrix](../contracts/role-permission-matrix.md); hiding a control in
the browser is not authorization, so every write is checked again by the Control Plane.

## Establish the Tenant

Confirm the Tenant name, lifecycle state, internal entitlement profile, home Region, Organizations and at least two accountable owners before
production use. Self-service is fixed to the active Standard profile; Enterprise/evaluation provisioning is available only to Owner/Admin in
the configured Platform Operator Tenant and requires an existing active Tenant owner account. The operator receives no
Tenant Membership. Direct inserts into tenancy tables are not a supported bootstrap mechanism. Keep an emergency recovery
owner who can access the Tenant if SSO configuration fails.

Platform Admins can also change an internal Tenant between Standard/Enterprise active or evaluation entitlement profiles with the current
version, explicit reporting period and reason. The public API uses `standard | enterprise` and `evaluation`; retained
`free | enterprise` and `trialing` values are database-only compatibility codes, not an external Plan, trial or subscription
offer. Payment-provider-managed state is unsupported. Use the
Tenant lifecycle workflow for suspension or closure instead of inventing a second execution-admission state.

Tenant status changes are versioned. Activate, suspend and close from Settings, using the current lifecycle version. A
closed Tenant may enter the deletion-recovery window; its owner can find the soft-deleted record in the dedicated recovery
inventory even after it disappears from the ordinary Tenant list. Do not request deletion until exports, Legal Holds,
retention and active resource cleanup have been reviewed.

## Configure identity safely

1. Add the OIDC or SAML identity connection and validate the exact redirect/entity metadata against the production origin.
2. Create a Domain verification challenge, publish the one-time DNS TXT value, then verify the claim in Settings.
3. Configure SCIM and explicit Group mappings. Do not assume an unmapped IdP group receives a permissive default role.
4. Keep a verified Domain, an active connection and a recovery owner before changing SSO enforcement to `required`.
5. Test a normal SSO login, a denied Domain/user, a disabled connection and recovery-owner access before enforcement.

SCIM and manual offboarding use the same transactional primitive. Suspending or removing a user suspends Organization
memberships, revokes active Tenant login sessions, revokes user-scoped Credentials and disables their automatic selection.
Reactivation restores only the Tenant membership; Organization access and Credentials are not silently resurrected.

## Govern people and machine identities

Use the Members view to change fixed RBAC roles, suspend/reactivate a member, revoke Tenant sessions or request removal.
Self-management is rejected and owner changes remain owner-only. Immutable dependencies can require retaining a suspended
membership instead of deleting it.

Service Account tokens are shown once. Rotate or revoke them from Settings and update the consuming system without placing
the token in tickets or chat. Provider Credentials support user, Organization, Tenant and explicitly enabled Platform scope.
Explicit Session binding never falls back; automatic selection is `user → organization → tenant → platform`, and an
ambiguous match is rejected. Platform fallback additionally requires the enterprise profile, an entitled Plan and Tenant
opt-in.

## Monitor usage, quota and operations

Review Tenant usage and cost by internal accounting period, then investigate expensive Sessions through their per-Turn breakdown.
Treat missing telemetry as unavailable. Configure hard concurrency quota separately from soft usage warnings.

The Usage & internal cost panel can export the current-period snapshot as byte-bound JSON for internal analysis. The export
contains Token, Network, Provider-cost coverage, soft-quota and platform-allocation fields; it is audited and is not an invoice.

The current product uses internal cost-center allocation. Review Provider cost separately from estimated or actual platform
allocation. A Tenant Owner or `cost_admin` assigns each active Project to a cost center and department in Usage & limits;
leave `unallocated` visible until ownership is known. Export the two-row-type CSV for the organization's existing finance
process: `usage` rows contain Token/Network totals and `cost` rows contain one currency at a time. Synara does not collect
payment details or expose Checkout/Customer Portal in this product boundary.

Daily operations should remain in the product UI:

- inspect Workers, Execution Targets, queued/active/failing Executions and Artifacts;
- review redacted Delivery Outbox metadata and replay one confirmed idempotent dead letter at a time;
- search/export Audit records without exposing payloads;
- rotate/revoke Credentials and Service Accounts;
- review Privacy Requests, data exports, retention and Legal Holds; and
- manage data-residency execution allow-lists and download the versioned statement.

The complete role-by-role procedure is in the
[enterprise administrator daily-operations runbook](../runbooks/enterprise-admin-daily-operations.md).

## Control support access

Support Access is disabled unless the Tenant opts in. A Support Engineer supplies a reason and expiry; a different Platform
Admin must approve it. The resulting Tenant view is read-only, visible in Tenant history and revocable immediately by a
Tenant administrator. Disable the policy to revoke every active grant. Never grant a support engineer ordinary workload-Tenant
membership as a substitute.

## Prepare a change or incident

Use Settings → Release history for product notices and follow any structured administrator deadline. For an incident, use
**Help → Service status** when the deployment advertises its independent internal Status Board, plus the published support
route; release notes and incident updates are separate records. A missing Service status item means the deployment has not
configured that public capability. Preserve relevant Audit IDs, request/trace IDs, UTC timestamps and redacted screenshots,
then follow the [incident-response runbook](../runbooks/enterprise-incident-response.md).
