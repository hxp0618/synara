# Enterprise operations UI v1

## Promise boundary

This contract defines the Stage 6 requirement that customer administrators and support staff complete normal Tenant
operations without a shell or database client. It does not prohibit infrastructure operators from using Kubernetes/cloud
tools for deployment incidents. Direct SQL is never a customer-support API, and a source matrix does not prove a deployed
browser workflow passed.

`docs/release-matrices/stage-6-operations-ui-v1.json` contains the required UI → typed client → authenticated HTTP route
chain for 49 daily operations, including exact-candidate lifecycle, separated release-role decisions, compliance-control evidence governance, hosted Provider commercial authorization, Owner-managed functional authority, governed incident/public-communication closure and SLO-window review. Its v2 schema additionally binds every row to an owning surface and host registration seam,
so a component that is not mounted cannot count as an available operation. It covers Standard-profile self-service, Platform-admin
enterprise provisioning and versioned internal entitlement management, Platform Tenant
triage, four-eyes Support Access, Tenant/member lifecycle, Domain
Verification/SSO, Service Accounts, Credential metadata/governance, Worker/release operations, redacted Outbox/replay,
Audit, Legal Hold, privacy/export, residency, usage/quota/internal cost visibility and Tenant support policy. Payment and
Checkout/Portal are not product operations in the `internal-self-hosted` boundary.

The typed-client source boundary is `@synara/control-plane-client` in `packages/control-plane-client`; matrix rows must not
point back to a Web-private client module.

Each v2 operation also freezes one denied actor so a candidate cannot substitute a weaker anonymous smoke test for the
reviewed role boundary:

- `unauthenticated`: `user.tenant-self-service`;
- `platform-operator`: Tenant provisioning, internal entitlement mutation and Support approval/revocation;
- `tenant-admin`: Platform Tenant overview, Support request/entry and Owner-only lifecycle/deletion recovery;
- `auditor`: member mutation, Worker/release mutation, Outbox replay, usage/quota mutation and Support policy mutation;
- `tenant-member`: identity, Service Account, Credential, Support Credential read, Outbox read, Audit export, Legal Hold,
  Privacy, Tenant export and data-residency mutation;
- `platform-admin`: Owner-only Stage 6 functional-authority assignment and revocation.

These are denial probes, not a complete role matrix. Positive authorization still uses the row's `actor`, and independent
security testing must cover other identities and object substitutions.

## Role and data boundaries

- Platform operators see aggregate Tenant health and request time-bounded Support Access. A different Platform Admin
  approves; the requester enters an audited `support_readonly` Tenant context.
- ADR 0004 assigns every cross-Tenant Platform operation to the separate `apps/admin` authority surface. Customer Settings
  cannot host those actions, and an existing unmounted component is migration source only—not a reachable Platform UI.
- Self-service creation is fixed to the active Standard profile. Enterprise/evaluation profile status and initial-owner selection exist only in the
  Platform Admin provisioning path, and that path never grants the operator customer membership.
- Support can inspect Worker, Outbox, Credential, Identity, Audit, lifecycle, retention and customer-resource metadata but
  cannot mutate it. The HTTP middleware remains the final non-GET block even if a button is rendered by mistake.
- Tenant Owner/Admin/Security/Cost Admin roles receive only their existing fixed-RBAC operations. Destructive actions retain
  server-side version, reason, dependency, last-Owner, Legal Hold, generation and four-eyes guards.
- Outbox UI/API omit payload and headers; the UI also redacts raw `lastError`. Credential UI never returns encrypted or
  plaintext payloads. Support evidence must not contain credentials, prompts, Provider payloads or customer content.

## Source and live validation

`scripts/stage6-operations/validate_operations_ui_matrix.py` rejects a missing/extra operation or surface binding, missing
source/registration marker, wrong authority surface, symlink/traversal, unknown actor/access mode, or any operation declaring
CLI/database dependency. It hashes every referenced source file and emits
`source-ui-routes-validated-all-surfaces-reachable-not-operations-passed`. The current matrix reports all 20 Tenant Web
operations and all 29 Platform Admin operations as source-reachable through their real host registrations. The former
parked customer-Settings component has been removed; Platform rows point only to `apps/admin`.

A GA candidate still requires a production/production-like browser exercise using real role-separated accounts. The
isolated Phase D browser exercise proves Owner/Admin/Security visibility, provisioning, versioned entitlement mutation,
four-eyes approval, `support_readonly` handoff, explicit 403 denied writes and correlated local Audit request IDs; it is
implementation evidence, not the deployed GA exercise. For each matrix row, record the UI entry, success result, matching
Audit/request ID, expected denied-role result and evidence link.
Support Access expiry/revocation and the middleware write denial are mandatory. Only the copied GA checklist can mark the
operations gate complete.

Every Grant freezes the exact Platform Operator Tenant that authorized the request. Customer reads, Tenant inventory and
Login Session authentication revalidate that the requester remains an active `owner|admin|security_admin` of an active
Operator Tenant; offboarding or role loss removes the customer Tenant from Session state immediately. A historical Grant
without this authority binding is ineligible and must be replaced through the four-eyes flow.

Every terminal Grant transition is Tenant-visible and correlated to the Grant ID. An explicit revoke writes
`support.access_revoked`; disabling the Tenant Support policy writes one such event for every active Grant in the same
transaction as the status changes; automatic reconciliation writes `support.access_expired` with a system actor. A missing
Audit write rolls back the corresponding status transition rather than leaving an unaudited revoke or expiry.

`scripts/stage6-operations/validate_operations_exercise_evidence.py` freezes that deployed evidence shape. Operations v2
requires the exact `support.access_revoked` and `support.access_expired` action declarations and separate visibility results;
Migration 162 makes v1 rows audit-only and reopens their approvals without rewriting historical receipt bytes. One exercise
must bind the exact candidate ID, environment class/ID, Control Plane/Web/Admin digests and HTTPS origins, the source matrix hash, eleven distinct pseudonymous
role accounts, all 49 positive results, the reviewed negative actor for every row, globally unique request IDs, and unique
hashed positive/negative evidence files. It also requires the Platform Operator requester, a distinct Platform Admin
approver, the Support Engineer subject, write denial, revoke/expiry, Tenant-visible Audit and distinct Operations/Security
approval evidence. A production or production-like run using real SSO/MFA accounts can become
`eligibleForHumanGateReview`; staging, fixture auth, failed/blocked rows, unexpected authorization, or any CLI, database
client or browser developer-tools fallback cannot. The assessment remains
`evidence-validated-not-operations-passed` even when review-eligible.
