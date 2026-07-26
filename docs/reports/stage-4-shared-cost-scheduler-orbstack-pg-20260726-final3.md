# Stage 4 shared Target cost scheduler — OrbStack PostgreSQL acceptance (final3)

## Result

Passed on 2026-07-26 against the real local OrbStack Kubernetes API and a fresh isolated PostgreSQL 16 Pod. This
extends the operator management `final2` evidence with strict runtime configuration, settlement gating, a dedicated
multi-replica scheduler lease, transaction fencing, standby exclusion, and safe leader-handoff replay.

This is `E3 local-runtime` evidence. It proves the local Kubernetes/PostgreSQL control-plane scheduler path; it does
not prove managed-cloud identity, native cloud invoice delivery, dynamic provider billing-period discovery,
account-level actual-invoice allocation, multi-zone failure, or production-duration soak.

The tested checkout was the dirty local branch `codex/saas-tenancy-user` at HEAD
`59d1d7b8c28bccd305f6e0b23c191facfba5f14f`. Source hashes below bind the executed behavior because the Stage 4 work
was not committed.

## Environment

- Kubernetes context: explicit `--context orbstack` on every command
- Kubernetes server: `v1.34.8+orb1`, single local OrbStack node
- Disposable namespace: `synara-stage4-shared-scheduler-pg-final3`
- PostgreSQL image and runtime image ID:
  `postgres@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777`
- Database exposure: temporary localhost port-forward `55480 -> 5432` only
- Cleanup: port-forward stopped; namespace deletion completed and a final lookup returned Kubernetes `NotFound`

## Runtime contract exercised

`SYNARA_BILLING_SHARED_ALLOCATION_MAPPINGS_JSON` accepts only strict static mappings with:

- one platform-shared `executionTargetId`;
- normalized provider and three-letter currency;
- explicit RFC3339 half-open `billingPeriodStartAt` / `billingPeriodEndAt`;
- `settlementDelay` between one minute and 90 days;
- `scheduleInterval` of at least one minute.

The period is capped at 366 days. Duplicate identities and overlapping periods for the same
Target/provider/currency fail during configuration normalization. The scheduler never infers “current month” or
“previous month” from process time and does not run before period end plus settlement delay.

The API process creates a separate `synara:billing-shared-allocation-scheduler` leadership Runner. Its database lease
uses a monotonic fencing token; the Runner attaches the exact epoch to every GORM create/update/delete transaction.
The per-process due timestamp only throttles attempts. A restart or takeover starts with empty throttle state and
therefore revalidates the configured period immediately; immutable deterministic Run/Slice identities are the durable
replay cursor.

## Executed gates

The fresh database installed and verified Migration 077:

```text
go test ./internal/database \
  -run '^TestPostgresSharedCostAllocationMigrationInstallsRetainedLedgerShape$' \
  -count=1 -v
```

```text
--- PASS: TestPostgresSharedCostAllocationMigrationInstallsRetainedLedgerShape (3.22s)
```

The allocation transaction, PostgreSQL negative gates, and operator-authorized manual sweep replay passed:

```text
go test ./internal/billing \
  -run '^TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays$' \
  -count=1 -v
```

```text
--- PASS: TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays (0.57s)
```

The atomic Coverage management path passed two simultaneous first-seal requests and changed-content conflict:

```text
go test ./internal/billing \
  -run '^TestPostgresConcurrentSharedLedgerCoverageSealSerializesExactReplay$' \
  -count=1 -v
```

```text
--- PASS: TestPostgresConcurrentSharedLedgerCoverageSealSerializesExactReplay (0.17s)
```

Finally, two independent scheduler services and two leadership services competed for the same PostgreSQL lease:

```text
go test ./internal/billing \
  -run '^TestPostgresSharedAllocationSchedulerLeadershipHandoffReplaysExplicitPeriod$' \
  -count=1 -v
```

```text
--- PASS: TestPostgresSharedAllocationSchedulerLeadershipHandoffReplaysExplicitPeriod (1.10s)
```

That handoff gate proved:

- the first holder alone ran the settled static period;
- the second holder remained standby for an observed 200 ms while the first lease was active;
- cancellation made the first Runner release its epoch;
- the standby then acquired token 2 and immediately replayed the same explicit period;
- each epoch wrote one `system` scheduled-attempt audit under the configured operator Tenant;
- both transactions were leadership-fenced, while deterministic allocation identity remained unchanged.

SQLite/unit tests additionally proved settlement waiting, immediate same-process throttling, replay after the interval,
partial Worker failure propagation, successful-Worker preservation, missing-operator fail-closed behavior, strict JSON
unknown-field rejection, RFC3339/duration validation, duplicate/overlap rejection, and minimum-runner interval
selection.

## Database truth after the gate

