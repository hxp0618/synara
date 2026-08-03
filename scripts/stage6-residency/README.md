# Stage 6 data-residency deployment evidence validator

`validate_data_residency_evidence.py` validates the deployment Annex evidence required by
`docs/contracts/data-residency-v1.md`. It never turns an execution Region statement into a customer residency approval and
every receipt permanently uses:

```text
evidence-validated-not-residency-approved
```

## Prepare an evidence bundle

Keep the eight finalized JSON attachments beneath one attempt root. Create a
`synara.data-residency-deployment-evidence-draft.v1` file with exactly `schemaVersion` and `evidence`; each evidence value
is a POSIX path relative to that root, not a hand-authored hash. Then run:

```bash
bun run stage6:residency:prepare -- \
  --evidence-root /secure/input/residency-attempt-001 \
  --draft residency-draft.json \
  --manifest-output residency-manifest.json \
  --receipt-output residency-receipt.json \
  --expected-candidate-binding-sha256 sha256:<64-lowercase-hex>
```

The preparer rejects traversal, symlinks, duplicate JSON fields, common credential material and unstable/oversized files.
It derives all eight manifest hashes from the same stable bytes used for validation, then publishes manifest, receipt and
both SHA-256 sidecars as exclusive `0600` files. If receipt publication fails, the new manifest pair is removed. Existing
attempt paths are never overwritten.

The preparer does not edit or synthesize Annex, exercise or approval documents. Exercises must already bind the finalized
Annex/runtime hashes, and the three approvals must already bind the finalized five-file base evidence set; this preserves
their external signature boundary.

## Evidence manifest

The `synara.data-residency-deployment-evidence.v1` manifest is intentionally only an index. It contains exact unique
`{path, sha256}` references for these eight UTF-8 JSON attachments:

| Manifest field         | Required attachment schema                                        |
| ---------------------- | ----------------------------------------------------------------- |
| `annex`                | `synara.data-residency-annex-evidence.v1`                         |
| `runtimeInventory`     | `synara.data-residency-runtime-inventory-evidence.v1`             |
| `browserStatement`     | `synara.data-residency-browser-statement-evidence.v1`             |
| `failoverExercise`     | `synara.data-residency-exercise-evidence.v1`                      |
| `evacuationExercise`   | `synara.data-residency-exercise-evidence.v1`                      |
| `operationsApproval`   | `synara.data-residency-approval-evidence.v1`, role `operations`   |
| `securityApproval`     | `synara.data-residency-approval-evidence.v1`, role `security`     |
| `privacyLegalApproval` | `synara.data-residency-approval-evidence.v1`, role `privacyLegal` |

The manifest cannot separately assert candidate, Tenant, policy, inventory, exercise outcome, or approval. Those values are
derived from the attachment contents.

## Common subject

Every attachment has the exact same `subject` with:

- Tenant ID;
- candidate ID, full commit, `bun.lock` SHA-256, environment class/ID, migration tail;
- Control Plane, Worker, Provider Host, Web, Admin, and all four native Desktop artifact digests;
- `synara-data-residency-statement-v1` policy version/digest/home Region/sorted allowed Regions/enforcement state.

Any cross-Tenant, cross-candidate, cross-environment, cross-artifact, cross-migration, or cross-policy attachment is rejected.
The validator also exposes `candidateBindingSha256` for the release pipeline's pre-collection candidate lock.

## Exact attachment shapes

Every root object contains only `schemaVersion`, `subject`, its payload fields below, and `verificationBoundary`:

- Annex: `annex` and `processingPlanes`. `annex` contains exactly `annexId`, `promiseScope`, `effectiveAt`,
  `reviewExpiresAt`, `disasterRecoveryOptInRequired`, and `noAllowedDestinationBehavior`.
- Runtime inventory: `inventory` and `processingPlanes`. `inventory` contains exactly `inventoryId`, `sourceAuthority`,
  `capturedAt`, and `validUntil`.
- Browser statement: `surface`, `generatedAt`, and `statement`. `statement` contains exactly `statementType`, `tenantId`,
  `homeRegion`, `policyVersion`, `policyDigest`, `enforcementState`, and `allowedRegions`.
