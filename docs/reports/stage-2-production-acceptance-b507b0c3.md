# Stage 2 Production Acceptance Report — `b507b0c3`

- Date: 2026-07-15
- Branch: `codex/saas-tenancy-user`
- Commit: `b507b0c386c782ea9a7ec1d8cf3e3e1bf6a8109c`
- Verification worktree: clean detached worktree at `/private/tmp/synara-stage2-b507b0c3`, removed after acceptance
- Schema: `000031_session_execution_cursor_lineage.sql`
- Result: **PASS** — all repository-controlled Stage 2 verification gates below passed
- External boundary: real AWS S3 remains untested without an explicitly authorized writable Bucket

This is the latest fixed Stage 2 evidence record. It supersedes
`stage-2-production-acceptance-0c42b0ec.md` as the current pointer without rewriting or invalidating any historical
report. The `PASS` result covers repository-controlled DDL, test, build, browser, Compose, failure-injection and
Kubernetes evidence. It does not convert the explicitly unexecuted real AWS S3 boundary into tested evidence.

## Schema and DDL evidence

The checked-in production schema remained a forward-only chain of exactly 31 migrations, from
`000001_identity_tenancy.sql` through `000031_session_execution_cursor_lineage.sql`. No historical migration was
rewritten. The latest three DDL files retained these SHA-256 digests:

```text
21f362c91fc969d389c56c16c0e800e13868f1a32648ecd8019aa4edda673ef1  000029_provider_runtime_release_policy.sql
b9bdde06087247d53bc838024640a9a39a9189d90d30a6056437bdbd9ec6f1fc  000030_execution_provider_cursor_snapshots.sql
d8a8895a46cb4c40715bb1d1cc291c62f2452a5fc19a528b482d68eafddf6eda  000031_session_execution_cursor_lineage.sql
```

The Single-node and Multi-replica runs used the repository migration loader and reported Migration `31`. The
authoritative Single-node PostgreSQL query returned `31|31` for `count(*)|max(version)`.

## TypeScript tests and production builds

Focused Stage 2 TypeScript suites passed without changing the tracked tree:

| Package/area  | Test files | Tests |
| ------------- | ---------: | ----: |
| Server Proxy  |          1 |     7 |
| Web Stage 2   |         13 |   205 |
| Provider Host |          6 |    64 |
| Contracts     |         12 |   122 |
| **Total**     |     **32** | **398** |

Production builds passed for Contracts, Server, Web and Provider Host. Output contained only non-blocking
deprecation, plugin-timing and chunk-size warnings; there was no build failure.

## Go, Race and PostgreSQL 17 audit

The Go Control Plane was tested from the detached `b507b0c3` worktree with serialized package execution:

```bash
cd /private/tmp/synara-stage2-b507b0c3/services/control-plane
go test -p 1 -count=1 ./...
go test -race -p 1 -count=1 ./...
```

Both commands passed with 33 tested packages, 6 packages without test files and 0 failed packages. The default run
took 56.859 seconds; the Race run took 158.401 seconds. Repository discovery found 446 top-level Go tests.

The PostgreSQL 17 integration run used the exact container
`synara-stage2-b507b0c3-pgtests`, loopback port `32772`, and four independent databases:

```text
synara_b507b0c3_general
synara_b507b0c3_stage3
synara_b507b0c3_workspace
synara_b507b0c3_checkpoint
```

The four database URLs were supplied through `SYNARA_TEST_DATABASE_URL`,
`SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL`,
`SYNARA_TEST_WORKSPACE_CLEANUP_MIGRATION_DATABASE_URL` and
`SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL`, followed by:

```bash
go test -p 1 -count=1 ./...
```

That run passed the same 33 tested packages plus 6 no-test packages in 66.588 seconds. General, Stage 3 and
Checkpoint each reported Migration `31|31`. The Workspace fixture deliberately uses isolated schemas rather than a
public Migration table; all of its isolated schemas were removed. All four databases ended with only the `public`
schema.

