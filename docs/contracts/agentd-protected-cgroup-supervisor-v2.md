# Agentd Protected cgroup Supervisor v2

> Superseded by [Agentd Protected cgroup Supervisor v3](agentd-protected-cgroup-supervisor-v3.md). Version 2 proves
> fenced process ownership and reliable termination but does not prove finite CPU, memory, or PID confinement.

This document describes the Linux-only protected cgroup supervisor and attestation contract implemented under
`services/control-plane/internal/agentd/protected_cgroup_supervisor*.go`.

It is a local contract for integration and acceptance work. It is not, by itself, evidence of a root deployment,
Kubernetes rollout, long-running recovery, or production escape resistance.

## Scope

The supervisor creates a fenced cgroup-v2 subtree under an already delegated and protected parent path:

```text
<protected-parent>/synara-e<execution-id>-g<generation>-w<worker-incarnation>-s<supervisor-instance>-r<runtime-instance>/
  agentd/
  provider/
```

Standalone and daemon-owned diagnostic probes use the parallel `synara-d...` prefix. Startup recovery never treats
that diagnostic namespace as a runtime orphan.

UUIDs in the directory name use their canonical 32 lowercase hexadecimal digits without hyphens. The authoritative
fence is the exact Execution ID, positive Execution generation, and Worker incarnation. The per-daemon supervisor
UUID and per-runtime UUID make bundle identity explicit; a UUID by itself is never treated as liveness evidence.

## Preconditions

- Linux only.
- `ParentPath` must be an absolute delegated cgroup-v2 subtree, not the cgroup mount root.
- The protected parent and every recognized subtree must be owned by the configured supervisor identity.
- The protected parent and every recognized subtree must not be group- or other-writable.
- Supervisor and Provider must not share a UID. A different GID alone is not a safe boundary because the Provider
  would still run as the owner of the supervisor-managed subtree.
- The identity string format used in Worker manifests and attestations is `uid:<uid> gid:<gid>`.
- A daemon must acquire a non-blocking exclusive `flock` on an `O_NOFOLLOW` directory fd for the exact protected
  parent before preflight or Worker registration. The fd pins the validated device/inode and remains open until
  daemon exit. The kernel releases the open-file-description lock on normal exit or crash.
- Protected daemon mode requires the exact service parent `cgroup.procs` to contain one PID: the current agentd PID.
  A missing self PID, an old v1 daemon PID, or any additional parent process fails closed. Provider processes belong
  in child bundles, not directly in the service parent. Standalone diagnostic mode does not acquire the daemon lease.

## Operations and fencing

- `AcquireProtectedCgroupRootLease(...)` / `RecoverOrphans()`
  - opens the protected parent fd-relative with `O_NOFOLLOW`
  - verifies the cgroup-v2 filesystem and delegated-subtree boundary
  - acquires an exclusive non-blocking root lease; a second or rolling-overlap daemon fails closed and performs no
    recovery while the first daemon is alive
  - validates the exact service-parent process fence after lease acquisition and again during recovery
  - only after successful lease acquisition, enumerates recognized v2 bundles for startup recovery
  - permits one successful recovery pass; ordinary runtime authorization is rejected until that pass completes, and
    recovery cannot be rerun after runtime authorization or after cleanup has entered a partially mutating failure
  - opens every bundle and child fd-relative, and validates the complete recovery set before the first `cgroup.kill`
  - freezes a strictly parsed lowercase controller allowlist from the held parent and each held bundle's
    `cgroup.controllers`; allows only
    `agentd/` and `provider/` child directories plus regular kernel `cgroup.*` and `<advertised-controller>.*`
    interfaces. Symlinks, sockets/devices, malformed controller lists, unadvertised prefixes, replacements, unknown
    entries, or unexpected ownership/mode abort recovery without mutation
  - after the recovery barrier, revalidates every held inode, re-enumerates the complete recognized parent set, and
    revalidates entries against the same frozen controller allowlist and `cgroup.procs` immediately before the first kill
  - kills and removes only recognized v2 leftovers
  - treats every legacy v1 `synara-g<generation>-i<worker-incarnation>` entry, populated or empty, as an unfenced
    migration boundary and fails before any kill. Operators must stop v1 and manually confirm/remove every legacy
    entry before v2 startup; v1 never held the v2 parent lease, so neither an empty bundle nor UUID/name proves it dead.

