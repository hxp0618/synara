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

## Describe

`Describe` returns the Host build, Adapter/CLI version, complete Capability Descriptor, command/message limits,
Runtime Event version range, credential delivery modes and Resume strategies. Static capability claims must be
verified by the shared Provider Acceptance Suite.

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

The existing unversioned one-shot runner is retained only as an explicit compatibility path during managed
Worker upgrades. It cannot advertise v2-only capabilities. Managed Stage 3 Worker images must negotiate v2
before Claiming Provider work.
