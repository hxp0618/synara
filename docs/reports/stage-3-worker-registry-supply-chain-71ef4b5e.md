# Stage 3 Worker Registry Supply-Chain Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `71ef4b5ec7026adfb44bb03cd132c38c11abd6bf`
- Worker version: `0.5.4`
- Gate run: `stage3-worker-registry-release-50d0b877-3b41-471c-8580-5cc6d9d97da4`
- Result: **PASS FOR THE DISPOSABLE REGISTRY SUPPLY-CHAIN SLICE; STAGE 3 RELEASE GATE REMAINS OPEN**

## 1. Scope

`registry_release_gate.py` ran from a clean checkout against a dedicated disposable Registry and
`docker-container` Buildx builder. It performed cached and independent no-cache
`linux/amd64,linux/arm64` pushes, then required:

- identical publishable platform manifests across both builds;
- Registry-returned OCI indexes with one SPDX and one SLSA attestation per platform;
- non-root runtime configuration and embedded Manifest/SBOM/lockfile/runtime identity;
- exact-digest Cosign signing and annotation verification with an isolated ephemeral key;
- Trivy vulnerability, Secret, OS EOSL, and database-freshness policy checks;
- exact local state cleanup and a final output Secret scan without Docker-wide prune.

The local Registry used HTTP only because it was disposable and bound to loopback. This is not evidence for a
production TLS Registry, production Registry Credential, or retention policy.

## 2. Reproducible build results

| Slot     | Registry tag                                | OCI index digest                                                          |
| -------- | ------------------------------------------- | ------------------------------------------------------------------------- |
| cached   | `stage3-71ef4b5ec702-92c221a566f5-cached`   | `sha256:1a824e40e19fbc36b3b3b10609cf8ce43570e713087d37e6dc80696dd41805a2` |
| no-cache | `stage3-71ef4b5ec702-92c221a566f5-no-cache` | `sha256:ed27ae93611bb83786a3636a1058e3a790f3f978c241f6a0d9e7b22f5961b9d2` |

The index digests differ because the attached provenance records distinct cached/no-cache invocations. The
publishable platform manifests are byte-identical:

| Platform      | Cached manifest                                                           | No-cache manifest                                                         | Result |
| ------------- | ------------------------------------------------------------------------- | ------------------------------------------------------------------------- | ------ |
| `linux/amd64` | `sha256:e9b07271bb8d7d9e17ebea8ec23102152d60487425bb7c3913121d146e00bb16` | `sha256:e9b07271bb8d7d9e17ebea8ec23102152d60487425bb7c3913121d146e00bb16` | pass   |
| `linux/arm64` | `sha256:884d49bffc20654471753c8484d84540f38fc342d62b8e8c944c5f832dd32fef` | `sha256:884d49bffc20654471753c8484d84540f38fc342d62b8e8c944c5f832dd32fef` | pass   |

Both platforms carry SPDX and SLSA v1 attestations. Image config is `User=10001:10001`,
`Entrypoint=/usr/local/bin/synara-agentd`, and `WorkingDir=/data`, with no credential-like environment name.

## 3. Signing and vulnerability policy

The checked-in supply-chain tool lock resolved to:

- Cosign `v3.1.1`:
  `gcr.io/projectsigstore/cosign:v3.1.1@sha256:6bbe0d281d955c79f85b325f0f7e651c1bcab5a4fa4ad4903d74955178a3b2eb`;
- Trivy `0.72.0`:
  `ghcr.io/aquasecurity/trivy:0.72.0@sha256:cffe3f5161a47a6823fbd23d985795b3ed72a4c806da4c4df16266c02accdd6f`;
- tool-lock SHA-256: `fa84a17b89e286a4a94b10ceaff670663b42af66cee49431f1a1f09f98c854ac`.

The gate generated an isolated ephemeral Cosign key, signed both exact OCI index digests, and verified one signature
per index with exact `git-sha`, `version`, `run-id`, and `slot` annotations. The private key and isolated signing
state were removed. Transparency log use was disabled, so `productionSigningPolicySatisfied=false`; production
KMS/keyless identity and tlog enforcement remain open.

