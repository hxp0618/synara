# Stage 3 Worker Registry Signing-Policy Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Production signing implementation commit: `914f013c99e51bea76ecc32d16c6a2b2618e9187`
- Clean gate commit: `7659dd5fef77b272d6ed10c8a87e0a832cf6a4b4`
- Worker version: `0.5.4`
- Gate run: `stage3-worker-registry-release-5fce0af4-60ca-453a-8f17-2983c6537aa2`
- Result: **PASS FOR THE SIGNING-POLICY IMPLEMENTATION AND DISPOSABLE REGISTRY SLICE; PRODUCTION SIGNING EVIDENCE REMAINS OPEN**

## 1. Scope and evidence boundary

The checked-in `deploy/worker/signing-policy.json` now selects one of three explicit Cosign modes:

- `ephemeral-key` keeps the disposable exact-digest mechanics gate. It creates an isolated key, disables the
  transparency log, verifies the source annotations, and removes private-key and signing state.
- `keyless` requires a TLS Registry, transparency-log upload and verification, a controlled identity-token
  environment value, and an exact or anchored RE2-compatible certificate identity and OIDC issuer. The token is
  written only to a gate-owned `0600` file accepted by Cosign, redacted before any command can fail, and deleted.
- `kms-key` requires a TLS Registry, transparency-log upload and verification, an approved AWS/GCP/Azure/Vault KMS
  URI, and only the credential environment names listed by the policy. Credential values are redacted and never
  placed in Docker arguments or reports.

Production modes reject `--insecure-registry`. Reports retain the non-Secret policy identity, signing-policy SHA,
certificate/KMS identity where applicable, transparency-log conclusion, and cleanup result. The checked-in default
remains `ephemeral-key`; this run therefore proves the implementation path and disposable cryptographic mechanics,
not a real production signer identity.

## 2. Clean-SHA reproducibility

The gate performed one cached and one independent no-cache `linux/amd64,linux/arm64` build and pushed both to the
dedicated loopback Registry.

| Slot     | OCI index digest                                                          |
| -------- | ------------------------------------------------------------------------- |
| cached   | `sha256:912223cbe7ef311b16e88302dd8a6afe9bdc508dbff26ec367b74fd1db669340` |
| no-cache | `sha256:630bff035f960cc0f895937b72db8af8328e350cdd40d7cb273e3a50b4e515bf` |

The indexes differ because their SLSA provenance records different cached/no-cache invocations. Their publishable
platform manifests are identical:

| Platform      | Cached manifest                                                           | No-cache manifest                                                         | Result |
| ------------- | ------------------------------------------------------------------------- | ------------------------------------------------------------------------- | ------ |
| `linux/amd64` | `sha256:2d0b9d8a98832afb530edc46044c5878e0f4acf04aeb2a3f58f22eec77d72b45` | `sha256:2d0b9d8a98832afb530edc46044c5878e0f4acf04aeb2a3f58f22eec77d72b45` | pass   |
| `linux/arm64` | `sha256:7fd11ce02d60140098f1b831d6acf762a3b4513140cf1e4f2a7d04cbd3e55192` | `sha256:7fd11ce02d60140098f1b831d6acf762a3b4513140cf1e4f2a7d04cbd3e55192` | pass   |

Both platforms contain one SPDX and one SLSA v1 attestation. Image configuration is `User=10001:10001`,
`Entrypoint=/usr/local/bin/synara-agentd`, and `WorkingDir=/data`, with no credential-like environment name. The
embedded Manifest, normalized SBOM, npm/Bun/APK lockfiles, Provider Host, agentd binary, and build revision match
across cached and no-cache builds.

## 3. Signing result

