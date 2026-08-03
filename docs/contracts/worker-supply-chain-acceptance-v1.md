# Worker Supply-Chain Acceptance v1

## Purpose

This contract binds the Worker image in one Stage 6 release manifest to the existing Stage 3 production Registry and
Vault/KMS admission reports. It reuses those gates; it does not introduce a second signing, scanning, or admission system.

## Inputs

`validate_worker_supply_chain_evidence.py` accepts three immutable JSON files:

- a `synara-stage6-release-evidence-v2` manifest collected from a clean commit;
- a passing `synara.worker-registry-release-gate.v2` report;
- a passing `synara.vault-kms-admission-gate.v1` report.

The validator computes the SHA-256 of the exact Registry report bytes. The admission report must contain that digest, and
all three inputs must name the same full Git commit. The release manifest Worker digest must equal both reproducible
Registry build digests and the digest component of the admission report's cached signed image.

Each input is stable-read once as a non-empty regular non-symlink file with a 32 MiB bound. The exact bytes used for hashes
are also used for common credential-material scanning and duplicate-field-safe UTF-8 JSON parsing. A file changed during
open/read, a symlink, duplicate JSON field, private key, AWS access key, bearer credential or credential-bearing URL is
rejected before semantic approval checks.

## Required controls

The Registry evidence must contain cached and no-cache builds with identical `linux/amd64` and `linux/arm64` platform
digests. Each platform must have SPDX and SLSA OCI attestations. The production profile must use KMS signing and verified
Rekor transparency-log, inclusion-proof, and signed-entry-timestamp evidence.

The vulnerability policy blocks `HIGH` and `CRITICAL`, does not ignore unfixed findings, rejects end-of-life operating
systems, permits no exceptions, and limits the Trivy database age to 24 hours. Both platforms must have zero blocked,
waived, and secret findings. Report-output secret scans must also be empty.

The admission evidence must admit the signed digest and deny unsigned, wrong-key, and tag-drift probes. Deployment,
StatefulSet, Job, and CronJob wrong-key probes must all be denied. Registry, signing, and admission temporary state must be
removed through exact-owner cleanup; broad cleanup is forbidden.

## Receipt and approval boundary

The receipt schema is `synara.stage6-worker-supply-chain-evidence.v1`. Its assessment is permanently
`evidence-validated-not-worker-supply-chain-approved`: structural and cryptographic evidence consistency is not a human
release approval and does not establish the authority of the production Registry, KMS, Rekor, cluster, or operators.
The receipt and SHA-256 sidecar are published by the shared Stage 6 immutable I/O boundary as exclusive `0600` files.
Existing attempt paths are rejected, and sidecar failure rolls back the new receipt before candidate preparation.

Stage 6 candidate bundle v3 requires this receipt as its tenth evidence item. The v3 migration covers its validator,
protected release workflow, Release Governance service and PostgreSQL/SQLite database boundaries; v2 remains historical
read-only evidence and cannot authorize a new or active candidate.

## Command

```bash
bun run stage6:worker-supply-chain:validate -- \
  --release-manifest /evidence/stage6-release.json \
  --registry-report /evidence/worker-registry-release-gate.json \
  --admission-report /evidence/vault-kms-admission-gate.json \
  --output /evidence/worker-supply-chain-receipt.json
```
