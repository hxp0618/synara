# Stage 2 Production Acceptance Report — `acf63b43`

- Date: 2026-07-14
- Branch: `codex/saas-tenancy-user`
- Commit: `acf63b432fa27dd5e6d632f23ce2105d940a1519`
- Worktree at verification start: clean and aligned with `origin/codex/saas-tenancy-user`
- Schema: `000031_session_execution_cursor_lineage.sql`
- Result: PASS for repository-controlled PostgreSQL 17, MinIO/S3-compatible, Compose and disposable Kind evidence
- External boundary: real AWS S3 remains untested without an explicitly authorized writable Bucket

This report supplements the historical `1a53c93a`/Migration `000028` report. It fixes the four Stage 2 deployment
suites to one immutable current baseline after migrations `000029` through `000031` were added.

## Schema and DDL evidence

The checked-in forward-only chain contained exactly 31 migrations. The new DDL files after the previous report
were preserved unchanged and had these SHA-256 digests:

```text
21f362c91fc969d389c56c16c0e800e13868f1a32648ecd8019aa4edda673ef1  000029_provider_runtime_release_policy.sql
b9bdde06087247d53bc838024640a9a39a9189d90d30a6056437bdbd9ec6f1fc  000030_execution_provider_cursor_snapshots.sql
d8a8895a46cb4c40715bb1d1cc291c62f2452a5fc19a528b482d68eafddf6eda  000031_session_execution_cursor_lineage.sql
```

Every PostgreSQL startup ran the repository migration loader, which rejects any previously applied checksum that
differs from the checked-in file. Single-node readiness returned `expectedVersion=31` and `appliedVersion=31`.
The authoritative table query returned `31|31` for `count(*)|max(version)`.

## Single-node Compose

The final isolated run used:

```text
Compose project: synara-stage2-single-acf63b43-1783999626
Web:             127.0.0.1:63238
MinIO:           127.0.0.1:63239
Web image:       synara-saas:stage2-acf63b43
Web image ID:    sha256:619a71f295a978920229e22d7c9181407540bfc94ac39e46649971105e6120b6
```

Random per-run PostgreSQL, MinIO, Web auth, Worker registration, Provider Cursor and Credential KMS values were
supplied through the environment and were not written to this report. The Acceptance Worker registration token
was explicitly bound to the same value as the Control Plane registration token.

The Web image was built from the same current worktree immediately before the immutable commit and then tagged
with the baseline name. The only source delta between that image build and `acf63b43` was the Stage 3 Python
acceptance harness and its tests; no Web, Server, Provider Host, package or Dockerfile runtime input changed. A
fresh tagged Web rebuild was attempted first, but Docker Hub returned a metadata transport failure for
`node:24-bookworm`:

```text
short read: expected 6741 bytes but got 0: unexpected EOF
```

That network-only attempt created no containers, networks or volumes. The final proof therefore rebuilt the Go
Control Plane from `acf63b43`, used the already built Web image by immutable image ID, and started Compose with
`--no-build` for the Web service.

`deploy/saas/acceptance.sh` passed through the published Web `/v1` proxy. It verified Dev Login,
Tenant/Organization/Project/Session creation, SSE, Worker registration and Lease recovery, Runtime Events,
Workspace readiness, Artifact upload/download and hash verification, isolation, membership, Audit and published
Outbox topics. This is the frontend-to-backend connectivity proof; the browser-facing Web process did not bypass
the same-origin Control Plane proxy.

The schema assertion was made from the `synara` container against `http://control-plane:3780/ready`, not against
the Web `/ready` endpoint. It returned `31/31`, and PostgreSQL returned `31|31`.

## Multi-replica Compose

Command shape:

```bash
SYNARA_MULTI_REPLICA_PROJECT=synara-stage2-multi-acf63b43-1783999188 \
  deploy/saas/multi-replica-acceptance.sh
```

The script derived Migration `31` from the checked-in DDL and required both replicas to report
`expectedVersion=31` and `appliedVersion=31`. It passed two healthy replicas, concurrent startup under one
Migration advisory-lock boundary, cross-replica SSE and `Last-Event-ID` catch-up, one legal Worker Claim,
sequential Turn ownership, cross-replica Login revocation, global SSE leases and continued service after one
replica was stopped.

Reported replicas:

```text
synara-stage2-multi-acf63b43-1783999188-control-plane-1
synara-stage2-multi-acf63b43-1783999188-control-plane-2
```

## Failure injection

```bash
SYNARA_FAILURE_ACCEPTANCE_PROJECT=synara-stage2-failure-acf63b43-1783999218 \
  deploy/saas/failure-acceptance.sh
```

The isolated script selected unused loopback ports and passed Worker heartbeat loss, Lease expiry,
`execution.recovering`, Generation 2 takeover, stale Generation fencing, MinIO outage/recovery, PostgreSQL
outage/recovery, retained Login Session and Worker Token continuity, and random Credential/Token/Cursor/Lease/
Prompt/Presigned-URL sentinel scans.

## Disposable Kind Kubernetes

```bash
KIND_BIN="$PWD/.tmp/bin/kind" \
SYNARA_KIND_CLUSTER=synara-stage2-acf63b43-1783999277 \
SYNARA_K8S_ACCEPTANCE_IMAGE=synara-control-plane:stage2-acf63b43 \
  deploy/kubernetes/kind-acceptance.sh
```

The disposable Kubernetes v1.33.1 cluster passed with two Enterprise Control Plane replicas and Migration `31`.
It verified PVC-backed PostgreSQL and MinIO, Bucket initialization, Control Plane Pod deletion without readiness
interruption, existing Worker Token continuity through replacement, PostgreSQL and MinIO outage/readiness
recovery, reconciler RBAC and sensitive-log sentinels.

The Stage 2 cluster was deleted by the wrapper. The pre-existing `synara-stage3-driver` Kind cluster remained the
only Kind cluster and was not reused or modified.

## Cleanup evidence

After the runs, all three exact Compose projects reported zero containers, zero networks and zero volumes:

```text
synara-stage2-single-acf63b43-1783999626   containers=0 networks=0 volumes=0
synara-stage2-multi-acf63b43-1783999188   containers=0 networks=0 volumes=0
synara-stage2-failure-acf63b43-1783999218 containers=0 networks=0 volumes=0
```

The exact Stage 2 Kind cluster and its kubeconfig context were absent after cleanup. Temporary Workspaces and
generated acceptance state were removed by their traps. Rebuildable local image tags are not production state.

## External AWS S3 boundary

MinIO was exercised through the repository's S3-compatible paths. This does not prove AWS IAM, KMS, Bucket
Policy, regional networking or the AWS service. Before an AWS-backed release, run the shared
`SYNARA_TEST_S3_*` Live Store suite against an explicitly authorized disposable Bucket and attach that separate
evidence. The missing AWS authorization does not invalidate this repository-controlled Stage 2 baseline, but the
AWS claim remains explicitly open.
