# Session and Execution State Machine

This contract freezes the Go Control Plane v1 state transitions and their concurrency rules.

## Agent Session

```text
active --suspend--> suspended --resume--> active
active --archive--> archived
```

- Only `active` Sessions accept new Turns.
- `active` Sessions whose frozen `absoluteExpiresAt` has elapsed remain readable, but any new execution-bearing
  operation (`turn.create`, Review, Compact), Claim/Claim replay, Lease Renew/Start, interaction resolution or
  recovery advancement is rejected or terminally cancelled with `session_absolute_expired`. Runtime Events and
  late Worker Complete/Fail cannot win after the deadline: the current Generation is cancelled instead. Resource
  directive polling atomically fences the Lease and returns `terminate`, while Kubernetes reconciliation omits
  these Executions so an expired Session cannot create or retain a Pod.
- A suspended Session remains readable and keeps its Event history, but cannot create an Execution.
- A suspended Session must be resumed before it can be archived.
- Archived Sessions are immutable from the interactive API and remain replayable until Retention deletes
  them.
- Suspend, Resume, and Archive accept `Idempotency-Key` and append exactly one durable Session Event.

## Agent Turn modes

Turn creation captures immutable `runtimeMode` (`approval-required | full-access`) and `interactionMode`
(`default | plan`) values in PostgreSQL. They are part of the idempotency request identity, the
`turn.created` Session Event, and the Worker Workload snapshot. Browser refresh, SSE reconnect, Server restart,
Worker replacement, and Provider native resume must reuse the persisted Turn values rather than current
composer state.

Codex maps approval-required to its native approval/sandbox controls and Plan Mode to its native collaboration
mode. Claude Agent SDK uses a host-owned PreToolUse policy gate plus native Plan permission mode so local Claude
allow rules cannot bypass the persisted Turn mode. A Provider that cannot implement a persisted mode returns a
stable unsupported error; it must not silently run the Turn under a different mode.

An Interrupt request is durable intent, not terminal confirmation. Control Plane creates a Generation-fenced
Control Command and appends `turn.interrupt-requested`; the Session remains running until Worker acknowledgement.
The acknowledgement transaction stores the Provider Resume Cursor, releases the Lease, marks the current Turn
and Execution `interrupted`, and appends `execution.interrupted`. The Agent Session remains active and accepts a
later Turn.

## Execution

```text
queued -> leased -> running -> completed
   |         |         |  \
   |         |         |   -> waiting-for-approval -> running
   |         |         |                         \
   |         |         |                          -> suspended -> recovering -> leased
   |         |         |                          -> failed
   |         |         |   -> interrupted
   |         |         -> failed
   |         -> recovering -> leased
   -> cancelled
```

The stable persisted terminal name is `completed`; it is the v1 equivalent of the product-level
"succeeded" state. Worker loss or an expired Lease moves the Execution to `recovering`. Resource suspend is
distinct from recovery: after a ready suspend Checkpoint, Control Plane deletes the current Lease, marks the
Execution `suspended`, and does not create a replacement Worker until an explicit user resolution returns the
Execution to `recovering`. Before checkpointing, agentd cancels Provider/control delivery and waits for its Provider
runner to stop. It then records a generation-fenced, one-time `providerQuiescedAt` ordering marker. In
`worker-attested-v1`, a signed strict-containment boundary authorizes completion. In
`kubernetes-pod-terminal-v1`, the marker never authorizes suspension by itself: agentd writes an immutable
checkpoint-ready handoff and exits the generation-bound Pod; only the Reconciler observing the same Pod UID and
Generation as `Succeeded`, with the sole agentd container terminated at exit code 0, may atomically write the
terminal proof, delete the Lease, and enter `suspended`. `DELETE` acceptance, API `NotFound`, `Failed`, and `Unknown`
are not completion proofs. Kubernetes registration for this mode is Pod-bound through target-scoped ServiceAccount
TokenReview plus a live Pod ownership lookup, and the attempt freezes Worker incarnation, namespace, Pod name and UID.
Quiesce/checkpoint recording and Suspend completion are idempotent; ambiguous or missing proof leaves the old
Generation fenced for Lease recovery rather than classifying it as suspended.
A user-requested, Provider-acknowledged Turn interrupt is also distinct: it releases
the Lease and moves the current Turn and Execution to the terminal `interrupted` state while leaving the Session
active for a later Turn.

