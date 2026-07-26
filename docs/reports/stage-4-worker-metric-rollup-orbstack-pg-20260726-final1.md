# Stage 4 terminal Worker metric rollup — OrbStack/PostgreSQL acceptance (final1)

## Result

Passed on 2026-07-26 against PostgreSQL 16.14 running in the real local OrbStack Kubernetes API.

This is `E3 local-runtime` evidence. It proves Migration `000079`, historical terminal-fact backfill, new terminal-fact
enqueue, exact daily aggregation, transaction rollback/replay, PostgreSQL metric projection, bounded database fences,
leader/session advisory-lock exclusion, transaction-lock handoff, and single-connection operation in the local
environment. It does not prove a production-duration soak, managed-cloud PostgreSQL behavior, or the still-pending
mergeable rollup for Generation P50/P95/P99 metrics.

The tested checkout was branch `codex/saas-tenancy-user` at HEAD
`260dd2d4465e565c5cd43ed0f26dd59d50b2c0ab`. The Worker rollup implementation and this report were local working-tree
changes on top of that commit. They were not committed or pushed by this gate.

## Environment

- Kubernetes context: explicit `--context orbstack` on every command
- Kubernetes server: `v1.34.8+orb1`, one local OrbStack node
- PostgreSQL namespace: `synara-stage4-metric-rollup-pg-final1`
- PostgreSQL version: `PostgreSQL 16.14`, Alpine/aarch64
- PostgreSQL image ID:
  `postgres@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777`
- Database exposure: temporary localhost port-forward `55482 -> 5432`
- Installed schema evidence: migration `79|worker_incarnation_metric_rollups`, tables
  `worker_incarnation_metric_rollups` and `worker_incarnation_metric_rollup_entries`
- Cleanup: port-forward stopped, namespace deleted, final namespace lookup returned Kubernetes `NotFound`, and port
  `55482` had no listener

## Durable authority and exactness

Migration `000079_worker_incarnation_metric_rollups.sql` adds two authorities:

1. `worker_incarnation_metric_rollup_entries` is append-only membership keyed by
   `(worker_id, worker_incarnation)`. A terminal fact can have only one entry, its fact identity/bucket cannot change,
   and a completed entry cannot return to pending or be deleted.
2. `worker_incarnation_metric_rollups` stores additive UTC-day totals keyed only by bounded Target kind, Pool mode, and
   capacity class. Identity is immutable, counters cannot regress, rows cannot be deleted, and NaN/Infinity totals are
   rejected.

The rollup service takes `synara:worker-incarnation-metric-rollup-cycle` with
`pg_try_advisory_xact_lock`, locks a bounded batch of pending entries, increments daily buckets, and sets each selected
entry's `rolled_up_at` in the same transaction. A crash or injected write failure rolls back both sides. A retry sees
the original pending entry; a committed entry is never selected again.

The API runs this service under the separate leader lease `synara:metric-rollup`. The internal transaction lock remains
a second exactness fence for direct calls or leader-transition overlap. `SYNARA_METRIC_ROLLUP_INTERVAL` defaults to
`1m`; `SYNARA_METRIC_ROLLUP_BATCH_SIZE` defaults to `500` and is bounded to `1..10000`.

## Scrape consistency

Worker metric projection reads a PostgreSQL `REPEATABLE READ`, read-only snapshot and combines:

- SQL-aggregated daily terminal rollups;
- terminal facts whose membership entry is still pending; and
- authoritative nonterminal facts.

This ordering is intentionally snapshot-bound. If a rollup commits during a scrape, the scrape sees either the old
bucket plus the pending raw fact or the new bucket plus the completed entry; it cannot see neither or both. A delayed
scheduler therefore increases `synara_metric_rollup_pending_facts{kind="worker-incarnation"}` and raw-tail work but
does not change the reported total. `synara_metric_rollup_buckets{kind="worker-incarnation"}` exposes retained daily
bucket inventory. A unit fixture also verified that two UTC-day rows with the same bounded dimensions are aggregated in
SQL before projection.

## PostgreSQL migration, replay, and metric gate

The isolated migration test started at schema 75, inserted a terminal Worker fact, then applied migrations 78 and 79.
It verified historical backfill, one exact daily bucket, no-op replay, post-migration trigger enqueue, cumulative totals,
the PostgreSQL scrape path, counter-regression rejection, non-finite-value rejection, and entry-delete rejection:

