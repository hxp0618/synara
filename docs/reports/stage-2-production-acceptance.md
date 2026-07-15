# Stage 2 Production Acceptance Report

> Historical evidence only: this report is fixed to the commits named below and its 16-Migration Kind baseline.
> It does not prove the current repository state. The current fixed Stage 2 evidence record is
> `docs/reports/stage-2-production-acceptance-b507b0c3.md`, fixed to Migration `000031` with Result `PASS`. The
> intermediate `docs/reports/stage-2-production-acceptance-1a53c93a.md` report remains fixed to `000028`.

- Date: 2026-07-13
- Branch: `codex/saas-tenancy-user`
- Implementation baseline commit: `425554e6`
- Final verification base: `702cb0d0` plus the Settings route chunk-warming fix described below
- Scope: Web authority cutover, local fallback, single-node/multi-replica operations, dependency failures,
  Kubernetes rollout and sensitive-log boundaries
- Result: PASS for repository-controlled and MinIO/S3-compatible evidence
- External evidence: real AWS S3 Live Store remains pending an explicitly authorized writable Bucket

## Web authority and local fallback

SaaS browser flow verified:

1. Dev login and Tenant/Organization Context rendered.
2. Main UI created a Control Plane Project and Session.
3. First message created a Turn and Execution.
4. Worker Claim/Start/Runtime Event/Complete produced visible SSE output.
5. Reload restored Project, Session and transcript from PostgreSQL.
6. A delayed local Snapshot could not overwrite the Control Plane Projection.
7. Settings idle warming now loads only the generated route chunk instead of creating a speculative Route Match.
   The persisted thread was reloaded and Settings was opened in three fresh tabs after idle warming; all runs had
   no relevant console warning/error.

Local-mode browser flow verified at `http://localhost:9893/` with an isolated Home and ports:

1. No Control Plane login gate appeared.
2. The repository root was added through the local project path UI.
3. The `synara` Project and local thread appeared in the sidebar.
4. Reload restored the Project from the isolated SQLite Snapshot.
5. The page had meaningful content, no framework overlay and no console warning/error.

## Compose fault acceptance

Command:

```bash
deploy/saas/failure-acceptance.sh
```

Verified:

- A running Worker stopped heartbeating; its Lease expired and the Execution entered `recovering`.
- A replacement Worker claimed the same Execution with Generation 2.
- The old Worker/Generation could not append a Runtime Event.
- PostgreSQL contained the `worker.offline` Outbox message.
- Stopping MinIO caused `/ready=503` while `/health=200`; authenticated metadata remained readable.
- Restarting MinIO restored Readiness and the same Pending Artifact completed with server-verified SHA-256.
- Stopping PostgreSQL caused `/ready=503` while `/health=200`.
- Restarting PostgreSQL restored the existing Login Session, Tenant and Worker Token.
- Random database, MinIO, Worker, Cursor, KMS, Lease and Prompt Sentinels were absent from Control Plane logs.
- Presigned URL query strings were absent from logs.

## Kubernetes enterprise acceptance

Command:

```bash
KIND_BIN=/tmp/synara-stage2-bin/kind \
SYNARA_KIND_CLUSTER=synara-stage2-0713 \
  deploy/kubernetes/kind-acceptance.sh
```

The repeatable wrapper created and removed a disposable Kind Kubernetes v1.33 cluster. It loaded local images
directly, avoiding dependence on a node-local proxy or registry credentials.

Verified:

1. PostgreSQL and MinIO used PVC-backed storage.
2. Two Enterprise Control Plane replicas became Ready against one PostgreSQL and S3-mode MinIO endpoint.
3. The authoritative migration table contained exactly 16 versions.
4. Thirty readiness probes remained successful while one Control Plane Pod was deleted.
5. Deployment created a replacement Pod, and an existing Worker Token successfully heartbeated through it.
6. Scaling PostgreSQL to zero removed all Control Plane Ready endpoints; scaling it back restored both replicas
   without losing database state.
7. Scaling MinIO to zero removed all Control Plane Ready endpoints; restoring MinIO and its Bucket restored both
   replicas.
8. Reconciler ServiceAccount could create Pods and Secrets as required by the checked-in RBAC.
9. Random PostgreSQL, MinIO, Worker, Cursor, KMS and Worker-token Sentinels were absent from Pod logs.
10. The Kind cluster, Namespace, ClusterRole and test volumes were removed after the run.

## Existing Stage 2 suites

- Single-node SaaS Acceptance covers Tenant, Organization, Session, SSE, Worker, Lease, Artifact, isolation and
  published Outbox lifecycles.
- Multi-replica Compose Acceptance covers cross-replica Login revocation, Turn/Claim concurrency, SSE catch-up,
  migration locking, replica loss and global SSE connection leases.
- Focused Web tests cover Control Plane Context, authoritative Projection and local recap isolation.
- At this historical baseline, Go tests covered Runtime Event v1 version/object/65,536-byte boundaries and Artifact
  fallback. The current managed path uses Runtime Event v2; v1 is retained only for the explicit legacy
  compatibility path and existing-row replay.

Final verification after the browser route fix:

- Go: `go test ./...` and `go test -race ./...` passed.
- Server: 153 test files passed, 1 skipped; 1,707 tests passed, 6 skipped; build passed.
- Web: 208 test files and 2,460 tests passed; build passed.

## External AWS S3 boundary

The shared `SYNARA_TEST_S3_*` Live Store suite is implemented, but this acceptance did not receive authority to
write to a real AWS S3 Bucket. MinIO was exercised both through the `minio` profile and through the Enterprise
`s3` adapter with a custom S3-compatible endpoint. This proves repository behavior and S3-compatible protocol
handling, but it is not evidence for IAM, AWS KMS, Bucket Policy, regional networking or the AWS service itself.

Before an AWS-backed release, run the shared Live Store suite against an explicitly authorized disposable Bucket
and attach the result to this report or the release record.