| Transition                                           | Actor                                         | Coordination                                                 | Durable side effects                                                                                      |
| ---------------------------------------------------- | --------------------------------------------- | ------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------- |
| `queued/recovering -> leased`                        | Worker                                        | Row Claim and Generation increment                           | Lease, authoritative Resume Snapshot and one Generation-scoped `execution.leased.providerResume` decision |
| `leased -> running`                                  | Worker                                        | Current Worker/Lease/Generation                              | Turn running, `execution.started` Event                                                                   |
| `running -> waiting-for-approval`                    | Worker Runtime Event                          | Current Lease/Generation                                     | Pending interaction and requested Event                                                                   |
| `waiting-for-approval -> running`                    | Authorized user                               | Current unexpired Lease/Generation                           | Resolution and resolved Event                                                                             |
| `waiting-for-approval -> suspended`                  | Worker-attested completion or Kubernetes Reconciler terminal proof | Current Lease/Worker incarnation/Generation, active suspend-attempt row lock, immutable quiesce/checkpoint proof; Kubernetes also requires exact Pod UID `Succeeded`/agentd exit 0 | Ready or unchanged Checkpoint, suspend attempt completion, Lease deletion, `execution.suspended`, resource-state suspend |
| `running -> suspended`                               | Worker-attested completion or Kubernetes Reconciler terminal proof | Provider Host 2.2 native `SuspendTurn`, immutable active/history/activity/cursor receipt, current Lease/incarnation/Generation; Kubernetes also requires exact Pod UID `Succeeded`/agentd exit 0 | One-time active checkpoint receipt, ready/unchanged Workspace, Lease deletion, `execution.suspended`; no authoritative-history fallback |
| `suspended -> recovering`                            | Authorized user resolving the final request   | Fenced Execution/Generation and resolved Interaction row     | Resolution recorded as `resume-recorded`, Turn re-queued, Workspace recovery marker, Recovery Outbox/Event; next Claim binds it to one immutable Bundle |
| `suspended -> recovering`                            | Explicit active-turn Resume or semantic activity crossing a committed suspend boundary | Completed unconsumed active receipt and exact native cursor | Recovery Outbox/Event; next Claim binds the receipt to exactly one `suspend-resume` Bundle/Generation |
| `suspended -> failed`                                | Suspended interaction expiry sweeper          | Fenced Execution/Generation and pending Interaction row      | Turn failure, `execution.failed`, Outbox                                                                  |
| `leased/running -> completed`                        | Worker                                        | Lease then Execution row lock                                | Lease deletion, Turn completion, Event                                                                    |
| `leased/running -> failed`                           | Worker                                        | Lease then Execution row lock                                | Lease deletion, Turn failure, Event                                                                       |
| `leased/running/waiting-for-approval -> interrupted` | Worker acknowledgement of durable user intent | Current Lease/Worker/Generation and Control Command row lock | Provider Cursor persistence, interaction expiry, Lease deletion, Turn interruption, Event, Outbox         |
| active state -> `cancelled`                          | Authorized user                               | Lease then Execution row lock                                | Lease deletion, Turn cancellation, Event, Outbox, Audit                                                   |
| `leased/running/waiting-for-approval -> recovering`  | Worker or expiry sweeper                      | Lease then Execution row lock                                | Recovery Outbox and Event                                                                                 |

Terminal transitions use the same Lease-before-Execution lock order. Cancel/Complete races therefore
produce exactly one legal terminal winner instead of relying on process-local synchronization.

## Semantic Provider Credential access

An immutable `execution_provider_credential_grants` row remains the root authority for one Execution
Generation. It freezes the Credential ID and version but never contains plaintext. Migration `000055` adds a
second, short-lived access authorization to the current Worker Lease and a
`meaningful_activity_sequence` watermark to the Session:

- only the explicit, Lease-fenced Grant resolve may attach access serial `1`; ordinary Lease renewal cannot
  initialize access before the Provider secret has been requested;
- later Worker Lease renewals may rotate the short access window, but they only read the server-authoritative
  `meaningful_activity_sequence / meaningful_activity_at`. Browser/SSE/WebSocket presence, read traffic,
  Worker heartbeat and the renewal request itself never advance that watermark;
- `waiting` and `checkpointing` use the frozen `waitingKeepAliveSeconds` refresh window;
  `provisioning`, `active` and `restoring` use `suspendAfterIdleSeconds`; `idle`, `suspended` and terminating
  states cannot refresh;
