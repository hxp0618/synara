# Stage 4 billing calendar scheduler — OrbStack/PostgreSQL acceptance (final1)

## Result

Passed on 2026-07-26 against PostgreSQL 16.14 running in the real local OrbStack Kubernetes API.

This is `E3 local-runtime` evidence. It proves Migration `000080`, strict static/monthly configuration, exact UTC-month
generation, durable due/claim/outcome state, restart throttle, leader handoff, two-replica claim exclusion, PostgreSQL
metric projection, and database mutation fences in the local environment. It does not prove AWS/GCP/Azure export
provenance, cloud Workload Identity, account-level actual-invoice allocation, or production-duration billing operation.

The tested checkout was branch `codex/saas-tenancy-user` at HEAD
`260dd2d4465e565c5cd43ed0f26dd59d50b2c0ab`. The calendar scheduler implementation and this report were local
working-tree changes on top of that commit. They were not committed or pushed by this gate.

## Environment

- Kubernetes context: explicit `--context orbstack` on every command
- Kubernetes server: `v1.34.8+orb1`, one local OrbStack node
- PostgreSQL namespace: `synara-stage4-billing-calendar-pg-final1`
- PostgreSQL version: `PostgreSQL 16.14`, Alpine/aarch64
- PostgreSQL image ID:
  `postgres@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777`
- Database exposure: temporary localhost port-forward `55483 -> 5432`
- Installed schema evidence: migration `80|billing_shared_allocation_calendar_schedules`, table
  `billing_shared_allocation_schedule_periods`, and trigger
  `trg_billing_shared_allocation_schedule_periods_immutable`
- Cleanup: port-forward stopped, namespace deleted, final namespace lookup returned Kubernetes `NotFound`, and port
  `55483` had no listener

## Configuration contract

`SYNARA_BILLING_SHARED_ALLOCATION_MAPPINGS_JSON` now accepts exactly two variants:

1. A static mapping supplies `billingPeriodStartAt` and `billingPeriodEndAt` as exact RFC3339 half-open bounds.
2. A calendar mapping supplies `calendar: "monthly-utc"`, an exact `firstPeriodStartAt` UTC month boundary, and an
   optional `lastPeriodEndAt` UTC month boundary.

Both variants require one Target, provider, currency, `settlementDelay`, and `scheduleInterval`. Durations use whole
seconds; settlement is bounded to `1m..2160h` and retry is at least `1m`. A local-zone month boundary is not accepted as
a UTC boundary. Calendar spans are bounded to 1200 months. A calendar mapping cannot coexist with another static or
calendar mapping for the same Target/provider/currency, and static periods in one scope cannot overlap.

Only fully ended months are generated. Each generated period remains an exact half-open UTC interval, including
variable month length and leap years; no provider-specific billing day, account scope, or browser clock is inferred.

## Durable period authority

Migration `000080_billing_shared_allocation_calendar_schedules.sql` creates one row per schedule-config SHA-256 and exact
period. The row freezes:

- Target, provider, currency, and `static | monthly-utc` mode;
- exact period start/end;
- settlement and retry intervals; and
- the next due time, attempt count, last start/finish/success, outcome, and bounded error code.

A due scheduler first inserts or locks this row, then atomically advances `attempt_count`, `last_started_at`,
`next_attempt_at`, and `last_outcome=running`. A separate fenced transaction records `completed` or `failed` after the
audited sweep. Timeline/count fields cannot regress, identity/settings cannot change, an old outcome cannot be rewritten
without a new claim and finish, rows cannot be deleted, and a retained parent Target cannot be deleted.

The timestamps are normalized to PostgreSQL microsecond precision before claim and finish comparisons. This was added
after review found that a nanosecond Go timestamp could otherwise be truncated by PostgreSQL and then falsely fail the
exact `last_started_at` completion check.

## Restart and multi-replica semantics

The previous static scheduler used a process-local last-start timestamp. Migration `000080` makes `next_attempt_at` the
durable throttle:

- a process restart before the retry interval observes the row and skips;
- a leader handoff after the interval claims and safely replays the deterministic Run/Slice graph;
- two replicas racing without the outer leader lease lock the same period row, so one attempts and one skips;
- a crash after claim waits until `next_attempt_at`; a crash after allocation commit replays the immutable allocation
  identity rather than creating a second graph; and
- every mutation still carries the `synara:billing-shared-allocation-scheduler` transaction write fence.

The scheduled system audit now retains schedule kind/config digest and, for monthly mappings, the configured first and
optional last boundary. High-cardinality digests and period timestamps do not become metric labels.

## PostgreSQL gates

The isolated migration test exercised a valid monthly row, claim, completion, migration replay, invalid two-month row,
attempt regression, outcome rewrite without a new claim, deletion, and parent Target deletion:

