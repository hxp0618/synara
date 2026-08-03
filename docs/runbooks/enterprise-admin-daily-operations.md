# Enterprise administrator and support daily operations

Use this runbook with [`enterprise-operations-ui-v1.md`](../contracts/enterprise-operations-ui-v1.md) and the machine
matrix in `docs/release-matrices/stage-6-operations-ui-v1.json`.

## Internal Tenant administrator workflow

Use Settings → Tenancy for Tenant status/deletion recovery, joiner/mover/leaver controls, Domain/SSO, Service Accounts,
Credentials, Workers/releases, Delivery Outbox, Audit, Legal Holds, privacy/export, data residency, usage/quota and Support
Access policy. Refresh after a version conflict and re-evaluate the new state. Do not retry a destructive action by editing
the database or switching to a legacy endpoint.

An authenticated internal user may create only an active Standard Tenant from the no-Tenant gate. Enterprise provisioning is a
separate Platform Admin operation in the configured Operator Tenant: select the existing active Tenant owner's email,
entitlement profile, Region and active/evaluation status. The operator must not receive Tenant Membership. An unknown owner is a stopped
provisioning request, not a reason to insert a User or Membership directly.

Before recording Release, Compliance, Provider commercial, evidence exercise or other governed decisions, use Platform Admin → Governance roles to confirm the
operator has the exact active function. Only an Operator Tenant Owner may assign or revoke it; follow
[`stage-6-governance-authority.md`](stage-6-governance-authority.md) and preserve the grant/revocation Audit request IDs.
Internal entitlement profile changes require the current version, reporting period and approval reason. They support only active/evaluation;
use Tenant lifecycle for suspension/closure. Payment-provider state, Checkout, Portal, invoices and settlement are not
supported. Use Usage & limits for Token totals, quota, Provider-reported cost and platform allocation only; `cost_admin`
is the fixed delegated role for these internal controls.

In Usage & limits, review every current-period Project row. A Tenant Owner or `cost_admin` assigns the exact cost center and
department using the displayed version; a stale version must be refreshed, never overwritten. Keep unknown ownership as
`unallocated`. Download the CSV and verify that each Project has one `usage` row plus zero or more per-currency `cost` rows
before importing it into the enterprise's existing finance workflow. Preserve the allocation and export Audit request IDs.

Member suspension and SCIM deprovisioning share one offboarding transaction: Organization Memberships suspend, this
Tenant's Login Sessions revoke, user Credentials revoke and automatic selection turns off. Reactivation restores only the
Tenant Membership. Deletion recovery is a separate owner-only inventory so a soft-deleted Tenant remains recoverable after
it leaves the normal session list.

Delivery Outbox shows redacted metadata. Wait for normal retry while the dependency is unhealthy. Replay only a dead letter,
only after confirming downstream idempotency and recovery, and one message at a time; replay writes Audit.

## Support workflow

1. Start in the configured Platform Operator Tenant and inspect the 30-second Tenant overview.
2. Submit a Support Access request with the internal-user-visible incident reason and shortest useful duration.
3. A different Platform Admin approves. Enter the read-only view only from the active grant.
4. Inspect the matrix's read-only surfaces. Do not ask for copied secrets, database dumps or Tenant payloads.
5. Confirm every attempted mutation is rejected. Ask the Tenant administrator to perform an approved mutation or use the
   dedicated Platform workflow; support impersonation never becomes write access.
6. Exit and revoke the grant immediately. Confirm target Tenant Audit contains request, approval, reads/denied write and
   revocation/expiry. Disabling Support Access must produce a `support.access_revoked` event for every active Grant;
   automatic timeout must produce `support.access_expired` with a system actor. Treat a terminal Grant without its
   Grant-correlated Audit event as a failed exercise.

## GA exercise

Run the matrix validator from the exact candidate commit:

```bash
python3 scripts/stage6-operations/validate_operations_ui_matrix.py \
  --repository-root . \
  --matrix docs/release-matrices/stage-6-operations-ui-v1.json \
  --output /secure/operations-exercise/source-receipt.json
```

Then execute all 49 rows through the deployed browser using the declared actor. Capture the corresponding Audit/request ID
and a negative role case. Record any CLI/database fallback as a failed row. The source receipt never closes the gate; attach
the signed browser-exercise result to the copied Stage 6 GA checklist.

Use eleven separate pseudonymous account references: every positive role from the source matrix plus `tenant-member` for
reviewed denial cases. Use the exact negative actor required by the deployed-evidence validator; do not substitute an
anonymous request when a role boundary is under test. Browser developer tools count as a fallback just like CLI or a
database client—the exercise is proving the shipped product surface, not whether an engineer can call the API manually.

