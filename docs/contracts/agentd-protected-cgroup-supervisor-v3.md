# Agentd Protected cgroup Supervisor v3

This contract defines the Linux-only protected cgroup-v2 supervisor implemented by
`services/control-plane/internal/agentd/protected_cgroup_supervisor*.go`. Version 3 retains the v2 fencing, root-lease,
fd-relative recovery, credential-drop, and `cgroup.kill` guarantees and adds an enforced resource-confinement boundary
for every Provider process tree.

This is a host-local process and resource boundary. It does not replace a container or VM filesystem, syscall, device,
or network boundary, and it is not evidence of a managed-cloud rollout.

## Required topology

The systemd service ControlGroup is the protected delegated parent. It must remain process-free:

```text
<service-control-group>/
  synara-agentd/                         # systemd MainPID leaf
  synara-e<execution>-g<generation>-.../ # one ordinary runtime
    agentd/
    provider/
  synara-d<execution>-g<generation>-.../ # one diagnostic runtime
    agentd/
    provider/
```

For managed SSH installations, the unit must use both `Delegate=yes` and `DelegateSubgroup=synara-agentd`. The latter
requires systemd 254 or newer. A non-systemd launcher may use protected mode only if it establishes the identical
process-free parent and `synara-agentd` leaf before agentd starts.

The daemon root lease fails closed unless:

- the parent is an absolute delegated cgroup-v2 subtree, not the cgroup mount root;
- the parent and `synara-agentd` leaf are owned by the supervisor UID/GID and are not group- or other-writable;
- parent `cgroup.procs` is empty;
- `synara-agentd/cgroup.procs` contains exactly the current agentd PID during lease acquisition and both recovery
  barriers;
- an exclusive non-blocking `flock` is held on the exact `O_NOFOLLOW` parent directory fd for the daemon lifetime;
- the configured Provider UID differs from the supervisor UID.

The held parent and supervisor-leaf fds pin their device/inode identities. A replacement path, symlink, unexpected
process, overlapping daemon, incomplete startup recovery, or unknown recovery entry fails before Provider launch.

## Required finite limits

Protected mode requires all four settings together:

```text
SYNARA_AGENTD_CGROUP_V2_PROVIDER_PIDS_MAX
SYNARA_AGENTD_CGROUP_V2_PROVIDER_MEMORY_MAX_BYTES
SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_QUOTA_MICROS
SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_PERIOD_MICROS
```

All values are positive base-10 integers. `pids.max` is limited to `1..1048576`; `memory.max` must fit a positive signed
64-bit byte count; the CPU period is `1000..1000000` microseconds; and the quota is at least 1000 microseconds and at
most 1024 times the period. SSH Target configuration exposes the corresponding
`cgroupV2ProviderPidsMax`, `cgroupV2ProviderMemoryMaxBytes`, `cgroupV2ProviderCpuQuotaMicros`, and
`cgroupV2ProviderCpuPeriodMicros` fields. Missing, partial, zero, negative, overflowing, or out-of-range values reject
configuration before the daemon can register.

`RecoverOrphans()` verifies that `cpu`, `memory`, and `pids` are delegated by the kernel, writes
`+cpu +memory +pids` to the held parent `cgroup.subtree_control`, and reads the enabled set back. Because the service
parent is process-free, this complies with cgroup-v2's no-internal-process rule.

For each runtime, `NewProtectedCgroupSupervisor(...)` then:

1. verifies the parent still has all three controllers enabled;
2. creates the fenced bundle fd-relative and enables the same controllers in its `cgroup.subtree_control`;
3. creates the supervisor-owned `agentd/` and `provider/` leaves;
4. writes the exact configured decimal values to `provider/pids.max`, `provider/memory.max`, and `provider/cpu.max`;
5. reads all three kernel interfaces back and requires exact canonical equality;
6. only then returns the Provider cgroup fd used by `UseCgroupFD` before the Provider's first instruction.

Any missing controller or interface, kernel rejection, short write, invalid readback, replaced fd/path, or partial setup
fails the launch and removes the caller's own empty bundle. No unbounded fallback is permitted. Limits apply to the
aggregate Provider subtree, including forked, threaded, and `setsid()` descendants.

## Fencing, recovery, and cleanup

The authoritative fence remains the exact Execution ID, positive Execution generation, and Worker incarnation. The
bundle name also carries per-daemon supervisor and per-launch runtime UUIDs. A process-local reservation prevents two
same-fence constructions in one daemon.

Startup recovery validates the complete recognized bundle set and controller-derived interface allowlist before the
first mutation, revalidates every held inode and the complete set after the race barrier, then uses `cgroup.kill`, waits
for `cgroup.events` to report `populated 0`, and removes `provider/`, `agentd/`, and the bundle. An unknown child,
symlink, device/socket, malformed controller list, interface outside the frozen allowlist, changed set, or populated
legacy v1 `synara-g...` entry fails with zero kill mutation. The fixed root `synara-agentd/` leaf is authority state,
not a recoverable runtime.

Cleanup requires the same fence and kills the entire Provider boundary on normal exit as well as cancellation. A
partial cleanup permanently poisons that same daemon/supervisor/Execution/generation/Worker reservation; a future
daemon holding a fresh root lease may recover the orphan.

## Attestation and compatibility fence

The mandatory live preflight now reports:

- containment mode `cgroup-v2`;
- supervisor version `agentd-protected-cgroup-supervisor-v3`;
- probe version `2` and the v2 probe digest;
- exact supervisor and Provider identities;
- successful credential drop, `UseCgroupFD`, finite-limit setup/readback, and `setsid()` descendant cleanup;
- the exact configured Provider resource limits.

When signing is configured, the existing signed-v1 envelope binds the Target/Worker/build/image identity plus
supervisor version, probe version/digest, and OS identities. The v3 code and probe versions semantically guarantee the
finite-limit behavior above. Server-authoritative ingestion, durable manifest consumers, resource suspension, and SSH
readiness accept only the exact v3 supervisor with probe version 2. Signed v1 or v2 supervisor statements and v3
statements carrying probe version 1 remain untrusted.

An upgrade must install `DelegateSubgroup=synara-agentd`, configure all four limits, restart agentd, acquire a fresh
lease, recover old `synara-e...` bundles, rerun preflight, and publish a new signed manifest. There is no silent v2
compatibility claim.

## Evidence boundary

The immutable [OrbStack v3 live acceptance report](../reports/stage-4-protected-cgroup-v3-live-acceptance-20260727-final3.md)
binds the current Linux test source and binary to a disposable Ubuntu arm64 VM. It proves real systemd 255
`DelegateSubgroup`, a process-free parent, controller enablement, exact `pids.max`/`memory.max`/`cpu.max` readback,
credential drop, `UseCgroupFD`, same-fence exclusion, legacy/unknown-entry zero-mutation rejection, real
`cgroup.kill`, crash-released flock, and orphan recovery. It does not prove a real Codex/Claude workload, a managed
cloud host, network/filesystem sandboxing, signed Control Plane projection, deployment, or release.
