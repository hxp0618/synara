# Worker Protocol v2 delta

Worker Protocol v2 is the current managed-Worker contract. It is a breaking, transport-neutral delta over
the [legacy Worker Protocol v1 baseline](./worker-protocol-v1.md), not a restatement of that document. Commands,
Lease and Generation fencing, Runtime Event rules, interaction delivery, and Provider control semantics remain
unchanged unless this delta says otherwise.

## Version negotiation

Registration and Heartbeat use `protocolVersion: 2`. A current Control Plane advertises `2` as both
`minimumSupported` and `maximumSupported`; a v1 Worker is rejected with HTTP `426` and
`worker_protocol_version_unsupported`. The separate Worker build/image `version`, Runtime Event version, and
Provider Host Protocol version are not changed by this major version.

## Required Worker instance identity

Registration requires `instanceUid`, a canonical lowercase non-zero UUID identifying the running Worker process
or Pod incarnation. Kubernetes Workers must use the real Pod UID; a synthesized or historical UID is not proof
that the Pod still owns its storage.

The Control Plane binds the Worker row, monotonically increasing `incarnation`, and `instanceUid` together.
Re-registration rotates the Worker credential and advances `incarnation`. Heartbeat, Claim, Lease-scoped writes,
command pulls, and Artifact authorization are accepted only for that current tuple. A request authenticated before
replacement is rejected with `worker_incarnation_fenced` and cannot mutate the replacement Worker or its Leases.

## Required Workspace layout v3

Every newly allocated managed Workspace materialization uses layout v3. Its Claim workload carries all of:

```text
remoteWorkspaceId, workspaceMaterializationId,
workspaceMaterializationIncarnationId, workspaceLayoutVersion=3
```

Agentd must reject a missing or unsupported layout contract. The materialization incarnation ID is an immutable
physical fence and participates in the managed root and Manifest identity, so rematerialization creates a distinct
root instead of reusing files from a previous physical incarnation. Cleanup commands carry the same materialization,
incarnation, target, storage-scope, and layout identity and may delete only that exact managed root.

Layout v2 remains an adoption-only compatibility path for an existing materialization explicitly migrated by the
Control Plane. It must not be selected for a new materialization, inferred from omitted v3 fields, or used to
downgrade a v3 Claim.

## Compatibility boundary

Worker Protocol v2 and v1 are not registration-compatible. Supporting a legacy on-disk layout v2 does not make a
Worker Protocol v1 client compatible with a v2 Control Plane. Provider Host Protocol and Runtime Event versions
continue to negotiate independently.

Managed Worker Protocol v2 agents currently advertise Runtime Event `{ minimum: 2, maximum: 2 }` and reject a
Provider Host Event outside the negotiated range. The explicit Provider Host v1 runner advertises Runtime Event v1
only and remains a bounded compatibility path; it must not claim canonical v2 support.

## Authoritative Resume Snapshot v1

Every claimed Execution carries an additive `workload.resumeSnapshot` object with `version: 1`. The legacy
`workload.conversationHistory` field remains during the compatibility window, but it is derived from the Snapshot
messages and is not built by a second history path.

The Snapshot is generated inside the Claim transaction from Session Events strictly before the current
`turn.created` Sequence. It contains bounded, credential-free recovery context:

- legacy `runtime.output.delta` and canonical v2 `content.delta` assistant text;
- user Turn/Steer messages, safe Tool summaries, Plan/Review state, and the latest Compact boundary;
- pending Interaction allowlisted metadata, never arbitrary Provider request payloads;
- Ready Artifact references and logical Workspace/Materialization/Checkpoint references, never object payloads or
  host filesystem paths;
- source and included Sequence ranges, fixed byte/token budgets, and explicit truncation reasons.

The Control Plane monotonically advances the Provider Runtime Binding's `authoritative_history_sequence` in the
same transaction. Claim replay may rebuild the same Snapshot, but must not move that cursor backwards. A Worker or
Provider Host must ignore unknown additive Snapshot fields; a future incompatible shape requires a new Snapshot
version rather than changing v1 semantics.

## Claim Resume selection

`workload.resumeSnapshot` is always the authoritative recovery input. The optional top-level
`providerResumeCursor` is an optimization supplied only when the same Claim transaction committed
`selectedStrategy=native-cursor` in `execution.leased.providerResume`; its absence means use the Snapshot, not
that durable history is absent.

Worker request receipts deliberately omit Cursor plaintext. A same-Generation Claim replay loads the persisted
Workload and committed Resume decision, then reopens only ciphertext whose Binding, authenticated `IssuedAt` and
source Execution/Generation/History Sequence still match. It does not reapply the age policy. If the selected
native Cursor has since been replaced, quarantined or become unavailable, Control Plane returns
`409 claim_replay_resume_cursor_unavailable` and does not return a Lease, Workload or alternate strategy. A new
Generation performs a new policy evaluation.
