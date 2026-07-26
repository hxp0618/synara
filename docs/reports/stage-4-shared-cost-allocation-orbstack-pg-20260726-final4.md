# Stage 4 shared Target cost allocation — OrbStack PostgreSQL acceptance (final4)

## Result

Passed on 2026-07-26 against the real local OrbStack Kubernetes API and a fresh isolated PostgreSQL 16 Pod. This
supersedes `final1`: it retains the end-to-end allocation proof and adds the post-review P1 gates for cross-period
serialization, SQLite replay parity, required resource facts, terminal-only right-boundary requests, hidden fallback
tariff boundaries, immutable Target scope, semantic Slice uniqueness, tariff/resource snapshot binding, and regional
tariff precedence.

This is `E3 local-runtime` evidence. It proves the local Kubernetes/PostgreSQL control and accounting path; it does
not prove managed-cloud identity, native cloud invoice delivery, or allocation of an account-level actual invoice.

The tested checkout was the dirty local branch `codex/saas-tenancy-user` at HEAD
`59d1d7b8c28bccd305f6e0b23c191facfba5f14f`. The source hashes below, rather than HEAD alone, bind the executed
behavior because the Stage 4 work was not committed.

## Environment

- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, Linux arm64
- Disposable namespace: `synara-stage4-shared-allocation-pg-final4`
- PostgreSQL image: `postgres@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777`
- Database exposure: local port-forward only
- Cleanup: port-forward stopped and the namespace deleted after evidence capture

## Executed gates

The fresh database first ran the exact Migration 077 installation gate:

```text
go test ./internal/database \
  -run '^TestPostgresSharedCostAllocationMigrationInstallsRetainedLedgerShape$' \
  -count=1 -v
```

```text
--- PASS: TestPostgresSharedCostAllocationMigrationInstallsRetainedLedgerShape (3.53s)
```

The same disposable database then ran the production allocation transaction and negative PostgreSQL gates:

```text
go test ./internal/billing \
  -run '^TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays$' \
  -count=1 -v
```

```text
--- PASS: TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays (0.34s)
```

That test proved:

- two simultaneous first allocations for one Worker/provider/currency/period return the same immutable Run and the
  same 27 deterministic Slices;
- a distinct overlapping billing period is rejected by the application before write;
- a direct overlapping Run is rejected by the PostgreSQL authority trigger under the same scope advisory lock;
- a duplicate semantic Slice with a different UUID is rejected by the unique semantic index;
- a Slice whose rate does not match its exact tariff is rejected;
- a Request at a partial Run's right boundary is rejected because that boundary is not the Worker terminal time;
- a Request exactly at Worker terminal time remains legal when terminal time is also the selected tariff's
  `effectiveEndAt`;
- the retained fixture contains two Tenants, two non-overlapping active intervals with sub-second boundaries,
  explicit platform-idle gaps, a region-specific tariff followed by a provider-global fallback, and three Requests.

Repository-level SQLite tests additionally prove concurrent first-write replay on the single-replica Service,
overlap rejection, missing positive-rate CPU/Memory/Ephemeral facts failing before Run creation, terminal-boundary
parity, global fallback rejection when a regional tariff is effective, rate/resource snapshot binding, semantic
uniqueness, immutable Target ownership/kind, and rejection of missing Release facts.

## Database truth after the gate

Migration and authority objects:

```text
control_plane_schema_migrations.version = 77
uq_billing_shared_estimated_charge_slices_semantic = installed
run/slice authority trigger count = 2
```

Retained allocation authority:

```text
runs = 1
claims = 3
releases = 3
tenant_allocated_seconds = 3600
platform_idle_seconds = 3600
exact_terminal_boundary_requests = 1
```

Charge-slice aggregate:

