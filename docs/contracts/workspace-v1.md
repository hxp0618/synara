# Remote Workspace and Checkpoint contract v1

Remote Workspace v1 separates the logical Session Workspace from any disposable Worker filesystem path.
PostgreSQL owns identity and lifecycle metadata; Git and ready Artifact payloads own recoverable content; a Worker
checkout is only a cache.

The DDL source of truth is the forward migration chain through
`000027_workspace_cleanup_dispatch.sql`; published migrations are never rewritten in place.

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

Agentd materializes a managed checkout at a stable path derived from Execution Target, Tenant, Project, Session
and logical Workspace IDs. The path no longer includes the current Execution ID, so a later Turn on the same
Target reuses the Session checkout. The Provider receives only the `checkout` directory; PostgreSQL never stores
that path.

The Worker-local layout is versioned and keeps disposable shared Git objects separate from the private mutable
Workspace generation:

```text
GIT_CACHE_ROOT/
  v1/<target>/<tenant>/<project>/<repository-fingerprint>/repo.git

WORKSPACE_ROOT/
  .locks/...
  v3/<target>/<tenant>/<project>/<session>/<workspace>/<materialization-incarnation>/
    manifest.json
    repo.git
    checkout/
```

Layout v3 binds a logical materialization ID to an immutable physical incarnation ID. Every new managed
materialization uses v3; layout v2 is adoption-only for an existing row explicitly migrated by Control Plane and
is never inferred from omitted v3 fields. Worker Protocol v2 carries both identities and rejects a downgrade.

The cache is a bare, read-only materialization input from the Provider's perspective. Agentd takes a cross-process
cache lock and performs a credential-authorized network Fetch on every Turn, so a warm cache cannot bypass a
revoked Credential. It then Fetches through an explicitly enabled, controlled `file://` transport into the
Workspace-private `repo.git` and creates a relative linked worktree under `checkout`. The private repository must
not use hardlinks, alternates, `--shared`, `--reference*`, or the shared cache as its Git common directory. Deleting
or rebuilding the cache therefore cannot corrupt an existing dirty Workspace.

One cross-process Workspace lock is held from materialization through Provider exit, final inspection,
Checkpoint creation and the terminal Complete/Fail/Release attempt. Lock ordering is always Workspace then cache.
The generation Manifest and linked-worktree metadata are revalidated before reuse or restore: `manifest.json`,
`checkout/.git`, `commondir`, `gitdir`, the private `repo.git` and all ancestors are checked. All directories must
be non-symlink directories below the configured Workspace root and all metadata files must be bounded regular files.
Absolute or escaping metadata paths, symlinks and object alternates are rejected.

Workspace preparation happens while the claimed Lease is being renewed and before `execution.started`:

1. Create and verify each Workspace path component without following symlinks.
2. Validate the Repository URL, default branch and repository fingerprint.
3. Under the cache lock, Fetch the default branch from the validated network remote on every Turn; atomically
   rebuild a missing or corrupt bare cache.
4. Build or validate a Workspace-private bare repository and relative linked worktree without shared object
   storage, then Fetch the requested ref from the controlled cache input.
5. Create/restore a deterministic `synara/session-<session-id>` branch when the checkout is still on the default branch.
6. Read the current branch, merge base and HEAD and revalidate the private Git metadata.
7. Report `workspace.ready` under the current Worker/Generation, then start the Provider.

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

Workspace Credential ownership is modeled by immutable `credential_bindings`, not by a mutable secret field on a
Project. Project stages use `git_fetch`, `git_push`, `registry_pull`, `registry_push`, `package_read` and
`package_publish`; Execution Targets use the separate `worker_image_pull` infrastructure stage. A Binding owns one
normalized non-secret selector and can only transition once from active to disabled. Claim creates a new immutable
Generation-fenced Grant for each active Project Binding. Claim receipt replay reuses those Grant IDs, while recovery
creates new Grants and snapshots the then-current Credential versions. Workload and Worker resolve APIs expose the
Grant descriptor, never the underlying Credential ID or version.

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
same SHA-256. The Artifact also carries an immutable reverse `workspace_checkpoint_id` binding, so upload retry,
retention and deletion can identify the owning recovery point without inspecting object keys. Failed Checkpoints
never replace `remote_workspaces.current_checkpoint_id`.

Agentd creates Checkpoints automatically after Provider execution while the Lease is still current. A clean Git
checkout uses `git-reference`; dirty Git uses a bounded deterministic Patch; managed non-Git content uses a
bounded deterministic Snapshot of regular files.

