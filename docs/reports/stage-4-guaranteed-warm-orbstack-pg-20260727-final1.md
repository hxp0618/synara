# Stage 4 guaranteed-warm / min-idle — OrbStack Kubernetes + PostgreSQL final1

Date: 2026-07-27 (Asia/Shanghai)
Branch: `codex/saas-tenancy-user`
HEAD observed during verification: `79a9067a2b92d91e70c76023c55fa8761d128064`
Evidence level: **E3 local real-kubelet/PostgreSQL mechanics evidence**

## Result

**PASS.** Migration `000087` and the current Kubernetes Reconciler were exercised against the real OrbStack Kubernetes
API/kubelet and a fresh PostgreSQL database at schema 87. A warm Pool configured with
`desiredIdleUnits=2`, `minIdleUnits=1`, `maxActiveUnits=2` first created two Running warm Pods. When a queued cold
Execution later competed for the Target-wide two-Pod budget, reconciliation:

1. deleted only best-effort slot 1 with the exact observed Pod UID;
2. preserved guaranteed slot 0 with the same physical Pod UID; and
3. created the cold Execution Pod only after Kubernetes reported the deleted UID absent.

The authoritative warm-capacity row advanced from the initial publication to version 3 and retained
`min_idle_units=1`, `desired_total_units=2`, `claimed_units=0`, `ready_idle_units=0`. The resulting deficit was therefore
explicitly `1`; budget truncation did not silently downgrade the guaranteed floor.

This closes the live OrbStack lane for the `guaranteed-warm` / min-idle increment. It does not prove multi-node or
multi-AZ scheduling, managed-cloud identity, Registry admission/signing, production-duration SLO, or cross-region data
plane behavior.

## Environment and ownership

- Kubernetes context: explicit `orbstack`; current context was never changed.
- Kubernetes server: `v1.34.8+orb1`, `linux/arm64`.
- Docker Engine: `29.4.0`, `linux/arm64`.
- isolated Namespace: `synara-stage4-guaranteed-warm-final1`.
- captured Namespace UID: `b25f15f2-5487-42c3-b96f-03ea7e207c73`.
- short-lived reconciler ServiceAccount: `synara-guaranteed-warm-reconciler`.
- PostgreSQL: `16.14 (Debian 16.14-1.pgdg13+1)`, fresh owned container and database.
- applied schema tail: `87 worker_pool_min_idle`, `86 worker_reconciliation_drains`,
  `85 agent_executions_claim_indexes`.
- test-owned image: `synara-worker-acceptance:stage4-guaranteed-warm-final1-20260727`.
- image config ID: `sha256:d36d0e74d63796142cce89e88ae824c770ec46d755c4f1caeacd903db3dafea9`.
- image metadata SHA-256: `37f8a9a4447653824fe510b524dc232904a165a8cea307804c9ccb505478b6d1`.

The image was built from the official `worker-acceptance` target with `--allow-dirty`, version
`0.6.1+stage4.guaranteed-warm.final1`, revision label matching the HEAD above, and dedicated
`synara.io/acceptance=stage4-guaranteed-warm-final1` / `synara.io/source-worktree=dirty-verification` labels. It was not
pushed or presented as a clean-SHA release.

The namespaced Role allowed the reconciler ServiceAccount to create Pods and patch NetworkPolicies. An impersonated
negative probe returned `no` for Namespace deletion. Worker Pod registration requests reached a bounded host-side
acceptance endpoint and were intentionally held before persistence, keeping both warm Pods Running but unregistered so
the real not-ready demand-eviction path could be exercised without a Worker Claim racing the scenario. No token or
credential value was written to evidence.

## Live test result

The current-source integration test is:

```text
TestKubernetesGuaranteedWarmOrbStackIntegration
```

Final output:

```text
PASS (7.48s test duration)
guaranteed UID = 4c09f295-c351-4f4a-9496-0f371b9028a5
evicted UID    = 13c691dd-6992-4631-af26-b961618741af
final Pods     = 2
authority      = version 3, deficit 1
scenario       = 5.945s
```

The final two-Pod set consisted of the original slot-0 warm Pod and the Execution-pinned cold Pod. Slot 1 was absent;
the guaranteed Pod UID never changed. The PostgreSQL scheduling fixture used a versioned Placement Policy and the
production immutable fixed-target Scheduling Decision writer. Its persisted graph was
`fixed-target-v1 / selected-only / 1 candidate`, and the Execution placement cluster was `kubernetes`.

