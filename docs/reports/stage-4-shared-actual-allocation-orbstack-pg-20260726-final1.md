# Stage 4 Shared Actual-Invoice Allocation — OrbStack PostgreSQL Final 1

- Date: 2026-07-26
- Evidence class: `E3 local-runtime`
- Kubernetes context: `orbstack` (`v1.34.8+orb1`)
- PostgreSQL: `17.10 (Debian 17.10-1.pgdg13+1)`
- Go: `go1.26.5 darwin/arm64`

## Result

Migration `000082` and the current Control Plane billing service passed a fresh-schema PostgreSQL run connecting one
operator-owned account invoice to a platform-shared Target. Two concurrent first requests serialized to one immutable
`sealed` allocation graph and one mutation audit.

The retained PostgreSQL result was:

```text
schema max version: 82
schema version 82 records: 1
sealed runs: 1
allocation lines: 1
actual charge slices: 6
mutation audits: 1
selected actual amount: 12,345,679 micros
allocated slice amount: 12,345,679 micros
unallocated account lines: 1
unallocated account amount: -45,679 micros
```

The selected actual line conserved every signed micro. The unrelated account line remained explicitly unallocated; it
was not assigned to a Tenant or platform idle by inference.

## Implemented graph

Migration `000082` adds:

- `billing_shared_actual_allocation_runs`, freezing operator/import/Target/coverage scope, provider, currency, exact
  period, source and scope-attestation digests, selected/unallocated totals, algorithm, creator, and seal state;
- `billing_shared_actual_allocation_lines`, giving every selected actual invoice line a globally single-use identity,
  complete estimate-slice digest, nonnegative estimate basis, and conserved signed allocation amount;
- `billing_shared_actual_charge_slices`, retaining each exact Tenant or `platform-idle` estimate owner and its signed
  share of the actual line, while allowing each estimate slice to participate in at most one actual allocation.

The operator API is:

```text
POST /v1/tenants/{tenantID}/billing/shared-targets/{executionTargetID}/actual-invoices/{invoiceImportID}/allocations
```

It requires `billing.manage`, the configured platform billing operator Tenant, and an explicit lowercase
`sourceScopeAttestationSHA256`.

## Algorithm and conservation

`proportional-shared-estimate-v1` selects only exact provider/currency/period/resource/kind matches. It orders the
complete immutable estimate-slice set and uses arbitrary-precision cumulative integer division over estimate micros.
Adjacent cumulative differences produce individual actual slices, so the last slice receives the exact remainder.
The source sign is applied after division; positive, zero, negative, and `MinInt64` boundaries are covered without
floating-point math.

The run is inserted as `building`. Only after all deterministic Line/Slice rows exist may the database validate and
atomically permit `building -> sealed`. PostgreSQL and SQLite check:

- import line count and signed amount totals;
- complete selected-line coverage for the Target;
- exact per-line estimate-slice count and estimate-weight sum;
- exact per-line and whole-run actual amount conservation;
- no cross-Target match ambiguity;
- exact provider, currency, period, resource, charge-kind, Tenant/idle, import, Target, and coverage scope.

## Concurrency and late-data fences

PostgreSQL uses a transaction advisory lock for the complete invoice import identity and a second provider/currency/
period snapshot lock shared with the estimate allocator. Concurrent identical first writes therefore returned one Run
identity; one caller created it and the other replayed it.

After seal, database triggers rejected:

- mutation of the retained Run;
- deletion of an actual charge Slice;
- appending another line to the sealed invoice import;
- appending another matching shared estimate slice;
- sealing a Run whose promised Line/Slice graph was incomplete;
- forged overlapping or duplicate shared estimate history already covered by the existing Migration `000077` gates.

An actual line and an estimate slice are each globally single-use in this graph. Exact replay returns the retained graph;
a different attestation or authoritative slice set is a conflict.

## Metrics

The implementation adds low-cardinality gauges:

```text
synara_billing_shared_actual_allocation_runs{provider,currency,state}
synara_billing_shared_actual_allocation_lines{provider,currency,state,kind}
synara_billing_shared_actual_allocation_amount_micros{provider,currency,state,kind}
```

`kind` is `selected` or `unallocated`. Tenant, Target, import, resource, digest, and period identities are not labels.

## Commands and verification

The disposable PostgreSQL deployment ran in the isolated namespace
`synara-stage4-shared-actual-pg-final1`. The focused runtime gate was:

```bash
SYNARA_TEST_DATABASE_URL='postgres://synara:***@127.0.0.1:55482/synara?sslmode=disable' \
go test ./internal/billing \
  -run '^TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays$' \
  -count=1 -v
```

Result:

```text
--- PASS: TestPostgresConcurrentSharedUsageAllocationSerializesAndReplays
PASS
```

The full Control Plane Go suite also passed:

```text
go test ./... -count=1
```

Focused SQLite, HTTP, observability, and billing tests covered the same seal transition, exact replay, concurrent first
write, signed remainder allocation, zero-basis rejection, cross-Target ambiguity, operator authorization, late-data
fences, immutable history, and bounded metric labels.

## Cleanup proof

The `kubectl` port-forward was stopped. Before deletion, the namespace owner label was exactly
`stage4-shared-actual-pg-final1`; `kubectl --context orbstack delete namespace ... --wait=true` completed successfully.
Final namespace and local port `55482` lookups returned nothing.

## Source hashes

| Artifact                                                          | SHA-256                                                            |
| ----------------------------------------------------------------- | ------------------------------------------------------------------ |
| `migrations/000082_shared_actual_invoice_allocation.sql`          | `00ab49f880c7f852c5527d6c70076fdac95145d4eb9faae66d3291c140f0fa38` |
| `internal/billing/shared_actual_allocation.go`                    | `c39c3091ea9b37b356ff1edb4552ffff52e3f1a9e50d9d8d57b7d878feaeefd5` |
| `internal/billing/shared_allocation.go`                           | `822af3b87b206cf13a59d7ea5b145c51cb9ab646dfec0e41ff5bcec344e3007b` |
| `internal/persistence/shared_actual_allocation_models.go`         | `1f2b4bb3b3a26dcbcff26e14fa2b6dedd4b06643e676c475144b312e1fef198b` |
| `internal/database/shared_actual_allocation_sqlite.go`            | `fb2277718470857ed983f90122439c1dcdf713436d1c8c1b48cbf95d6794bf3d` |
| `internal/billing/shared_actual_allocation_test.go`               | `b2b24b92dfa0d7f949ecfb671cbc60e73563d9e2eb08b24ef3f5db993c3eecea` |
| `internal/billing/shared_allocation_postgres_integration_test.go` | `3c2dac07350095c7b8456485c97cfc4b4b8b35df0384a5a702a21e38f93cb85f` |
| `internal/observability/distributed_routing_billing_metrics.go`   | `b64b538e6dfcbf6a31d0f957a33b9236f3ffa79e9d76717ffd5db266d248c254` |

Paths in this table are relative to `services/control-plane` except the report itself.

## Boundary

This is strong local database/runtime E3 evidence. The source-scope attestation in the test is synthetic. It does not
prove AWS/GCP/Azure account ownership, real export settlement, Workload Identity, object delivery/versioning, cloud
credential rotation, multi-AZ operation, or production-duration soak. Each provider still requires its independent
managed-cloud E4 gate.