- `NewProtectedCgroupSupervisor(...)`
  - never scans or cleans another bundle
  - requires non-zero Execution, Worker, supervisor, and runtime UUIDs plus a positive Execution generation
  - for ordinary Provider launches, requires the live daemon root lease to match the exact parent inode, identities,
    and supervisor UUID
  - permits a no-lease path only when explicitly marked as a diagnostic; that path creates and cleans only its own
    randomly fenced `synara-d...` probe bundle
  - reserves the parent/supervisor/Execution/generation/Worker fence in a process-local registry until cleanup, so
    concurrent same-fence starts cannot both succeed
  - creates the fenced bundle plus `agentd/` and `provider/` child cgroups and validates every directory

- `AttachAgentdPID(...)` / `AttachProviderPID(...)`
  - require an exact Execution ID, generation, and Worker-incarnation fence match
  - attach the requested PID by writing to the exact child `cgroup.procs`

- `Cleanup(...)`
  - requires the same exact authoritative fence
  - writes `cgroup.kill`
  - waits for `cgroup.events` to report `populated 0`
  - removes `provider/`, `agentd/`, then the fenced bundle
  - releases the in-process same-fence reservation only after every kill, empty wait, fd close, and directory removal
    succeeds; any failure permanently poisons that daemon/supervisor/Execution/generation/Worker fence
  - does not poison a different Execution fence. A later daemon process may acquire the root lease, recover the old
    v2 bundle, and establish a new supervisor authority.

Agentd creates one supervisor UUID for each daemon process and one runtime UUID for each Provider launch. Startup
ordering is strict: stop v1 and clear legacy entries, acquire root lease, recover old v2 bundles, run the mandatory diagnostic live preflight, then
register the Worker. The Runner retains the same lease and supervisor UUID for all ordinary launches. The diagnostic
preflight uses an independent fence and is non-destructive toward every other bundle.
When run inside the daemon it reuses the daemon's exclusive lease authority. A standalone preflight may coexist with
a live daemon or startup recovery because its namespace is never selected for runtime-orphan cleanup; it still has
no authority to enumerate, kill, or remove any bundle except the one it created.

## Agentd wiring and attestation

Protected mode is Linux-only and guarded by:

- `SYNARA_AGENTD_CGROUP_V2_ROOT=<delegated-protected-parent>`
- optional paired `SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID` / `SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID`
- optional paired `SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID` /
  `SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE`

When the Provider UID/GID pair is configured, agentd fails closed unless the delegated root is present and owned by
the current supervisor identity. It rejects a Provider UID matching the supervisor UID, drops Provider credentials
before the first instruction, and binds the child to the protected Provider cgroup through `CgroupFD`.

When attestation is configured, agentd additionally requires a valid Worker build git SHA, immutable image digest,
absolute private-key path, and a root-owned regular key file with no group/other permissions. Before Worker
registration it performs a live preflight probe equivalent to:

```sh
synara-agentd protected-cgroup-preflight
```

The probe proves that Provider credentials were dropped, `UseCgroupFD` is active, a `setsid()` descendant is killed
by `cgroup.kill`, and the observed Provider identity exactly matches configuration. Only a successful signed probe
may advertise trusted `workerRuntime.processContainment`.

The signed statement binds the Target and Worker identity, Worker build and image identity, OS/architecture,
containment mode, `agentd-protected-cgroup-supervisor-v2`, probe version/SHA, and both OS identities. Changing from v1
to v2 deliberately invalidates previously signed manifests; upgraded Workers must restart, rerun preflight, and
publish a fresh manifest before strict-containment policy can pass again. Server-authoritative strict containment
verification accepts `agentd-protected-cgroup-supervisor-v2` for `cgroup-v2`; a correctly signed v1 statement is still
rejected. Targets without strict signed containment retain their existing local/non-attested behavior.
The exact-v2 predicate is also applied when reading stored manifests: persisted signed v1 rows project as
`untrusted` and cannot authorize resource suspension or SSH protected-cgroup readiness.