The final Target Health projection was:

```text
status=healthy
capacityStatus=saturated
allocatedCapacityUnits=2
reservationAcknowledgedUnits=1
reservationAuthorityMode=exact-active-v1
version=3
```

This shows that the real API objects, queue reservation, Target-wide Pod budget, and warm capacity publication agreed
on the same post-reconcile state.

## PostgreSQL negative gate

The existing migration integration test also ran against the owned PostgreSQL instance:

```text
TestPostgresWorkerPoolWarmCapacityRejectsScopeAndMutation  PASS (0.15s)
```

It proved that the database rejected wrong Target scope, wrong release scope, an authority-row min-idle value that no
longer matched its Pool, a non-monotonic version/observation update, and deletion of the authoritative observation. A
valid version-1-to-version-2 observation update remained accepted.

## Regression verification

Focused packages passed:

```text
go test ./internal/executiontargets ./internal/database ./internal/observability ./internal/placement ./internal/warmcapacity -count=1
```

The full Control Plane Go suite then passed:

```text
go test ./... -count=1
```

`git diff --check` produced no errors. Per repository instructions, no `bun fmt`, `bun lint`, or `bun typecheck` command
was run.

## Harness correction retained as negative evidence

The first scenario attempt reached two real Running warm Pods but its manually inserted cold Execution omitted the Pool's
effective `placement_cluster_id`. PostgreSQL correctly rejected it with
`Execution placement location does not match its effective routing and Worker Pool authority`. The final harness did not
weaken or bypass that trigger: it added an explicit version-1 Placement Policy, froze `placement_cluster_id=kubernetes`,
and created the Execution through `schedulingdecision.CreateExecution`, producing the immutable selected-only Decision
graph described above. The database was recreated before the final pass.

## Cleanup proof

The test itself deleted only its exact Target-labelled Pods using each observed UID. Final cleanup then rechecked the
Namespace UID and owner label and submitted an API-server-enforced UID precondition before waiting for absence. The exact
PostgreSQL container and exact labelled image tag/config ID were removed. The build metadata file was moved to
`/Users/huang/.Trash/synara-stage4-guaranteed-warm-final1-metadata.json`, where it remains recoverable.

Post-cleanup checks reported:

```text
namespace=absent
postgresContainer=absent
acceptanceImage=absent
acceptanceImageId=absent
postgresPort=closed
metadata=trash
```

The pre-existing `synara-system` Control Plane remained `2/2` Ready and available. The pre-existing
`synara-warm-final6b` Pod retained UID `9eb6984a-b500-4676-80f5-ca66e80d95dd` and remained Running. No unrelated
Namespace, container, image, or listener was removed.

The deleted Namespace and disposable PostgreSQL database are not recoverable. The metadata file in Trash is
recoverable until the user empties Trash.

## Source hashes

```text
29ab98cf37b830146a5013ff62163b875ab714e8d4788b9dc23ab6977f94aefe  migrations/000087_worker_pool_min_idle.sql
00766f914c44d6e3eefcf131549cd179258dec2ee48eb4768c46aaba9c47e999  executiontargets/kubernetes_reconciler.go
04c8851cd291154c7aabe66032f1087d0a84f4f113133910e444c15a3ec997cf  executiontargets/kubernetes_warm_pool.go
50f69061efb0ccde3356e9f62443989b37574d4b386b169f6619b1a37e8199f1  executiontargets/managed_kubernetes_warm_capacity_publisher.go
a034c6e71d0bc215582590d0d1000bcd0aea9ec7594cb953ccc17e65a9bb6642  observability/distributed_routing_billing_metrics.go
148e0d8c6b404eb6b8ab6a6cb51dc2e5352022af0e35be8655c0acc49631c9ae  executiontargets/kubernetes_guaranteed_warm_orbstack_integration_test.go
e8ae6f642d709b2ac0f795d59341199a9727e312bc47383f84dbb35766ce5a96  docs/contracts/worker-pool-placement-v1.md
4e48b1efa7014155ae0fd91766fb348ecb113d33bec033112b9d648d63deb69a  deploy/kubernetes/monitoring/prometheus-rules.yaml
```

These hashes bind the report to the dirty working-tree sources used by the final run, not merely to HEAD.
