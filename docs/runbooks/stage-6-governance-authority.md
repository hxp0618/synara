# Stage 6 functional governance authority

## Purpose

Release, Compliance, Provider commercial, Recovery, Penetration, Capacity, Incident exercise, Operations exercise and internal cost decisions must not trust a role selected in a request body. Platform Admin →
Governance roles records who is authorized to act for each exact function before the corresponding service and database
accept a decision.

## Assignment boundary

- Only an active Operator Tenant `owner` may assign or revoke authority.
- The Owner cannot assign authority to themselves; every decision remains separated from authority administration.
- The governed user must remain an active `owner`, `admin` or `security_admin` in the active Operator Tenant.
- Every grant fixes one user, one authority key, a credential-free HTTPS evidence reference, the non-zero
  `sha256:<64 lowercase hex>` of the exact corporate-delegation evidence bytes, reason and expiry of at most 366 days.
- A user may hold multiple explicitly assigned functions, but a decision workflow still enforces its own distinct-person and
  creator/self-review rules.
- Expiry, revocation, user suspension/deletion, Membership suspension or Operator Tenant suspension immediately rejects new
  decisions with `governance_authority_required`.

## Supported functions

The fixed keys cover Release Engineering/Operations/Security/Product/Privacy-Legal, Compliance start-gate and evidence-
review roles, Provider commercial Legal/Privacy/Security/Product, Recovery Database/KMS/Operations/Security/Storage and
Penetration Engineering/Product/Security, Capacity Engineering/Operations and Incident exercise Operations/Communications
decisions. The Incident exercise keys are exactly `incident_exercise.operations` and
`incident_exercise.communications`; the Operations exercise keys are exactly `operations_exercise.operations` and
`operations_exercise.security`; the internal cost keys are exactly `internal_cost.operations` and
`internal_cost.owner`. Historical `billing_exercise.*` keys cannot authorize a current release. Adding a function requires a Migration,
typed client update, Admin UI update and database trigger update; free-form authority names are forbidden.

## Operating procedure

1. Confirm the person is an active Operator Tenant operator and is not the assigning Owner.
2. Open Platform Admin → Governance roles and select the exact function.
3. Attach an access-controlled HTTPS record proving organizational responsibility and enter the SHA-256 of the exact
   reviewed bytes; do not paste credentials, contract contents or signed URLs.
4. Set the shortest practical expiry and record the assignment reason.
5. Before role rotation or offboarding, revoke the grant and verify the target governance action returns
   `governance_authority_required`.
6. Preserve `governance.authority_granted` and `governance.authority_revoked` Audit request IDs with the release or control
   evidence.

Grant identity and history cannot be edited or deleted. Revocation is the only update and uses version fencing. PostgreSQL
and SQLite both enforce the authority lookup again on Release, Compliance, Provider commercial, Recovery, Penetration, Capacity, Incident exercise, Operations exercise and internal cost
decisions, so bypassing the service does not permit a self-asserted role.

Migration `000140` revokes every active legacy grant whose evidence was URL-only. The history remains visible with version
and automatic revocation reason, but it cannot authorize a new decision. Recreate the assignment from exact byte-bound
delegation evidence; missing, malformed, all-zero or later-mutated digests are rejected by the service and database.

This record proves Synara's internal authorization state. It does not prove employment title, corporate delegation,
external counsel authority, auditor independence or evidence-repository authenticity; those remain external GA gates.
