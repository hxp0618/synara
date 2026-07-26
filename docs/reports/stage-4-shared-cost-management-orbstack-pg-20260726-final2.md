# Stage 4 shared Target cost management — OrbStack PostgreSQL acceptance (final2)

## Result

Passed on 2026-07-26 against the real local OrbStack Kubernetes API and a fresh isolated PostgreSQL 16 Pod. This
extends the earlier shared-allocation `final4` evidence with the production management path: operator-authorized,
atomic Coverage sealing plus an explicit closed-period, replay-safe shared allocation sweep.

This is `E3 local-runtime` evidence. It proves the local Kubernetes/PostgreSQL authority, concurrency, audit, and
replay paths; it does not prove managed-cloud identity, native cloud invoice delivery, an unattended scheduler,
account-level actual-invoice allocation, multi-zone failure, or production-duration soak.

The tested checkout was the dirty local branch `codex/saas-tenancy-user` at HEAD
`59d1d7b8c28bccd305f6e0b23c191facfba5f14f`. Source hashes below bind the executed behavior because the Stage 4 work
was not committed.

## Environment

- Kubernetes context: explicit `--context orbstack` on every command; kubeconfig had no default current context
- Kubernetes server: `v1.34.8+orb1`, single local OrbStack node
- Disposable namespace: `synara-stage4-shared-management-pg-final2`
- PostgreSQL image:
  `postgres@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777`
- Runtime image ID matched the same digest
- Database exposure: temporary localhost port-forward `55479 -> 5432` only
- Cleanup: port-forward stopped; namespace deletion was confirmed by a final Kubernetes `NotFound`

## Executed gates

The fresh database first installed and verified Migration 077:

```text
go test ./internal/database \
  -run '^TestPostgresSharedCostAllocationMigrationInstallsRetainedLedgerShape$' \
  -count=1 -v
```

```text
--- PASS: TestPostgresSharedCostAllocationMigrationInstallsRetainedLedgerShape (3.89s)
```

The production allocation transaction then ran its concurrency/negative gates and replayed the graph through the
new operator-authorized sweep:

```text
go test ./internal/billing \
  -run '^TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays$' \
  -count=1 -v
```

```text
--- PASS: TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays (0.93s)
```

That gate retained one Run and 27 Slices, then called `SweepSharedUsageChargesAuthorized` for the same closed period.
The sweep scanned the one overlapping shared Worker, replayed the same deterministic graph, returned `completed`, and
created exactly one `billing.shared_cost_allocation_sweep_requested` audit entry. The intentional constraint and
duplicate-key log lines came from negative direct-write gates; the test passed after each invalid write was rejected.

The new Coverage authority then ran two simultaneous first-seal requests from separate PostgreSQL connections:

```text
go test ./internal/billing \
  -run '^TestPostgresConcurrentSharedLedgerCoverageSealSerializesExactReplay$' \
  -count=1 -v
```

```text
--- PASS: TestPostgresConcurrentSharedLedgerCoverageSealSerializesExactReplay (0.15s)
```

That gate proved:

- only an active Tenant with `billing.manage` that exactly matches the configured platform billing operator can seal;
- a platform-shared Target is required;
- the Target-scoped PostgreSQL advisory lock serializes concurrent first seals;
- one request creates the deterministic Coverage while the other returns the same identity as an exact replay;
- exact replay does not create a second audit entry;
- changing the writer-version assertion conflicts without mutating the retained row;
- Coverage and `billing.shared_target_ledger_coverage_sealed` audit commit atomically.

Repository tests additionally proved HTTP status/authorization boundaries, `201` first seal versus `200` replay,
operator-Tenant isolation, missing-Coverage `404`, exact GET, invalid/conflicting assertions, closed-period enforcement,
partial Worker failure reporting, successful allocation preservation, and idempotent sweep replay on SQLite.

## Database truth after the gate

```text
schema_version=77
semantic_index=1
coverage_scope_trigger=1
run_scope_trigger=1
slice_scope_trigger=1

coverage_rows=2
coverage_seal_audits=1
shared_sweep_audits=1
allocation_runs=1
allocation_slices=27
tenant_seconds=3600
platform_idle_seconds=3600
```

The two Coverage rows have different purposes: the original allocation fixture inserts one retained authority row to
exercise the allocator, and the management gate seals a second Target through the authenticated service. Only the
latter has a seal audit, exactly once:

