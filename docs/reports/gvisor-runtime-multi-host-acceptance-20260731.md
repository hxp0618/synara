# gVisor K3s multi-host Provider acceptance — 2026-07-31

Status: **PASS on the final Codex × Claude matrix.**

This report supersedes the cluster-level non-claims in
`gvisor-runtime-external-host-acceptance-20260730.md`. The earlier standalone
OCI evidence remains valid for that run, but the acceptance recorded here also
installs a real K3s CRI handler, deploys `synara-gvisor` and the Node attestor,
and executes the checked-in Stage 5 Provider matrix.

## Safety and host boundary

- `103.217.189.80` (`stage-c-host-a`) remained the K3s control plane and SSH
  jump host.
- `192.168.31.234`, reached through WireGuard as `10.77.0.2`
  (`stage-c-host-b`), was the dedicated gVisor Worker.
- `188.239.23.134` provided an independent no-KVM K3s RuntimeClass and attestor
  check. Its existing Docker container `remote-synara-1` remained healthy when
  that check completed.
- `160.191.28.123` is a production host and was not used.
- Provider credentials were injected only from operator environment-variable
  names into the acceptance processes. The reports did not persist the names'
  values.

The `.234` Worker was scheduled for power-off after evidence collection and
owned-resource cleanup. The final shutdown observation is recorded below.

## Pinned runtime and attestation

Both gVisor installations used `release-20260727.0` and handler `runsc`, following
the upstream [installation layout](https://gvisor.dev/docs/user_guide/install/)
and [containerd configuration](https://gvisor.dev/docs/user_guide/containerd/configuration/).
K3s consumes the runtime through its containerd template mechanism documented
in the [K3s advanced configuration](https://docs.k3s.io/advanced).

- `runsc` SHA-256:
  `6ec46808a22c94b7ea68dd9521e831b44c69e0d3267a2cc862f9a6ae290cee91`.
- `containerd-shim-runsc-v1` SHA-256:
  `1aa4615f9a07be7898816aea5545e243a847e6c6b72b7ee2a62bbda4fa8a2a48`.
- Runtime configuration SHA-256:
  `225513d48b9023dd433050629778b50bb2263a8256970eb2a3f2c4c6090a1b78`.
- `stage-c-host-b` was Ready with `synara.io/gvisor-ready=true`; its attestor
  Pod was Ready on that Node and published the exact runtime binary/config
  hashes plus `runsc version release-20260727.0 spec: 1.2.1`.
- `synara-gvisor` resolved to handler `runsc` and scheduled its canaries only to
  attested Nodes.

## Independent no-KVM evidence

Host `188.239.23.134` was Debian 12 with K3s `v1.36.2+k3s1`, containerd
`2.3.2-k3s2`, and no `/dev/kvm`. The pinned gVisor runtime, real
`synara-gvisor` RuntimeClass, and `synara-gvisor-node-attestor` were deployed.
A real BusyBox Pod scheduled through that RuntimeClass completed successfully
and reported `Linux version 4.19.0-gvisor`.

This proves that the ordinary hardened tier can operate on a cloud VM without
nested KVM. It does not turn this small 1.8 GiB host into a Provider performance
reference; the full Provider matrix ran on `.234`.

## Final Worker image

- Git commit: `36a03bb84dc4de9710e3f74814aba0411d4684ef`.
- Registry reference:
  `10.77.0.2:5001/synara/worker@sha256:e5c75108d20d718e6843ad1657e7385df3dc0b7490dc3dc2f6225e76bbd289ac`.
- Local image ID on the build host:
  `sha256:ca34e861cbf02e71fbfa52cbab0c5fe3416bb89e004ce4226b9c4bbeea7c505c`.
- Embedded Worker manifest SHA-256:
  `10fbb2c9d4614aaf8e8d4a6e67ca172a6f57452bf01d71ea3310b90443680d4c`.
- Default user: `10001:10001`; compatibility probe mode: `0444`; embedded
  revision matched the full Git commit.

The image was built from a clean `git archive` of the commit. The checked-in
Alpine lock remained authoritative; only the repository endpoint was changed to
the Aliyun Alpine mirror because the direct upstream route stalled.

## Provider matrix

The final aggregate report is
`.tmp/stage5-provider-isolation/matrix-36a03bb8-r2/stage5-provider-isolation-matrix.md`.
It completed with `Status: PASS` against `stage-c-host-b`:

| Provider | Status | Duration |
| --- | ---: | ---: |
| Codex | pass | 223455 ms |
| Claude Agent | pass | 255023 ms |

Every cell passed:

- Worker discovery on the exact selected Node and RuntimeClass;
- Stage 5 metadata egress isolation;
- Provider credential scope;
- malicious issue-content denial;
- the full gVisor toolchain/runtime compatibility probe;
- control-plane restart; and
- two-Turn Provider continuity plus output secret scan.

Compatibility distributions were complete for both samples. Total probe time
was P50 `19096 ms`, P95/P99 `19805 ms`; maximum RSS was P50 `135728 KiB` and
P95/P99 `299172 KiB`. These are acceptance observations, not production SLOs.

One earlier aggregate run was retained as a failed diagnostic: Claude passed,
while Codex made one non-deterministic tool invocation that did not preserve the
requested foreground wait and exited before the probe completed. An immediate
Codex-only retained rerun passed, and the final full matrix then passed both
cells together. The failed run is not counted as release evidence.

## Implementation correction found by real acceptance

Claude can move a long Bash command to a background task. Its abbreviated
`task_notification` arrived before PostToolUse, while PostToolUse carried the
complete stdout shortly afterward. Provider Host now keeps the background
notification pending and gives the hook priority, falling back to the bounded
summary or controlled artifact only if the hook never arrives.

Claude's hook also normalizes away a trailing stdout newline. The compatibility
probe contract therefore emits one bounded base64url record with no CR/LF, and
the parser rejects embedded line breaks instead of requiring the runtime to
invent a byte that the Provider did not return.

## Validation and cleanup

Local final gates passed:

- `bun fmt`;
- `bun lint` (existing warnings only);
- `bun typecheck`;
- 32 focused Claude Agent SDK runtime tests; and
- the focused gVisor compatibility parser/schema test.

Owned acceptance namespaces, diagnostic image build directories, temporary
transfer material, the local kubeconfig copy, and the callback tunnel were
removed after the final report was captured. The `.134` runtime installation,
RuntimeClass, and attestor remain installed as the requested no-KVM capability.
That host became unreachable at SSH banner exchange during final cleanup, both
directly and through `.80`; its temporary smoke namespace and build directories
therefore require a later exact-scope audit when the host is reachable. The
earlier post-install check had confirmed `remote-synara-1` healthy.

Final `.234` shutdown result: **complete**. The power-off command returned after
`sync`; subsequent SSH through `.80` to `10.77.0.2` timed out, and the K3s
control plane changed `stage-c-host-b` from `Ready=True` to
`Unknown / NodeStatusUnknown`. `.80` remained online. The acceptance callback
`socat` listener and reverse SSH tunnel were then terminated.

## Evidence boundary

This run proves real K3s `runsc` scheduling, current Node attestation, and the
checked-in Codex × Claude Stage 5 isolation matrix on the named lab hosts. It
does not prove managed-cloud IAM/Region integration, production admission
signing, Firecracker/Cocoon hardware gates, production performance SLOs, or a
production rollout. No production host was used.