The vulnerability policy SHA-256 is
`fc64869e7dd6a7d82453ccc1fe80d2878d75406c9b6b798eb5f2d804c9d1401f`. It blocks `HIGH` and `CRITICAL`, does not
ignore unfixed findings, has zero exceptions, rejects EOSL operating systems, and requires a database no older than
24 hours.

| Platform      | Critical | High | Medium | Low | Unknown | Secrets | EOSL |
| ------------- | -------- | ---- | ------ | --- | ------- | ------- | ---- |
| `linux/amd64` | `0`      | `0`  | `0`    | `0` | `1`     | `0`     | no   |
| `linux/arm64` | `0`      | `0`  | `0`    | `0` | `1`     | `0`     | no   |

Both images use Alpine `3.24.1`. The Trivy DB was about `29,129` seconds old, below the 24-hour maximum.

### Reviewed `UNKNOWN` finding

Trivy retained `GO-2026-5932` against `golang.org/x/crypto v0.54.0` in the agentd binary as a module-level
`UNKNOWN` finding. The official Go record applies only to the deprecated `golang.org/x/crypto/openpgp` package and
has no fixed version. No waiver was added:

- `go mod why golang.org/x/crypto/openpgp` reports that the main module does not need the package;
- the agentd dependency graph contains no `golang.org/x/crypto/openpgp` package;
- `govulncheck v1.6.0 -show verbose ./cmd/agentd` reports `Your code is affected by 0 vulnerabilities` and classifies
  the record only as one required module whose vulnerable package is not imported or called.

The finding remains visible in both raw scan summaries for future review instead of being suppressed.

## 4. High-vulnerability and reproducibility fixes

- Go security dependencies were updated to `x/crypto v0.54.0`, `x/net v0.57.0`, `x/sys v0.47.0`, and
  `x/text v0.40.0`, with their required `x/sync` and `x/term` versions.
- The Worker now installs locked npm `12.0.1`, whose bundled undici is `6.27.0`, and points npm, npx, and node-gyp
  to that checked-in dependency tree after removing the vulnerable npm bundled in the base image.
- The provider-tools lock delta is restricted to npm `12.0.1` and its bundled subtree and uses only the official
  npm Registry URL.
- An initial clean-SHA gate correctly failed reproducibility before signing because npm startup left
  non-deterministic `/tmp/node-compile-cache` bytes in the replacement layer. The final Dockerfile removes that
  cache in the same layer. A targeted cached/no-cache build using the release exporter's
  `rewrite-timestamp=true` then produced the same manifest
  `sha256:40b3fc729ca93755383860cc744b844d06656bc06155f038798f353507b59955` before the complete gate was rerun.

This failure was fixed at the image-content boundary; no reproducibility, vulnerability, or Secret gate was
weakened.

## 5. Cleanup and report identity

The gate recorded `broadCleanupUsed=false`, removed the ephemeral private key and isolated supply-chain state, and
retained the two Registry tags through report generation as designed. The output Secret scan covered two files and
`28,253` bytes, exercised two known-secret canaries, and found zero private-key, AWS access-key, GitHub-token, or
OpenAI-style key patterns.

The raw output directory was `/tmp/synara-stage3-worker-registry-supply-chain-71ef4b5e/`. The final task cleanup
removed this directory, the dedicated builder, the disposable Registry/container volume, and diagnostic images by
exact name after the following hashes are recorded:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `b4189a4261b15d76395a3d6b08964900e95d32ab6b632111078d7dfd29ceccd8` |
| Markdown | `3f2e8382b7c6c657eb8f9652891a90d4763549d5a49bf3e172b1890c6fbfa07d` |

## 6. DDL boundary and remaining gates

This slice adds or changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

This evidence closes the disposable clean-SHA multi-architecture reproducibility, embedded supply-chain identity,
ephemeral exact-digest signing mechanics, `HIGH/CRITICAL=0`, Secret=0, EOSL, and vulnerability-database freshness
slice. It does not close:

1. Production KMS/keyless signing identity, transparency-log enforcement, Registry Credential, or retention.
2. Real Codex/Claude SSH, Docker, and Kubernetes four-matrix release gates.
3. Immutable Revision canary, promote, rollback-under-load, and multi-node Kubernetes evidence.
4. Cross-Target Artifact/Checkpoint/Retention concurrency and long-session soak.
