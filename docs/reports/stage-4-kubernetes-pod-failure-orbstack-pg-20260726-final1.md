# Stage 4 Kubernetes Pod failure facts — OrbStack/PostgreSQL acceptance (final1)

## Result

Passed on 2026-07-26 against the real local OrbStack Kubernetes API and an isolated PostgreSQL 16 Pod.

This is `E3 local-runtime` evidence. It proves Kubernetes status parsing, bounded classification, PostgreSQL/SQLite
durability, exact Generation scope, idempotent replay, Worker terminalization ordering, and trailing-30-day metric
projection in the local environment. It does not prove a managed-cloud node-pressure eviction, multi-zone behavior,
production-duration soak, or long-retention metric pre-aggregation.

The tested checkout was branch `codex/saas-tenancy-user` at HEAD
`260dd2d4465e565c5cd43ed0f26dd59d50b2c0ab`. The code was already present in that local commit when final verification
ran; the report and final contract/TODO wording remained local working-tree changes. Nothing was pushed by this gate.

## Environment

- Kubernetes context: explicit `--context orbstack` on every command
- Kubernetes server: `v1.34.8+orb1`, one local OrbStack node
- PostgreSQL namespace: `synara-stage4-pod-facts-pg-final1`
- Runtime-fixture namespace: `synara-stage4-pod-facts-runtime-final1`
- PostgreSQL image ID:
  `postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20`
- Runtime fixture image ID:
  `busybox@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028`
- Database exposure: temporary localhost port-forward `55481 -> 5432`
- Cleanup: port-forward stopped, temporary ClusterRoleBinding deleted, both namespaces deleted, and final namespace
  lookups returned Kubernetes `NotFound`

## Durable authority

Migration `000078_kubernetes_pod_failure_facts.sql` adds four monotonic Generation timestamps:

- first Pod apply/provisioning attempt;
- first observed Pending state;
- first observed Running state; and
- last Pod observation.

It also adds one immutable first proof per Generation/failure class. Repeated observations may only advance
`last_observed_at`; they do not manufacture a poll count. The bounded classes are:

| Class              | Evidence                                                    |
| ------------------ | ----------------------------------------------------------- |
| `pod-apply-failed` | Kubernetes API request failed before a Pod UID existed      |
| `pending-timeout`  | current Pod age exceeded the server-authoritative threshold |
| `unschedulable`    | `PodScheduled=False`, reason `Unschedulable`                |
| `image-pull`       | bounded image/registry waiting reason                       |
| `container-start`  | bounded container-start waiting reason                      |
| `evicted`          | Pod reason `Evicted`                                        |
| `oom-killed`       | current or last container termination reason `OOMKilled`    |
| `pod-failed`       | failed phase with no more specific bounded proof            |

Pending age is computed from the current Kubernetes Pod `metadata.creationTimestamp`, with a first-observation fallback;
it is not inherited from an older replacement UID. Raw status messages, Tenant IDs, Execution IDs, Pod names, and Pod
UIDs never become metric labels. A registered terminal Worker is closed with a specific Evicted/OOMKilled reason before
the exact-UID deletion path checks leases, so recovery does not depend only on lease expiry.

## Real Kubernetes status evidence

Four isolated Pods produced these API states:

```text
evicted|Failed|Evicted|||
image-pull|Pending||ImagePullBackOff||
oom-killed|Failed|||OOMKilled|
unschedulable|Pending||||Unschedulable
```

- `unschedulable` used an impossible node selector and was classified from the scheduler-authored condition.
- `image-pull` used a non-existent image and was classified from the kubelet-authored waiting reason.
- `oom-killed` ran PID 1 directly against a 16 MiB cgroup limit and terminated with exit 137 / `OOMKilled`.
- `evicted` used an isolated Pod with a non-default, unserved scheduler and an API status-subresource patch. This proves
  the real API/parser boundary without draining or pressuring the shared OrbStack node; it is not a real eviction drill.

The production parser/classifier gate then read those exact Pods through the same bearer-token/CA HTTP client:

```text
=== RUN   TestKubernetesPodFailureClassificationAgainstRealAPIServer
--- PASS: TestKubernetesPodFailureClassificationAgainstRealAPIServer (0.02s)
PASS
```

## PostgreSQL and service gates

The fresh PostgreSQL database installed all migrations through schema 78 and exercised identity, timeline, terminal,
scope, immutability, and delete fences:

```text
=== RUN   TestExecutionGenerationFactsMigrationFencesIdentityTimelineAndTerminalOutcome
--- PASS: TestExecutionGenerationFactsMigrationFencesIdentityTimelineAndTerminalOutcome (3.42s)
PASS
```

The PostgreSQL service path then replayed the same image-pull class twice, retained its first proof, advanced only its
last proof, and independently inserted the Pending-timeout class:

```text
=== RUN   TestPostgresObserveKubernetesExecutionPodReplaysFailureClass
--- PASS: TestPostgresObserveKubernetesExecutionPodReplaysFailureClass (3.62s)
PASS
```

SQLite tests exercised the same timeline, apply failure without UID, image-pull replay, Pending timeout, Running,
OOMKilled, immutable failure identity, and delete fences. Focused Reconciler tests additionally proved API status 429
classification before UID assignment and terminal Failed observation before UID-preconditioned safe deletion.

## Metrics

The durable facts project these bounded trailing-30-day gauges:

- `synara_execution_pod_queue_duration_seconds_30d`: dispatch to first apply P50/P95/P99;
- `synara_execution_pod_provisioning_duration_seconds_30d`: first apply to first Running P50/P95/P99;
- corresponding sample gauges; and
- `synara_execution_pod_failure_generations_30d{failure_class,target_kind}`.

The existing dispatch-to-Provider-ready metric remains the end-to-end cold-start measure. These scrape-time windows are
bounded to 30 days but are not yet a long-retention rollup table.

## Repository verification

- `go test ./... -count=1` under `services/control-plane`: passed.
- `bash deploy/kubernetes/validate-resilience-assets.sh`: passed, including 24 managed-hook tests, Python validation,
  Kustomize rendering, and Kubernetes asset checks.
- `git diff --check`: passed.
- No `bun fmt`, `bun lint`, `bun typecheck`, or `bun test` command was run.

## Source binding

| Source                                                                | SHA-256                                                            |
| --------------------------------------------------------------------- | ------------------------------------------------------------------ |
| `migrations/000078_kubernetes_pod_failure_facts.sql`                  | `6e9111778184a1c10cc4016fadd546da16d4a86ce7a4297eb4539c18e21b87e1` |
| `internal/executiontargets/kubernetes_reconciler.go`                  | `865c812b7e4e2ec86b3c9a5aeb66b2b4ba3a651b5cff32671b1c002c672a3154` |
| `internal/executiontargets/kubernetes_reconciler_integration_test.go` | `b55e3d7bb580fbefea6f22c00008ffd0952f911388d0049dcbea15c78d86660c` |
| `internal/executions/kubernetes_execution_pod_observation.go`         | `e2f2943c012b2700482d35a9f9e41a332ab6b7529b3359cfd3bb8d04b2c7f76c` |
| `internal/executions/kubernetes_execution_pod_observation_test.go`    | `578731563fa5a9d7538b4dd11126ae472b18778911f6d7d26eec7c6fafaadebf` |
| `internal/executions/kubernetes_worker_observation.go`                | `c9d09680c1558ee43956e1a10b1eb27c876ce2809e210fdcee39d450e331ba3c` |
| `internal/observability/generation_fact_metrics.go`                   | `309a246108822d19aa36cd3a77eaa54ff907774019a08cbd12b105785656d1f9` |
| `internal/persistence/generation_fact_models.go`                      | `b0ecec9f9a550d7ed7c3bae14ca87f3431649ee224cc7efe685615d8bae4846c` |
| `internal/config/config.go`                                           | `4543800b40b4cf2d67e391fe5c89a7b33222697dd495db73488e644b2ec296e7` |
| `cmd/api/main.go`                                                     | `5908612721e2e2ab6c1afce86197a4be7cc50d452427dba5968aa89f6289b34b` |

## Remaining gates

- Produce a real kubelet/node-controller `Evicted` state in a disposable multi-node or managed cluster; the local
  status-subresource fixture cannot close that gate.
- Exercise Pod failure/recovery under multi-replica Control Plane leadership handoff and long soak.
- Add long-retention rolling pre-aggregation for Generation and Worker facts.
- Managed-cloud RBAC, Node partition, workload identity, multi-zone behavior, and production SLOs remain separate E4
  acceptance gates.
