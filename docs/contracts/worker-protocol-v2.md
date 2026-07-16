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

Physical cleanup also owns interrupted-install siblings under the same logical Workspace lock. For layout v3,
the incarnation-bearing active basename makes its `.staging-*` and `.backup-*` siblings exact cleanup targets.
For adopted layout v2, agentd must reopen and verify each sibling Manifest and delete only the requested
materialization/incarnation; stale or replacement incarnations are preserved. Symlink, non-directory, or
unverifiable residue fails closed, and acknowledgement is forbidden until the canonical generation, quarantine,
and every exact sibling are durably absent.

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

## Generation-fenced Workspace Credential Grants

Claim snapshots each active Project Workspace Credential Binding into an immutable
`execution_credential_grants` row inside the same transaction that advances the Execution Generation and creates
the Lease. The additive `workload.credentialGrants[]` descriptor contains only:

```text
grantId, bindingKind, purpose, provider, credentialType, selector
```

It never contains the underlying Credential ID, Credential version, ciphertext, wrapped key or plaintext. A
same-Generation Claim receipt replay returns the same Grant IDs. Recovery advances the Generation and creates new
Grant IDs from the then-current active Bindings and Credential versions. Disabled Bindings and Infrastructure-only
`worker_image_pull` Bindings are not exposed to agentd.

Agentd resolves plaintext only through:

```text
POST /v1/workers/executions/{executionId}/credential-grants/{grantId}/resolve
```

with the current Worker bearer token and exact `tenantId`, `generation` and `leaseToken`. The Control Plane locks
and rechecks the Lease, Grant, immutable Binding, Project owner, scope, selector, Credential version, expiry and
revocation before decrypting. Rotation fences the old Generation Grant; Binding disablement or Credential
revocation makes it unavailable. The response is `Cache-Control: no-store`, carries no Credential ID, and is valid
only for the controlled stage named by `bindingKind`. Workers must not resolve every descriptor eagerly.

Agentd selects at most one descriptor for the exact stage and rejects duplicate active descriptors as ambiguous
before any plaintext request. The Workspace preparation path requests only `git_fetch`; `git_push`, Registry and
Package descriptors remain unresolved unless a distinct controlled operation explicitly requests their matching
stage. Descriptors and plaintext are never forwarded to Provider Host or Provider Runner input.

For Git HTTPS, agentd uses the execution-scoped Unix-socket AskPass channel and clears it before Provider start. For
Git SSH, agentd requires an `ssh://user@host[:port]/path` repository that matches the Grant, rejects non-public DNS
answers, pins the selected IP, fixes the exact stored Host Key, and loads exactly one private key into a temporary
agent socket. Only the public identity and Host Key are written to a private temporary directory. The private key and
passphrase are not written to the Workspace, command line, normal environment, Git config or Provider input, and the
temporary agent is removed immediately after Clone/Fetch. Ambient `SSH_AUTH_SOCK`, SSH config, ProxyCommand and host
Credential stores are not inherited.

## Idempotent Worker Artifact upload

For non-Checkpoint `generated_file`, `terminal_log`, and `diff` candidates, agentd sends the opaque bounded key in
`X-Synara-Artifact-Idempotency-Key` on `POST /v1/workers/executions/{executionId}/artifacts`. It is intentionally a
header rather than a new JSON field, so an older strict v2 Control Plane can ignore it instead of rejecting the
request body. The key is scoped by the current Execution and Generation and is derived from stable candidate
metadata plus verified payload identity; it never contains the raw Workspace path or payload bytes.

A supporting Control Plane advertises `X-Synara-Artifact-Idempotency: v1` on successful Worker registration.
Agentd enables ambiguous-response retries only after that negotiation. Without the header it performs one legacy
upload attempt without the key, preserving mixed-version functionality without claiming response-loss safety.
Control Plane replicas must all support this feature before upgraded Workers are rolled out; rollback reverses that
order.

The same key and Create/Complete request IDs must be reused after an ambiguous response. Control Plane returns the
same deterministic Artifact ID, refreshes a pending upload grant, or returns the ready Artifact without another
upload. Reusing a key with different ownership or Artifact metadata is a conflict. Checkpoint Artifacts continue to
derive their identity from `checkpointId` and do not use this field.