- Exercise: `exercise`, containing exactly `exerciseId`, `exerciseKind`, `executedAt`, `sourceRegion`,
  `selectedDestinationRegion`, `result`, `outOfPolicyProbe`, `annexSha256`, and `runtimeInventorySha256`.
- Approval: `approval`, containing exactly `role`, `approverId`, `decision`, `approvedAt`, `expiresAt`, and
  `evidenceSubjects`. `evidenceSubjects` has exactly the five base-evidence manifest fields.

Both `processingPlanes` objects contain exactly these keys:

```text
postgresqlPrimary, postgresqlReplicas, postgresqlBackups, postgresqlRestoreProcessing,
objectStoragePrimary, objectStorageReplicas, objectStorageBackups, objectStorageDeletionProcessing,
kmsKeys, kmsKeyBackups, queue, logs, traces, metrics, incidentBundles, supportProcessing,
providerRequestProcessing, providerRetention, subprocessorProcessing, disasterRecoveryDestinations
```

Every plane contains exactly `status`, `regions`, and `disclosed`. `known` requires a non-empty sorted unique Region list;
`unknown` and `global` require an empty list and can never make the evidence review-eligible.

## Semantic bindings

- The Annex and runtime inventory each contain every required PostgreSQL, object storage, KMS, Queue, observability,
  incident, Support, Provider, subprocessor, retention, deletion, backup/restore, and DR processing plane. Their normalized
  inventories must be exactly equal.
- The browser statement must exactly reproduce the common Tenant and policy subject.
- Failover and evacuation bind the exact Annex and runtime-inventory file digests. They must select an allowed DR inventory
  destination and record a rejected destination outside the allowed Regions.
- Each approval binds the exact Annex, runtime inventory, browser statement, failover, and evacuation digest set. Operations,
  Security, and Privacy-Legal require distinct roles, files, and approver identities.
- Runtime inventory, browser statement, exercises, Annex effective date, approval times, review expiry, and validation time
  are checked as one ordered validity window.

The receipt's `evidenceSetSha256` is SHA-256 over the eight sorted lines
`<manifest-field>=sha256:<digest>\n`. A changed attachment therefore creates a new approval subject and evidence set.

## Verification boundary

This validator performs strict JSON schema, content, digest, subject, time, and cross-file consistency validation. It does
**not** verify a cryptographic signature, approver identity, deployment inventory authority, deployed browser origin, or live
exercise authority. Every attachment must say so through its exact `verificationBoundary.externalVerification` value, and
the receipt always records:

```json
{
  "strictAttachmentContentAndSubjectValidated": true,
  "cryptographicSignaturesVerified": false,
  "externalSignatureIdentityAndAuthorityVerificationRequired": true
}
```

The signed Annex, role identities/signatures, real deployment authority, and live exercise provenance remain mandatory human
GA evidence. An attachment that claims this validator verified a signature is rejected.

## Eligibility and filesystem limits

`eligibleForHumanGateReview` is true only for a `full-data-residency` Annex in a `production` or `production-like`
environment with a restricted policy, allowed home Region, DR opt-in, exact Annex/runtime agreement, all planes
known/disclosed/allowed, both exercises satisfied, and approvals current through Annex review. It is still not a residency
approval.

Evidence paths must be traversal-free POSIX paths below a regular non-symlink root. Symlinks, duplicate resolved files,
invalid JSON, hash mismatches, files over 16 MiB, aggregate evidence over 64 MiB, manifests over 1 MiB, and more than 16
Regions per plane are rejected.

For independent checking of an already materialized manifest, run:

```bash
bun run stage6:residency:validate -- \
  --manifest /secure/input/data-residency-manifest.json \
  --evidence-root /secure/input/evidence \
  --expected-candidate-binding-sha256 sha256:<64-lowercase-hex> \
  --output /secure/output/data-residency-receipt.json
```

Run the focused suite with:

```bash
python3 -m unittest scripts/stage6-residency/test_validate_data_residency_evidence.py
```

The validator stable-reads each bounded attachment exactly once for secret scanning, hashing and strict duplicate-safe JSON
parsing. The receipt remains evidence consistency only; it does not authenticate the signatures, identities, deployed
browser, inventory authority or live exercise provenance.