```text
=== RUN   TestPostgresBillingSharedAllocationSchedulePeriodMigration
--- PASS: TestPostgresBillingSharedAllocationSchedulePeriodMigration (3.59s)
PASS
```

The full-schema scheduler gates then proved leader handoff after one durable retry interval and a direct two-replica
race for one generated monthly period. The monthly test deliberately supplied a nanosecond-resolution scheduler time,
exercising the production precision normalization:

```text
=== RUN   TestPostgresSharedAllocationSchedulerLeadershipHandoffReplaysExplicitPeriod
--- PASS: TestPostgresSharedAllocationSchedulerLeadershipHandoffReplaysExplicitPeriod (3.50s)
=== RUN   TestPostgresMonthlySharedAllocationScheduleDurableClaimAllowsOneReplica
--- PASS: TestPostgresMonthlySharedAllocationScheduleDurableClaimAllowsOneReplica (0.68s)
PASS
```

The final database inventory showed one static period with `attempt_count=2` and one monthly period with
`attempt_count=1`, all `completed`. Repeated test fixtures retained the same shape with additional unique monthly rows;
no period had duplicate attempts from the concurrent race.

## Metrics

The control plane exposes:

```text
synara_billing_shared_allocation_schedule_periods{schedule_kind,outcome,due_state}
```

`schedule_kind` is bounded to `static | monthly-utc`, `outcome` to `never | running | completed | failed`, and
`due_state` to `due | waiting`. PostgreSQL uses database `CURRENT_TIMESTAMP` for due classification. The PostgreSQL
monthly concurrency test gathered this metric after completion; SQLite separately verified both due/failed monthly and
waiting/never static projections. Target IDs, schedule digests, periods, and error details are forbidden labels.

## Repository verification

- Focused billing/config/database/observability/API Go packages: passed.
- SQLite metadata safety: valid claim/completion and repeated migration passed; invalid calendar shape, counter
  regression, outcome rewrite, row deletion, and Target deletion were rejected.
- `go test ./... -count=1` under `services/control-plane`: passed after final source changes.
- `bash deploy/kubernetes/validate-resilience-assets.sh`: passed.
- `git diff --check`: passed after report and contract changes.
- No `bun fmt`, `bun lint`, `bun typecheck`, or `bun test` command was run.

## Source binding

| Source                                                                               | SHA-256                                                            |
| ------------------------------------------------------------------------------------ | ------------------------------------------------------------------ |
| `migrations/000080_billing_shared_allocation_calendar_schedules.sql`                 | `2d253822e64d01243c4d72de455a79412a4e8bde7bdd8f7fdb661b6270bd538a` |
| `internal/billing/scheduler.go`                                                      | `df11822e9f0058daa64ff0370430977e9157bc02be2dee2ce8ddff971db51680` |
| `internal/billing/runtime.go`                                                        | `126aab1d805af0478859a8db7778f7ddfd484b6b27a1168329b93df7fb028e19` |
| `internal/config/config.go`                                                          | `7c1c72fc41becdb354aa1ec8058446118ad510838ceb0ef50c6c83974814c29d` |
| `internal/persistence/shared_cost_allocation_models.go`                              | `a9b814ce13cf2fc543d0608d5c4765bbfa4a0a6a67ab657850f15e289da827d4` |
| `internal/database/billing_shared_allocation_schedule_sqlite.go`                     | `9712e21d1503e2c14c885d0e8ef82e436ca66a87b9bef07a3a2d184e4024ed21` |
| `internal/database/billing_shared_allocation_schedule_migration_integration_test.go` | `e68f9bfb0eddfa8608eb2b74d384e46ed8a7a9fbbda8e3250eac5a0001bf5711` |
| `internal/billing/shared_management_postgres_integration_test.go`                    | `78b4b8033c7c60e90299720b21dc03c70a14cc4f50e8e40aa566c3c475e04988` |
| `internal/observability/distributed_routing_billing_metrics.go`                      | `813db22ba728cacb1e8960e0b38577624ad2a8470732967d1aaf74189d292f6e` |
| `cmd/api/main.go`                                                                    | `e98bbbb7da88a42bd71e395984d6573b5c00f5ecd66317a5fe4cdaed68ad1ad3` |

## Remaining gates

- Build a separate conserved allocation graph for account-level actual invoices before shared Tenant slices can
  participate in actual reconciliation. The current actual invoice model remains tenant-owned.
- Validate AWS CUR/GCP Billing/Azure Cost exports with real cloud Workload Identity, immutable export execution
  provenance, account scope, and rotation/revocation. OrbStack cannot close those E4 gates.
- Run production-duration multi-replica billing soak and operational alert tuning; the deterministic local race and
  handoff are E3 correctness evidence, not an E4 SLO.
