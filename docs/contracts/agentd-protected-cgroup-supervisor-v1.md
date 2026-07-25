# Agentd Protected cgroup Supervisor v1

This document describes the Linux-only protected cgroup supervisor and attestation contract implemented under
`services/control-plane/internal/agentd/protected_cgroup_supervisor*.go`.

It is a local contract for future integration work. It is not evidence of root deployment, Kubernetes rollout, or
production escape-resistance on its own.

## Scope

The contract creates a fenced cgroup-v2 subtree under an already delegated and protected parent path:

```text
<protected-parent>/synara-g<generation>-i<worker-incarnation>/
  agentd/
  provider/
```

The fence binds the subtree to exactly one Execution generation and one Worker incarnation. A stale supervisor must
not attach or clean up a replacement runtime.

## Preconditions

- Linux only.
- `ParentPath` must be an absolute delegated cgroup-v2 subtree, not the cgroup mount root.
- The protected parent and created subtree must be owned by the configured supervisor identity.
- The protected parent and created subtree must not be group- or other-writable.
- Supervisor and Provider must not share a UID. A different GID alone is not a safe boundary because the Provider
  would still run as the owner of the supervisor-managed subtree.
- The identity string format used in Worker manifests and attestations is `uid:<uid> gid:<gid>`.

## Operations

- `NewProtectedCgroupSupervisor(...)`
  - opens the protected parent fd-relative with `O_NOFOLLOW`
  - verifies cgroup-v2 filesystem and delegated subtree semantics
  - creates the fenced bundle plus `agentd/` and `provider/` child cgroups
  - validates owner and mode on every created directory

- `AttachAgentdPID(...)` / `AttachProviderPID(...)`
  - require an exact generation/incarnation fence match
  - attach the requested PID by writing to the exact child `cgroup.procs`

- `Cleanup(...)`
  - requires an exact generation/incarnation fence match
  - writes `cgroup.kill`
  - waits for `cgroup.events` to report `populated 0`
  - removes `provider/`, `agentd/`, then the fenced bundle

## Agentd Wiring

Current agentd wiring is Linux-only and guarded by:

- `SYNARA_AGENTD_CGROUP_V2_ROOT=<delegated-protected-parent>`
- optional paired `SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID` / `SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID`
- optional paired `SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID` / `SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE`

When the Provider UID/GID pair is configured, agentd must fail closed unless the delegated root is present and
owned by the current supervisor identity, must reject any Provider UID that matches the current supervisor UID
before creating the fenced subtree or starting the child, must drop the Provider child credentials before its first
instruction, and must bind that child to the protected Provider cgroup via `CgroupFD`. This wiring is still
local-only and does not by itself claim signed strict containment, root deployment, or production verification.

When attestation is configured, agentd additionally requires:

- a valid Worker build git SHA
- a valid immutable Worker image digest
- an absolute attestation private-key path
- a root-owned regular private-key file with no group/other permissions

Agentd performs a live preflight probe before Worker registration:

```sh
synara-agentd protected-cgroup-preflight
```

The probe must prove all of the following against the current host/runtime wiring:

- Provider credentials drop to the configured UID/GID before the first instruction.
- `UseCgroupFD` is active for the Provider child.
- a `setsid()` descendant is still killed by `cgroup.kill`.
- the observed Provider UID/GID exactly match configuration.

The preflight JSON includes `mode`, `supervisorVersion`, `probeVersion`, `probeSha256`,
`supervisorIdentity`, `providerIdentity`, `useCgroupFD`, `setsidDescendantKilled`, and the observed Provider UID/GID.
When attestation is configured successfully, it additionally includes:

- `attestationKeyId`
- `attestationPublicKeySha256`
- `attestationPublicKeyBase64`
- `attestation`

Only when both the live probe and attestation succeed may agentd advertise
`workerRuntime.processContainment` in the Worker manifest. Unsigned or probe-less local cgroup wiring must not be
materialized as trusted strict containment.

The signed statement binds:

- Execution Target ID and Target kind
- Worker instance UID and physical Worker location identity
- Worker build version, build git SHA, and image digest
- operating system and architecture
- containment mode, supervisor version, probe version/SHA, and supervisor/provider identities

## SSH Integration

For SSH-managed Workers, protected cgroup mode additionally requires:

- `serviceUser=root`
- `Delegate=yes` in the installed systemd unit
- the exact managed systemd service ControlGroup as the cgroup-v2 delegated subtree; the provisioner derives
  `/sys/fs/cgroup/system.slice/<managed-service>.service` and rejects a conflicting configured path
- an operator-supplied absolute attestation private-key path
- explicit build identity (`agentdVersion`, build git SHA, image digest)

The provisioner freezes a new `SYNARA_AGENTD_INSTANCE_UID` into each install/upgrade EnvironmentFile. This keeps the
running service registration, a separately invoked live preflight, and the signed manifest statement on one physical
Worker identity. It validates the cgroup-v2 mount and key before installation, then requires the restarted service to
be active and its reported `ControlGroup` to match the derived root before marking the Target active.

The installer verifies only that the delegated subtree and key file exist. It does not create or rotate the host
key material, does not infer a public-key trust policy, and does not auto-enable strict containment on behalf of an
Execution Target. Operator policy remains explicit via the Target `processContainmentPolicy`, and release gating
must verify that policy against the live preflight output before claiming trusted containment. A passing SSH host
gate must also require the control-plane projected Worker-manifest view to report
`processContainment.trustState = verified`; a local signature check alone is not sufficient release evidence.
The SSH host gate additionally binds that projection to a currently active/running service: its MainPID executable,
EnvironmentFile, allowlisted live process environment, cgroup membership, and configured cgroup root must all match
the same managed unit and registration context.

## Non-goals in v1

- automatic daemon or Provider wiring
- privilege escalation
- claims of production verification
- claims that the current process identity model is already sufficient for strict containment
- Docker protected mode or any non-Linux containment design
