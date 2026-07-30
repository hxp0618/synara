# gVisor external host standalone acceptance — 2026-07-30

Status: **external Linux host standalone runtime passed; K3s RuntimeClass and
real-Provider release gates were deliberately not changed.**

This report records an operator-authorized validation on the existing
`stage-c-host-a` host. It extends the no-KVM local Kind result with an independent
x86_64 Linux host that exposes both systrap and KVM. It does not claim that the
host's active K3s cluster is configured for gVisor.

## Safety boundary

The host was already the only K3s control-plane node for an active Stage C
cluster. It was running CoreDNS, local-path-provisioner, metrics-server, the
sandbox-operator, a second physical Worker node, and two Cocoon virtual-kubelet
nodes. Installing `runsc` into the system path and changing the K3s containerd
template would require a K3s restart and would interrupt the API server.

The acceptance therefore used only an isolated directory under `/var/tmp`, a
standalone OCI bundle, and private `runsc --root` state directories. It did not:

- restart or reload K3s, containerd, kubelet, or another service;
- write K3s/containerd configuration;
- create Kubernetes resources, RuntimeClasses, labels, or annotations;
- install `runsc` into a system path; or
- mount a K3s socket, configuration file, or workload directory into gVisor.

## Host and inputs

- OS: Debian GNU/Linux 12 (bookworm), x86_64.
- Kernel: `6.1.0-15-amd64`.
- Capacity observed at start: 40 logical CPUs and 62 GiB memory.
- Virtualization: `systemd-detect-virt=none`, VT-x present, `/dev/kvm` present,
  and `kvm_intel` plus `kvm` loaded.
- K3s: `v1.36.1+k3s1`; containerd: `v2.2.3-k3s1`.
- gVisor: fixed point release `release-20260727.0`, OCI spec `1.2.1`, obtained
  using the upstream [installation layout](https://gvisor.dev/docs/user_guide/install/).
- Archive SHA-512:
  `94a7280655629330f02ff06fbec0493b7f2f4041dd145576daeff2340577cde0fb45fe28e5f1d209f7d7c08f70b9c3e333aa1073063b1d132742120763eaf0ad`.
- `runsc` SHA-256:
  `6ec46808a22c94b7ea68dd9521e831b44c69e0d3267a2cc862f9a6ae290cee91`.
- `containerd-shim-runsc-v1` SHA-256:
  `1aa4615f9a07be7898816aea5545e243a847e6c6b72b7ee2a62bbda4fa8a2a48`.
- Current-branch Linux amd64 `synara-agentd` SHA-256:
  `d28e924333ade6346e019accee616b4b61c0fc0445bece793780864cf5c55821`.
- Strict OCI configuration SHA-256:
  `654644a52c6164a30d6e6800c45ce6c3edd37a754c587f0fb0a6bbe589aaac65`.

The archive and its SHA-512 sidecar came from the same fixed upstream release
path. The archive was verified locally, uploaded, and verified again on the
host before extraction. The new multi-file layout kept `runsc`,
`containerd-shim-runsc-v1`, and adjacent `gvisor-bin/` assets together.

## Canary contract

The OCI rootfs contained the current branch's statically linked
`synara-agentd` and the minimum BusyBox shell needed by its child-process probe.
The strict bundle used:

- read-only rootfs;
- uid/gid `10001:10001`;
- `noNewPrivileges=true`;
- zero bounding, effective, inheritable, permitted, or ambient capabilities;
- a 16 MiB `/tmp` tmpfs with `nosuid`, `nodev`, and `noexec`; and
- `--network=none`, which retains loopback without host or external networking.

`synara-agentd --verify-gvisor-runtime` requires all of the following to pass:

1. `/proc/version` contains the gVisor kernel identity;
2. a private temporary file can be written, fsynced, closed, and read back;
3. a child `/bin/sh` process exits successfully; and
4. a loopback TCP listener/client round trip preserves the exact payload.

Running the same binary directly on the native host exited `1` with the stable
message `gVisor runtime verification failed`. This proves the externally used
binary did not accept the native host kernel as gVisor.

## Results

Both platforms passed the strict canary once with zero stdout/stderr and no
remaining runtime-state entries:

| Platform | Status | Elapsed |
| --- | ---: | ---: |
| systrap | 0 | 236 ms |
| KVM | 0 | 416 ms |

Thirty sequential fresh-sandbox samples per platform all passed:

| Platform | Passed | Min | P50 | P95 | P99 | Max | Mean |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| systrap | 30/30 | 192 ms | 259 ms | 285 ms | 291 ms | 291 ms | 260.2 ms |
| KVM | 30/30 | 351 ms | 407 ms | 451 ms | 470 ms | 470 ms | 405.5 ms |

The raw timing-list SHA-256 digests before cleanup were:

- systrap:
  `1215d5cab37aa2895f30f53cd6489d6a0ab2607da3025afdc405bef14f27defe`;
- KVM:
  `a645324edd6e4a6b3107e7eb1960e448ca634de66c8d14df46ecb6bb642f1723`.

Eight simultaneous fresh sandboxes also passed on each platform:

| Platform | Passed | Total wall time | Output bytes | Remaining state entries |
| --- | ---: | ---: | ---: | ---: |
| systrap | 8/8 | 309 ms | 0 | 0 |
| KVM | 8/8 | 461 ms | 0 | 0 |

These measurements cover only the bounded runtime canary on this host. They are
not application benchmarks and are not operator-approved Provider performance
budgets. The result happens to show lower canary startup time for systrap on this
host; it does not establish that systrap is faster for general workloads.

## Cluster non-impact and cleanup

Before and after the acceptance:

- K3s retained PID `990` and the same active-since timestamp;
- `/var/lib/rancher/k3s/agent/etc/containerd/config.toml` retained SHA-256
  `238d41ca904f8520df35810ef9c940df690d916cceced50680bb68a5001c4b19`;
- `/etc/rancher/k3s/config.yaml` retained SHA-256
  `1cbb4816b889c1d78c77e9c7c1f986476d5a70c7230c30889832458901f5321a`;
- all four Nodes were Ready and every active Pod was Running;
- `synara-gvisor` did not exist as a RuntimeClass; and
- neither `runsc` nor `/usr/local/bin/runsc` existed in the system installation.

The exact remote probe directory (493 MiB) and local build/download directory
(194 MiB) were permanently removed after hashes and aggregate results were
recorded. No standalone `runsc` process or state remained.

## Deliberate non-claims and next gate

This run proves that the current Synara gVisor canary succeeds on an independent
x86_64 Linux host using both upstream systrap and KVM platforms. It does not
prove:

- actual K3s RuntimeClass, CRI handler, Node attestor, or live-Pod registration;
- Codex/Claude compatibility or the full language/package/tool matrix;
- production CNI, DNS, metadata, credential, egress, or malicious-input boundaries;
- multi-node gVisor consistency, Node loss, attestation expiry, or drift handling; or
- production P50/P95/P99 budgets and soak/SLA acceptance.

The next Kubernetes gate requires either a dedicated expendable node or an
approved maintenance window. That gate must install the pinned release into the
K3s-visible system path, configure exact runtime type `io.containerd.runsc.v1`,
restart K3s safely, run the real `synara-gvisor` RuntimeClass Pod and Node
attestor, and then execute the checked-in Stage 5 Provider matrix. Until then,
the platform-shared release gate remains closed.
