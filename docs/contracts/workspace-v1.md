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

Agentd materializes a managed checkout at a stable path derived from Tenant, Project, Session and logical
Workspace IDs. The path no longer includes the current Execution ID, so a later Turn on the same Worker reuses
the Session checkout. The Provider receives only the checkout directory; PostgreSQL never stores that path.

Workspace preparation happens while the claimed Lease is being renewed and before `execution.started`:

1. Create and verify each Workspace path component without following symlinks.
2. Validate the Repository URL and default branch.
3. Clone the default branch or Fetch the explicit remote ref into an existing checkout.
4. Create/restore a deterministic `synara/session-<session-id>` branch when the checkout is still on the default branch.
5. Read the current branch, merge base and HEAD.
6. Report `workspace.ready` under the current Worker/Generation, then start the Provider.

Preparation failure reports `workspace.failed` and fails the Execution with a stable `workspace_invalid` code.
The ready/failed endpoints are idempotent and Generation-fenced; an obsolete Worker cannot overwrite state from
the replacement Worker.

After Provider execution, agentd inspects the checkout again while the Lease is still being renewed. A changed
tracked/untracked Git checkout, or any generated content in a managed non-Git Workspace, is reported through
the idempotent Generation-fenced `workspace.dirty` endpoint before Execution completion. The Control Plane
persists the latest branch/HEAD and appends `workspace.dirty`; an obsolete Generation cannot mark or overwrite
the replacement Worker's Workspace state. Unsafe Git configuration discovered after Provider execution fails
the Execution instead of being accepted as a recoverable state.

## Repository network policy

The managed remote implementation accepts HTTPS repositories on the default HTTPS port. It rejects embedded
Userinfo, query strings, fragments, non-HTTPS schemes, ambiguous paths, localhost, private, loopback,
link-local, multicast and unspecified addresses. All resolved addresses must be public.

Network Git commands pin the validated address with `http.curloptResolve` and disable HTTP redirects, closing
the DNS-rebinding and redirect-to-metadata-service gaps between validation and Clone/Fetch. Git runs without
ambient Credential helpers, SSH Agent, global/system Git config or LFS smudge downloads. Repository URLs with
embedded Credentials are never accepted or persisted.

Private HTTPS uses a Project-bound, purpose-isolated `git/https_token` Credential. Agentd resolves it only for
the current Worker, Lease and Generation and exposes it to Git through an ephemeral Unix-socket AskPass helper.
The helper accepts only Username/Password prompts containing the exact validated Repository hostname. The
username/token are not placed in argv, the Repository URL, ordinary environment variables or the Workspace,
and the socket is removed immediately after Clone/Fetch. Fetch rejects a checkout whose local `.git/config`
contains Credential helpers, AskPass, hooks, SSH commands, proxy/extra headers, URL rewrites, includes, filters,
or remote upload/receive command overrides.

SSH repositories remain blocked until the separate short-lived SSH Agent and pinned Host Key delivery contract
is implemented. Agentd never falls back to host credentials or interactive login.

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

The schema, Session/Execution bindings, public/private HTTPS Clone/Fetch materialization, Project Git Credential
binding and Generation-fenced state reporting are active. Stage 3 still must implement short-lived SSH
Credential delivery, shared read-only Git cache plus `git worktree` materialization, Patch/Snapshot Checkpoint
creation and restore, cleanup/retention and the shared Local/SSH/Docker/Kubernetes acceptance fixture.
