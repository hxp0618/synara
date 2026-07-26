# Stage 4 Generation Metric Rollup — OrbStack PostgreSQL E3

Date: 2026-07-26  
Evidence class: `E3 local-runtime`  
Kubernetes context: `orbstack`  
Temporary namespace: `synara-stage4-generation-rollup-pg-final1`

## Result

Migration `000081` passed on PostgreSQL 16.14 running as a real Pod in OrbStack Kubernetes. The test applied migrations
through `000080`, created terminal Generation and Pod-failure facts, applied `000081`, verified historical membership
backfill, and started two independent PostgreSQL connections against the same isolated schema. The two rollup services
processed exactly one Generation and one Pod-failure membership in total. Replay processed zero facts.

The test also gathered the projected trailing-30-day metrics and directly proved that PostgreSQL rejects:

- mutation of a sealed Generation metric source;
- regression of an aggregated sample count;
- deletion of a Generation membership cursor.

This is local E3 evidence. It does not prove managed-cloud topology, production data volume, multi-hour soak, or a
real cross-Region database failover.

## Runtime

```text
Kubernetes: v1.34.8+orb1
Node: orbstack
PostgreSQL: PostgreSQL 16.14 on aarch64-unknown-linux-musl
Image: postgres:16-alpine
Image digest: sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777
Port forward: 127.0.0.1:55484 -> service/postgres:5432
```

## Acceptance command

```bash
SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL='postgres://<local-test-user>@127.0.0.1:55484/synara_test?sslmode=disable' \
  go test ./internal/database \
  -run TestPostgresExecutionGenerationMetricRollupMigrationAndReplay \
  -count=1 -v
```

Final output:

```text
=== RUN   TestPostgresExecutionGenerationMetricRollupMigrationAndReplay
--- PASS: TestPostgresExecutionGenerationMetricRollupMigrationAndReplay (4.55s)
PASS
ok github.com/synara-ai/synara/services/control-plane/internal/database 6.398s
```

## Semantics exercised

1. `execution_generation_metric_rollup_entries` backfills only terminal facts with a non-null dispatch boundary.
2. `execution_generation_pod_failure_metric_rollup_entries` independently backfills immutable first-failure proofs.
3. The transaction advisory lock admits at most one rollup writer cycle across two database connections.
4. Categorical outcome/warm/failure counts and duration histogram buckets increment before membership completion in
   the same transaction.
5. Integer histogram bounds are generated without floating-point logarithms and have at most two-percent relative
   width plus one microsecond at the smallest values.
6. Metrics merge only complete UTC-day buckets inside the 30-day window. Both partial boundary days, nonterminal facts,
   and pending memberships remain raw inputs under a PostgreSQL `REPEATABLE READ` snapshot.
7. Exact replay returns the already committed graph without another increment.

Focused non-PostgreSQL tests also passed for histogram coverage, SQLite enqueue/source sealing, transaction rollback,
raw/rollup boundary merging, and package compilation:

```text
go test ./internal/metricfacts ./internal/metricrollup ./internal/observability ./internal/database ./cmd/api -count=1
```

The final repository-level Go regression also passed:

```text
go test ./... -count=1
PASS (all control-plane packages; slowest package agentd 24.313s)
```

Kubernetes deployment and resilience assets remained valid:

```text
bash deploy/kubernetes/validate-resilience-assets.sh
Ran 24 tests in 1.883s — OK
Python resilience validation passed
Kubernetes resilience assets validation passed
```

## Cleanup

The port-forward process was interrupted, namespace
`synara-stage4-generation-rollup-pg-final1` was deleted, a final namespace lookup returned no object, and no listener
remained on TCP port `55484`.

## Source hashes

| Source                                                                               | SHA-256                                                            |
| ------------------------------------------------------------------------------------ | ------------------------------------------------------------------ |
| `migrations/000081_execution_generation_metric_rollups.sql`                          | `6ea018c6e230b1460841fab232fc6b06064e55693d35f4cb932cc8e159bb1cab` |
| `internal/metricfacts/generation.go`                                                 | `60e3fa8449cca2ddb25b94f5628ccae0d2facb0d01d5bb6c14253668143bbd04` |
| `internal/metricrollup/service.go`                                                   | `8f1942f18a9ebe715c8acd474833b21de18a084d68db1f0a25798458ebeecdef` |
| `internal/observability/generation_fact_metrics.go`                                  | `4e8308d1d4f7443ea644c44182f64d9dc0d5bbea53df5ed1cad28efee87a1945` |
| `internal/database/execution_generation_metric_rollup_sqlite.go`                     | `de9fd7b18b6f2a32cc8974fb3e1dbf15dcabf4cc7ccab01ad4579d665a709371` |
| `internal/database/execution_generation_metric_rollup_migration_integration_test.go` | `3120e140a31396e95073d2cf4df29caa28aa685d91f8587a9d03c7049b5b68f5` |
| `internal/persistence/metric_rollup_models.go`                                       | `1f7cc6935d3ee48471a8b59bd68b337887e8d2cea300989b747de985e412c4ce` |
| `cmd/api/main.go`                                                                    | `1a87d81e6f19c8a28625b79dd5a99700d7a2dae2a296d03b039bffeac65c270b` |
