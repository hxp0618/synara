# Artifact v1 contract

Artifact is the tenant-scoped authority for attachments, generated files, terminal logs, large Diffs, workspace
snapshots, and checkpoints. Payload bytes never live in Session Event JSON or provider resume state.

PostgreSQL migration `000041_diff_artifact_kind.sql` adds the `diff` kind through a forward-only replacement of the
existing `artifacts_kind_check`; historical migrations remain immutable. SQLite continues to use the same Service
kind validation boundary and its metadata-store safety mirror.

## Ownership and object keys

Every Artifact persists `tenant_id`, `organization_id`, `project_id`, and `session_id`. Worker-created
Artifacts also require `execution_id`. PostgreSQL composite foreign keys prevent cross-tenant or
cross-project associations.

Checkpoint payload Artifacts additionally persist `workspace_checkpoint_id`. It is an immutable reverse binding
to the same Tenant/Session/Execution and to the strategy-specific kind (`checkpoint` for Patch,
`workspace_snapshot` for Snapshot). One Checkpoint can own at most one Artifact.

Object keys are deterministic and never accepted from clients:

```text
tenants/{tenantId}/organizations/{organizationId}/projects/{projectId}/
sessions/{sessionId}/executions/{executionId}/artifacts/{artifactId}
```

Session-level user attachments use the reserved execution segment `_session`.

## Lifecycle

States are `pending`, `ready`, `deleting`, `deleted`, and `failed`.

1. An authorized user or current Worker Lease creates pending metadata.
2. Local storage receives a short-lived random upload token; MinIO/S3 receives a short-lived
   presigned PUT URL for a random temporary key that cannot overwrite the final key.
3. The uploader submits size, SHA-256, and Content-Type.
4. The control plane stats and re-reads the stored object, computes SHA-256 itself, and marks the
   Artifact ready only when all values match.
5. Downloads require RBAC first. Local storage receives a short-lived database-backed download
   token; MinIO/S3 receives a presigned GET URL.
6. Deletion transitions through `deleting`, removes the payload idempotently, then persists
   `deleted` and `deleted_at`.

For a Checkpoint upload, the Control Plane derives a deterministic Artifact ID from `checkpointId` and binds it
before returning the first grant. Pending replay rotates the Local token or re-signs the same S3/MinIO temporary
key. Ready replay returns metadata without another PUT and first revalidates Lease/Generation plus
Size/SHA-256/Content-Type. UploadGrant secrets are not persisted as long-lived Worker receipts.

Ordinary Worker `generated_file`, `terminal_log`, and `diff` uploads use the same response-loss safety without borrowing
Checkpoint identity. Agentd hashes the regular-file payload before Create and derives an opaque idempotency key
from Execution, Generation, root-relative candidate path, Artifact metadata, size and SHA-256. The Control
Plane derives one deterministic Artifact ID from Execution, Generation and that key. A pending Create replay
refreshes the upload grant, while a ready replay returns the same Artifact without another PUT. Reusing the key
with different ownership or metadata fails closed. Agentd resolves every path component below the declared
anchored Workspace or Runtime Output Root descriptor bound before Provider start, rejects symlinks and non-regular
files, then retains the safely opened file descriptor through pre-hash, retry, PUT and Ready replay verification.
Replacing a root path or checked parent directory therefore cannot redirect the upload outside the bound root.
Content-Type is normalized before identity derivation and confirmation, and pre-hash plus Checkpoint archive copies
honor Execution/Drain cancellation. Snapshot traversal checks the cancellation context for directories as well as
files. Agentd refuses to Complete if the bytes uploaded no longer match the precomputed identity.

Provider-retained Terminal output uses a separate agentd-owned Runtime Output Root rather than the Workspace Root.
Agentd creates and binds that private root before Provider start, passes only its path in the internal Runner input,
and accepts only a root-relative `terminal_log` candidate correlated to an already-started `terminalId`. The
Provider Host never opens the reported file. Agentd rejects traversal, absolute paths, symlinks, non-regular files,
size drift, or a candidate that follows inline bytes for the same Terminal; the latter prevents duplicated prefixes.
It then reads the bound descriptor from byte zero, emits at most a 32 KiB safe UTF-8 preview, and persists the full
stream as fixed 1 MiB Artifact segments. Invalid UTF-8, NUL, ANSI/control, and bidirectional-control content receive
no Session Event preview and are stored as binary Artifact payload. The physical Runtime Output path is removed
after the Execution and never becomes an object key, original name, Event field, or resume-state value.