| Allocation    | Charge            | Slices | Billable seconds | Amount micros |
| ------------- | ----------------- | -----: | ---------------: | ------------: |
| platform-idle | cpu               |      4 |             3600 |     5,400,000 |
| platform-idle | ephemeral-storage |      4 |             3600 |     3,000,000 |
| platform-idle | memory            |      4 |             3600 |     3,000,000 |
| platform-idle | pod               |      4 |             3600 |     5,400,000 |
| tenant-claim  | cpu               |      2 |             3600 |     5,400,000 |
| tenant-claim  | ephemeral-storage |      2 |             3600 |     3,000,000 |
| tenant-claim  | memory            |      2 |             3600 |     3,000,000 |
| tenant-claim  | pod               |      2 |             3600 |     5,400,000 |
| tenant-claim  | request           |      3 |                0 |     1,500,000 |

For every time-based charge kind, Tenant plus platform-idle amounts equal the full two-tariff Worker estimate.
Request charges are bound to the exact Claim/Tenant, have zero billable seconds, and never assign platform idle to a
synthetic Tenant.

## Repository verification

- `go test ./internal/billing ./internal/database ./internal/persistence ./internal/config -count=1`: passed.
- `go test ./... -count=1` under `services/control-plane`: passed.
- `git diff --check`: passed after final documentation updates.
- No Bun command was run.

## Source binding

| Source                                                                   | SHA-256                                                            |
| ------------------------------------------------------------------------ | ------------------------------------------------------------------ |
| `migrations/000077_shared_target_cost_allocation.sql`                    | `cdcb6d0ad52bbb9695d88081cc7319f4100081f10fb9f3e786ee09c69108c250` |
| `internal/persistence/shared_cost_allocation_models.go`                  | `effb05c4646171998ed3d1572a546331eb7cd1ef4a2565902ea647ebce61e6fc` |
| `internal/persistence/schema.go`                                         | `47d6c336a99d2350d124aaa8cbe748c5da255f35bf85b144c0bc182cd7a68c14` |
| `internal/database/shared_cost_allocation_sqlite.go`                     | `2285d4b8d86293bacba7be6587358a980e0258a18e99c3955f0c3108538bffbd` |
| `internal/database/shared_cost_allocation_sqlite_test.go`                | `b994decb46a9c55b552069a911f3b1c3b72f8d4756673e2f81b6a79607001e83` |
| `internal/database/shared_cost_allocation_migration_integration_test.go` | `f7d1560e3b79fc218e1946da2256c9e688bb2f6daa9a0cf110922592b5f66b56` |
| `internal/database/store.go`                                             | `8e3d2c615cd3f2b52c53cfe8987db3818b17d9e0f843c6d086abda98c294c453` |
| `internal/billing/service.go`                                            | `007a589c275472374213dff6201f2076446d130fcb13ca36c1afed0a8ff91881` |
| `internal/billing/shared_allocation.go`                                  | `64b832eae9811b46266d3fb3c4f2b33abcca54b68bef514f07e5ffd66b63c629` |
| `internal/billing/shared_allocation_test.go`                             | `deaa6b5bb717c68ce986ca27da1b08badee77ff87335b54623e52c82e40db225` |
| `internal/billing/shared_allocation_postgres_integration_test.go`        | `442f77d3d6f092791eeb3679045a1e4a24082b96ed0adc9c1a484ac4d3d5dd47` |

## Boundaries and remaining gates

- Coverage is explicit and immutable; Migration 077 performs no historical backfill. A Worker registered before the
  sealed `completeFromAt`, a missing Release, count drift, overlap, missing priced resource, invalid timeline, or a
  non-terminal Worker fails closed.
- Whole-second ownership and cumulative monetary conservation remain allocation-core invariants. Direct write
  access to these tables must be restricted to the Control Plane role; database triggers additionally bind parent
  scope, non-overlapping periods, semantic uniqueness, selected tariff precedence, rates, resource snapshots, and
  Request amount shape.
- This lane produces estimated shared-cost slices from versioned tariffs. It does not split a provider's aggregated
  actual invoice among Tenants; that requires a separate actual-allocation authority and conservation contract.
- The existing tenant-owned estimate sweeper remains closed to shared Targets. Enabling a shared-target sweeper
  requires an operator coverage-sealing API/runbook plus PostgreSQL load/soak and starvation evidence.
- OrbStack is a real local Kubernetes runtime but a single local node. The result cannot close AWS/GCP/Azure
  Workload Identity, native export provenance, managed multi-zone failure, or production-duration soak gates.
