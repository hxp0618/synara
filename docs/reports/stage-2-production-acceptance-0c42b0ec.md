# Stage 2 Production Acceptance Report — `0c42b0ec`

- Date: 2026-07-14
- Branch: `codex/saas-tenancy-user`
- Commit: `0c42b0ec2aa948e023abb47e2d81c0b84c528612`
- Worktree at verification start: clean and aligned with `origin/codex/saas-tenancy-user`
- Schema: `000031_session_execution_cursor_lineage.sql`
- Result: PASS for repository-controlled PostgreSQL 17, MinIO/S3-compatible, Compose and disposable Kind evidence
- External boundary: real AWS S3 remains untested without an explicitly authorized writable Bucket

This report supersedes `stage-2-production-acceptance-acf63b43.md` as the latest fixed Stage 2 deployment
baseline. It re-runs all four deployment suites after the Session/Execution capability-projection changes through
`d430fd7a`; `0c42b0ec` only adds repository ignore hardening after that runtime commit. The Control Plane image
was cache-reused from the post-`d430fd7a` build because the Control Plane build context is identical at
`0c42b0ec`; the Web image was rebuilt after `0c42b0ec`.

## Schema and DDL evidence

The checked-in forward-only chain contained exactly 31 migrations. The latest three DDL files retained the
following SHA-256 digests:

```text
21f362c91fc969d389c56c16c0e800e13868f1a32648ecd8019aa4edda673ef1  000029_provider_runtime_release_policy.sql
b9bdde06087247d53bc838024640a9a39a9189d90d30a6056437bdbd9ec6f1fc  000030_execution_provider_cursor_snapshots.sql
d8a8895a46cb4c40715bb1d1cc291c62f2452a5fc19a528b482d68eafddf6eda  000031_session_execution_cursor_lineage.sql
```

Every PostgreSQL startup used the repository migration loader, which validates the checksum of every previously
applied migration. Single-node readiness returned `expectedVersion=31` and `appliedVersion=31`. The authoritative
`public.control_plane_schema_migrations` query returned `31|31` for `count(*)|max(version)`.

## Single-node Compose

The successful isolated run used:

```text
Compose project:       synara-stage2-single-0c42b0ec-1784019871
Web:                   127.0.0.1:61038
MinIO:                 127.0.0.1:62785
Web image:             synara-saas:stage2-0c42b0ec-1784019461
Web image ID:          sha256:277a813c1b074d25204ff2f0c6b896da14e66b71d81195679d3fcd04924b72e0
Control Plane image ID: sha256:43b3658c835482ffcdb3d38134b275ba5791435c08c6c129d11a961be705a2f1
```

Random per-run PostgreSQL, MinIO, Web auth, Worker registration, Provider Cursor and Credential KMS values were
supplied only through the process environment. The Acceptance Worker registration token was bound to the same
value as the Control Plane registration token.

`deploy/saas/acceptance.sh` passed through the published Web `/v1` proxy. It verified Dev Login,
Tenant/Organization/Project/Session creation, SSE, Worker registration and Lease recovery, Runtime Events,
Workspace readiness, Artifact upload/download and hash verification, tenant isolation, membership, Audit and
published Outbox topics. This proves the browser-facing Web server's same-origin `/v1` proxy connectivity to the
Control Plane; the request path did not bypass that proxy. Browser rendering and hydration remain covered by the
separate Stage 2 browser acceptance evidence.

The direct Control Plane `/ready` response was `ready`, with the PostgreSQL schema dependency reporting
`expectedVersion=31` and `appliedVersion=31`. PostgreSQL independently returned `31|31` from
`control_plane_schema_migrations`.

## Multi-replica Compose

```text
Compose project: synara-stage2-multi-0c42b0ec-1784019915
MinIO:           127.0.0.1:62943
Replicas:        synara-stage2-multi-0c42b0ec-1784019915-control-plane-1
                 synara-stage2-multi-0c42b0ec-1784019915-control-plane-2
Migration:       31
```

`deploy/saas/multi-replica-acceptance.sh` passed concurrent startup under one migration advisory lock,
cross-replica SSE and `Last-Event-ID` catch-up, one legal Worker Claim, sequential Turn ownership, cross-replica
Login revocation, global SSE leases and continued service after one replica was stopped.

## Failure injection

```text
Compose project:    synara-stage2-failure-0c42b0ec-1784019955
Control Plane port: 62647
MinIO port:         64394
```

`deploy/saas/failure-acceptance.sh` passed Worker heartbeat loss, Lease expiry, `execution.recovering`, Generation
2 takeover, stale Generation fencing, MinIO outage/recovery, PostgreSQL outage/recovery, retained Login Session
and Worker Token continuity, plus random Credential/Token/Cursor/Lease/Prompt/Presigned-URL sentinel scans.

## Disposable Kind Kubernetes

```text
Cluster:          synara-stage2-0c42b0ec-1784020024
Context:          kind-synara-stage2-0c42b0ec-1784020024
Image:            synara-control-plane:stage2-0c42b0ec-1784020024
Image ID:         sha256:41d202292a8bc25d5ded80f2555d4dcf1f70723b1c82e1b30c77014af344906f
Ready replicas:   2
Registered worker: a1588349-653c-4287-b8b5-43e62af724b9
Migration:        31
```

The disposable Kubernetes v1.33.1 cluster passed PVC-backed PostgreSQL and MinIO, Bucket initialization, two
Enterprise Control Plane replicas, Control Plane Pod deletion without readiness interruption, existing Worker
Token continuity through replacement, PostgreSQL and MinIO outage/readiness recovery, reconciler RBAC and
sensitive-log sentinels.

The wrapper deleted this Stage 2 cluster. The pre-existing `synara-stage3-driver` Kind cluster remained present
and was not reused, modified or deleted.

## Cleanup evidence

After the runs, all exact Compose projects reported zero containers, zero networks and zero volumes:

```text
synara-stage2-single-0c42b0ec-1784019871   containers=0 networks=0 volumes=0
synara-stage2-multi-0c42b0ec-1784019915    containers=0 networks=0 volumes=0
synara-stage2-failure-0c42b0ec-1784019955  containers=0 networks=0 volumes=0
```

The exact Stage 2 Kind cluster and its kubeconfig context were absent after cleanup. Temporary Workspaces and
generated acceptance state were removed by their traps. Rebuildable local image tags are not production state.

## External AWS S3 boundary

MinIO was exercised through the repository's S3-compatible paths. This does not prove AWS IAM, KMS, Bucket
Policy, regional networking or the AWS service. Before an AWS-backed release, run the shared `SYNARA_TEST_S3_*`
Live Store suite against an explicitly authorized disposable Bucket and attach that separate evidence. The
missing AWS authorization does not invalidate this repository-controlled Stage 2 baseline, but the AWS claim
remains explicitly open.