A large Turn Diff uses kind `diff`, original name `turn.diff`, and canonical
`text/x-diff; charset=utf-8`. Provider Host stages it only beneath the Runtime Output Root and supplies exact size
plus file/addition/deletion counts. Agentd performs the anchored open and text Secret Guard before upload, then
requires the Ready Artifact metadata to match the guarded bytes. Only after that confirmation does it append an
Artifact-backed `turn.diff.updated`; the Event contains Artifact ID, size, SHA-256, Content-Type and summary, never
the physical path or Diff bytes. An invalid candidate or incomplete/mismatched Ready response fails closed.

A Patch Checkpoint uses kind `checkpoint` and a deterministic tar containing `tracked.patch`, authoritative raw
tracked upserts under `tracked/`, and the included regular files under `untracked/`. The PostgreSQL Checkpoint
manifest is the metadata authority; the tar does not duplicate it. The Manifest declares that whole ignored
directory trees are excluded only under the versioned rebuildable dependency/tool-cache segment policy;
individually ignored files and other ignored directories remain durable. Download verifies the Artifact
size/SHA-256 first, then restore rejects unknown, missing, duplicate, traversal, `.git`, type, size, digest or
executable-mode mismatches before the payload can replace the active Workspace.

The distributed retention sweep also owns upload-expiry cleanup. An expired `pending` Artifact is first sealed
as `failed`; if it belongs to a Checkpoint, the same transaction fails that Checkpoint, releases the Workspace
from `checkpointing` and appends `checkpoint.failed`. Object deletion then runs outside the database transaction.
Local uploads terminate cleanup immediately after a successful delete. S3/MinIO keeps the isolated temporary key
for one bounded grace interval, deletes it once more to catch an in-flight late PUT, then clears the cleanup handle
so terminal Artifacts stop entering the sweep. A `ready` Artifact's verified final object is never touched.

Retention skips every Artifact referenced by a `pending`, `uploading` or `ready` Checkpoint. Terminal Execution
restore references are cleared before an unreferenced non-current Checkpoint becomes `expired`; only then can its
Artifact be deleted. User deletion under an active Checkpoint reference returns
`artifact_checkpoint_referenced`.

Worker create and completion both validate Worker ID, Tenant ID, Execution ID, Generation, Lease
token, and lease expiry. A user cannot confirm an Artifact created by a Worker, so an old Generation
cannot use the user API to bypass fencing.

## Storage profiles

- `personal`: `LocalStore`, private files under `SYNARA_ARTIFACT_LOCAL_PATH`.
- `single-node`: MinIO or S3-compatible storage.
- `enterprise`: standard S3/object storage.

`SYNARA_ARTIFACT_ENDPOINT` is the control-plane connection endpoint.
`SYNARA_ARTIFACT_PUBLIC_ENDPOINT` is optional and is used only to sign browser-reachable URLs. This
supports Compose where the control plane connects to `minio:9000` while clients use a published host
port.

The MinIO/S3 adapter supports path-style lookup, explicit region and bucket, static credentials or
the standard IAM credential provider, streaming puts, stat, streaming reads, deletion, and presigned
PUT/GET URLs.

`internal/artifacts/s3_store_integration_test.go` is the shared live-store compatibility suite. It runs
against MinIO in normal acceptance and can run unchanged against a writable AWS S3 test bucket by
supplying the `SYNARA_TEST_S3_*` environment variables.

## Personal payload migration

Personal export includes Artifact metadata plus verified payload references: Artifact ID, source
object key, size, SHA-256, and Content-Type. It never embeds bytes or upload tokens in the JSON
manifest.

Import first restores metadata transactionally, then `--source-artifact-dir` copies each ready payload
to MinIO/S3, verifies the destination bytes, updates the Artifact bucket/version, and records
`artifact_payload_migrations`. Repeating the same import validates the existing destination object and
reports it as replayed instead of creating duplicate metadata.