```text
shared-allocation-pg-...|stage4-pg-test|seal_audits=0
shared-coverage-pg-...|stage4-pg-writer-v1|seal_audits=1
```

Charge-slice aggregate remained identical after the authorized sweep replay:

| Allocation | Charge | Slices | Billable seconds | Amount micros |
| --- | --- | ---: | ---: | ---: |
| platform-idle | cpu | 4 | 3600 | 5,400,000 |
| platform-idle | ephemeral-storage | 4 | 3600 | 3,000,000 |
| platform-idle | memory | 4 | 3600 | 3,000,000 |
| platform-idle | pod | 4 | 3600 | 5,400,000 |
| tenant-claim | cpu | 2 | 3600 | 5,400,000 |
| tenant-claim | ephemeral-storage | 2 | 3600 | 3,000,000 |
| tenant-claim | memory | 2 | 3600 | 3,000,000 |
| tenant-claim | pod | 2 | 3600 | 5,400,000 |
| tenant-claim | request | 3 | 0 | 1,500,000 |

## Repository verification

- `go test ./internal/billing ./internal/httpapi ./cmd/api -count=1`: passed.
- `go test ./... -count=1` under `services/control-plane`: passed after the final implementation and tests.
- `git diff --check`: passed.
- No `bun fmt`, `bun lint`, `bun typecheck`, or `bun test` command was run.

## Source binding

| Source | SHA-256 |
| --- | --- |
| `migrations/000077_shared_target_cost_allocation.sql` | `cdcb6d0ad52bbb9695d88081cc7319f4100081f10fb9f3e786ee09c69108c250` |
| `internal/billing/shared_allocation.go` | `64b832eae9811b46266d3fb3c4f2b33abcca54b68bef514f07e5ffd66b63c629` |
| `internal/billing/shared_allocation_postgres_integration_test.go` | `4016f45e3a173137c6ff20495cf4898ee2c06417161507332096844f36be8395` |
| `internal/billing/shared_management.go` | `4c9240cae3d650dbaefc622748f2b92e5e1c22709e367ed021a555cb2a5c0040` |
| `internal/billing/shared_management_test.go` | `adfe76db650db44a71c69a7fc5cec29c8177d5e597cb7c4bce38d460c8f0a73f` |
| `internal/billing/shared_management_postgres_integration_test.go` | `78c828b62f8b69c383dee6a656c528e5d734f832fae5917521206809edc427f8` |
| `internal/billing/service.go` | `c2d3629bf3e47e22c0c0d851c5ba77d8f664d6de6483d0e0ce6d9d55e9ad191c` |
| `internal/billing/management.go` | `17c61a8ee4c0216c8889a762f42574a8157be65a1a7c4704e8693651b8b0425d` |
| `internal/httpapi/billing_api.go` | `8d9e9589963ffe322c537b7f382f8880e842c620df840fedad31c24a950a773d` |
| `internal/httpapi/billing_api_test.go` | `36d717282453357fd7031df6e95c41609682cb40d18bdf37d8c61dee86f14882` |
| `internal/httpapi/server.go` | `1dbaf56fbabb4946f73cead6111df1ab7cedfec8f419d42d493c7ed8500c8a04` |
| `docs/contracts/cloud-cost-accounting-v1.md` | `8e733bafa08bfc414f4818f82c28a97b75dd4f430a0d00169c7874f71306e90e` |

## Boundaries and remaining gates

- Sealing is an explicit operator assertion. The API validates shape, scope, time, immutability, and concurrency but
  cannot independently prove that the supplied writer version and deployment digest are deployed everywhere. The
  operator must establish that fact before sealing; no historical backfill or browser heartbeat is inferred.
- The sweep is a production-safe manual/operational primitive, not an unattended schedule. It commits per Worker,
  returns bounded failures, and is safe to replay after a crash. A future periodic runner still needs leader election,
  a durable closed-period policy, load/soak, and starvation evidence.
- Shared slices remain tariff-derived estimates. Splitting an aggregated provider account invoice among Tenants still
  requires a separate source-scope and amount-conservation authority.
- OrbStack is a real local Kubernetes runtime but a single local node. This result cannot close AWS/GCP/Azure Workload
  Identity, native export provenance, managed multi-zone failure, multi-Region recovery, or production-duration soak.
