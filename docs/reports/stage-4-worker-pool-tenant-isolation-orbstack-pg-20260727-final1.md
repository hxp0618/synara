# Stage 4 Worker Pool Tenant isolation — OrbStack Kubernetes + PostgreSQL final1

Date: 2026-07-27 (Asia/Shanghai)
Branch: `codex/saas-tenancy-user`
HEAD observed during verification: `79a9067a2b92d91e70c76023c55fa8761d128064`
Evidence level: **E3 local real-kubelet/PostgreSQL mechanics evidence**

## Result

**PASS.** Migration `000088` and the current Control Plane were exercised with two real OrbStack Kubernetes Pods and
a fresh PostgreSQL 16 database. The same platform-shared Target shape was tested under both immutable Pool policies:

- `pinned`: the general Worker atomically bound to Tenant A on its first exact Claim, completed two Tenant A
  Executions, skipped an earlier Tenant B candidate, and left that Execution `queued` with no Worker.
- `shared`: the general Worker remained unbound and completed Tenant A followed by Tenant B under the normal shared
  fair-queue path.

The persisted Worker IDs, Pod names, namespaces, cluster IDs, and instance UIDs matched the kubelet-authored physical
Pod identities. Both claimed Executions reached `completed`; neither test left a Worker Lease behind.

This closes the local isolation/capacity behavior lane for reusable `pinned` and explicitly trusted `shared` Workers.
It does not close scrub-on-release, production Workload Identity, managed-cloud multi-node isolation, or long-duration
multi-Tenant soak.

## Implemented authority

Migration `000088_worker_pool_tenant_isolation.sql` adds:

- immutable `worker_pools.tenant_isolation = pinned | shared`, defaulting existing and new Pools to `pinned`;
- nullable `worker_instances.tenant_binding_id` with a Target-scoped claim index;
- PostgreSQL trigger enforcement that a binding belongs only to a `general-pool` Worker, remains within Target
  ownership, and cannot be cleared or changed; and
- Pool CAS enforcement that cannot change isolation underneath live or future Worker semantics.

Execution and Workspace-cleanup Claim both lock the current Worker row, filter an existing binding before candidate
ordering, and atomically write the first binding for `pinned`. `shared` deliberately leaves the binding null.
Heartbeat and logical Worker re-registration preserve the binding; a bound identity cannot return under another Worker
mode. Personal SQLite has equivalent triggers and a verified partial composite index.

The tenant-facing Pool UI/API keeps platform-shared Pool mutation operator-owned. The web view now reads and displays
the authoritative `pinned`/`shared` state for platform-shared Targets without exposing tenant mutation controls.

## Environment and ownership

- Kubernetes context: explicit `orbstack`; the unset current context was never changed.
- Kubernetes server: `v1.34.8+orb1`, `linux/arm64`.
- isolated Namespace: `synara-stage4-tenant-isolation-final1`.
- captured Namespace UID: `a4157d76-6a07-48d3-89de-14777b6415fe`.
- PostgreSQL: `16.14 (Debian 16.14-1.pgdg13+1)`, fresh owned container and database.
- applied schema tail: `88 worker_pool_tenant_isolation`, `87 worker_pool_min_idle`,
  `86 worker_reconciliation_drains`, `85 agent_executions_claim_indexes`.
- current-worktree acceptance image:
  `synara-worker-acceptance:stage4-tenant-isolation-final1-20260727`.
- image config ID: `sha256:db7aa37c11491797211a004ca92d9b90ef97e596602c8ea3a2e20286d7b59e78`.
- build metadata SHA-256: `160a90f4326bd985116287c6027a9550982eb77b6f5cbee2ca5f64eb249a36f5`.

The image was built with `--allow-dirty`, version `0.6.1+stage4.tenant-isolation.final1`, revision label matching the
observed HEAD, and dedicated `synara.io/acceptance=stage4-tenant-isolation-final1` /
`synara.io/source-worktree=dirty-verification` labels. It was not pushed or represented as a clean-SHA release.

## Real OrbStack result

The repeatable current-source test is:

```text
TestOrbStackKubernetesGeneralWorkerTenantIsolation
```

Final output:

```text
PASS (4.90s scenario; 5.771s package process)
pinned Worker = ee41e855-16a1-41e0-bea0-d8bcbd99a91a
pinned Pod UID = 9d3b14d3-bb49-481f-b7b3-dc88c23ac96f
shared Worker = e5351c79-05d5-4ec9-b1bf-dd5ffeea8958
shared Pod UID = 1bab66a2-74d8-4a0a-b7c1-ccc9757b925d
pinned other-Tenant Execution = queued
```

Each Pod ran a bounded Node probe from inside the current-worktree Linux Worker image. It registered as a real
`targetKind=kubernetes`, `workerMode=general-pool` identity using downward-API Pod name/UID fields, then exercised the
production Register, Claim, Complete, Worker row locking, placement-policy lookup, fair queue, Recovery Bundle, Lease,
Claim Fact, and release transitions. The Pod entrypoint was intentionally overridden with this probe, so this case
proves physical Pod/network/UID-to-Control-Plane mechanics, not the full agentd polling loop.

