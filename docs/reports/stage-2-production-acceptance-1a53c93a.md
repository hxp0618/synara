# Stage 2 Production Acceptance Report — `1a53c93a`

- Date: 2026-07-13
- Branch: `codex/saas-tenancy-user`
- Verified source baseline: `1a53c93adff88d9c13b44b06cc2380e176d52230`
- Verification scope: current Control Plane through migration `000028`, Runtime Event v2 compatibility,
  Worker Protocol v2, SaaS Web projection, multi-replica recovery and deployment failure handling
- Result: PASS for repository-controlled, PostgreSQL 17, MinIO/S3-compatible and disposable Kind evidence
- External boundary: a real AWS S3 Live Store still requires an explicitly authorized writable test Bucket

This report supplements rather than overwrites the historical Stage 2 reports. It was created because the
previous production report was tied to an earlier 16-migration baseline and could not prove the current build.

## Evidence status after this baseline

The repository migration chain now reaches `000031_session_execution_cursor_lineage.sql`. This report remains
valid only for the immutable `1a53c93a`/`000028` source baseline: no complete current-HEAD rerun of
`deploy/saas/acceptance.sh`, `deploy/saas/multi-replica-acceptance.sh`,
`deploy/saas/failure-acceptance.sh` and `deploy/kubernetes/kind-acceptance.sh` is recorded here. Until a new report
pins the tested commit, expected/applied Migration `000031`, commands and cleanup evidence, this document must not
be used to claim current-HEAD deployment acceptance.

## Compatibility fixes found by `1a53c93a` baseline verification

The first baseline failure run exposed two stale acceptance assumptions introduced before Stage 3 hardened
the Worker boundary:

1. Acceptance Heartbeats omitted `protocolVersion: 2` and were rejected with
   `426 worker_protocol_version_unsupported`.
2. A replacement fake Worker completed an Execution without first reporting the logical Workspace ready and was
   rejected with `409 workspace_checkpoint_required`.

The Single-node, failure and Kind clients now send Worker Protocol v2 on every Heartbeat. The Single-node and
failure clients also mark the replacement Workspace ready before reporting completion. The expected Session Event
sequence assertions include the resulting `workspace.ready` Event.

## Go and database verification

Passed against the current worktree:

```bash
cd services/control-plane
go test -p 1 -count=1 ./...
go test -race -p 1 -count=1 ./...
```

A disposable PostgreSQL 17 server used four independent databases so general concurrency tests and destructive
migration fixtures could not pollute each other:

```bash
SYNARA_TEST_DATABASE_URL='postgres://.../synara_general?sslmode=disable' \
SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL='postgres://.../synara_stage3?sslmode=disable' \
SYNARA_TEST_WORKSPACE_CLEANUP_MIGRATION_DATABASE_URL='postgres://.../synara_workspace?sslmode=disable' \
SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL='postgres://.../synara_checkpoint?sslmode=disable' \
  go test -p 1 -count=1 ./...
```

This covered fresh migration through `000028`, existing Stage 3 state backfill, Workspace cleanup dispatch upgrade,
Checkpoint lifecycle upgrade, Interaction Runtime Event version backfill, PostgreSQL locking and multi-connection
concurrency semantics. The disposable database container was removed after the run.

## TypeScript and Web verification

Passed:

- Server Control Plane Proxy: 1 file / 7 tests; build passed.
- Web Control Plane client, Projection authority, Tenant scope, Turn dispatch, Session logic and ChatView:
  7 files / 254 tests; production build passed.
- Provider Host Protocol/Runtime Event v2: 2 files / 12 tests; build passed.
- Shared Provider Host/Runtime contracts: 2 files / 12 tests; declaration build passed.

Per repository policy, `bun fmt`, `bun lint` and `bun typecheck` were not run because the operator did not
explicitly request those heavyweight commands in this conversation.

## Runtime acceptance

All suites used isolated projects and removed their own containers, networks, volumes and Kind cluster.

### Single-node Compose

`deploy/saas/acceptance.sh` passed against a freshly built Control Plane with PostgreSQL 17 and MinIO. It covered
Tenant/Organization/Project/Session creation, ordered SSE replay, Worker Claim/Lease/Generation recovery,
Workspace readiness, Runtime Event append, Artifact validation/download, cross-Tenant isolation, Outbox and
Session archive.

### Two-replica Compose

`deploy/saas/multi-replica-acceptance.sh` passed with two Control Plane replicas. It covered cross-replica Login
revocation, SSE catch-up, unique Claim, concurrent idempotent Turns, replica loss and Last-Event-ID recovery. The
script now derives the latest checked-in migration version and requires both `expectedVersion` and `appliedVersion`
to equal it instead of accepting the historical Stage 2 minimum.

### Failure injection

`deploy/saas/failure-acceptance.sh` passed after the compatibility fixes above. It verified Worker-offline recovery,
Generation fencing, replacement completion, MinIO readiness degradation/recovery, PostgreSQL outage/recovery and
dynamic Credential/Token/Prompt/Presigned-URL log leakage scanning.

### Disposable Kind Kubernetes

```bash
KIND_BIN=/tmp/synara-stage2-bin/kind \
SYNARA_KIND_CLUSTER=synara-stage2-head-1a53c93 \
  deploy/kubernetes/kind-acceptance.sh
```

Passed with two Enterprise Control Plane replicas, PVC-backed PostgreSQL/MinIO, Pod deletion with continuous
readiness, Worker token continuity, database and object-store outage recovery, RBAC checks and Sentinel log scan.
The acceptance result reported `migrations=28`. The cluster was deleted afterward.

## Monitoring

The Prometheus Operator rule payload was extracted from the `PrometheusRule` CRD and checked with Prometheus 3.5
`promtool`: 14 rules passed, including Worker Offline Surge and Execution Recovery Surge.

## External AWS S3 boundary

MinIO exercises both the native MinIO profile and the S3-compatible adapter behavior, but it is not evidence for
AWS IAM, KMS, Bucket Policy, regional networking or the AWS service. Before an AWS-backed release, run the shared
`SYNARA_TEST_S3_*` Live Store suite against an explicitly authorized disposable Bucket and attach that evidence to
the release record.
