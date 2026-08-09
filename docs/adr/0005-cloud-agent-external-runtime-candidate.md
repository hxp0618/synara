# ADR-0005: Cloud Agent public runtime is an immutable external candidate

- Status: Accepted; immutable RC consumer verified
- Date: 2026-08-09

## Context

Synara previously carried editable copies of seven `@synara/cloud-agent-*` packages and the scripts that packed them. T3 Code needed the same runtime bits, but two editable host copies cannot provide one source of truth or same-bits evidence. A Synara root `bun.lock` also identifies the entire host graph; it is not the public Runtime candidate identity.

## Decision

`hxp0618/cloud-agents` is the only editable source for the public Protocol, Provider ABI, Runtime, Codex and Claude adapters, testkit, and Distribution. Synara deletes those source directories and its producer-side release helpers. Public fixes are made in `cloud-agents`, cut as one immutable GitHub Release candidate, and then consumed here through exact release-asset URLs.

Synara keeps only its host-owned surfaces: Effect Schema and re-exports in `packages/contracts`, the `apps/provider-host` compatibility bin, agentd/Control Plane lifecycle authority, Artifact/Workspace/Credential adapters, and Worker/Docker packaging.

The root `cloud-agent-candidate.lock.json` is the authority for the public source commit, candidate digest, standalone Runtime digest, and all seven package URL/version/SHA-256 tuples. Production manifests and root overrides must match it. `scripts/verify-cloud-agent-candidate.ts` checks installed and remote same bits. Worker publication records embed this candidate identity instead of presenting the Synara root lock as the public Runtime identity.

The accepted candidate is the immutable `cloud-agent-m1-rc.1` GitHub Release from source `49e8cdc6a3a4f88c7324d055ce519e9f25a8ca8a`, with candidate digest `sha256:b9931233d46aeaf1392197095483c2e3409f628a47b2ba92c8e57bb38b444676`. Anonymous downloads, the regenerated Bun lock, the installed Distribution entrypoint, and the standalone Runtime were verified against `cloud-agent-candidate.lock.json`. The earlier mutable/local candidates remain suspended historical evidence and are not accepted by this lock.

## Gate closure record

| Gate            | Status  | Evidence and remaining boundary                                                                                                                                                                                                                                                                                                                |
| --------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `G-BASELINE`    | closed  | Public source `49e8cdc6a3a4f88c7324d055ce519e9f25a8ca8a`, immutable Release tag, Node `>=24.13.1 <25`, and Bun `1.3.14` are fixed. This is an RC baseline, not GA.                                                                                                                                                                             |
| `G-ARCH`        | closed  | The seven editable public package trees and Synara release producer helpers are deleted. Synara retains only its Effect contracts, compatibility bin, Control Plane/agentd/adapters, and Worker packaging.                                                                                                                                     |
| `G-SCHEMA`      | closed  | Public Protocol 2.2/2.3 stays imported from the release artifact; Synara Effect projections pass the complete 210-test contracts suite without copying the public schema or changing its `$id`.                                                                                                                                                |
| `G-PKG`         | closed  | Anonymous SHA-256 checks pass for all seven tarballs and the standalone Runtime; the regenerated `bun.lock`, installed versions, exact GitHub URLs, candidate digest, and installed Distribution/standalone bytes agree. Sigstore CLI verification remains a release-level open item below.                                                    |
| `G-CONFORMANCE` | closed  | The final packed Provider Host passed Protocol 2.2/2.3 negotiation, 64-command bounded multiplexing/backpressure, correlation, generation metadata, multi-instance isolation, illegal-frame/crash observation, and no-tool policy under Node `24.13.1`. The testkit separately returns the real-provider cases as open gates.                  |
| `G-T3-DRAIN`    | closed  | T3 source/focused-test evidence at `454555c3` retains per-command terminal publication, async listener acknowledgement, `awaitMessageDrain(commandId)`, projection deadlines, and fail-closed session teardown. This is consumer integration evidence, not cross-host or GA evidence.                                                          |
| `G-E2E`         | open    | Authenticated Codex/Claude lifecycle, late terminal behavior, sustained provider backpressure, secret/path containment, cross-host same-bits, and soak still require real-provider execution. The full Synara Worker image is additionally blocked before candidate-manifest generation by the host Alpine package-lock drift described below. |
| `G-RELEASE-M1`  | blocked | M1 cannot close until `G-E2E`, the complete Worker image, cross-host/soak evidence, and independent Sigstore CLI verification close. GitHub RC consumer verification must not be reported as M1 completion, npm publication, public beta, or GA.                                                                                               |

## Consequences

- Host builds fail closed while the immutable RC is absent or its digest differs.
- No `workspace:`, `file:`, Git dependency, or unpublished npm semver may substitute for the release assets.
- Protocol `$id` and public transport/schema implementations remain owned by `cloud-agents`; Synara may project them into Effect schemas but must not copy them.
- GitHub RC validation is not npm publication, deployment, public beta, or GA.
- Authenticated real Codex/Claude turns remain an external-credential acceptance gate and cannot be inferred from static packaging or Describe tests.
- The Dockerfile and Provider Host stage validate, but the final Worker image rebuild remains blocked before candidate-manifest generation by drift in the pre-existing Alpine package lock (`openjdk21 21.0.11` is no longer served by the pinned base repositories). Refreshing that host-owned lock is a separate Worker supply-chain gate, not a public Runtime fix.