```text
schema_version=77
coverage_rows=3
allocation_runs=1
allocation_slices=27
tenant_seconds=3600
platform_idle_seconds=3600

coverage_seal_audits=2
manual_sweep_audits=1
scheduled_sweep_audits=2
scheduler_fencing_token=2
```

Audit actor truth:

```text
billing.shared_cost_allocation_sweep_requested|actor=user|count=1
billing.shared_cost_allocation_sweep_scheduled|actor=system|count=2
billing.shared_target_ledger_coverage_sealed|actor=user|count=2
```

The three Coverage rows belong to the allocation fixture, the concurrent management gate, and the scheduler target.
The scheduler target intentionally had no Worker facts: both leader epochs proved empty-period revalidation and audit
without fabricating a Run. The separate allocation fixture remained exactly one Run / 27 Slices after manual replay,
with 3,600 Tenant seconds plus 3,600 platform-idle seconds.

## Repository verification

- `go test ./internal/billing ./internal/config ./internal/observability ./cmd/api -count=1`: passed.
- `go test ./... -count=1` under `services/control-plane`: passed after the final scheduler implementation.
- `bash deploy/kubernetes/validate-resilience-assets.sh`: passed, including 24 managed-hook tests, Kustomize rendering,
  shell/Python validation, and resilience asset checks.
- `git diff --check`: passed.
- No `bun fmt`, `bun lint`, `bun typecheck`, or `bun test` command was run.

## Source binding

| Source | SHA-256 |
| --- | --- |
| `internal/billing/shared_management.go` | `4c9240cae3d650dbaefc622748f2b92e5e1c22709e367ed021a555cb2a5c0040` |
| `internal/billing/shared_management_postgres_integration_test.go` | `fc6390e5e0f9ecafdfcfef060124e76f0d6db3cfaf8b5b329fef3a625ca4cb7d` |
| `internal/billing/shared_allocation_postgres_integration_test.go` | `4016f45e3a173137c6ff20495cf4898ee2c06417161507332096844f36be8395` |
| `internal/billing/runtime.go` | `0914623a34699d6c86ee3beadb353df2f046a8c3ed1d5766edceb1317bb7f1d0` |
| `internal/billing/runtime_test.go` | `13e5b659483cfca4e416a416a66f6ddf35d34777f9a508a56a1fcf178be3ea0c` |
| `internal/billing/scheduler.go` | `c42dc1a0815831cc9b1da36ff85fcd1f60bd2954007710254cb3e1c5cdcbbdfe` |
| `internal/billing/scheduler_test.go` | `fbcce8b223e07ffed24d04787cd7b8952523a949e657cd64fd2fb82825d39559` |
| `internal/billing/service.go` | `cd2a7d17db4905abb51f8a762102104b4abf13897e8faec36240abae83105692` |
| `internal/config/config.go` | `b0063b8d1023e6bb7f8ba6bd43849e4fb532389b831ef8eb3b53f4c5df627e49` |
| `internal/config/config_test.go` | `cd9185f8b7f0e4806eb44631cf8ef8b86a12df2f6d212eeaf234ce83cac0b841` |
| `cmd/api/main.go` | `5c6db1311ee27315ae6a5cfa39ac942a2009333cbe60db5cf27492e5778f7a37` |
| `internal/observability/metrics.go` | `968a1889b3173e99cbb694d3731f16a3ce584399fa7d2c5d3bd50a7fdcd8fa68` |
| `internal/observability/distributed_routing_billing_metrics.go` | `4de708dc9f5b60581dcb850cf700eb7f0d564c77ab428a26f28b75d2f6c3eddf` |
| `deploy/kubernetes/deployment.yaml` | `788f86d4dd50c3a5548f27438dfa57cbfcc3fa7b39c60edeb8410583efa98a38` |
| `deploy/kubernetes/config.example.yaml` | `cd87f296ba45933a0d007f51884f1e2cb91dff619191660415361762e8088d36` |
| `docs/contracts/cloud-cost-accounting-v1.md` | `3031379a80222a6d4a39c8d7a18ac949da34b9737ad23d5b3608fa48022e23a9` |

## Boundaries and remaining gates

- Mappings are intentionally static. Dynamic calendar-period creation would need an authoritative provider/export
  completion cursor and cannot be inferred safely from local time alone.
- The scheduler repeatedly revalidates retained mappings to catch late Worker facts. Operators remove a mapping only
  after external ingestion authority proves the period complete; there is not yet a provider settlement-watermark row.
- Shared slices remain tariff-derived estimates. Splitting an aggregated provider account invoice among Tenants still
  requires a separate source-scope and amount-conservation graph.
- OrbStack is a real local Kubernetes runtime but a single local node. This result cannot close AWS/GCP/Azure Workload
  Identity, native export provenance, managed multi-zone failure, multi-Region recovery, or production-duration soak.