Artifact resolution rejects traversal, non-regular files, and any symlink component below the candidate's bound
root. Before Provider start, agentd binds both the Workspace and private Runtime Output roots to anchored Root
descriptors; each candidate is opened beneath the declared root and the same regular-file descriptor is reused
through hashing and upload retries. Replacing a root path or checked parent cannot redirect the source. MIME
normalization happens before identity derivation, and pre-hash/Ready verification stops when the Execution context
is cancelled.

### Controlled Runtime Output Root

Agentd also creates an execution-scoped private Runtime Output Root and adds its absolute path to the internal
Runner input as optional `runtimeOutputDirectory`. This is not a Control Plane path field and requires no DDL. The
new field is additive: an older Host ignores it, while a newer Host without it must fall back to bounded inline
output and must not forward a Provider-returned file path.

Provider Host Protocol v2 may return a `terminal_log` `ArtifactCandidate` with
`sourceRoot=runtime-output`, a root-relative path, `terminalId`, `encoding`, and optional `reportedSize`. Agentd
accepts that shape only for a Terminal that already emitted `terminal.started`; Workspace candidates continue to
default to `sourceRoot=workspace` and cannot carry Runtime Output metadata. Unknown roots, absolute or traversal
paths, invalid encodings, missing Terminal correlation, and size mismatches are protocol failures.

It may also return a `diff` candidate with `sourceRoot=runtime-output`, `encoding=utf-8`, exact `reportedSize`, and
the complete `fileCount`/`additions`/`deletions` summary. Agentd accepts only the canonical
`text/x-diff; charset=utf-8` Content-Type, opens the file beneath the already bound root, passes it through Secret
Guard, and uploads it with ordinary Artifact idempotency. The completed Artifact must be Ready, kind `diff`, and
its Content-Type, size and SHA-256 must match the local guarded bytes. Agentd then appends a stable
`turn.diff.updated` Runtime Event containing only the Artifact reference and summary. Missing or mismatched Ready
metadata fails the Execution; an inline Diff and Artifact reference are never persisted together.

The Runtime Output Root is bound before the Provider starts and opened with the same no-symlink, regular-file
descriptor rules as Workspace Artifacts. Provider Host never reads the path itself. Agentd streams the bound file
through the rolling Terminal collector, forbids mixing a full-file candidate after inline bytes for the same
Terminal, applies the 32 KiB preview and 1 MiB segment limits, and removes the physical root after the Execution.
Unsafe controls or invalid UTF-8 are Artifact-only binary data. Session Events contain only the bounded preview,
typed lifecycle metadata, and ready Artifact references.

## Drain deadline Workspace preservation

After the Drain deadline cancels an active Provider, agentd stops the normal renewal loop while retaining the
Workspace lock. A separate two-second context first renews the exact Worker/Lease/Generation as a fencing probe,
then reuses the normal Inspect, dirty-state and terminal Checkpoint pipeline. One probe owns a unique stable request
ID and retries only ambiguous transport loss within that same deadline; deterministic Lease rejection is not
retried. Only a Ready Checkpoint or a byte-for-byte match with the already restored Ready Checkpoint counts as
preserved. Snapshot traversal checks cancellation for every visited file or directory, including empty trees.

Release uses a stable request ID and retries ambiguous responses. Its `reason`, which is projected into the
`execution.recovering` Event and Outbox, includes `workspace-checkpoint=ready` or
`workspace-checkpoint=unchanged` on proof of preservation. Any Lease probe, inspection, capture, upload or Ready
failure uses `data-loss-risk=workspace-checkpoint-failed`; it must never claim that local changes were saved.
If every Release attempt is lost and the Lease later expires, the Control Plane preserves the exact
`lease_expired` reason and adds `risk=data-loss-risk=workspace-checkpoint-unconfirmed` to the recovering Event and
Outbox whenever the Worker had entered Drain and that Execution Generation has no confirmed Ready Checkpoint.
This fallback is deliberately conservative because an unreachable Worker cannot prove the `unchanged` case.

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
