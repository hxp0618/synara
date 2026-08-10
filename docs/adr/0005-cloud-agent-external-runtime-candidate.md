# ADR-0005: Cloud Agent public runtime is an immutable external candidate

- Status: Accepted; immutable RC consumer verified
- Date: 2026-08-09

## Context

Synara previously carried editable copies of seven `@synara/cloud-agent-*` packages and the scripts that packed them. T3 Code needed the same runtime bits, but two editable host copies cannot provide one source of truth or same-bits evidence. A Synara root `bun.lock` also identifies the entire host graph; it is not the public Runtime candidate identity.

## Decision

`hxp0618/cloud-agents` is the only editable source for the public Protocol, Provider ABI, Runtime, Codex and Claude adapters, testkit, and Distribution. Synara deletes those source directories and its producer-side release helpers. Public fixes are made in `cloud-agents`, cut as one immutable GitHub Release candidate, and then consumed here through exact release-asset URLs.

Synara keeps only its host-owned surfaces: Effect Schema and re-exports in `packages/contracts`, the `apps/provider-host` compatibility bin, agentd/Control Plane lifecycle authority, Artifact/Workspace/Credential adapters, and Worker/Docker packaging.

The root `cloud-agent-candidate.lock.json` is the authority for the public source commit, candidate digest, standalone Runtime digest, and all seven package URL/version/SHA-256 tuples. Production manifests and root overrides must match it. The Node verifier and Worker producer share a fail-closed validator that binds the exact immutable RC tuples and reproduces the producer digest from sorted `name@version sha256` lines with a trailing newline. agentd and the Registry gate independently parse the embedded lock and cross-bind it to the Worker manifest projection. Worker publication records embed this candidate identity instead of presenting the Synara root lock as the public Runtime identity.

The accepted candidate is the immutable `cloud-agent-m1-rc.1` GitHub Release from source `49e8cdc6a3a4f88c7324d055ce519e9f25a8ca8a`, with candidate digest `sha256:b9931233d46aeaf1392197095483c2e3409f628a47b2ba92c8e57bb38b444676`. Anonymous downloads, the regenerated Bun lock, the installed Distribution entrypoint, and the standalone Runtime were verified against `cloud-agent-candidate.lock.json`. The earlier mutable/local candidates remain suspended historical evidence and are not accepted by this lock.

## Consumer integration refs (2026-08-10)

| Surface                 | Baseline                                                        | Integration evidence                                                                                                 | Publication boundary                                                 |
| ----------------------- | --------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------- |
| Public packages         | `hxp0618/cloud-agents@49e8cdc6a3a4f88c7324d055ce519e9f25a8ca8a` | immutable `cloud-agent-m1-rc.1`, candidate `sha256:b9931233d46aeaf1392197095483c2e3409f628a47b2ba92c8e57bb38b444676` | GitHub source and prerelease assets are public; npm is not published |
| Synara clean consumer   | `hxp0618/synara@95cd068a9f3b1ec3a80b50e4551eae1957aa26ea`       | `feat/cloud-agent@2f15f7437ef193057d73ac00c588a5019ab286fe`                                                          | pushed for review; not merged to `main`                              |
| T3 clean consumer       | `hxp0618/t3code@8101cd044911c7dc2a2adf7c7a9ba7962abf57b6`       | `feat/cloud-agent@9584a266e91fa94354e8c07f79af3a5e01755d16`                                                          | pushed for review; not merged to `main`                              |
| Synara native/full path | `hxp0618/synara@b86d30b1aa6f383cf3a8453e6944abeaefe2db65`       | `codex/cloud-agent-external-runtime@10fd9754b65ef720a78e233c0861d681d7895acb`                                        | pushed for review; not merged back to `codex/saas-tenancy-user`      |

These refs identify source and local/packaged evidence only. They are not deployment, real-provider E2E, Public Beta, or GA records.

## Gate closure record

| Gate            | Status  | Evidence and remaining boundary                                                                                                                                                                                                                                                                                                                                                                                                               |
| --------------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `G-BASELINE`    | open    | Public source, immutable Release tag, Node `>=24.13.1 <25`, and Bun `1.3.14` are fixed. The architecture Gate also requires pre/post-refactor real Codex/Claude happy, authentication, unavailable/rate-limit, and resume-failure characterization; those records do not yet exist.                                                                                                                                                           |
| `G-ARCH`        | closed  | The seven editable public package trees and Synara release producer helpers are deleted. Synara retains only its Effect contracts, compatibility bin, Control Plane/agentd/adapters, and Worker packaging.                                                                                                                                                                                                                                    |
| `G-SCHEMA`      | closed  | Public Protocol 2.2/2.3 stays imported from the release artifact; Synara Effect projections pass the complete 210-test contracts suite without copying the public schema or changing its `$id`.                                                                                                                                                                                                                                               |
| `G-PKG`         | closed  | The mandatory offline build path strictly validates every immutable tuple and recomputes the canonical candidate digest before Worker manifest generation; agentd/Registry cross-bind the embedded lock. The explicit online verifier was manually run for anonymous SHA-256 checks of all seven tarballs and standalone Runtime. It is not an implicit network dependency of ordinary Docker builds. Sigstore CLI verification remains open. |
| `G-CONFORMANCE` | open    | Deterministic packed tests passed Protocol 2.2/2.3 negotiation, bounded multiplexing/backpressure, correlation, generation metadata, multi-instance isolation, illegal-frame/crash observation, and no-tool policy under Node `24.13.1`. Authenticated provider lifecycle, provider-originated late terminals, real secret/path containment, and sustained provider backpressure remain unexecuted.                                           |
| `G-T3-DRAIN`    | open    | T3 source/focused evidence at `9584a266e91fa94354e8c07f79af3a5e01755d16` proves per-command terminal publication, async listener acknowledgement, `awaitMessageDrain(commandId)`, projection deadlines, and fail-closed teardown. The Gate definition also requires real server/runtime restart and durable-history recovery evidence, which is still missing.                                                                                |
| `G-E2E`         | open    | Authenticated Codex/Claude lifecycle, late terminal behavior, real workspace edit/checkpoint/revert, cross-host reconnect, app.asar execution, dual-instance bounded soak, and real secret/path containment still require execution. The full Synara Worker image is additionally blocked before candidate-manifest generation by the host Alpine package-lock drift described below.                                                         |
| `G-RELEASE-M1`  | blocked | M1 cannot close until `G-E2E`, the complete Worker image, cross-host/soak evidence, and independent Sigstore CLI verification close. GitHub RC consumer verification must not be reported as M1 completion, npm publication, public beta, or GA.                                                                                                                                                                                              |

## Consequences

- Host builds fail closed while the immutable RC is absent or its digest differs.
- No `workspace:`, `file:`, Git dependency, or unpublished npm semver may substitute for the release assets.
- Protocol `$id` and public transport/schema implementations remain owned by `cloud-agents`; Synara may project them into Effect schemas but must not copy them.
- GitHub RC validation is not npm publication, deployment, public beta, or GA.
- Authenticated real Codex/Claude turns remain an external-credential acceptance gate and cannot be inferred from static packaging or Describe tests.
- The Dockerfile and Provider Host stage validate, but the final Worker image rebuild remains blocked before candidate-manifest generation by drift in the pre-existing Alpine package lock (`openjdk21 21.0.11` is no longer served by the pinned base repositories). Refreshing that host-owned lock is a separate Worker supply-chain gate, not a public Runtime fix.