- the access window is analogous to an access token while the semantic activity window is analogous to refresh
  authority. A previously issued access window may remain usable for its short TTL after refresh authority
  closes, but neither it nor a later semantic event can cross the Session absolute expiry, Credential expiry,
  frozen Credential version, or revocation boundary;
- every durable access change advances a monotonic serial. The database binds the row to the exact
  Tenant/Execution/Generation/Grant, freezes its issuance time and hard cap, and rejects sequence, activity-time,
  renewal-time or expiry rollback;
- agentd validates every renewal projection against the initial Grant, serial, semantic sequence and hard cap.
  Missing metadata, Credential revocation/rotation/scope loss, access expiry, or a closed renewal stream cancels
  the Provider and fails the Generation without exposing Credential material.

The current Provider Host contract still supplies a static `apiKey` or `authToken` to one Provider process. This
access Broker authorizes how long that frozen secret may remain in use; it does not silently replace it with a new
Credential version mid-Generation and is not a generic upstream OAuth refresh-token adapter. KMS remains the
envelope-decryption boundary, using its own workload identity/SDK credential lifecycle; KMS keys and long-lived
plaintext are not extended by Session activity.

## Provider Resume decision

Each successful new-Generation Claim commits one Resume decision in the same transaction as the Lease and the
existing `execution.leased` Event. The payload records only bounded metadata: requested/selected strategy, stable
reason code, Cursor state, configured maximum age, authenticated issued/expiry timestamps, source
Execution/Generation/History Sequence and the exact authoritative Resume Snapshot Sequence. Cursor plaintext,
ciphertext, Credential material and Binding Digest are forbidden.

The default maximum age is 720 hours through `SYNARA_PROVIDER_CURSOR_MAX_AGE`, capped at 8760 hours. A Cursor is
expired when `now >= issuedAt + maximumAge`; an authenticated `IssuedAt` more than five minutes in the future is
invalid. Wrong-key, missing-Cipher, unsupported/legacy Envelope, non-native Runtime, expiry and future-clock cases
preserve ciphertext as `quarantined`. Explicit Binding/Credential mismatch may clear it to `absent`. Extending TTL,
restoring a key or returning to a compatible Runtime never revives quarantined ciphertext; only a fresh Cursor from
the current Execution can restore `usable`.

Reason codes are a bounded contract: `cursor_usable`, `cursor_absent`, `cursor_quarantined`, `cursor_expired`,
`cursor_issued_in_future`, `cursor_binding_unavailable`, `cursor_binding_mismatch`,
`cursor_cipher_unavailable`, `cursor_authentication_failed`, `cursor_legacy_unbound`,
`cursor_envelope_unsupported`, `cursor_payload_invalid`, `cursor_lineage_mismatch`, `cursor_open_failed`,
`cursor_unusable`, and `resume_strategy_authoritative_history`.

Claim receipt replay uses the committed Workload and `execution.leased.providerResume` decision and does not
reapply Cursor age inside the same Generation. It may return a native Cursor only when current ciphertext,
Binding, authenticated `IssuedAt` and complete source Lineage still match the committed decision. Otherwise it
returns `409 claim_replay_resume_cursor_unavailable`; replay must never silently switch a committed native
selection to authoritative history after Turn activity may have begun.

## Approval and user input

Legacy Runtime Event v1 uses `approval.requested` or `user-input.requested`. Canonical Runtime Event v2 uses
`request.opened` for Approval and `user-input.requested` for Structured Input. All forms carry the stable
`requestId` correlation extension. The Control Plane persists the request and its exact Event version in
`execution_interactions` in the same transaction as the Session Event and moves the Execution to
`waiting-for-approval`.

`requestId` is stable for retries within one Execution Generation, while the Provider Host namespaces a native
Provider request with the current Generation. Recovery expires the obsolete Interaction before a replacement
Worker claims the next Generation; if the Provider replays the same native request, the replacement Interaction
must therefore receive a different external `requestId`. PostgreSQL enforces execution-wide request uniqueness,
and Personal SQLite creates the matching unique index during metadata migration.

Authorized users resolve requests through:

```text
GET  /v1/sessions/{sessionID}/interactions
GET  /v1/executions/{executionID}/interactions
POST /v1/executions/{executionID}/approvals/{requestID}/resolve
POST /v1/executions/{executionID}/user-input/{requestID}/resolve
```

The Session endpoint returns only pending, unexpired requests and a `snapshotSequence` watermark for
refresh/reconnect reconciliation. It requires `ExecutionApprove`; users who may read the Session but
cannot approve receive sequence-preserving redacted Interaction Events instead of request or resolution
details.