Cleanup removed the exact PostgreSQL container, its random-password and container-ID files, and the detached test
worktree registration. The protected `synara-model-switch-1784023952087` Compose project remained healthy and the
pre-existing `synara-stage3-driver` Docker node remained running. No global Docker or Network prune was used. The
shared worktree contained only the intentional acceptance-document edits being prepared concurrently; the Go audit
did not modify or stage repository files.

## Protocol version boundary

The current managed path uses Worker Protocol v2 and Runtime Event v2. Worker Protocol v1 is a legacy contract,
and Runtime Event v1 remains only for the explicit Provider Host v1 compatibility path and replay of existing
Events. The browser recovery fixture below intentionally submitted the legacy-compatible
`runtime.output.delta` name to verify that the Web projection still replays both vocabularies; it is not evidence
of a managed-path downgrade. A managed Provider Host v2 emits canonical `content.delta`.

## Browser SSE stop/restart recovery

The real Web UI was exercised at
`http://127.0.0.1:55130/d5062fd4-fc7b-49c5-805e-ccedb488a453` with:

```text
Web image:    synara-saas:stage2-b507b0c3
Web image ID: sha256:95741cb6bbb3356c4f96ec73f94cc82f609821e4128fbf676bad1b87de5895c2
```

Verified behavior:

1. With the SSE open, the reconnect banner was non-interactive and collapsed (`aria-hidden=true`, `inert=true`,
   height `0px`, opacity `0`).
2. Stopping the Control Plane expanded the banner to `100px`/opacity `1` and displayed
   `Reconnecting to Session Events`; the Session was not incorrectly projected as Completed.
3. Restarting the same Control Plane container did not hide the banner early. It collapsed only after the Event
   Stream reopened.
4. A temporary Worker claimed Generation 3 and sent
   `workspace.ready -> execution.started -> runtime.output.delta -> execution.completed`.
5. The live transcript rendered the new output exactly once:

   ```text
   Stage 2 SSE live after Control Plane restart on b507b0c3.
   ```

6. Refresh restored both the earlier and newly streamed output from PostgreSQL.
7. PostgreSQL contained 17 Events with 17 distinct Sequences and Event IDs, Sequence `1..17`, `gap_free=true`, a
   completed Execution and `generation=3`.
8. The browser console had no Warning or Error entries related to the flow.

## Isolated local SQLite browser path

The local path used the repository-required Node 24 runtime (`v24.14.0`), an isolated Home and non-default ports.
The successful dry-run resolved Server `58090`, Web `8891` and a dedicated base directory before any process was
started:

```bash
PATH=/Users/huang/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin:$PATH \
  env -u SYNARA_AUTH_TOKEN \
  SYNARA_PORT_OFFSET=3158 \
  SYNARA_NO_BROWSER=1 \
  bun run dev -- \
  --home-dir ./.synara-stage2-b507-local \
  --port 58090 \
  --dry-run
```

The live instance used:

```bash
PATH=/Users/huang/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin:$PATH \
  env -u SYNARA_AUTH_TOKEN \
  SYNARA_PORT_OFFSET=3158 \
  SYNARA_NO_BROWSER=1 \
  SYNARA_AUTO_BOOTSTRAP_PROJECT_FROM_CWD=0 \
  bun run dev -- \
  --home-dir ./.synara-stage2-b507-local-ui \
  --port 58090
```

The in-app browser opened `http://localhost:8891/` and verified there was no Login or Control Plane gate. Through
the real sidebar UI it created Project `synara-stage2-b507b0c3` for
`/private/tmp/synara-stage2-b507b0c3`, created Thread
`b3cdb827-a61f-4265-a669-fd2d5a888a0e`, and renamed it to
`Stage 2 Local SQLite Acceptance` without starting a Provider turn.

The isolated SQLite Snapshot contained the Project row, the Thread row with `runtime_mode=full-access`,
`interaction_mode=default` and `env_mode=local`, plus 4 gap-free orchestration Events at Sequences `1..4`. Reloading
the same route restored both the Project and Thread title from SQLite. The local-only reconnect banner remained
properly collapsed (`aria-hidden=true`, `inert=true`, height `0`, opacity `0`), and the browser Console contained no
Warning or Error entries.

