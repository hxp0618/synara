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

Registration and Heartbeat carry `protocolVersion`. Version `1` is currently both the minimum and maximum
supported version. Unsupported versions return HTTP `426` with stable code
`worker_protocol_version_unsupported` and `minimumSupported` / `maximumSupported` details. `version`
remains the Worker build/image version and is not used as the protocol number.

A successful Claim includes a read-only `workload` snapshot with Tenant/Organization/Project/Session/
Turn IDs, Session title, Provider and Model, Turn input, repository URL, and default branch. The
snapshot is loaded in the claim transaction and is preserved across idempotent Claim replay. Workers
never query control-plane tables directly.

The Workload also carries the immutable `runtimeMode` (`approval-required | full-access`) and
`interactionMode` (`default | plan`) captured when the Control Plane created the Turn. A Worker must not infer
these values from browser state or reuse a later Session setting. Provider Host maps them to native Provider
permission and collaboration-mode controls for that Turn.

Runtime events additionally conform to `runtime-event-v1.schema.json`. `eventVersion` is required and
must be `1`; an unsupported version is rejected with `runtime_event_version_unsupported` and the
supported range. The serialized `payload` object is limited to 65,536 bytes. Larger output, binary data,
or bulky structured results must use the Artifact lifecycle and place only an `artifactId` reference in
the Runtime Event.

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
9. A Worker may set `draining=true` on Heartbeat. Draining Workers retain and renew current Leases but
   cannot Claim new Executions. `draining=false` explicitly returns the Worker to `online`.
10. Re-registering the same target/cluster/namespace/pod identity rotates the Worker token atomically.
    The previous token becomes invalid immediately; retrying registration with the installation
    registration credential returns a fresh usable token.

## Approval and user input boundary

`approval.requested` and `user-input.requested` Runtime Events include a stable `requestId`. The Control
Plane persists the pending request, current Worker, and Generation. User resolution is rejected when the
Lease has expired or the Generation is fenced and is replayed as `approval.resolved` or
`user-input.resolved` Session Events.

The current Worker/Generation pulls resolved commands from the execution-scoped resolution endpoint. A
delivery carries the persisted `interactionId`, stable `commandId`, `ResolveApproval` or `ResolveUserInput`
command type, and the validated resolution payload. The Worker marks the command `delivered` only after it is
written to the Provider Host channel and marks it `acknowledged` only after the Host returns a correlated
terminal message. Both transitions are idempotent. `delivered` commands remain pullable until acknowledged so
an agentd restart can replay the stable command ID without duplicating the Provider action. A stale Worker or
Generation is permanently fenced from pull, delivery, and acknowledgement.

## Boundary

Workers receive domain commands and return versioned events. They do not receive SQL, table names,
or access to the control-plane database. Provider credentials are delivered as short-lived or
revocable secrets and must never appear in event payloads or logs.

The generic `synara-agentd` implementation keeps Worker and Lease credentials outside provider
runners, renews the Lease while a runner is active, validates runner Artifact paths against the
Execution workspace, and negotiates Provider Host Protocol v2 independently from Worker Protocol v1.
`agentd-runner-v1.md` documents only the explicit legacy compatibility boundary.
