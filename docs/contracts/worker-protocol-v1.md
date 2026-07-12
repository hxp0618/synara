# Worker Protocol v1 draft

The protocol is transport-neutral. Initial implementations may use HTTP/WebSocket JSON, while
message names and fencing behavior remain stable if the transport later moves to gRPC.

## Commands

`RegisterWorker`, `Heartbeat`, `ClaimExecution`, `StartSession`, `ResumeSession`, `SendTurn`,
`InterruptTurn`, `ResolveApproval`, `ResolveUserInput`, `RuntimeEvent`, `UploadArtifact`,
`CompleteExecution`, `FailExecution`, and `ReleaseLease`.

## Common envelope

Every execution-scoped message carries:

```text
requestId, tenantId, executionId, workerId, generation, occurredAt
```

Registration binds a worker to one persisted `executionTargetId` and `targetKind`. Claim requests use
the same fields and cannot select another target. `shared_pool` and `dedicated_pool` are not Worker
Protocol v1 domain values.

A successful Claim includes a read-only `workload` snapshot with Tenant/Organization/Project/Session/
Turn IDs, Session title, Provider and Model, Turn input, repository URL, and default branch. The
snapshot is loaded in the claim transaction and is preserved across idempotent Claim replay. Workers
never query control-plane tables directly.

Runtime events additionally conform to `runtime-event-v1.schema.json`.

## Lease and fencing rules

1. An Execution has at most one current lease row.
2. Every successful reassignment increments `generation`.
3. Lease tokens are random secrets; the database stores only a hash.
4. Heartbeats, events, artifacts, and terminal state changes must match both the current Worker ID
   and generation.
5. An expired lease may be reclaimed, but an old generation can never become valid again.
6. Duplicate event IDs are idempotent. Sequence collisions with different event IDs are errors.
7. Worker loss moves the Execution into recovery; it does not immediately destroy the Agent
   Session.
8. SSH, Docker, and Kubernetes workers must advertise `leaseSupported=true` and
   `fencingSupported=true`; registration and claim both enforce the contract.

## Boundary

Workers receive domain commands and return versioned events. They do not receive SQL, table names,
or access to the control-plane database. Provider credentials are delivered as short-lived or
revocable secrets and must never appear in event payloads or logs.

The generic `synara-agentd` implementation keeps Worker and Lease credentials outside provider
runners, renews the Lease while a runner is active, validates runner Artifact paths against the
Execution workspace, and implements the runner boundary in `agentd-runner-v1.md`.
