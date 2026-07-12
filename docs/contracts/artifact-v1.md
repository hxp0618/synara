# Artifact v1 contract

Artifact is the tenant-scoped authority for attachments, generated files, terminal logs, workspace
snapshots, and checkpoints. Payload bytes never live in Session Event JSON or provider resume state.

## Ownership and object keys

Every Artifact persists `tenant_id`, `organization_id`, `project_id`, and `session_id`. Worker-created
Artifacts also require `execution_id`. PostgreSQL composite foreign keys prevent cross-tenant or
cross-project associations.

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

The distributed retention sweep also owns upload-expiry cleanup. An expired `pending` Artifact loses
its temporary key plus any final key orphaned by a crash between promotion and metadata commit, then
becomes `failed`. An expired upload grant for an already `ready` Artifact can only recreate the isolated
temporary key; the sweep deletes that key and clears the grant metadata without touching the verified
final object. Cleanup is idempotent and runs under the same PostgreSQL Advisory Lock as retention.

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
