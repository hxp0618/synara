# Stage 4 protected cgroup v3 live acceptance — 2026-07-27 final3

Status: **PASS**

The authoritative machine-readable evidence is
[`stage-4-protected-cgroup-v3-live-acceptance-20260727-final3.json`](stage-4-protected-cgroup-v3-live-acceptance-20260727-final3.json).
This report is immutable after generation.

## Bound source and host

- Git HEAD: `79a9067a2b92d91e70c76023c55fa8761d128064`; the worktree was intentionally dirty and the gate hashed the tested
  source set instead of claiming clean-commit evidence.
- Source-set SHA-256: `346c1fe27828107847b734734baf4a23546eaa75e23d9e44e8560d5481dab1bb`.
- The source manifest explicitly includes both `internal/cgroupv2limits/limits.go` and its tests in addition to the
  complete `internal/agentd` Go source set, module files, and live gate.
- Linux arm64 test binary: 52,721,684 bytes,
  SHA-256 `7e2b0de18332010511453b41857b0d71687fbff670db193e687eb7a94250291c`.
- Disposable host: Ubuntu 24.04 arm64, systemd `255 (255.4-1ubuntu8.16)`, unified `cgroup2fs`, OrbStack kernel
  `7.0.11-orbstack-00360-gc9bc4d96ac70`.

## Real cgroup-v2 proof

The test ran under a real systemd service with `Delegate=yes`, `DelegateSubgroup=synara-agentd`, and
`KillMode=process`. After daemon recovery:

- the delegated service parent contained zero processes;
- `synara-agentd/cgroup.procs` contained only the exact MainPID;
- parent `cgroup.subtree_control` reported `cpu`, `memory`, and `pids` enabled;
- the Provider cgroup kernel readback was exactly:
  - `pids.max = 128`;
  - `memory.max = 536870912` bytes;
  - `cpu.max = 200000 100000`;
- the lower-privilege Provider and its `setsid()` descendant were in the protected Provider subtree and both were
  removed by real `cgroup.kill`/`cgroup.events` cleanup;
- standalone preflight beside the active runtime proved credential drop, `UseCgroupFD`, the finite-limit setup, and
  descendant cleanup without killing the active runtime;
- overlapping daemon lease and duplicate same-fence runtime construction failed closed;
- an extra PID in the supervisor subgroup rejected recovery with zero mutation while the populated runtime fixture
  stayed alive;
- a legacy v1 entry rejected the complete recovery set before any kill;
- an unknown child rejected recovery with zero mutation; removing only that empty fixture allowed a fresh lease to
  recover the orphan;
- SIGKILL released the root flock, `KillMode=process` preserved the non-`Pdeathsig` Provider descendant, and a fresh
  holder recovered it through real cgroup interfaces.

All five top-level scenarios passed in 0.29 seconds.

## Cleanup and preservation

The gate proved ownership through a run-random root-only cloud-init marker and stable opaque VM identity. OrbStack
2.2.1's known exact-ID CLI panic matched the pinned version, commit, and panic site, so the bounded owner-controlled
Unix-socket compatibility path issued one `ContainerDelete` call with the captured opaque ID as its sole parameter.
The RPC acknowledged deletion, final inventory proved the captured ID absent, and the pre-existing `debian` VM identity
was unchanged. No name-based deletion or ambiguous write retry was used.

## Evidence history and boundary

`final2` is retained as an immutable failed attempt. It sampled `cgroup.subtree_control` before `RecoverOrphans()` had
performed the new controller enablement. The test was corrected to capture the topology after that authority step;
`final3` is the passing source-bound evidence.

This proves the protected cgroup v3 host-local topology, finite resource limits, fencing, termination, and crash
recovery on the observed kernel/systemd combination. It does **not** prove a real Codex or Claude workload, filesystem,
syscall, device, or network sandboxing, a managed-cloud host, signed Control Plane projection, deployment, or release.
