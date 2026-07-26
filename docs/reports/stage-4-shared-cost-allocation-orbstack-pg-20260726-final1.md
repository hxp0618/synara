# Stage 4 shared Target cost allocation — OrbStack PostgreSQL acceptance (final1)

Superseded by
[`final4`](stage-4-shared-cost-allocation-orbstack-pg-20260726-final4.md), which adds post-review overlap,
terminal-boundary, SQLite parity, semantic uniqueness, tariff/resource binding, and negative PostgreSQL gates.

## Result

Passed on 2026-07-26 against the real local OrbStack Kubernetes API and an isolated PostgreSQL 16 Pod. This is
`E3 local-runtime` evidence: it proves the Migration 077 schema, PostgreSQL triggers, allocation transaction,
concurrent first-write serialization, deterministic replay, tenant/idle interval reconstruction, tariff fallback,
whole-second conservation, and final-boundary request handling. It does not prove managed-cloud identity, native
cloud invoice export delivery, or allocation of an account-level actual invoice.

The tested checkout was the dirty local branch `codex/saas-tenancy-user` at HEAD
`59d1d7b8c28bccd305f6e0b23c191facfba5f14f`. The source hashes below, rather than HEAD alone, bind the executed
behavior because the Stage 4 work was not committed.

## Environment

- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, Linux arm64
- Disposable namespace: `synara-stage4-shared-allocation-pg-final1`
- PostgreSQL image: `postgres@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777`
- Database exposure: local port-forward only; the namespace was deleted after evidence capture

## Executed gate

The acceptance ran only the named PostgreSQL integration test, with the disposable database URL supplied through
`SYNARA_TEST_DATABASE_URL`:

```text
go test ./internal/billing \
  -run '^TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays$' \
  -count=1 -v
```

Observed result:

```text
=== RUN   TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays
--- PASS: TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays (4.35s)
PASS
ok  github.com/synara-ai/synara/services/control-plane/internal/billing  5.507s
```

The test performed two simultaneous first allocations for the same Worker/incarnation/provider/currency/period.
The PostgreSQL transaction advisory lock and Worker-fact row lock serialized them; both callers returned the same
immutable Run and the same 27 deterministic Slices. The fixture included two Tenants, two non-overlapping active
claim intervals with sub-second boundaries, explicit idle gaps, one claim exactly at terminal `usageEnd`, one
region-specific tariff, and a later provider-global tariff fallback.

## Database truth after the gate

Migration state:

```text
version = 77
```

Retained allocation authority:

```text
runs = 1
claims = 3
releases = 3
tenant_allocated_seconds = 3600
platform_idle_seconds = 3600
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

For every time-based charge kind, tenant plus platform-idle amounts equal the full two-tariff Worker amount. Request
charges are attributed only to their exact Claim/Tenant, including the terminal-boundary claim; platform idle never
receives a request charge or a synthetic Tenant.

## Source binding

| Source                                                            | SHA-256                                                            |
| ----------------------------------------------------------------- | ------------------------------------------------------------------ |
| `migrations/000077_shared_target_cost_allocation.sql`             | `df93010cff7243eb62ee1424aed95c9e830bfd129b5377d429b168ef5db056e7` |
| `internal/persistence/shared_cost_allocation_models.go`           | `effb05c4646171998ed3d1572a546331eb7cd1ef4a2565902ea647ebce61e6fc` |
| `internal/database/shared_cost_allocation_sqlite.go`              | `83b199026865b2e66fe60157169018ccf43beba0ab2e608b17440791b348828a` |
| `internal/billing/shared_allocation.go`                           | `c207014a4717e51b2910379dca709c39f083b90cadc8a1478a0027d4528961ff` |
| `internal/billing/shared_allocation_test.go`                      | `1112191ead6b4a0688a58f310798e5ceff582bbb399e074bd4774b53bd7a57a0` |
| `internal/billing/shared_allocation_postgres_integration_test.go` | `22ca4d41acfd9102e534fc38fc80f19e0a5c28d2e55acdf8bb11fa33f343d9f7` |

## Boundaries and remaining gates

- Coverage is explicit and immutable; Migration 077 performs no historical backfill. A Worker registered before the
  sealed `completeFromAt`, a missing release, count drift, overlapping claims, or a non-terminal Worker fails closed.
- This lane produces estimated shared-cost slices from versioned tariffs. It does not split a provider's aggregated
  actual invoice among Tenants; doing so requires a separate actual-allocation authority and conservation contract.
- The existing tenant-owned estimate sweeper remains closed to shared Targets. Enabling a shared-target sweeper
  requires an operator coverage-sealing API/runbook plus PostgreSQL load/soak and starvation evidence.
- OrbStack is a real local Kubernetes runtime but a single local node. The result cannot close AWS/GCP/Azure
  Workload Identity, native export provenance, managed multi-zone failure, or production-duration soak gates.
