# Stage 3 Worker Registry Clean-SHA Release Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `dc43a4d6a1cf94f29bb5335037f66ed360bf5d0a`
- Worker version: `0.5.4`
- Gate run: `stage3-worker-registry-release-437bdbd5-353f-4120-a83d-a7d3feee6eba`
- Result: **PASS FOR THE CLEAN-SHA REGISTRY REPRODUCIBILITY SLICE; STAGE 3 RELEASE GATE REMAINS OPEN**

## 1. Scope

`registry_release_gate.py` ran from a clean checkout and pushed two independent multi-platform Worker builds to a
disposable OCI Registry:

```text
clean SHA -> cached linux/amd64 + linux/arm64 build -> Registry push -> platform inspection
          -> no-cache linux/amd64 + linux/arm64 build -> Registry push -> platform inspection
          -> platform digest consensus -> cleanup and output Secret scan
```

The gate required Registry-returned OCI digests, exactly one `linux/amd64` and one `linux/arm64` image manifest,
per-platform SPDX and SLSA attestations, non-root runtime configuration, embedded Worker Manifest/SBOM/lockfiles,
Provider Host and agentd binaries, and a build-revision marker equal to the source Git SHA.

## 2. Clean-commit build results

| Slot     | Registry tag                                | OCI index digest                                                          | Result |
| -------- | ------------------------------------------- | ------------------------------------------------------------------------- | ------ |
| cached   | `stage3-dc43a4d6a1cf-7147b3665d3a-cached`   | `sha256:5b5f7f4834c395305aa5afe347efb6d914b7c0b85ee4ddcaadfdef553e4986db` | pass   |
| no-cache | `stage3-dc43a4d6a1cf-7147b3665d3a-no-cache` | `sha256:17eff0ebbc7458cfdd53efd21f69577103d9b3692c4b2016024e28c7d367ba68` | pass   |

The two OCI index digests intentionally differ because BuildKit provenance records the cached versus no-cache
invocation. The publishable platform image manifests are byte-identical across both builds:

| Platform      | Cached manifest                                                           | No-cache manifest                                                         | Consensus |
| ------------- | ------------------------------------------------------------------------- | ------------------------------------------------------------------------- | --------- |
| `linux/amd64` | `sha256:452f18ab77af515bede9c7d7bef3d1c24e4dabf37d03c9603b11c96e3506d8fb` | `sha256:452f18ab77af515bede9c7d7bef3d1c24e4dabf37d03c9603b11c96e3506d8fb` | pass      |
| `linux/arm64` | `sha256:05c6821b51de1edac5af3d2d6ebf29fac8528579d565ada0b33d2b396597f2d7` | `sha256:05c6821b51de1edac5af3d2d6ebf29fac8528579d565ada0b33d2b396597f2d7` | pass      |

## 3. Supply-chain and runtime evidence

Both build slots and both platforms passed the same assertions:

- Source records `worktreeDirty=false`, Git SHA `dc43a4d6a1cf94f29bb5335037f66ed360bf5d0a`, and
  `SOURCE_DATE_EPOCH=1784231314`.
- The checked-in BuildKit scanner resolves to
  `docker.io/docker/buildkit-syft-scanner@sha256:79e7b013cbec16bbb436f312819a49a4a57752b2270c1a9332ae1a10fcc82a68`.
- Each platform has exactly one `https://spdx.dev/Document` and one `https://slsa.dev/provenance/v1` attestation.
- Image config is `User=10001:10001`, `Entrypoint=/usr/local/bin/synara-agentd`, `WorkingDir=/data`, with no
  credential-bearing runtime environment name.
- The embedded Manifest records the clean source SHA, target architecture, three digest-pinned base images, and
  Provider runtimes: Codex CLI `0.144.1`, Claude Code `2.1.197`, and Claude Agent SDK `0.3.207`.
- Embedded npm, Bun, and APK lockfiles match the clean checkout byte-for-byte. The normalized SPDX creation time,
  Provider Host bundle, architecture-specific agentd binary, canonical wrapper, and `.build-revision` are present.
- Cached and no-cache inspection produced identical per-platform Manifest, SBOM, lockfile, Provider Host, agentd,
  and build-revision hashes.

## 4. Cleanup and security boundary

- Every gate-owned inspection container, local pulled image, and isolated state directory was removed.
- `broadCleanupUsed=false`; no Docker or BuildKit prune was used.
- The gate retained both Registry tags through report generation. After the report hashes were recorded, the
  disposable Registry, its anonymous data volume, the dedicated Buildx builder, and the pre-existing manual
  inspection container from this diagnostic slice were removed by exact name.
- The output Secret scan covered two report files and `20,390` bytes, exercised one known-secret canary, and returned
  zero findings for private-key, AWS access-key, GitHub token, and OpenAI-style key patterns.

## 5. Raw report identity

The raw output directory was `/tmp/synara-stage3-worker-registry-release-dc43a4d6/`. It was removed after the hashes
below were recorded; this report preserves the immutable source and report identities without checking in generated
logs or temporary state.

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `3ca9c1e0df7d2c90ee2b26a903cfc4619d9be933b229db13371194cb3605eb4a` |
| Markdown | `5788217fb15d43684853a265d06053eed0fa3e2a9be942887e252e63ca47167b` |

## 6. DDL boundary

This slice adds or changes no database DDL. The checked-in migration boundary remains
`000041_diff_artifact_kind.sql`.

## 7. Remaining release gates

This evidence closes the clean-SHA disposable Registry push, required multi-arch image shape, reproducible platform
content, embedded supply-chain inputs, and BuildKit SBOM/provenance attachment. It does not close:

1. Image signing, signature-policy enforcement, vulnerability policy, or production Registry retention.
2. Production Registry Credential and Tenant/Target-scoped pull-binding validation.
3. Real Codex/Claude SSH, Docker, and Kubernetes four-matrix release gates.
4. Immutable Revision canary, promote, rollback-under-load, and multi-node Kubernetes evidence.
5. Cross-Target Artifact/Checkpoint/Retention concurrency and long-session soak.
