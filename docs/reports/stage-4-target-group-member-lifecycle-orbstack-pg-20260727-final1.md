# Stage 4 Target Group Member Lifecycle — OrbStack PostgreSQL E3 Final 1

Date: 2026-07-27 (Asia/Shanghai)
Result: **PASS**
Evidence class: **E3 local PostgreSQL/Kubernetes runtime**

This increment exposes the existing `active / draining / disabled` Target Group Member authority through a
tenant-authorized, exact-version API and proves its launch/disable ordering with two real PostgreSQL connections.

## Provenance

- Branch: `codex/saas-tenancy-user`.
- HEAD observed after the run: `82a64f8012ff696433684910d00448475e12d427`.
- The shared worktree was dirty; the commit alone is not source provenance.
- Kubernetes context: explicit `orbstack`.
- PostgreSQL: `16.14`, `aarch64`, in an acceptance-owned OrbStack Pod.
- Applied Control Plane schema: `85`.
- PostgreSQL container content digest:
  `postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20`.

## Implemented authority

New route:

```text
PATCH /v1/tenants/{tenantID}/execution-target-groups/{targetGroupID}/members/{targetGroupMemberID}
```

The body contains `expectedVersion` and `status` only.

- `active -> draining`: immediately removes the Member from new placement without interrupting an existing Execution.
- `draining -> active`: explicit drain abort.
- `active | draining -> disabled`: allowed only when that exact Group/Target owns no `queued`, `leased`, `running`,
  `waiting-for-approval`, `recovering`, or `suspended` Execution.
- `disabled` is terminal.
- Exact same-status replay at `expectedVersion` or `expectedVersion + 1` performs no write and returns
  `Idempotency-Replayed: true`.
- Any other stale version conflicts.
- Status, next version, and one bounded Audit row commit in the same transaction.

The API requires the path Tenant to be active and the caller to hold `worker.manage`. It does not expose Target encrypted
configuration or credential data.

## Package and HTTP verification

Focused tests passed for `internal/routing` and `internal/httpapi`.

They cover:

- active Member selected before drain, fallback selected while draining, and the original Member selected after abort;
- exact replay without a second Audit row;
- terminal disabled state and invalid transition rejection;
- disable rejection with one suspended Execution, followed by success after that Execution becomes terminal;
- unauthenticated, active-Tenant mismatch, and missing-`worker.manage` HTTP rejection;
- stable JSON projection and CAS conflict response; and
- the exact `drain_started`, `drain_aborted`, and `disabled` Audit actions.

## OrbStack PostgreSQL two-connection proof

Owned namespace: `synara-stage4-member-lifecycle-pg-final2`.

Command under test:

```text
SYNARA_TEST_DATABASE_URL=<ephemeral OrbStack PostgreSQL URL> \
  go test ./internal/routing \
  -run '^TestMemberDisablePostgresSerializesWithExecutionCommitAndFailsClosed$' \
  -count=1 -v
```

Result: PASS.

The test exercised both commit orders:

1. A launch transaction locked the final Target/Group/Member authority. A second connection attempted Member disable
   and remained blocked throughout a 150 ms negative window. The launch inserted its routed queued Execution and
   committed. Disable then resumed, recounted the committed Execution, returned
   `target_group_member_execution_active`, and left Member status/version unchanged.
2. After the Execution-first rejection, a drain transition committed. Reusing the pre-drain Scheduling Selection then
   returned `target_routing_selection_stale`; the old decision could not cross the new Member version.

PostgreSQL also proved:

- exact drain replay retained the same Member version and exactly one drain Audit for that run;
- a direct status update without `version = old + 1` was rejected by the database constraint trigger; and
- direct Member deletion was rejected.

The complete Control Plane module also passed `go test ./... -count=1`; the focused routing and HTTP packages passed
uncached before that full run.

## Runbook integration

[`kubernetes-cluster-lifecycle.md`](../runbooks/kubernetes-cluster-lifecycle.md) now uses this authority for planned
maintenance and routing offboarding. It deliberately keeps these boundaries explicit:

- Member drain does not itself trigger failover; a fresh Location Outage authority is required for planned evacuation.
- Group Member disable is not physical Kubernetes Target deletion.
- Generic Target disable/delete, fixed-Target Session migration, and encrypted Target configuration rotation still lack
  public product APIs. Operators must not bypass those gaps with direct database updates or manual Namespace deletion.

## Source identity

| File | SHA-256 |
| --- | --- |
| `internal/routing/service.go` | `1a2b70faf5713b8ba0b29a4381a98b9b9770957f6a3b495bdde32248b23b929a` |
| `internal/routing/service_test.go` | `96e60b8c88549fb1e582ec751bee39e2f7c01795e343de07e277393c0626fb21` |
| `internal/routing/commit_validation_postgres_integration_test.go` | `c636ca3f2d596fdf69cd82be312bb51c4d001e9adad3b68fd8d5d12aadcf15dd` |
| `internal/httpapi/routing_api.go` | `d0e30a440d02c7ba490505bf8f8b807e06e7546f29de26c9d76f78912c422a98` |
| `internal/httpapi/routing_api_test.go` | `9f4893e6e819aa868af48bad052270738f58b2e7636d7d5c51d49a4e1121c863` |
| `docs/contracts/global-target-routing-dr-v1.md` | `9b9eabec79e8fdc1e4f109305891b9b6bd30f9a440cdaad37ecb2ba214a084a0` |
| `docs/runbooks/kubernetes-cluster-lifecycle.md` | `37c8b065277ddc7d72d4b0ed07c68f7af42f2272ede7ce444f955b26a2a3ac37` |

## Cleanup and evidence boundary

The Pod create and namespace delete calls each observed one transient Kubernetes transport `unexpected EOF`; both were
treated as ambiguous writes and reconciled by exact read-back. Before the test, the expected Pod was Running with the
pinned PostgreSQL image digest. Before cleanup, the exact namespace owner label and UID were revalidated; afterwards the
namespace and local port-forward listener on `127.0.0.1:55487` were confirmed absent.

This is E3 concurrency and database-authority evidence. It does not prove a production cloud Cluster drain, live
multi-Tenant load, actual cross-Region data replication, or a complete physical Target decommission. Those gates remain
open.
