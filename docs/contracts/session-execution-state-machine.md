# Session and Execution State Machine

This contract freezes the Go Control Plane v1 state transitions and their concurrency rules.

## Agent Session

```text
active --suspend--> suspended --resume--> active
active --archive--> archived
```

- Only `active` Sessions accept new Turns.
- A suspended Session remains readable and keeps its Event history, but cannot create an Execution.
- A suspended Session must be resumed before it can be archived.
- Archived Sessions are immutable from the interactive API and remain replayable until Retention deletes
  them.
- Suspend, Resume, and Archive accept `Idempotency-Key` and append exactly one durable Session Event.

## Execution

```text
queued -> leased -> running -> completed
   |         |         |  \
   |         |         |   -> waiting-for-approval -> running
   |         |         -> failed
   |         -> recovering -> leased
   -> cancelled
```

The stable persisted terminal name is `completed`; it is the v1 equivalent of the product-level
"succeeded" state. An interrupted Worker attempt releases or expires its Lease and moves the Execution
to `recovering`; interruption is not a second terminal state.

| Transition | Actor | Coordination | Durable side effects |
| --- | --- | --- | --- |
| `queued/recovering -> leased` | Worker | Row Claim and Generation increment | Lease, `execution.leased` Event |
| `leased -> running` | Worker | Current Worker/Lease/Generation | Turn running, `execution.started` Event |
| `running -> waiting-for-approval` | Worker Runtime Event | Current Lease/Generation | Pending interaction and requested Event |
| `waiting-for-approval -> running` | Authorized user | Current unexpired Lease/Generation | Resolution and resolved Event |
| `leased/running -> completed` | Worker | Lease then Execution row lock | Lease deletion, Turn completion, Event |
| `leased/running -> failed` | Worker | Lease then Execution row lock | Lease deletion, Turn failure, Event |
| active state -> `cancelled` | Authorized user | Lease then Execution row lock | Lease deletion, Turn cancellation, Event, Outbox, Audit |
| `leased/running/waiting-for-approval -> recovering` | Worker or expiry sweeper | Lease then Execution row lock | Recovery Outbox and Event |

Terminal transitions use the same Lease-before-Execution lock order. Cancel/Complete races therefore
produce exactly one legal terminal winner instead of relying on process-local synchronization.

## Approval and user input

Worker Runtime Events with type `approval.requested` or `user-input.requested` must contain a
`requestId`. The Control Plane persists the request in `execution_interactions` in the same transaction
as the Session Event and moves the Execution to `waiting-for-approval`.

Authorized users resolve requests through:

```text
GET  /v1/executions/{executionID}/interactions
POST /v1/executions/{executionID}/approvals/{requestID}/resolve
POST /v1/executions/{executionID}/user-input/{requestID}/resolve
```

Resolution is rejected after Lease expiry or Generation fencing. The response is idempotent, audited,
and appended to Session Event replay. Delivering the resolved command into a live Provider Runner is a
Stage 3 Provider Runtime responsibility; Stage 2 makes the database and API state authoritative.

## Browser/API idempotency

Project Create, Session Create, Turn Create, Session Suspend/Resume/Archive, Execution Cancel, and
interaction resolution accept `Idempotency-Key`.

- Same Tenant, actor, key, operation, and normalized request return the original status and response.
- A key reused for different content returns `409 idempotency_conflict`.
- The idempotency row and all business/Event/Outbox/Audit writes commit in one short transaction.
- Concurrent requests on different Control Plane replicas serialize on the database key; exactly one
  executes business side effects.
- Replayed responses include `Idempotency-Replayed: true`.
