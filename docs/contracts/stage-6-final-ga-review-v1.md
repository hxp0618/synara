# Stage 6 final GA review archive v1

## Purpose and boundary

The final archive closes the mechanical gap between an eligible internal-self-hosted Candidate v5 evidence bundle and the external GA decision.
It proves that the protected release approval, final public asset set, copied GA checklist, published-change record,
Platform Audit export, functional decisions and residual-risk record all refer to one candidate and immutable byte
references. It does not authenticate external evidence authority, corporate approver authority, external signatures or
delivery by a third-party publication channel.

The validator receipt schema is `synara.stage6-final-ga-review-validation.v1`; its permanent assessment is
`final-review-consistent-not-ga-authority-verified`. `eligibleForExternalGAAuthorityReview=true` means the archive is
internally consistent and complete enough to present to the real GA authority. It is not a GA approval.

## Candidate identity and immutable inputs

The `synara.stage6-final-ga-review.v1` manifest fixes one `candidateId`/release tag, source commit and deployment-unique
environment ID. Every file reference is `{path, sha256}`, relative to one non-symlink evidence root, and is read as a
bounded, stable, regular non-symlink file. The validator requires:

- an eligible `synara.stage6-candidate-evidence-bundle-validation.v5` consistency receipt whose `compatibilityMatrix`
  projection binds the exact source-current cross-component matrix and whose `internalCost` projection binds Token,
  Provider cost, and actual-over-estimated platform allocation evidence without payment data;
- the exact `synara.stage6-release-approval.v4` protected-environment record whose candidate-receipt digest matches those
  bytes;
- the canonical `synara.release-final-asset-set.v1` manifest whose internal asset-set digest matches the protected record;
- a copied Stage 6 GA checklist with every row completed and the exact candidate and commit recorded;
- a completed change notice with publication proof and the exact candidate and commit;
- a Platform Audit export containing the exact candidate, commit and every declared request UUID; and
- evidence-backed control and final-approval decisions inside one bounded review window.

The output receipt and sidecar use exclusive `0600` paths. A collision fails without modifying existing files, and sidecar
failure removes the newly published receipt.

## Product binding and release gate

After the candidate reaches `observing`, an Operator Tenant Owner or Admin uploads the exact validation receipt in
Platform Admin → Release governance. The Control Plane decodes and hashes the exact bytes again, rejects unknown receipt
fields, revalidates candidate/commit/environment/lockfile/Desktop/final-asset identity, all 32 controls, separated final
approvers, timestamps, Audit request UUIDs, residual risks and the four false external-authority flags, then stores one
append-only record. The `released` transition is unavailable until that record exists and its decision summary,
`none | accepted` disposition and canonical residual-risk inventory exactly match the transition request. Migration 135
enforces the same final gate for direct PostgreSQL writes: it independently checks the exact 32-ID/owner inventory,
non-null arrays, relative evidence references, review time closure, distinct required roles and final approvers, Audit
request UUID uniqueness, risk shape and candidate projections instead of trusting service-derived counts. The SQLite
safety triggers enforce the equivalent exact-byte digest, inventory/role/identity/time boundary for personal/local mode;
the database driver registers deterministic `synara_sha256(blob)` before opening any SQLite connection so the trigger
recomputes the receipt projection instead of checking digest length only. A duplicate-control receipt with a freshly
recomputed digest, or unchanged bytes paired with a different digest, is rejected by both database paths. The Platform
Admin released transition reads its summary, disposition and risk inventory back from this immutable binding and submits
those exact values read-only rather than asking the operator to transcribe them. The exact-candidate readiness API keeps
the nine `approved` gates unchanged and separately projects `finalReviewGate` plus
`releaseTransitionEligible`; Platform Admin therefore distinguishes “internal approval gates complete” from “observing
candidate may enter released.” This binding proves product-state consistency only and does not turn eligibility into
external GA approval.

## Fail-closed preparation

The preferred `stage6:final:prepare` entry point consumes a
`synara.stage6-final-ga-review-draft.v1` document whose evidence values are path-only. It resolves every path below one
evidence root without following symlinks, performs bounded stable reads, computes the references instead of trusting
operator-transcribed digests, validates the fully materialized v1 manifest from a private temporary file, and then publishes
the final manifest, receipt and both sidecars. Existing output, invalid semantics or evidence drift leaves no new archive
files. The lower-level validator remains available for offline revalidation but also requires a new output path.

## Required control inventory

The manifest contains each control exactly once with the fixed owner role, `passed | failed | blocked` status, UTC decision
time, bounded approver identity and at least one immutable evidence reference.

| Owner         | Control IDs                                                                                                                                                                                                                           |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Product       | `tenant-registration`, `offer-admission`, `usage-explainability`, `documentation`, `change-notice`                                                                                                                                    |
| Operations    | `tenant-lifecycle`, `operations-matrix`, `authority-views`, `incident-communications`, `recovery`, `slo`, `capacity`                                                                                                                  |
| Engineering   | `usage-reconciliation`, `quota-concurrency`, `desktop-native`, `desktop-postgres-concurrency`, `compatibility-rollback`                                                                                                               |
| Security      | `identity-lifecycle`, `sso-domain`, `identity-governance`, `support-access`, `audit-retention-legal-hold`, `compliance-program`, `desktop-security`, `tracing-isolation`, `penetration-stage5`, `worker-supply-chain`, `key-rotation` |
| Operations    | `internal-cost-reconciliation`                                                                                                                                                                                                        |
| Privacy/Legal | `privacy-workflows`, `data-residency`, `provider-compliance`                                                                                                                                                                          |

The receipt projects a canonical control-inventory SHA-256. Any unknown, duplicate or missing control rejects the archive;
a non-passed control preserves a non-eligible historical receipt rather than being rewritten as success.

## Final decisions and residual risk

Engineering, Operations, Security and Product approvals are always required. Personal-data, Provider-terms,
regulated-customer, Residency or security-incident impact additionally requires Privacy/Legal. Final approvers must be
distinct from one another and from the Release Manager, and their decision cannot predate the protected approval or any
control decision. A denial is retained but makes the archive ineligible.

Residual risk uses `none` with an empty inventory or `accepted` with stable IDs, owners, future UTC due dates, bounded
reasons and HTTPS evidence references. Residual risk cannot convert a failed or blocked mandatory control into an eligible
archive; prohibited waivers remain governed by the Release Governance runbook.