- Signing-policy SHA-256: `02d816c3db0faceeaa9c441bed75cb14c1ae51ee19ce22b38baa472c738380f0`.
- Selected mode: `ephemeral-key`.
- `productionSigningPolicySatisfied=false`; transparency-log use is intentionally false for this disposable mode.
- Ephemeral public-key SHA-256: `b1ce8cee4555d53288b62e10fb8477648586c1f408cdb3bc5ec0ea3e39265526`.
- Both exact OCI index digests verified one signature with the exact Git SHA, version, run ID, and slot annotations.
- `privateKeyRemoved=true`, `identityTokenRemoved=true`, and `signingSecretStateRemoved=true`.

A direct help probe against fixed Cosign `v3.1.1` confirmed that `--identity-token` accepts either the token or a
file path, matching the keyless `0600` file implementation. Keyless and KMS behavior is covered by
command-boundary, identity/tlog, insecure-Registry, missing-credential, and cleanup tests; no real production token,
KMS key, certificate, or Rekor entry was created in this run.

## 4. Vulnerability and Secret policy

The vulnerability-policy SHA-256 is
`fc64869e7dd6a7d82453ccc1fe80d2878d75406c9b6b798eb5f2d804c9d1401f`. Both Alpine `3.24.1` images passed with
`HIGH=0`, `CRITICAL=0`, Secret=0, no EOSL result, no waiver, and no stale exception. The Trivy database was `32,227`
seconds old against the checked-in 24-hour maximum.

`GO-2026-5932` remains visible as one `UNKNOWN` module-level finding for `golang.org/x/crypto v0.54.0` on each
platform. The prior reviewed evidence remains unchanged: agentd does not import the affected deprecated
`openpgp` package and `govulncheck v1.6.0` reports zero affected vulnerabilities, so no waiver was introduced.

The first gate attempt at `914f013c` completed reproducible builds and signing but failed closed when the Trivy DB
OCI download ended with a transient `unexpected EOF`. Commit `7659dd5f` adds one bounded retry only when the failure
is both a vulnerability-DB download and a recognized transient network error. Scan findings, stale databases,
invalid reports, policy failures, and a second download failure still fail immediately. This passing run used
`transientDatabaseDownloadRetries=0`.

## 5. Cleanup and report identity

- `broadCleanupUsed=false` at both release-gate and supply-chain layers.
- Every gate-owned inspection container/image and isolated state directory was removed.
- The output Secret scan covered two files and `28,841` bytes, exercised two known-secret canaries, and found zero
  private-key, AWS access-key, GitHub-token, or OpenAI-style key patterns.
- The two Registry tags and their signatures were retained through report generation, then the task cleanup removed
  the dedicated Registry, builder, volumes, raw reports, and diagnostic images by exact identity without prune.

The raw output directory was `/tmp/synara-stage3-worker-registry-signing-policy-7659dd5f/`. Its report hashes were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `a0847cd548101bbe97ab5267ec5195967a60e2fee3564db39f0316986e9b3365` |
| Markdown | `e16e92e9d4eb119c7dfaefce6a28176094d77880d98b0e2a5976af5ff38d17a4` |

## 6. Automated validation and DDL boundary

- Registry release tests: `22/22`.
- Registry supply-chain tests: `18/18`.
- All release-gate tests: `90/90`.
- Stage 3 Python tests: `193/193`.
- Python compilation and `git diff --check`: pass.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 7. Required production evidence

To close production signing, an operator must choose and commit one policy using a TLS production Registry:

1. Keyless: approved certificate identity or anchored regexp, approved OIDC issuer or anchored regexp, and the
   environment name containing the short-lived identity token.
2. KMS: approved `awskms://`, `gcpkms://`, `azurekms://`, or `hashivault://` key URI and the minimal allowed
   credential environment names.
3. Both modes: production Registry authentication configured outside gate arguments, retained immutable digests,
   transparency-log evidence, admission-policy verification, and the Registry retention record.

Real Codex/Claude SSH, Docker, and Kubernetes gates, immutable Revision canary/promote/rollback under load,
multi-node production behavior, cross-Target Artifact/Checkpoint/Retention concurrency, and long-session soak also
remain open.
