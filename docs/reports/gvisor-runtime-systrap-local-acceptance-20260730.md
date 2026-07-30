# gVisor systrap local runtime acceptance — 2026-07-30

Status: **local runtime substrate passed; Synara real-Provider release gate not yet run**.

This report records a disposable local Kind acceptance run for the runtime and
node-attestation substrate implemented on branch `codex/gvisor-runtime-isolation`.
It is not production-cluster, multi-node, managed-cloud, Codex/Claude compatibility,
or performance-budget evidence.

## Scope

- Cluster: disposable Kind `synara-gvisor-v1`, Kubernetes `v1.33.1` node image.
- Host environment: OrbStack Docker on macOS; the Linux Kind node exposed no `/dev/kvm`.
- gVisor package: official `aarch64` release tarball, SHA-512 sidecar verified before extraction.
- `runsc`: `release-20260727.0`, OCI spec `1.2.1`.
- platform: explicitly fixed to `systrap` in `/etc/containerd/runsc.toml`.
- RuntimeClass: `synara-gvisor`, exact handler `runsc`, node selector
  `synara.io/gvisor-ready=true`.

## Observed results

1. containerd accepted handler `io.containerd.runsc.v1` with the checked-in
   RuntimeClass shape.
2. A disposable Pod using the current `synara-agentd --verify-gvisor-runtime`
   code and pinned to `runtimeClassName: synara-gvisor` reached `Succeeded` on
   `synara-gvisor-v1-control-plane`, exited `0`, and emitted zero bytes.
3. The verifier therefore passed its gVisor `/proc/version`, writable/fsync
   file, child-process, and loopback-TCP checks. A separate read-only diagnostic
   Pod observed `Linux version 4.19.0-gvisor` from `/proc/version`.
4. The checked-in `gvisor-node-attestor` image built successfully and its
   DaemonSet reached desired `1`, ready `1`, available `1`.
5. The attestor published bounded Node annotations containing exact handler,
   runsc version, lower-hex SHA-256 digests for both the exact runtime binary and
   containerd configuration, a UUID instance identity, and an RFC3339Nano
   heartbeat. Both published digests matched direct hashes inside the Kind Node.
6. The executed DaemonSet mounted only the exact read-only
   `/usr/local/bin/runsc` and `/etc/containerd/config.toml` host files; no runtime
   socket, runtime directory, or writable host path was mounted.
7. Control-plane tests proved that Target policy changes require a drained
   Execution set, invalidate stale observations/Worker manifests, and leave the
   Target offline until reconciliation succeeds.
8. Target plus active Worker Release compatibility declarations are both
   required before gVisor selection. Missing compatibility falls back only when
   `auto` policy and `minimumProfile` permit it; explicit gVisor fails before a
   Pod is created.
9. The Stage 5 coordinator now adds a strict real-Provider
   `gvisor-compatibility` case whenever an exact RuntimeClass is supplied. Its
   unit-validated report contract covers Git and all required language/package
   toolchains, PTY, signal delivery, file metadata/watch, loopback TCP,
   per-probe durations, maximum RSS, and complete Provider × Node P50/P95/P99
   aggregation. This local substrate run did not execute that real-Provider
   case.

## Deliberate non-claims

- The run did not execute the Codex/Claude × gVisor compatibility matrix.
- It did not approve P50/P95/P99 performance budgets.
- It did not validate all eligible nodes in a multi-node production pool.
- It did not promote Managed Docker + `runsc` beyond
  `single-tenant-trusted-v1`.
- It does not make `gvisor-sandboxed-v1` a default release requirement.

The release gate remains the checked-in real-Provider matrix with an explicit
`--kubernetes-runtime-class synara-gvisor` boundary. Production promotion must
attach its clean-source JSON/Markdown reports and operator-approved performance
budget separately.
