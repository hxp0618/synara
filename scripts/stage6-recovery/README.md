# Stage 6 recovery evidence validator v2

`validate_recovery_evidence.py` validates an exact release candidate's PostgreSQL, object storage, KMS, and Queue recovery
drill without declaring the production control passed. Every receipt permanently uses:

```text
evidence-validated-not-control-passed
```

## Candidate boundary

`synara.recovery-drill-evidence.v2` binds the drill to the canonical release identity:

- candidate ID, full source commit and `bun.lock` SHA-256;
- production, production-like, staging, or fixture environment class plus deployment-unique environment ID;
- sorted deployment Regions and credential-free Control Plane/Web/Admin HTTPS origins;
- Control Plane, Worker, Provider Host, Web, Admin, and four native Desktop artifact digests;
- exact migration tail name and digest.

`restoredReleaseIdentity` repeats only commit, lockfile, artifacts, and migration tail as the identity observed in the restored
deployment. It must exactly match `candidate`; a prior image, Web bundle, Desktop set, lockfile, commit, or migration is
rejected as a stale restore target. A release pipeline should also pass the pre-collection canonical candidate hash through
`--expected-candidate-binding-sha256`.

## Drill and component schema

The manifest root contains exactly `schemaVersion`, `candidate`, `drill`, `restoredReleaseIdentity`, `components`, and
`approvals`. `drill` contains UUID `drillId`, `startedAt`, and `completedAt`.

The four components appear exactly once. Every component records:

- approved profile, source/restore authority, Region, and distinct failure domain;
- `restoredThroughAt`, `sourceCutoffAt`, `recoveryStartedAt`, and `serviceReadyAt`;
- `restoreServedCanary` as an explicit boolean;
- unique path/SHA-256 references for backup, restore, and real-client evidence.

The validator derives RPO/RTO from timestamps and compares them with the contract objectives. A missed objective or failed
client canary is retained in a valid non-pass receipt and makes `eligibleForHumanGateReview=false`; it is not discarded or
misreported as a schema error.

## Recovery subject and approvals

`recoverySubjectSha256` is the SHA-256 of canonical JSON containing the normalized candidate, drill UUID/window,
`restoredReleaseIdentity`, sorted components, measurements, canary results, and all component evidence references.

Use the preparer to freeze that subject before collecting approvals. The path-only draft uses
`synara.recovery-drill-evidence-draft.v2`; each component evidence field is a relative path below one evidence root, and the
preliminary draft has an empty `approvals` object. The preparer reads each source once as a bounded, stable regular file,
rejects traversal, symlinks, duplicate JSON fields and common credential material, derives the exact hashes, validates the
materialized manifest, then atomically publishes the manifest/result pairs:

```bash
bun run stage6:recovery:prepare -- \
  --mode subject \
  --evidence-root /secure/recovery \
  --draft drill-subject-draft.json \
  --expected-candidate-binding-sha256 sha256:<64-lowercase-hex> \
  --manifest-output recovery-subject-manifest.json \
  --result-output recovery-approval-subject.json
```

The resulting `synara.recovery-drill-approval-subject.v1` is not a recovery receipt. Copy only its
`recoverySubjectSha256` into the five approval files, add their references to the final manifest, and run normal validation.

The final draft must contain five unique relative approval JSON paths under `database`, `kms`, `operations`, `security`,
and `storage`. Each referenced `synara.recovery-drill-approval-evidence.v1` file contains exactly:

- matching role and a distinct approver ID;
- `approved-for-human-gate-review` or `reviewed-not-approved` decision;
- approval and expiry timestamps;
- the exact `recoverySubjectSha256`;
- an explicit boundary stating strict content/subject validation occurred, while approver identity and cryptographic
  signature remain externally required and were not verified by this script.

Approvals must follow drill completion, precede validation, and remain unexpired. Validation must happen after completion and
within seven days. Missing roles, reused roles/identities, stale subjects, invalid ordering, duplicate files, symlinks,
traversal, or hash tampering fail closed.

## Eligibility

`eligibleForHumanGateReview` requires all of the following:

- production or production-like candidate;
- exact restored release identity;
- every RPO/RTO within objective;
- every real-client canary passed;
- five current `approved-for-human-gate-review` decisions bound to the exact subject.

The receipt still does not prove a real backup authority, restore target, client path, reviewer identity, or cryptographic
signature. Those remain external release evidence.

After the five approvals bind the frozen subject, prepare the final immutable manifest and receipt:

```bash
bun run stage6:recovery:prepare -- \
  --mode final \
  --evidence-root /secure/recovery \
  --draft drill-final-draft.json \
  --expected-candidate-binding-sha256 sha256:<64-lowercase-hex> \
  --manifest-output recovery-final-manifest.json \
  --result-output recovery-receipt-v2.json
```

`stage6:recovery:validate` remains available for independently revalidating an already materialized immutable manifest.

Run focused tests with:

```bash
python3 -m unittest discover -s scripts/stage6-recovery -p 'test_*.py'
```

Both approval-subject and final recovery outputs use the shared Stage 6 immutable publisher. The receipt and sidecar are
exclusive `0600` files; use a distinct path per attempt, and treat a collision as an existing immutable result.