The shared Kubernetes Targets and Pool policies lived only in the test's isolated PostgreSQL schema. Managed
tenant-owned Kubernetes Targets cannot represent a platform-shared pool by design; production shared Kubernetes
capacity therefore remains an external/operator-owned Target integration.

## PostgreSQL and concurrency gates

Two focused cases passed against the owned PostgreSQL instance:

```text
TestPostgresWorkerTenantIsolationRejectsPolicyAndBindingMutation       PASS (0.13s)
TestPostgresConcurrentPinnedGeneralWorkerClaimsNeverCrossTenant       PASS (1.49s)
```

The migration gate required check/trigger-class rejection for an isolation-policy mutation, an unknown policy, a
binding change, a binding clear, a tenant-owned Target mismatch, and a binding on a non-general Worker. Disposable token
hashes made the test repeatable and prevented uniqueness failures from masquerading as isolation evidence.

The concurrency case used two independent database connections against one isolated schema. Both attempted to Claim
through the same unbound physical Worker. One transaction committed the Execution plus Tenant binding; the other was
serialized by the Worker row lock and returned `worker_busy`. The final binding exactly matched the winner, while the
other Tenant's Execution remained queued.

The isolated-schema harness now keeps its schema first and `public` second in `search_path`, so migrations create every
table inside the disposable schema while still resolving `public.pgcrypto.digest`. This also repairs existing fresh
schema concurrency tests after migration `000076` introduced pgcrypto-backed Scheduling Decision hashes.

## Regression verification

SQLite-focused tests cover pinned claim filtering, shared rotation, binding preservation across Heartbeat and
re-registration, immutable binding triggers, Workspace-cleanup filtering, and the exact partial composite index shape.
Placement tests cover safe defaulting, validation, projection, and immutable Pool identity.

Verification passed:

```text
go test ./... -count=1

bun run test -- \
  src/components/settings/WorkerPoolPlacementControls.test.tsx \
  src/components/settings/ExecutionTargetWorkerManagement.test.tsx \
  src/lib/controlPlaneClient.test.ts
# 3 files, 54 tests passed

git diff --check
```

Per repository instructions, `bun fmt`, `bun lint`, and `bun typecheck` were not run.

## Negative evidence retained

The final case followed three fail-closed harness corrections:

1. The first Pod attempt declared `targetKind=kubernetes` against a reused Docker Target fixture. Registration returned
   `execution_target_kind_mismatch`; the final fixture creates an actual Kubernetes Target and does not weaken the
   target-kind fence.
2. The next attempt supplied a Worker version that differed from the injected immutable Manifest build version.
   Registration returned `invalid_worker_manifest`; the final handler derives the Manifest fixture from the Pod's
   declared version and re-signs the exact Pod identity.
3. Re-running the PostgreSQL negative test exposed a fixed test token hash collision. The fixture now uses unique hashes
   and asserts the check/trigger error class, preventing a unique-key failure from producing false isolation evidence.

The Pod registration route used a short-lived shared test token and a test-only signed containment fixture. It did not
exercise Kubernetes TokenReview/Pod GET Workload Identity; those are separate previously established local gates and
remain a required production-cloud gate for this increment.

## Security boundary and remaining work

`pinned` makes cross-Tenant Worker reuse impossible after the first successful Claim, including Workspace cleanup. It
does not erase Tenant A's files and does not make an escaped or same-UID Provider safe. Scrub-on-release for Workspace,
`/tmp`, ownership reversal, and Docker's shared-volume/static-UID posture remain open defense-in-depth work.

`shared` intentionally permits cross-Tenant reuse and must not be enabled merely because cleanup usually succeeds. It
requires a separately trusted workload and storage/process boundary. The acceptance proves this explicit behavior; it
does not certify a multi-Tenant-safe shared filesystem.

## Cleanup proof

Both exact test Pods were deleted after UID verification. Namespace cleanup re-read the owner label and exact UID, then
submitted an API-server-enforced `DeleteOptions.preconditions.uid` through a short-lived loopback Kubernetes proxy and
waited for absence. The exact PostgreSQL container and exact labelled image tag/config ID were removed. Build metadata
was moved to `/Users/huang/.Trash/synara-stage4-tenant-isolation-final1-metadata.json`, where it remains recoverable.

Post-cleanup checks reported:

```text
namespace=absent
postgresContainer=absent
acceptanceImage=absent
postgresPort=closed
kubeProxyPort=closed
metadata=trash
```

The pre-existing `synara-system` Control Plane remained `2/2` Ready. The pre-existing `synara-warm-final6b` Pod retained
UID `9eb6984a-b500-4676-80f5-ca66e80d95dd` and remained Running. No unrelated Namespace, container, image, listener, or
worktree file was removed.
