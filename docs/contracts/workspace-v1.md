# Remote Workspace and Checkpoint contract v1

Remote Workspace v1 separates the logical Session Workspace from any disposable Worker filesystem path.
PostgreSQL owns identity and lifecycle metadata; Git and ready Artifact payloads own recoverable content; a Worker
checkout is only a cache.

The DDL source of truth is `000020_remote_workspaces_checkpoints.sql`.

## Identity and states

Each Agent Session has at most one `remote_workspaces` row. It is scoped by Tenant, Organization, Project,
Session and Execution Target and never stores a host path, Repository Credential or embedded Credential URL.

Workspace states are:

```text
pending -> preparing -> ready -> dirty -> checkpointing
                    \-> recovering
pending/ready/dirty/recovering -> cleanup-pending -> cleaned
any active preparation state -> failed
```

Changing the Session Execution Target moves the logical Workspace to `recovering`; it does not reuse a mutable
checkout from the previous Target.

## Execution binding

Every new Turn ensures the Session has one Runtime Binding and one logical Workspace. The resulting Execution
stores `provider_runtime_binding_id` and `remote_workspace_id`. Claim records the current Worker Manifest,
Worker/Generation and last-use time before Provider work begins. Database constraint triggers reject a Runtime
Binding or Workspace from another Session, Provider, Tenant or Execution Target.

## Checkpoints

A Checkpoint uses a stable idempotency key and one strategy:

```text
git-reference | patch | snapshot
```

Patch and snapshot payloads are Artifacts. A Checkpoint cannot become `ready` unless the referenced Artifact is
also `ready`, belongs to the same Session/Execution, has kind `checkpoint` or `workspace_snapshot`, and has the
same SHA-256. Failed Checkpoints never replace `remote_workspaces.current_checkpoint_id`.

## Recovery order

Recovery prefers:

1. committed Git reference;
2. ready Patch or Workspace Snapshot Checkpoint;
3. an explicitly reported unrecoverable/data-loss-risk outcome.

An absent Worker directory is never interpreted as an empty authoritative Workspace.

## Remaining implementation gate

The schema and Session/Execution bindings are active. Stage 3 still must implement Repository URL/SSRF policy,
Credential-scoped Clone/Fetch/Worktree materialization, Checkpoint creation/upload, cleanup/retention and the
shared Local/SSH/Docker/Kubernetes acceptance fixture.
