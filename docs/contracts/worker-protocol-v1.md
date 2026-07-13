# Worker Protocol v1 (legacy)

> Legacy baseline only. New managed Workers implement the breaking
> [Worker Protocol v2 delta](./worker-protocol-v2.md); this document is retained for historical
> compatibility and for the semantics that v2 does not replace.

The protocol is transport-neutral. Initial implementations may use HTTP/WebSocket JSON, while
message names and fencing behavior remain stable if the transport later moves to gRPC.

## Commands

`RegisterWorker`, `Heartbeat`, `ClaimExecution`, `StartSession`, `ResumeSession`, `SendTurn`,
`InterruptTurn`, `ResolveApproval`, `ResolveUserInput`, `RuntimeEvent`, `UploadArtifact`,
`ReportWorkspaceReady`, `ReportWorkspaceDirty`, `ReportWorkspaceFailed`, `CreateWorkspaceCheckpoint`,
`ReadyWorkspaceCheckpoint`, `FailWorkspaceCheckpoint`, `DownloadWorkspaceCheckpointArtifact`,
`CompleteExecution`, `FailExecution`, and `ReleaseLease`.

## Common envelope

Every execution-scoped message carries:

```text
requestId, tenantId, executionId, workerId, generation, occurredAt
```

Registration binds a worker to one persisted `executionTargetId` and `targetKind`. Claim requests use
the same fields and cannot select another target. `shared_pool` and `dedicated_pool` are not Worker
Protocol v1 domain values.

Registration and Heartbeat carry `protocolVersion`. On a legacy v1 Control Plane, version `1` is both the
minimum and maximum supported version. Unsupported versions return HTTP `426` with stable code
`worker_protocol_version_unsupported` and `minimumSupported` / `maximumSupported` details. `version`
remains the Worker build/image version and is not used as the protocol number.

A successful Claim includes a read-only `workload` snapshot with Tenant/Organization/Project/Session/
Turn IDs, Session title, Provider and Model, Turn input, repository URL, default branch, and the
Project's optional `gitCredentialId`. The
snapshot is loaded in the claim transaction and is preserved across idempotent Claim replay. Workers
never query control-plane tables directly.

The Workload includes the logical `remoteWorkspaceId`. Before `StartExecution`, agentd renews the Lease,
materializes the stable Session checkout and reports either `workspace.ready` with Repository fingerprint,
branch, base commit and HEAD, or `workspace.failed` with a safe stable error. Both reports require the current
Worker ID, Lease Token and Generation. Provider execution cannot begin from an unreported managed Workspace.

Materialization uses separate configured Workspace and Git cache roots. The cache is keyed by Execution Target,
Tenant, Project and repository fingerprint, protected by a cross-process lock and network-Fetched on every Turn.
The mutable Workspace owns a private bare repository plus relative linked worktree; it cannot use the cache as its
Git common directory or object alternate. Agentd holds the Workspace lock until the Provider process has left its
managed process scope, final inspection and Checkpoint work have finished, and the terminal Execution
request has been attempted. A cancelled or failed Runner must terminate managed descendants before that lock can
be released.

When `gitCredentialId` is present, agentd resolves it through the dedicated execution-scoped
`/git-credentials/{credentialID}/resolve` Worker endpoint. The request is fenced by Worker ID, Lease Token and
Generation. Control Plane additionally verifies the Project still owns that binding and the Credential host
matches the HTTPS Repository hostname. The returned username/token is consumed only by the ephemeral Git
AskPass channel during Clone/Fetch and is cleared before `workspace.ready` and Provider start.

After Provider execution, agentd reports `workspace.dirty` before `CompleteExecution` when tracked/untracked
Git content or managed non-Git files changed. The report remains Lease/Generation-fenced and carries only the
safe current branch and HEAD metadata, never file contents. Before a dirty managed Workspace can complete,
agentd must create and persist a ready Checkpoint. Clean Git uses a Git reference, dirty Git uses a bounded Patch
Artifact, and non-Git content uses a bounded Snapshot Artifact. A Patch is the deterministic `baseCommit` to
final-Worktree binary diff plus authoritative raw tracked upserts and regular untracked files. Individually
ignored files and unknown ignored directories are included, while ignored trees containing versioned known
dependency/tool-cache path segments are declared rebuildable and excluded. Restore validates the Patch and file
manifest in sibling staging, overlays raw tracked bytes, and requires index/worktree consistency before replacing
the active checkout. It restores the file tree, tracked
versus untracked classification and branch name with tracked deltas staged at `baseCommit`; it does not promise
to recreate an unpushed local Commit graph. A new Turn freezes the last ready Checkpoint into its Workload, and
agentd downloads and restores it before Provider start.

