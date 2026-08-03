# Stage 6 candidate evidence bundle v2

> Superseded for new release promotion by
> `docs/contracts/stage-6-candidate-evidence-bundle-v5.md`. The validator retains v2 read compatibility for historical
> audit, but protected publication and Release Governance require v3 and its tenth Worker supply-chain receipt.

## Authority and purpose

A Stage 6 GA decision must apply to one immutable release candidate. Independently valid Desktop, billing, operations,
recovery, residency, SLO, incident, capacity and penetration receipts cannot be combined when they refer to different
commits, deployments or Artifact digests. This contract adds the cross-receipt identity boundary; it does not replace any
underlying control validator or human approval.

The canonical candidate identity is the `synara-stage6-release-evidence-v2` manifest produced from an exact clean commit by
`scripts/stage6-evidence/collect_release_evidence.py`. It fixes:

- candidate ID, 40-character source commit and `bun.lock` SHA-256;
- Control Plane, Worker, Provider Host, Web, Platform Admin and four native Desktop Artifact digests;
- `production | production-like | staging | fixture` environment class plus a deployment-unique environment ID;
- Control Plane HTTPS base URL and distinct customer Web/Platform Admin HTTPS origins;
- sorted Region set, every Migration checksum and exact Migration tail.

Changing any field creates a new candidate. A redeploy with a different environment ID is not the same evidence target,
even when it uses byte-identical artifacts.

For Desktop publication, the GitHub build run ID is an additional promotion boundary. The protected Enterprise GA release
job must remain in the same workflow run that produced the four accepted artifacts. Its environment approval record reads
the actual bounded v2 receipt, recomputes its SHA-256, verifies its eligibility and candidate/commit/lockfile/Desktop-set
identity, then binds candidate ID, source commit, build run ID and receipt SHA-256 before the planned tag and updater
payloads become public. A rebuild from the same commit is still a new Desktop candidate because signing/notarization bytes
may differ. The workflow rejects reruns and requires the exact review comment printed by preflight.
The approval record captures GitHub's current configured reviewers, self-review prevention, disabled administrator bypass,
the actual approving reviewer, both workflow actors, and normalized configuration/comment/review digests. GitHub's one-review
technical gate still does not replace the independent role signatures required by the GA checklist.

The validator derives `desktopArtifactSetSha256` from each target's `artifactAttestation`, `attestationBundle` and
`provenance` reference digests in sorted target/field order (`target/field=sha256:...` plus LF per line). Each provenance
manifest fixes the immutable native payload layer: installers, update archives and blockmaps. Updater YAML is deliberately
excluded because it is transformed across platform lanes. Before the protected gate, `finalize_release_assets` performs the only updater merge/copy/delete operations
and writes a canonical `release-final-asset-set.json` over every final public file. The protected job verifies both the raw
four-lane Desktop set and this finalized set before writing publication authorization. A separate branded cross-binding
requires every final installer, archive, blockmap, provenance and attestation file to match the approved raw file digest and
size; only the predefined updater YAML transformations may differ. The final-set verifier parses those YAML files and checks
default/channel alias equality plus version, basename, architecture, size, SHA-512 and Windows blockmap references against
the actual payloads. The publication job repeats all three checks and performs no further byte mutation. A rebuild, changed
payload, mixed trust file, stale updater entry or post-finalize YAML change cannot reuse the prior approval.

## Required receipts

The bundle contains exact `{path, sha256}` references for the release-evidence manifest and these nine validator receipts:

| Receipt     | Candidate fields that must match the release manifest                                                                                  |
| ----------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| Billing     | candidate ID, commit, environment ID/class, Control Plane/Web digests and URL, Migration tail                                          |
| Desktop     | candidate ID, commit, lockfile, environment ID/class, Control Plane URL, Migration tail, installer, provenance and attestation digests |
| Operations  | candidate ID, commit, environment ID/class, Control Plane/Web/Admin digests and Web/Admin origins                                      |
| Incident    | commit, environment ID/class and customer service origin                                                                               |
| Recovery    | candidate/commit/lockfile/Artifact/Migration identity, environment ID/class, Regions, origins and restored release identity            |
| Residency   | candidate/lockfile/Artifact/Migration identity, environment ID/class, sorted Regions, policy head and semantic evidence-set digest     |
| SLO         | commit, query revision, environment ID/class and public service origin                                                                 |
| Capacity    | commit and environment ID/class                                                                                                        |
| Penetration | commit, environment ID/class and Control Plane/Web/Worker/Provider Host digests                                                        |

All references must resolve to unique bounded regular files below one non-symlink evidence root, and every SHA-256 is
recomputed. The validator rejects an unsupported receipt schema or a receipt that changes its permanent non-pass
assessment. It records a failed or ineligible control as not ready; it does not delete the evidence or relabel it passed.

## Validation and review

Run:

```bash
python3 scripts/stage6-candidate/validate_candidate_evidence_bundle.py \
  --manifest /secure/stage6-rc1/bundle.json \
  --evidence-root /secure/stage6-rc1 \
  --output /secure/stage6-rc1/candidate-bundle-receipt.json
```

`eligibleForCandidateEvidenceReview=true` means only that the same release-eligible candidate owns all nine receipts and
each receipt is ready for its next review boundary. The assessment is permanently
`evidence-consistent-not-ga-approved`. The Residency receipt proves its eight strict JSON attachments share one subject and
that Annex/runtime/browser/exercise/approval semantics are internally consistent; the bundle records its
`evidenceSetSha256` and the explicit fact that cryptographic signer authority was not verified. The Recovery v2 receipt
similarly fixes the full candidate and restored-release identity, one recovery subject, five distinct role approvals and
the complete drill time window; it still records that real backup authority, approver authority and cryptographic
signatures require external verification. Neither receipt itself approves the customer promise or recovery control.
Provider/Legal/Privacy approval, secret rotation, release communication,
documentation review, residual-risk acceptance and the final Security/Operations/Product/Engineering decision remain
separate mandatory checklist items. The exact Billing receipt can additionally be imported into Platform Billing Exercise
Governance and approved by distinct Finance/Security/Release authorities before Release Governance advances. That internal
database gate retains the receipt's permanent non-pass assessment and does not authenticate Stripe settlement or approvers.