A Patch is generated from `baseCommit` to the final Worktree with binary/full-index output and rename detection
disabled. It therefore captures tracked changes introduced by local commits after the base as well as staged,
unstaged, binary, deleted and executable tracked paths. The deterministic tar contains `tracked.patch`, the
final raw bytes or symlink target of every tracked upsert under `tracked/`, and regular untracked files under
`untracked/`. Individually ignored regular files such as `.env` are durable; whole ignored directory trees such
as dependency/tool caches are excluded only when they contain a versioned rebuildable path segment (for example
`node_modules`, `.bun`, `.turbo`, `.venv`, `.next` or `target`). Other ignored directories remain durable and are
enumerated like ordinary untracked content. The Manifest declares the rebuildable exclusion policy so a normal
package install cannot make a Checkpoint unusable or silently redefine durability. Unmerged indexes,
assume-unchanged/skip-worktree/sparse entries, Gitlinks/Submodules, non-reproducible local Git
attributes/configuration and non-regular included untracked files are rejected.

The `rebuildable-ignored-directory-segments-v1` set is `.astro`, `.bun`, `.gradle`, `.mypy_cache`, `.next`,
`.nuxt`, `.pnpm-store`, `.pytest_cache`, `.ruff_cache`, `.turbo`, `.venv`, `.vite`, `.yarn`, `__pycache__`,
`coverage`, `node_modules`, `target`, `venv`, `.synara`, `.synara-*` and `.vitest-*`.

Patch restore clones an isolated sibling staging checkout, anchors its branch to `baseCommit`, applies the tracked
Patch to the index, overlays the authoritative raw tracked payload, restores untracked files, and verifies every
path, size, SHA-256, mode, classification, index/worktree consistency and regenerated Patch identity before
replacing the active Workspace. The raw overlay makes a same-Turn `.gitattributes` change reproducible rather
than depending on the base checkout's conversion rules. The restored tracked delta is staged and the untracked
files remain untracked. This contract restores the authoritative file tree and branch name; it does not recreate
local Commit objects or claim that an unpushed source HEAD/Commit graph survived. The installed HEAD is the
available `baseCommit`.

Snapshot capture excludes `.git`, rejects symlinks/devices/FIFOs/sockets, records a sorted
path/size/SHA-256/executable manifest, and verifies the complete archive again in an isolated staging directory
before replacing a Workspace during restore. Provider execution starts only after the frozen ready Checkpoint
has been downloaded, size/hash verified and restored.

Checkpoint Artifact creation is deterministic and retryable. The first create transaction binds the Artifact and
moves the Checkpoint to `uploading`. A lost Create/PUT/Complete response reuses the same Artifact; Local upload
tokens rotate, while S3/MinIO re-sign the same temporary key. A ready replay is Lease/Generation-fenced and the
Worker rechecks the local payload metadata before accepting it.

Expired pending uploads atomically fail their Checkpoint, return the Workspace from `checkpointing` to
`ready`/`dirty`, and preserve the previous current Checkpoint. Retention clears terminal Execution restore
references, expires only non-current/unreferenced Checkpoints, and deletes an Artifact only after no
`pending`/`uploading`/`ready` Checkpoint references it. User deletion returns the stable
`artifact_checkpoint_referenced` conflict while such a reference exists. S3/MinIO temporary upload keys use one
bounded post-expiry cleanup grace pass to catch late presigned PUTs, then leave the sweep permanently.

## Recovery order

Recovery prefers:

1. committed Git reference;
2. ready Patch or Workspace Snapshot Checkpoint;
3. an explicitly reported unrecoverable/data-loss-risk outcome.

An absent Worker directory is never interpreted as an empty authoritative Workspace.

## Physical cleanup

Control Plane creates a durable cleanup command only after the logical Workspace reaches a cleanup-eligible state.
The command carries the exact Target, storage scope, materialization ID, physical incarnation ID and layout version.
Agentd revalidates that complete tuple against the managed root and Manifest before deletion, takes the same
Workspace lock used by execution, refuses symlink/path escape, and deletes only that physical root. Kubernetes
cleanup additionally requires the current Pod UID/Worker incarnation evidence and fails closed when ownership is
ambiguous.

Cleanup Claim, renew, complete, fail and release are Worker-incarnation and Lease fenced. The scheduler interleaves
cleanup and Execution Claims so cleanup cannot starve ordinary work or monopolize a Worker. A stale Worker,
Generation, layout-v2 inference, or historical physical incarnation cannot acknowledge or delete a replacement
Workspace.

## Implemented boundary and future scaling

Stage 3 completes the schema, Session/Execution bindings, public/private HTTPS Clone/Fetch materialization,
Project Git Credential binding, short-lived SSH Agent/Host Key delivery, cross-process locked shared Git cache,
Workspace-private relative `git worktree` generations, Generation-fenced state reporting,
Git-reference/Patch/Snapshot Checkpoint capture/restore, safe Checkpoint/Artifact retention, interrupted
staging/backup reconciliation, and the shared Local/SSH/Docker/Kubernetes acceptance boundary.

Kubernetes cross-Pod cache sharing still requires an explicitly configured RWX-equivalent PVC; the default cache
remains Pod-local and disposable. This is a Stage 4 capacity/storage scaling option, not a Stage 3 correctness or
release blocker.
