# Provider Host Protocol v2

Provider Host Protocol v2 is the versioned JSONL boundary between `synara-agentd` and a Provider Host. It is
independent from Worker Protocol and Runtime Event versions.

The schema source of truth is `packages/contracts/src/providerHost.ts`.

## Version

```json
{ "major": 2, "minor": 0 }
```

- Major mismatch is incompatible and makes the Host/Provider combination non-schedulable.
- A newer Minor may add optional fields. Unknown optional fields are ignored.
- Missing required fields, unknown commands and undeclared capabilities are rejected.

## Command envelope

Every stdin command is one JSON line with:

```text
requestId, protocolVersion, executionId, generation,
commandType, commandId, occurredAt, payload
```

Supported command IDs are defined by `PROVIDER_HOST_COMMAND_TYPES`. A retry with the same `commandId` must not
repeat Provider Turns, tool side effects or Fork creation.

## Message envelope

Every stdout line is one of:

```text
Event, InteractionRequest, ArtifactCandidate, Checkpoint,
Result, Error, Progress
```

Each command produces exactly one terminal `Result` or `Error`. Output after a terminal message, a duplicate
terminal, malformed JSONL or an oversized line is a `protocol_violation`.

Provider diagnostics use stderr, are bounded and redacted, and are never parsed by Control Plane. Large logs,
files, Diffs and snapshots use Artifact references.

## Bidirectional command execution

The Host keeps reading stdin while `SendTurn` is active. Agentd multiplexes stdout by `commandId`, so a live
Turn can emit Runtime Events or an `InteractionRequest` while the same Host process accepts
`ResolveApproval`, `ResolveUserInput`, or `InterruptTurn`. A terminal message is correlated to the command that
created it; it does not terminate unrelated commands.

Interaction delivery is durable across the Control Plane boundary:

1. The Host emits an `InteractionRequest` correlated to the active `SendTurn`.
2. Agentd persists the normalized requested Event through Worker Protocol.
3. The authorized user resolution receives a stable resolution `commandId` and Worker/Generation target.
4. Agentd pulls the resolution, writes the command to the active Host, then marks it `delivered`.
5. After a correlated Host terminal message, agentd marks the resolution `acknowledged`.

Delivered-but-unacknowledged commands remain pullable and use the stable command ID for replay. A stale
Worker/Generation cannot pull, deliver, or acknowledge them. Lease recovery expires unresolved requests and
supersedes unacknowledged deliveries from the obsolete Generation.

Codex and Claude currently expose process termination as an `emulated` `interrupt-turn`. Approval and
Structured User Input remain `unsupported` in the production CLI runner while it uses non-interactive bypass
modes. The protocol and Worker lifecycle are implemented and tested with an interactive fixture, but the
capability must not be promoted until a real Provider runtime supplies the corresponding resolver methods.

## Describe

`Describe` returns the Host build, Adapter/CLI version, complete Capability Descriptor, command/message limits,
Runtime Event version range, credential delivery modes and Resume strategies. Static capability claims must be
verified by the shared Provider Acceptance Suite.

## agentd negotiation and v1 boundary

Managed Local, SSH, Docker and Kubernetes Workers use v2. Agentd appends `--protocol-v2`, performs
side-effect-free `Describe` probes before Worker registration, and publishes the returned Codex and Claude
plus explicit Local-only Provider descriptors under the registered `providerHost` capability. It performs
another Describe in the actual Host process before Start/Resume and rejects incompatible Major versions,
unavailable Provider CLIs, Local-only or
missing `send-turn` capability, incompatible Runtime Event ranges, unsupported Credential delivery and invalid
Resume strategies before sending the Provider Turn.

`SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL=v1` is the explicit, bounded compatibility switch for an older one-shot
runner. The default is `v2`; there is intentionally no `auto` mode and no fallback after a v2 command. Replaying
the same Workload through v1 after a partial v2 execution could duplicate a Provider Turn, tool call or external
side effect.

## Stable errors

Errors use the codes in `PROVIDER_HOST_ERROR_CODES` and include:

```text
retryable
requiresNewExecution
requiresUserAction
canReconstructFromHistory
canMoveWorker
```

The message is safe for users and logs. It must not contain Credential values, Worker/Lease Tokens, complete
Provider stderr or Presigned URLs.

## v1 boundary

The existing unversioned one-shot runner is retained only through the explicit switch above during managed
Worker upgrades. It advertises a legacy Protocol summary and cannot advertise v2-only capabilities.
