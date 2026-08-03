# Stage 6 operations UI matrix validator

The validator proves that every required daily-operation row has source markers in a UI implementation, typed client and
authenticated HTTP route, and that the matrix declares no CLI/database dependency. Matrix v2 also binds every operation
to an owning product surface and a real host registration seam. All 20 Tenant Web and 29 Platform Admin operations are
now `reachable`; the former unmounted component in Tenant Settings has been removed, and every Platform row points to
the independently built `apps/admin` host.

```bash
python3 scripts/stage6-operations/validate_operations_ui_matrix.py \
  --repository-root . \
  --matrix docs/release-matrices/stage-6-operations-ui-v1.json \
  --output /secure/stage6-rc1/operations-ui-source-receipt.json

python3 -m unittest discover -s scripts/stage6-operations -p 'test_*.py'
```

The receipt always states
`source-ui-routes-validated-all-surfaces-reachable-not-operations-passed`. A deployed role-separated browser exercise
with Audit evidence remains required even though every source surface is reachable. Its optional file output and sidecar
use the shared immutable `0600` publisher and reject collisions.

The deployed exercise validator binds all 49 rows to one exact candidate and deployment-unique environment ID, the checked-in matrix hash, eleven distinct
role accounts (including the negative `tenant-member` account), positive and reviewed negative-role browser evidence,
globally unique request IDs, Support Access lifecycle evidence, and distinct Operations/Security approvals:

For a real exercise, prefer the fail-closed preparer. Put the draft and every browser/Audit evidence file below one
access-controlled bundle root. The draft uses
`synara.stage6-operations-browser-exercise-draft.v2`, omits `matrixSha256`, and represents each
`positiveEvidence`, `negativeEvidence`, `supportAccess.evidence` and approval `evidence` as a relative path string. The
preparer reads every input as a stable bounded non-symlink file, rejects obvious credential material, derives the current
source-matrix digest, materializes all evidence SHA-256 values, validates the complete manifest, then exclusively publishes
the manifest/receipt and both sidecars as `0600` files:

```bash
bun run stage6:operations:prepare -- \
  --evidence-root /secure/operations-exercise \
  --draft operations-draft.json \
  --repository-root . \
  --manifest-output manifest.json \
  --receipt-output receipt.json
```

Outputs must be new paths. A validation failure publishes nothing; a receipt publication failure removes the already
published manifest pair. The preparer computes integrity metadata only. SSO/MFA sessions, browser actions, request IDs and
Operations/Security decisions remain externally produced facts and are never inferred or upgraded by this command.

The lower-level validator remains available for an already materialized manifest:

```bash
python3 scripts/stage6-operations/validate_operations_exercise_evidence.py \
  --manifest /secure/operations-exercise/manifest.json \
  --evidence-root /secure/operations-exercise/evidence \
  --repository-root . \
  --matrix docs/release-matrices/stage-6-operations-ui-v1.json \
  --output /secure/operations-exercise/receipt.json
```

Failed, blocked, unexpectedly authorized, fixture-authenticated, staging, CLI-assisted, developer-tools-assisted or
database-assisted rows remain valid failed evidence but are not review-eligible. The receipt always states
`evidence-validated-not-operations-passed`; only the copied GA checklist and human approvals can close the operations gate.
The receipt and sidecar use the shared Stage 6 immutable publisher with `0600`, exclusive paths and sidecar rollback.
Operations v2 additionally requires exact `support.access_revoked` and `support.access_expired` action declarations plus
Tenant-visible evidence for both terminal paths. Historical v1 receipts remain immutable audit records but cannot authorize
a new Exercise approval, Candidate projection or Release transition after Migration 162.