```text
=== RUN   TestPostgresWorkerIncarnationMetricRollupMigrationAndReplay
--- PASS: TestPostgresWorkerIncarnationMetricRollupMigrationAndReplay (4.60s)
PASS
```

The asserted PostgreSQL metric output included:

```text
synara_worker_incarnation_facts{capacity_class="unassigned",mode="unassigned",state="terminated",target_kind="local"} 2
synara_worker_incarnation_run_seconds{capacity_class="unassigned",mode="unassigned",state="terminated",target_kind="local"} 20
synara_metric_rollup_pending_facts{kind="worker-incarnation"} 0
synara_metric_rollup_buckets{kind="worker-incarnation"} 1
```

## Leadership and connection-pool gate

The first PostgreSQL run exposed a real defect: a session advisory lock occupied the only connection in the isolated
test pool, while the rollup transaction waited forever for a second connection. The service was changed to acquire a
transaction advisory lock inside the write transaction. The same one-connection migration test then completed, and a
separate multi-connection PostgreSQL gate proved both outer leader-lock exclusion and transaction-lock release/takeover:

```text
=== RUN   TestPostgresBackgroundAdvisoryLocksAllowOnlyOneReplica/synara:metric-rollup
--- PASS: TestPostgresBackgroundAdvisoryLocksAllowOnlyOneReplica/synara:metric-rollup
=== RUN   TestPostgresTransactionAdvisoryLockReleasesWithRollupTransaction
--- PASS: TestPostgresTransactionAdvisoryLockReleasesWithRollupTransaction
PASS
```

## Repository verification

- `go test ./... -count=1` under `services/control-plane`: passed.
- Focused SQLite tests proved backfill, exact replay, repeated safety migration without requeue, rollback after injected
  entry-completion failure, immutable rollup identity/totals, and delete fences.
- `bash deploy/kubernetes/validate-resilience-assets.sh`: passed, including 24 managed-hook tests, Python validation,
  Kustomize rendering, and Kubernetes asset checks.
- `git diff --check`: passed after the report and contract changes.
- No `bun fmt`, `bun lint`, `bun typecheck`, or `bun test` command was run.

## Source binding

| Source                                                                             | SHA-256                                                            |
| ---------------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| `migrations/000079_worker_incarnation_metric_rollups.sql`                          | `35ef69b178b478077a60347dcb0b23e64bd40fb0ea00b3be3b93215b89645e4a` |
| `internal/metricrollup/service.go`                                                 | `a9d419c3f820f046ac29a1003a91a0dcc4665d57c0277f6ad1d22d99d9b90246` |
| `internal/metricfacts/worker_incarnation.go`                                       | `0c376733be7eae903b0f3067f58081c84115dd87b4285604040588f17d2677cc` |
| `internal/observability/worker_incarnation_fact_metrics.go`                        | `fdbda52a0b2ac5322e32fddadcc355b9903fc0ea741f4ea692afed340f1e5d3d` |
| `internal/database/worker_incarnation_metric_rollup_migration_integration_test.go` | `c23b66775e73bb76c57d4abceb2669d403b59e9d74206a4db4de30fd0355989e` |
| `internal/persistence/metric_rollup_models.go`                                     | `f0b16fb2ba43b61592a861d9fac78892f3d7793a6df230c97188fd3009602f5a` |
| `internal/config/config.go`                                                        | `5f1ef27a4d015e8bf1513efa39132734e40b0eb36744b40131ea0221e254d70c` |
| `cmd/api/main.go`                                                                  | `e6a9507809ba37424f0a7cf3bd2b6da176a7df0b119637feef101e6e2562ccd9` |

## Remaining gates

- Add a mergeable long-retention distribution for trailing-30-day Generation cold-start, queue, Pod provisioning, and
  outcome P50/P95/P99. Summing percentile buckets is not acceptable.
- Run multi-replica Control Plane rollup/recovery under production-duration load and chaos; the local lock handoff gate
  is deterministic E3 evidence, not an E4 soak.
- Managed-cloud PostgreSQL, multi-zone behavior, provider billing provenance, Workload Identity, and production SLOs
  remain separate E4 acceptance gates.