Resolution is rejected after Interaction expiry, Lease expiry, or Generation fencing. The response is
idempotent, audited, and appended to Session Event replay. Approval accepts the durable SaaS decisions
`accept` and `decline`; `acceptForSession` remains a local Provider-runtime behavior and is not silently
mapped to `accept`. SaaS Cancel creates durable Interrupt intent. Local mode keeps the existing Native API
response and empty-answer cancel behavior.

Structured User Input answers must contain exactly the persisted question keys. Values are non-empty,
bounded strings or string arrays. A string is the free-form/single-answer representation even when the
question offers suggested options; arrays represent multi-select labels and every label must match a
persisted option without duplicates.

A legacy request resolves with its v1 event; a canonical Approval resolves as `request.resolved` and
canonical Structured Input as `user-input.resolved`. The persisted command is delivered through
Worker-scoped endpoints:

```text
POST /v1/workers/executions/{executionID}/interaction-resolutions/pull
POST /v1/workers/executions/{executionID}/interaction-resolutions/{interactionID}/delivered
POST /v1/workers/executions/{executionID}/interaction-resolutions/{interactionID}/acknowledged
```

Pull validates the live Lease and returns only commands targeted to the authenticated Worker/Generation.
`delivered` is recorded after the command is written to the Provider Host; `acknowledged` is recorded after a
correlated terminal Host message. Both transitions are idempotent. Lease recovery expires unresolved requests
and supersedes unacknowledged resolution delivery for the obsolete Generation before a replacement Worker can
claim the Execution. The quiesced-suspend fallback is stricter: a user resolution recorded after Provider quiesce
but before suspend commit is atomically converted to `resume-recorded` before the Lease is released, so recovery
does not discard an answer that the fenced Provider never received. A resolution already marked `delivered` has
crossed the Provider write boundary: losing that Generation before a correlated ACK produces terminal
`outcome-unknown` rather than an unsafe replay.

If the Execution is already `suspended`, the same browser resolve path still records the durable decision, but
`delivery_status` becomes `resume-recorded` and no Worker/Generation is attached. The fenced Provider process
never receives that resolution directly. When the final pending request resolves, the Execution moves to
`recovering`, the Turn returns to `queued`, and the next Recovery Bundle freezes the recorded resolution for
Generation `N+1`. In the same Claim transaction, that row becomes `resume-bound` with the exact Bundle ID and
Generation. It cannot be rebound or included in another Bundle; if the bound Generation is later lost before
Provider application can be proven, it becomes terminal `outcome-unknown` and the Execution fails closed.

Pending Interactions have a bounded 24-hour wait. For a live leased `waiting-for-approval` Generation, Lease Renew
performs a targeted expiry check for its Execution, while Claim/recovery and the background retention pass use the
existing pending-expiry partial index to sweep unattended rows. Expiry acquires locks in the same
`Lease -> Execution -> Interaction` order as Resolve, deletes the obsolete Lease, marks all pending requests for
that Generation `expired/superseded`, returns the Turn to `queued`, moves the Execution to `recovering`, and
appends exactly one `execution.recovering` Event/Outbox message with reason `interaction_expired`. The previous
Worker is fenced before a replacement can claim the next Generation.

Suspended Interactions keep the same deadline but no longer have a live Lease. Their expiry path locks the fenced
Execution and pending Interaction rows; once the final suspended request expires, the Turn and Execution fail
instead of spawning a replacement Provider automatically.

Concurrent Resolve semantics are database-defined across Control Plane replicas:

- Different decisions produce one resolved terminal winner; the loser receives
  `409 interaction_resolution_conflict`.
- The same decision with different idempotency keys converges on the same Interaction and resolution command
  without duplicating the resolved Event, Audit row, or Worker delivery.
- A request after the Interaction deadline receives `409 interaction_expired`, including after the expiry sweep
  has already removed the old Lease.

## Browser/API idempotency

Project Create, Session Create, Turn Create, Session Suspend/Resume/Archive, Execution Cancel, and
interaction resolution accept `Idempotency-Key`.

- Same Tenant, actor, key, operation, and normalized request return the original status and response.
- A key reused for different content returns `409 idempotency_conflict`.
- The idempotency row and all business/Event/Outbox/Audit writes commit in one short transaction.
- Concurrent requests on different Control Plane replicas serialize on the database key; exactly one
  executes business side effects.
- Replayed responses include `Idempotency-Replayed: true`.