Checkpoint Artifact create carries `checkpointId`. The Control Plane derives one deterministic Artifact identity,
binds it to the Checkpoint and changes the Checkpoint to `uploading` before issuing a grant. Create/PUT/Complete
may be replayed after an ambiguous response without creating another Artifact or another `artifact.ready` event.
Ready replay still validates the current Lease/Generation and submitted Size/SHA-256/Content-Type.

The retention sweeper fails abandoned or upload-expired Checkpoints with a durable `checkpoint.failed` event.
It never replaces the last ready recovery point and never deletes an Artifact referenced by a
`pending`, `uploading` or `ready` Checkpoint.

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
command type, and the validated resolution payload. The Worker marks the command `delivered` before writing it
to the Provider Host channel and marks it `acknowledged` only after the Host returns a correlated
terminal message. Both transitions are idempotent. `delivered` commands remain pullable until acknowledged so
an agentd restart can replay the stable command ID without duplicating the Provider action. A stale Worker or
Generation is permanently fenced from pull, delivery, and acknowledgement.

## Durable Provider control commands

User control intent is persisted separately from interaction resolution. `InterruptTurn` and `SteerTurn` use
the reusable Control Command channel; the same Generation-fenced delivery model is reserved for Compact,
Rollback, Fork, Review and Stop Session. A command requested before Claim remains unbound until the Execution
receives a Worker and Generation. Lease recovery returns delivered-but-unacknowledged commands to `pending`
and binds them to the replacement Generation instead of losing user intent.

Each durable command has one frozen Provider Capability ID. Claim filters Workers against their immutable
Provider Manifest before changing the Execution Generation or Lease. Legacy Workers without a Manifest and
Workers that explicitly advertise the capability as unsupported cannot Claim that Execution. Explicit Claims
return `capability_unsupported`; ordinary queue Claims skip incompatible work so compatible Workers can
continue serving the queue.

Agentd marks a Control Command delivered before writing it to Provider Host and acknowledges it only after the
correlated terminal Result. Interrupt acknowledgement carries the Provider Resume Cursor. Control Plane stores
that Cursor, releases the Lease, marks the Execution and Turn `interrupted`, and appends
`execution.interrupted` in one transaction. A stale Generation cannot acknowledge the command, and agentd does
not subsequently misreport the acknowledged user interrupt as `execution.failed`.

Steer acknowledgement keeps the Execution running and appends `turn.steered`; the original persisted
`turn.steer-requested` event remains the user-visible intent. Retrying the stable command ID cannot create a
second Provider Steer action.

## Worker Drain

On SIGINT/SIGTERM, agentd sends an immediate Heartbeat with `draining=true`, stops Claiming new Executions and
continues Heartbeat plus Lease renewal for the active Execution. If the Runner reaches a normal terminal result
before `SYNARA_AGENTD_DRAIN_TIMEOUT`, agentd reports Complete/Fail normally. At the deadline it cancels the
Provider Runner and explicitly releases the Lease; it never reports a forced shutdown as successful completion.

Runner cancellation and Provider Host abort apply to the isolated Unix process group or Windows Job Object, not
only the direct child. Deliberate Unix process-group escape remains a process-sandbox release gate; support claims
must not treat the current group boundary as containment of an adversarial detached descendant.

Managed SSH, Docker and Kubernetes Workers use a 20-second Drain deadline inside a 30-second process/container
termination window. The embedded Local Worker drains before the Control Plane HTTP server shuts down, so its
final Heartbeat and Execution terminal request still have a live endpoint. A Draining Worker that stops
heartbeating is eventually marked Offline by the same stale-Worker sweep used for Online Workers.

## Boundary

Workers receive domain commands and return versioned events. They do not receive SQL, table names,
or access to the control-plane database. Provider credentials are delivered as short-lived or
revocable secrets and must never appear in event payloads or logs.

The generic `synara-agentd` implementation keeps Worker and Lease credentials outside provider
runners, renews the Lease while a runner is active, validates runner Artifact paths against the
Execution workspace, and negotiates Provider Host Protocol v2 independently from this legacy Worker Protocol.
`agentd-runner-v1.md` documents only the explicit legacy compatibility boundary.
