# Stage 6 candidate evidence bundle preparer and validator

This gate binds the release-evidence manifest and the internal usage-and-cost, capacity, Desktop, incident, operations, penetration,
recovery, residency, SLO and Worker supply-chain receipts to one exact candidate. It rejects mixed commits, environments,
origins, Regions, migration tails, lockfiles, Artifact digests or Worker Registry/Vault-KMS report bindings before the
bundle reaches human review.

The Recovery input must be the v2 receipt. It binds the full candidate identity, exact restored release identity, Regions,
origins, Artifact and Migration identity, one canonical recovery subject, five distinct role approvals and a closed drill
time window. The validator checks those semantics but does not claim that a real backup was restored or that external
approver identity, authority or cryptographic signatures were verified.

Prepare a new internal-self-hosted v5 bundle with the fail-closed entry point. Every input and output is relative to the non-symlink evidence
root. The output directory must already exist. The command computes the Release Evidence, compatibility matrix and ten control-receipt digests, validates the complete
candidate in a private temporary directory, and only then publishes the manifest, validation receipt, and both SHA-256
sidecars. It never overwrites an existing candidate file.

```bash
bun run stage6:candidate:prepare -- \
  --evidence-root protected/stage6-rc1 \
  --release-evidence release-evidence.json \
  --compatibility-matrix compatibility-matrix.json \
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

The lower-level validator remains available for offline revalidation and historical v2/v3/v4 audit. Its requested output path
must be new; the validation receipt and sidecar are published as exclusive `0600` files and never overwrite an earlier
audit result:

```bash
python3 scripts/stage6-candidate/validate_candidate_evidence_bundle.py \
  --manifest protected/stage6-rc1/bundle.json \
  --evidence-root protected/stage6-rc1 \
  --output /secure/stage6-rc1/candidate-bundle-revalidation-receipt.json
```

The current v5 bundle manifest contains exact `{path, sha256}` references for `releaseEvidence`, the copied
source-current compatibility matrix and all ten required receipts. The Candidate validator reruns the compatibility
checker against the captured matrix bytes and current source, projects its source inventory, and requires its Migration
tail to equal Release Evidence. The Worker receipt must match the exact release-manifest bytes, commit and Worker digest and retain the Registry
and admission report hashes. A
successful result may set `eligibleForCandidateEvidenceReview=true`, but its permanent assessment is
`evidence-consistent-not-ga-approved`. Legal, Privacy, Operations, Security, Product and Engineering still make the final
release decision from the copied GA checklist.

The receipt also emits `candidate.desktopArtifactSetSha256`, derived from the validated provenance, signed attestation
bundle and installer-attestation verification references for all four Desktop targets. Use that value for the protected
release environment; after the environment reviewer releases the job, the workflow recomputes it from the same-run files
before writing publication authorization and again before publication.

The Desktop projection additionally emits `desktopEnrollmentEvidenceSetSha256`. Before deriving it, the candidate
validator independently rechecks four native eligible targets, all Enrollment results, the structured runtime
candidate/target/window binding, canonical transition order, zero prohibited request counts, credential continuity and the
four unique Enrollment evidence references. The protected TypeScript verifier requires this projected digest to be
present and non-zero, so an old Desktop receipt shape cannot authorize the protected job.

The protected workflow reads only the actual bounded v5 receipt and recomputes its digest. Its independent v2 release
binding verifier requires the exact schema, assessment, safe path, non-zero digest, readiness flag and validation time for
all ten projected receipts plus the compatibility-matrix projection; it also validates the complete candidate Artifact/origin/Region/Migration shape, Release
Evidence projection, Desktop/Residency cross-fields and external-authority boundaries. Empty projections, duplicate paths,
future validation times, a standalone digest string or a historical v2/v3/v4 receipt cannot authorize publication. The Python
validator retains v2/v3/v4 read compatibility for audit only. New receipts contain no Stripe, Checkout, Portal, invoice,
subscription, tax, card, settlement, or other payment semantics.

After the candidate receipt passes external review, generate the exact protected Environment configuration without
manually transcribing hashes or base64:

```bash
bun run stage6:environment:prepare -- \
  --receipt /secure/stage6-rc1/candidate-bundle-receipt.json \
  --candidate-id v0.6.4-stage6.1 \
  --source-commit 0123456789abcdef0123456789abcdef01234567 \
  --build-run-id 123456789 \
  --output /secure/stage6-rc1/protected-environment.json
```

The command reuses the v2 release-binding verifier, rejects candidate/commit/run drift, and creates the configuration plus
SHA-256 sidecar with mode `0600` and no overwrite. Its stdout contains only output paths and digests; the receipt base64 is
never printed. The file contains the five exact Environment variables, one Environment secret and required structured
review comment.

GitHub's documented Environment update API does not expose the administrator-bypass toggle. An authorized administrator
can first create and verify the API-supported baseline with `bun run stage6:environment:baseline -- ...`, using the exact
protected branch HEAD and numeric GitHub User/Team IDs. The command is plan-only unless `--apply` and its exact
`--confirm-sha256` are supplied; it refuses an unprotected or drifting branch and will not replace a wait timer or different
reviewer set. Its receipt explicitly remains `environment-baseline-applied-manual-admin-bypass-required` while bypass is
enabled. The administrator must then disable **Allow administrators to bypass configured protection rules** in GitHub
settings and rerun the baseline verification until it reports `environment-baseline-ready-not-release-approved`. Then
apply and read back the exact candidate configuration:

```bash
bun run stage6:environment:apply -- \
  --configuration /secure/stage6-rc1/protected-environment.json \
  --repository synara-ai/synara \
  --reviewer User:123456 \
  --reviewer Team:654321 \
  --output /secure/stage6-rc1/protected-environment-application.json
```

The apply command first reads the exact GitHub Actions run and requires a first-attempt active `workflow_dispatch` of
`.github/workflows/release.yml` at the configured source commit. It then requires that run's source branch to enforce
administrators, stale-review dismissal, at least one approval, strict status checks, and no force-push or deletion. It
refuses all writes unless administrator bypass is already disabled, requires two to six unique reviewers, enables
self-review prevention and protected-branch-only deployments, and passes the receipt secret to `gh` through stdin rather
than argv. It reads back the Environment, all five variables and secret presence before writing a private, non-overwriting
application receipt. GitHub does not expose secret values after write, so the receipt records that boundary and is not a
deployment approval. Keep both configuration files outside source control and remove them according to the private
release-evidence policy.