Cleanup finalized every acceptance browser tab, stopped the exact dev process, released ports `58090` and `8891`,
removed `.synara-stage2-b507-local-ui`, and removed the recreated detached worktree path and Git worktree
registration. No default Synara Home, port or running instance was reused.

## Single-node Compose

```text
Compose project:        synara-stage2-single-b507b0c3-20260715
Isolated host ports:    55140 / 55141 / 55142
Migration table:        31|31
Web image ID:           sha256:95741cb6bbb3356c4f96ec73f94cc82f609821e4128fbf676bad1b87de5895c2
Control Plane image ID: sha256:7e99693972587ca36fb9ac25160069638c727af6557b1bfff212457ff670e6fd
```

`deploy/saas/acceptance.sh` passed through the published Web `/v1` proxy. It verified the same-origin frontend to
Control Plane route, Tenant/Organization/Project/Session creation, SSE, Worker registration and Lease recovery,
Runtime Events, ready Workspace completion, Artifact upload/download and server-side hash verification, isolation,
membership, Audit and published Outbox topics.

## Multi-replica Compose

```text
Compose project: synara-stage2-multi-b507b0c3-20260715
Migration:       31
Result:          PASS
```

`deploy/saas/multi-replica-acceptance.sh` passed concurrent startup under the Migration lock, cross-replica SSE
and catch-up, exactly one legal Worker Claim, cross-replica Login revocation and continued service after replica
loss. The suite itself printed `PASS` and exited successfully. A surrounding zsh helper subsequently attempted to
assign to zsh's readonly `status` parameter; that wrapper-only error did not change the suite result.

## Failure injection

```text
Compose project:     synara-stage2-failure-b507b0c3-20260715
Isolated host ports: 55161 / 55162
Result:              PASS
```

`deploy/saas/failure-acceptance.sh` passed Worker loss, Lease expiry, recovery and Generation fencing; MinIO
outage/readiness recovery; PostgreSQL outage/readiness recovery with persisted state; and random sensitive-log
Sentinel scans. No Credential, Token, Prompt or Presigned-URL sentinel was found in the audited logs.

## Disposable Kind Kubernetes

```text
Cluster:        synara-stage2-b507b0c3-20260715
Image:          synara-control-plane:stage2-b507b0c3-20260715
Image ID:       sha256:3c369d27d7f99e79b2416fa98ecd57b1f88ddfcc436a15eecd6ce238e69172d1
Ready replicas: 2
Migration:      31
Result:         PASS
```

The disposable Kind suite passed two Enterprise Control Plane replicas, Migration `31`, Control Plane Pod
deletion, existing Worker Token continuity, PVC-backed PostgreSQL and MinIO outage/recovery, reconciler RBAC and
sensitive-log Sentinels.

The exact Stage 2 cluster was deleted. The pre-existing `synara-stage3-driver` cluster was not reused, modified or
deleted, and the `kubectl` context was restored to `orbstack`.

## Cleanup evidence

All exact Stage 2 Compose projects reported zero remaining containers, networks and volumes:

```text
synara-stage2-single-b507b0c3-20260715   containers=0 networks=0 volumes=0
synara-stage2-multi-b507b0c3-20260715    containers=0 networks=0 volumes=0
synara-stage2-failure-b507b0c3-20260715  containers=0 networks=0 volumes=0
```

The disposable Stage 2 Kind cluster and its context were absent after cleanup. Cleanup was scoped to the exact
project and cluster names; no global Docker, Network or Volume prune was used.

The PostgreSQL 17 audit container and four disposable databases were removed. The isolated local SQLite process,
state directory and ports were also absent, and `/private/tmp/synara-stage2-b507b0c3` was absent from both the
filesystem and `git worktree list` after the final browser refresh.

## External AWS S3 boundary

MinIO was exercised through the repository's S3-compatible paths. This is not evidence for AWS IAM, KMS, Bucket
Policy, regional networking or the AWS service. A real AWS S3 Live Store run was not executed because no explicitly
authorized writable Bucket was supplied.

Before an AWS-backed release, run the shared `SYNARA_TEST_S3_*` suite against an authorized disposable Bucket and
attach that separate evidence. MinIO success must never be presented as proof of AWS behavior.