## SSH integration

For SSH-managed Workers, protected mode additionally requires:

- `serviceUser=root`
- `Delegate=yes` in the installed systemd unit
- the exact managed service ControlGroup as the delegated subtree
- an operator-supplied absolute attestation private-key path
- explicit agentd version, build git SHA, and image digest

The provisioner freezes a new `SYNARA_AGENTD_INSTANCE_UID` into each install or upgrade EnvironmentFile. It validates
the cgroup-v2 mount and key before installation, restarts the unit, and requires its active ControlGroup to equal the
derived root before activating the Target. The installer does not create or rotate host keys and does not infer a
public-key trust policy.

A passing host gate must verify both the signed live preflight and the control-plane projection with
`processContainment.trustState = verified`. It must bind that projection to the currently active unit, including its
MainPID executable, EnvironmentFile, allowlisted process environment, cgroup membership, and registration context.
Each core host sample re-reads Active/SubState, MainPID/starttime, ControlGroup, parent `cgroup.procs`, User/Delegate,
cgroup filesystem, and root device/inode at its own tail; two such core samples form each snapshot, and bracketed
snapshots are taken both before and after standalone preflight and local manifest verification. A pass requires the same User/Delegate state,
active/running MainPID, `/proc/<pid>/stat` starttime, ControlGroup, executable, bound environment, process environment,
exact parent `cgroup.procs`, cgroup2 filesystem type, and cgroup-root device/inode at every boundary; this prevents
`Restart=always`, PID reuse, or a replaced cgroup root from splicing evidence across Worker incarnations.
Standalone preflight never sources the EnvironmentFile. It runs through absolute `/usr/bin/env -i` with only the
strict non-secret identity/build/cgroup/attestation allowlist, a fixed loopback control-plane URL, fixed dummy token,
fixed `/bin/false` Runner command, and fixed positive SSH generation. Registration credentials, configured Runner,
capabilities, inherited process environment, and Provider secrets are excluded.
The production daemon applies the same environment boundary below the gate: both the credential-dropped probe helper
and its `setsid()` sentinel child are started with an explicit non-nil empty environment. Their handshake reports and
requires zero environment entries, so registration tokens, Runner configuration, capabilities, attestation key path,
and other daemon variables cannot reach either lower-privilege process.

## Local live acceptance

The disposable OrbStack lane binds its claim to the current source set, Linux arm64 test binary, a run-random
root-owned cloud-init marker, stable opaque VM ID, and a pre-existing preservation oracle. Cleanup never targets a
name. It first calls OrbStack's documented opaque-ID delete; the private compatibility transport is permitted only
for the exact OrbStack 2.2.1 build 2020100 nil-pointer signature currently covered by the gate. That path validates an
owner-controlled Unix socket and stable socket identity, then sends one `ContainerDelete` JSON-RPC request with the
captured opaque ID as its only parameter. It does not retry an ambiguous write. Final exact-ID inventory absence is
required, while a remaining ID or different same-name object fails closed.

[The 2026-07-26 final3 report](../reports/stage-4-protected-cgroup-v2-live-acceptance-20260726-final3.md) records all five
real systemd/cgroup-v2 scenarios passing together with acknowledged exact-ID cleanup and an unchanged preservation
oracle. This compatibility code belongs only to the disposable acceptance harness; it is not supervisor authority,
production lifecycle behavior, or evidence that an arbitrary OrbStack version implements the same private method.

## Remaining non-goals

- privilege escalation
- Docker protected mode or non-Linux containment
- proof of recovery across a real host crash without an active Provider workload acceptance lane
- proof on every target kernel/filesystem that directory-fd `flock` and delegated cgroup cleanup behave as expected;
  the production host gate must include live overlap, crash, orphan recovery, and active workload assertions
- an automated in-place v1-to-v2 rolling upgrade. Operators must stop v1 and manually confirm/remove all legacy
  entries before starting v2; any legacy entry is an intentional fail-closed migration gate.
- production or managed-cloud verification