After the browser run, create a draft using
`synara.stage6-operations-browser-exercise-draft.v2`. Keep only relative evidence paths in the draft and omit
`matrixSha256`; do not manually calculate or paste evidence hashes. Materialize, validate and atomically publish the
protected manifest and receipt with:

```bash
bun run stage6:operations:prepare -- \
  --evidence-root /secure/operations-exercise \
  --draft operations-draft.json \
  --repository-root . \
  --manifest-output manifest.json \
  --receipt-output receipt.json
```

The materialized manifest has exact top-level fields `schemaVersion`, `candidate`, `matrixSha256`, `startedAt`, `completedAt`,
`accounts`, `operations`, `supportAccess` and `approvals`. Every operation records its source-matrix `id/surface/actor/access`,
status, bounded timestamps, unique request IDs, fixed negative actor/result, the three fallback booleans and distinct
positive/negative `{path, sha256}` evidence. Keep screenshots, traces and Audit exports in the access-controlled evidence
root; do not store customer content, credentials, prompts or raw Provider payloads in the repository.

In the v2 draft, `supportAccess` must declare `revocationAuditAction=support.access_revoked` and
`expiryAuditAction=support.access_expired`, with both visibility results true only after the matching Grant-correlated
Tenant Audit rows were observed. One terminal path cannot stand in for the other.

The preparer rejects duplicate JSON fields, traversal/symlinks, reused evidence, oversized input and obvious private-key,
AWS-key, Bearer or credential-bearing URL material. It publishes nothing until the complete validator succeeds and rolls
back the manifest pair if receipt publication fails. It does not authenticate the declared accounts, browser execution or
approver authority.

`eligibleForHumanGateReview=true` is only a readiness signal. Operations and Security still inspect the evidence and sign
the copied GA checklist; the validator receipt itself never declares the operation gate passed.

## Platform Operations exercise governance

Import the validator's exact receipt bytes through Platform Admin → Operations exercises. The Control Plane accepts at most
2 MiB, verifies the candidate bundle SHA-256 and recomputes candidate/Commit/environment, three Artifact digests, distinct
Web/Admin HTTPS origins, eleven distinct production-authenticated accounts, all 49 positive and fixed denied-role results,
globally unique request IDs, zero CLI/database/developer-tool fallback, Support Access lifecycle, evidence cardinality and
the source Operations/Security decisions. Receipt replacement and cross-candidate reuse are forbidden.

The candidate creator cannot approve the imported record. Two different users holding active
`operations_exercise.operations` and `operations_exercise.security` authority append immutable decisions before the record
becomes internally `approved`. Migration `000129_stage6_operations_exercise_governance.sql` enforces the same projection
and authority boundary in PostgreSQL and SQLite. Migration
`000130_stage6_release_operations_exercise_approval_gate.sql` prevents the exact Release candidate from entering
`approved` without that exact internally approved record and blocks direct database bypass.

Each Operations/Security decision must also bind the exact reviewed evidence bytes with a nonzero lowercase SHA-256.
Migration `000147_stage6_operations_exercise_approval_evidence_digests.sql` supersedes historical URL-only decisions,
reopens an affected approved exercise to `recorded`, and allows replacement decisions while retaining the immutable
history. Release transitions through `approved`, `deploying`, `observing` and `released` revalidate exactly two active
byte-bound decisions; a URL or parent `approved` state alone cannot satisfy the gate.

The Operations-receipt import/decision meta-actions are intentionally not rows in the 49-row exercise they govern; requiring a receipt to
contain its own later import would be a self-referential deadlock. They remain covered by API, Admin and browser tests. This
internal record does not authenticate the deployed hosts, browser sessions, Audit request IDs, evidence repository,
signatures, execution or external organizational authority, so it cannot check the GA operations item by itself.

The historical Billing exercise is not part of the current self-hosted product. Do not execute Stripe scenarios or import a
payment receipt. Release evidence binds the exact-candidate internal usage/cost projection through the current
`internal-self-hosted` Candidate and Release gates.

Platform Admin → Release governance → **Exact-candidate readiness** is the operator's consolidated read path for all nine
internal approval gates. Use **Refresh gates** after completing work on another governance page; do not submit transitions
repeatedly to discover one missing gate at a time. A satisfied row remains paired with its external boundary and never means
that production execution, evidence signatures or organizational authority were verified by Synara.
