# Stage 6 candidate evidence bundle v4

> Historical schema. New candidates use
> [Stage 6 candidate evidence bundle v5](stage-6-candidate-evidence-bundle-v5.md), which also binds the exact
> source-current compatibility matrix. Terminal v4 bytes remain audit history only after Migration 000163.

## Authority and purpose

The v4 bundle is the internal-self-hosted release-authorizing successor to v3. It binds ten control receipts to the exact
`synara-stage6-release-evidence-v2` candidate, replaces the historical Stripe exercise with an internal usage-and-cost
control, and retains the Worker supply-chain control without weakening identity, artifact, deployment, recovery,
residency, or Desktop publication invariants.

The canonical candidate fixes the candidate ID, clean source commit, lockfile, all runtime and Desktop Artifact digests,
Migration chain and tail, environment ID/class, origins, and Regions. Any difference is a new candidate.

## Required receipts

The manifest schema is `synara.stage6-candidate-evidence-bundle.v4`. It contains exact unique `{path, sha256}` references
below one non-symlink evidence root for the release manifest and these ten receipts:

1. Internal usage and cost
2. Capacity
3. Desktop
4. Incident communication
5. Operations browser exercise
6. Third-party penetration
7. Recovery
8. Data residency
9. SLO window
10. Worker supply chain

The first nine retain the v2 field bindings. The Worker receipt must use
`synara.stage6-worker-supply-chain-evidence.v1`, retain the permanent
`evidence-validated-not-worker-supply-chain-approved` assessment, and match the release manifest's exact source commit,
Worker digest, and raw manifest SHA-256. Its six controls must all pass, and the candidate projection retains the exact
Registry and Vault/KMS admission report SHA-256 values.

The validator recomputes every referenced file digest. Duplicate paths, missing or extra receipts, schema drift, mixed
commits, Worker digest drift, altered release-manifest binding, stale validation timestamps, or a non-pass Worker control
fail closed.

The Desktop receipt is not trusted only because its top-level eligibility flag is true. The candidate validator
independently requires all four targets to be native and eligible, all 15 Enrollment results to pass, and the projected
`synara.stage6-desktop-enrollment-runtime-evidence.v1` candidate/target/window bindings to match the release evidence. It
rechecks the six canonical mode transitions, zero pre-choice/Local Cloud request counts, credential continuity, and the
unique `evidence.enrollment` reference for each target. The receipt projects a stable
`receipts.desktop.desktopEnrollmentEvidenceSetSha256` over those four references. Old-shape Desktop receipts and runtime
projection drift therefore fail before candidate review.

## Validation and promotion

```bash
bun run stage6:candidate:prepare -- \
  --evidence-root /secure/stage6-rc1 \
  --release-evidence release-evidence.json \
  --internal-cost-receipt internal-cost-receipt.json \
  --capacity-receipt capacity-receipt.json \
  --desktop-receipt desktop-receipt.json \
  --incident-receipt incident-receipt.json \
  --operations-receipt operations-receipt.json \
  --penetration-receipt penetration-receipt.json \
  --recovery-receipt recovery-receipt.json \
  --residency-receipt residency-receipt.json \
  --slo-receipt slo-receipt.json \
  --worker-supply-chain-receipt worker-supply-chain-receipt.json \
  --manifest-output bundle.json \
  --receipt-output candidate-bundle-receipt.json
```

The preparer accepts only unique bounded regular files below one non-symlink evidence root, computes the references rather
than trusting operator-supplied digests, performs the full v4 semantic validation before publishing, and creates both
files plus their SHA-256 sidecars without overwriting prior evidence. A missing receipt, unsafe path, semantic mismatch, or
existing output leaves no new candidate files. The lower-level validator remains available for offline revalidation and
historical v2/v3 audit; it is not the preferred v4 creation path.

The upstream `synara-stage6-release-evidence-v2` collector likewise rechecks the exact HEAD and clean worktree after all
source/evidence hashes are computed and before publication. It publishes the manifest and sidecar as exclusive `0600`
files with rollback, so candidate preparation cannot consume an overwritten or half-published root identity.

All ten required validators use an immutable publisher for their receipt and sidecar. Internal cost evidence binds
Token totals, Provider cost coverage, currency-safe known cost, and actual-over-estimated platform allocation without
payment fields; Capacity, Desktop, Incident, Operations, Penetration, Recovery, Residency, SLO and Worker supply-chain
share the same tested I/O module. Each attempt therefore has an exclusive `0600` byte identity. Collisions fail and
sidecar failure rolls back the receipt before the candidate preparer can reference it.

The lower-level candidate-bundle validator, Documentation and Operations UI source validators, and downstream final GA
review archive validator use that same publisher for their file receipts. Revalidation must choose a new output path, so
historical candidate verdicts and source-gate snapshots cannot be silently replaced after review. A source regression test
rejects direct final-output writes in all fourteen publishers.

The output schema is `synara.stage6-candidate-evidence-bundle-validation.v4` with
`requiredReceiptCount=10`. Its assessment remains `evidence-consistent-not-ga-approved`.
`eligibleForCandidateEvidenceReview=true` proves same-candidate consistency and release-environment eligibility only.

The Python validator continues to read historical v2/v3 bundles for offline audit. Protected Desktop publication,
Release Governance service ingestion, SQLite triggers, and PostgreSQL Migration 153 accept only v4 for a new or active
candidate. Migration 153 preserves terminal v2/v3 rows as immutable history but fails if an active commercial candidate
has not been closed or replaced. This prevents a historical payment-oriented candidate from advancing in the internal
self-hosted product profile.

Protected publication does not trust the validation receipt's top-level eligibility booleans by themselves.
`synara.stage6-candidate-release-binding.v2` independently requires all ten projected receipt schemas, permanent non-pass
assessments, safe unique paths, non-zero hashes, readiness flags and timestamp closure. It also rejects incomplete candidate
Artifacts/origins/Regions/Migration identity, malformed Release Evidence projection, Desktop artifact-set drift, Residency
Region drift, a missing/zero Desktop Enrollment evidence-set digest, or weakened Recovery/Residency external-authority boundaries. The Python-to-TypeScript contract test executes
the real preparer output through that verifier so the two implementations cannot silently drift.

`bun run stage6:environment:prepare -- ...` converts the verified receipt into
`synara.stage6-protected-environment-configuration.v1`. It derives the receipt digest, Desktop artifact-set digest,
lockfile and single-line receipt base64 from the exact verified bytes while requiring the planned candidate ID, source
commit and same workflow run ID as independent inputs. The private `0600` output is immutable, has a SHA-256 sidecar and
contains exactly the five Environment variables, one Environment secret and structured review comment consumed by the
protected workflow. It does not call GitHub or grant approval. The 32 KiB receipt bound ensures its base64 remains below
GitHub's documented 48 KiB secret limit.

Human authority over the Registry, Vault/KMS, Rekor, cluster, Security, Operations, Product, Engineering, Legal, and
Privacy remains outside the structural receipt. The receipt never changes its non-GA assessment.
