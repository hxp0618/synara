# Third-party penetration test runbook

Use this runbook with [`third-party-penetration-acceptance-v1.md`](../contracts/third-party-penetration-acceptance-v1.md)
for each Stage 6 GA candidate. It is an evidence workflow, not authorization to probe any system.

## 1. Authorize and freeze the engagement

1. Copy the Stage 6 GA checklist and record the full release commit plus Web, Control Plane, Worker and Provider Host
   digests.
2. Confirm the exact Stage 5 accepted deployment profile is an ancestor of the candidate and rerun its target-specific
   acceptance on every in-scope Ready/non-cordoned Worker. Do not reuse self-managed evidence for a managed-cloud profile.
3. Obtain a signed statement of work: assets, Tenant test accounts, source addresses, UTC windows, permitted techniques,
   emergency contacts, data handling, stop conditions and cleanup duties.
4. Confirm the assessor is a contracted third party, has declared independence/no conflict and can issue a signed final
   report. Keep assessor personal data outside the manifest.
5. Use a production-like environment unless Operations explicitly authorizes production. It must use the candidate
   artifacts and production-equivalent identity, network, storage, admission and observability policies.

Stop before testing if the Stage 5 profile/target evidence is missing, the candidate digests can change, authorization is
ambiguous, monitoring is unavailable, or the environment contains unapproved customer data.

## 2. Prepare evidence storage and safety controls

- Create an access-controlled engagement folder outside the repository. Grant only the assessor, Security, designated
  Operations staff and release approvers.
- Create nine distinct evidence files matching the contract. A `none` statement may be used for remediation or risk
  acceptance when there are no such records; never reuse another file to satisfy the slot.
- Enable audit, trace, network and runtime telemetry with the retention needed for investigation. Do not add Tenant/User IDs
  to metric labels or capture Provider payloads, prompts, credentials or exploit secrets in the receipt.
- Prepare rollback/isolation, Credential revocation and incident escalation. A suspected real compromise follows the
  enterprise incident-response runbook immediately; the engagement schedule does not delay containment.

## 3. Execute the agreed scope

The assessor records Web/API business-logic tests and Worker/Provider runtime tests against the fixed artifacts. Exercise
cross-Tenant nested-ID substitution, revoked/expired identity replay, SSRF, command/argument injection, path traversal,
supply-chain verification/admission and container/credential containment. Run positive controls so a blocked request is not
mistaken for a broken harness. Preserve exact timestamps and correlate observations through redacted evidence.

If an area or release asset cannot be tested, record it as `tested=false` / `not-tested`; do not silently narrow the scope.
The validator will preserve the incomplete run and keep it ineligible.

## 4. Triage, remediate and retest

1. Put every finding in the finding register with an opaque ID, severity, affected asset types and current status.
2. Critical findings require a fixed candidate and independent successful retest.
3. High findings require either the same remediation/retest or a maximum 30-day acceptance signed by distinct Security and
   business approvers with explicit compensating controls. The release checklist may still reject an accepted High risk.
4. Keep report, remediation, retest and acceptance evidence immutable. Put the nine source files below one
   access-controlled evidence root and reference them by relative POSIX path from a
   `synara.third-party-penetration-evidence-draft.v1` draft. Do not edit a prior report to erase a failed result; issue a
   new version and preserve lineage.
5. Revoke test Credentials, remove test Tenants/data, close temporary network access and retain cleanup evidence.

## 5. Validate the evidence bundle

```bash
bun run stage6:penetration:prepare -- \
  --evidence-root /secure/engagement \
  --draft penetration-draft.json \
  --manifest-output penetration-manifest.json \
  --receipt-output penetration-receipt.json
```

Verify the receipt sidecar, store both immutably and attach their references to the copied GA checklist. A nonzero exit means
the schema or evidence integrity is invalid. A zero exit can still contain `eligibleForHumanGateReview=false`; retain that
failed/incomplete result and do not check the release item.

The preparer reads each source as a stable bounded regular file, rejects duplicate JSON fields, Secret-like material,
symlinks, traversal and file reuse, and publishes manifest/receipt sidecars as exclusive `0600` files. If receipt
publication fails, it removes the just-published manifest pair. Use `stage6:penetration:validate` only for an independent
recheck of an already materialized immutable manifest.

## 6. Human release decision

Security verifies assessor signature and independence, Stage 5 ancestry/target scope, all findings and compensating controls.
Engineering verifies artifact identity and remediation. Operations verifies cleanup and no unresolved incident. Product or
the business risk owner reviews every acceptance.

1. The candidate creator imports the exact receipt through Platform Admin → Penetration reviews. The uploaded SHA-256 must
   already be the Penetration receipt reference in that candidate's immutable evidence bundle.
2. Confirm the projected Commit, environment and four Artifact digests match the protected report before deciding.
3. Distinct operators holding active `penetration.engineering`, `penetration.product` and `penetration.security` authority
   record immutable decisions, protected HTTPS evidence references and the nonzero lowercase `sha256:<64-hex>` digest of
   the exact external evidence bytes each operator reviewed. A URL without that byte binding is not an active decision.
4. Release Governance remains blocked until the same candidate's engagement reaches internal `approved`.

Migration `000144_stage6_penetration_approval_evidence_digests.sql` automatically supersedes historical URL-only
decisions and reopens their previously approved engagement for replacement review. The three roles must submit new
byte-bound decisions; Release candidates already in a forward state are revalidated fail-closed against those active
digests.

Only the copied GA checklist and externally verified report can mark the penetration item complete. Platform internal
approval and the validator receipt always remain distinct from `evidence-validated-not-penetration-passed` and do not
authenticate the assessor, report signature or execution.
