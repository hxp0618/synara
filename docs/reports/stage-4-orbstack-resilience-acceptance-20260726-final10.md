# Stage 4 OrbStack Kubernetes resilience acceptance — 2026-07-26 final10

This run supersedes final9 for the current dirty Stage 4 worktree. It includes Migration `000070` plus the narrow
HTTP retry mapping for Migration 70 logical-identity lock contention, then validates the exact final10 image against
the retained local OrbStack PostgreSQL, MinIO, two-replica Control Plane, and separate warm-pool namespace.

## Result

- Status: **pass**
- Evidence window: `2026-07-25T17:03:14Z` to `2026-07-25T17:05:07Z`
- Total duration: `113` seconds
- Baseline: skipped; the retained final10 deployment was inspected before the run
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, local single-node OrbStack cluster
- Planned cases: `rbac`, `leader-takeover`, `control-plane-failover`
- Case totals: `passed=3`, `failed=0`, `skipped=0`
- Final evidence:
  [stage-4-orbstack-resilience-acceptance-20260726-final10.json](stage-4-orbstack-resilience-acceptance-20260726-final10.json)
- Progress journal:
  [stage-4-orbstack-resilience-acceptance-20260726-final10.json.journal.jsonl](stage-4-orbstack-resilience-acceptance-20260726-final10.json.journal.jsonl)
- Atomic progress snapshot:
  [stage-4-orbstack-resilience-acceptance-20260726-final10.json.partial.json](stage-4-orbstack-resilience-acceptance-20260726-final10.json.partial.json)
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`
- Source worktree dirty: `true`
- Test image: `synara-control-plane:stage4-orbstack-final10-20260726`
- Test image ID: `sha256:547aff214706d6c29d63c73601d4540b2a9a563a3d248d3edff02a53f2edeb9e`

This is local real-kubelet evidence from an uncommitted worktree. It is not a pushed artifact, managed-cloud
deployment, or production release gate.

## Frozen acceptance inputs

| Asset                                                                         | SHA-256                                                            |
| ----------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| `deploy/kubernetes/acceptance.sh`                                             | `476b38842dc09d416b4abbebb30e12916c06270f936bf22402b07faa96c6d3ac` |
| `deploy/kubernetes/resilience-acceptance.sh`                                  | `c2171e0f0fcef6ae1c6d826e29d4df326db6a4478a7882b012b0fab086fded63` |
| `deploy/kubernetes/validate-resilience-assets.py`                             | `4cb1b1d48f913ce63cc094b65880d277d5c2045dedd51bc9a59a7afd282f1d3d` |
| `services/control-plane/Dockerfile`                                           | `cd8747438e2b1ef24bbcaf23f58a4cd589e5e89776f319052fd8ee3afe561a4c` |
| `services/control-plane/go.mod`                                               | `65c8f3ad8b88e3730fa2ad79b288178e057bec34f90afa1435cb7dbb0e3dde47` |
| `services/control-plane/migrations/000069_worker_pool_warm_capacity.sql`      | `3c3563ff70e589afa3d99424aba70898528c5dd757f97b5b71aabf8676ba9812` |
| `services/control-plane/migrations/000070_kubernetes_pod_deletion_fences.sql` | `e9a936a14a481180fabaacb6fe2814caddd39dd510f8f65fc44c49dc2482dfcd` |
| `services/control-plane/internal/podlifecycle/pod_lifecycle.go`               | `f5a58324b885a04758e5ba95faca094848c475bcc5e6172c0e1d0f9e54b1827d` |
| `services/control-plane/internal/httpapi/server.go`                           | `a6f1b7a81e63b6d87bbdc903a0338510d01485ecb3f96f473699d9b1038be47e` |
| Final JSON evidence                                                           | `650d0dbca8cdd72ddeb411017712a20261ec4ac99eefc3336c65524835b10d58` |
| JSONL progress journal                                                        | `7d9aaabf2b63092b93badcfa6275342347f62ae3e77a0b24144e7e7ec1588515` |
| Atomic progress snapshot                                                      | `621308b189c686bc587a800ea3b697960c9eeb46bde38b4dbe73f11016d4ffac` |

The hashes were captured after the proof. The exact final10 image and retained Kubernetes resources remain
available for follow-up inspection.

### Provenance clarification

The input hashes above identify dirty files observed at final10 time; they are not a reconstructible source tree.
In particular, `validate-resilience-assets.py` was subsequently extended for the dual-cluster lane and now hashes to
`75e666fe4f8e7ff607acd4d25c1978c3c165316fa068b35a85419c1bc4c6e5f4`. The historical `4cb1...` bytes were neither
committed nor archived, so final10 must not be presented as source-reproducible evidence.

The linked final10 JSON is authoritative only for its three recorded scenarios; it has `topology=null` and no soak.
The 2/2 Pod readiness, six leadership gauges, Migration 70 inspection, and retained warm-capacity samples described
below were supplemental read-only observations and were not embedded in that scenario JSON. A 2026-07-26 follow-up
again observed both Control Plane Pods and the retained Warm Pod Ready with zero restarts, but that dynamic observation
does not retroactively expand the machine evidence.

## Resilience evidence

- The final10 deployment rolled out successfully and returned to `2/2` Ready replicas. Both current Pods use the
  exact final10 image ID and have zero restarts.
- All `28/28` harness RBAC expectations matched: `21` required permissions were allowed and `7` prohibited
  permissions were denied.
- The active `synara:kubernetes-execution-reconciler` holder changed from
  `synara-control-plane-8686686f95-48cxf` to `synara-control-plane-8686686f95-qkl4f` after the exact holder Pod was
  deleted.
- The reconciler fencing token advanced exactly once, from `13` to `14`; leader takeover recorded `0` readiness
  probe failures.
- The independent Control Plane failover case replaced `synara-control-plane-8686686f95-qkl4f` with
  `synara-control-plane-8686686f95-2bq64` and recorded `0` readiness probe failures.
- After both disruptions, the deployment was again `2/2` Ready, `/ready` returned HTTP 200, and all six
  database-authoritative reconciler leadership gauges reported one active holder.
- `unexpectedExitStatus` remained `null`; the final report recorded `status=passed`.

## Migration 70 and deletion-fence inspection

The live PostgreSQL deployment and final10 `/ready` response were inspected after the resilience cases:

- `/ready` reported `expectedVersion=70` and `appliedVersion=70`;
- migration row
  `70|kubernetes_pod_deletion_fences|e9a936a14a481180fabaacb6fe2814caddd39dd510f8f65fc44c49dc2482dfcd`
  matched the checked-in migration bytes;
- table `kubernetes_pod_deletion_fences`, its primary key, and
  `idx_kubernetes_pod_deletion_fences_requested` existed;
- all three immutable-fence triggers, both Worker fencing/reactivation triggers, and the shared
  `try_lock_worker_logical_identity` function existed.

The durable lifecycle contract is keyed by exact
`(execution_target_id, namespace, pod_name, pod_uid)`. The Reconciler serializes deletion intent with Worker
registration on the same logical-identity lock, checks every Execution Lease and active Workspace cleanup delivery,
persists an immutable fence, drains the exact Worker, commits, and only then calls the Kubernetes DELETE API with a
UID precondition. The exact fenced UID cannot register, authenticate, heartbeat, claim, or be reactivated, while a
same-name replacement with a new UID remains valid.

Migration 70 raises a package-owned PostgreSQL `40001` only when that logical-identity lock is unavailable. final10
matches both the SQLSTATE and exact migration message at the HTTP boundary and returns retryable
`503 worker_logical_identity_lock_unavailable`. Unrelated PostgreSQL serialization failures retain their original
error classification. The mapping runs after the failed transaction has rolled back; it does not retry inside the
transaction or change lock/commit semantics.

Focused PostgreSQL tests proved fence scope, immutability, idempotency, target cascade, direct-write guards,
same-name/new-UID replacement, and two-transaction logical-identity serialization. The concurrency test now also
forces the real fence trigger to raise `40001` and verifies the exact matcher. SQLite AutoMigrate and trigger guards,
the HTTP mapping/preservation tests, the full Control Plane Go suite, and both isolated-schema same-request Claim
concurrency tests passed.

The resilience harness itself does not create the Register/Claim/Delete race or force lock contention. Migration 70
race and retry classification are therefore supported by the focused PostgreSQL/SQLite/HTTP tests, while this run
proves that the exact resulting image remains healthy under local Kubernetes leader and Pod failover.

## Warm-pool continuity

The retained warm-pool proof used a separate namespace, `synara-warm-final6b`, which the resilience harness did not
modify:

- Pod `synara-warm-3a5601e801-v1-unmanaged-s0` retained exact UID
  `9eb6984a-b500-4676-80f5-ca66e80d95dd`;
- it remained `Running`, Ready, and at zero restarts through the final9/final10 rollouts and four Control Plane
  disruption cases;
- its current Worker remained `online/active`, Protocol v2, `compatible`, lease/fencing-capable, and
  `kubernetes-pod-bound-v1`;
- four post-final10 Reconciler samples advanced warm-capacity version from `4676` to `4689` while holding
  `desiredTotal=1`, `claimed=0`, `readyIdle=1`, with the authority TTL continuously refreshed;
- Prometheus reported `desired_total=1`, `claimed=0`, and `ready_idle=1` for capacity class `standard`.

Direct RBAC probes also confirmed that the Control Plane ServiceAccount can create and delete Pods in the warm
namespace, while the agentd ServiceAccount cannot get Pods or list Secrets.

## Retained resources

- `synara-system` remains Active with PostgreSQL, MinIO, and two final10 Control Plane replicas.
- `synara-warm-final6b` and its Ready warm Pod remain Active for follow-up inspection.
- The final9 and final10 Docker images remain present in OrbStack.
- No broad Kubernetes, Docker, or database cleanup was performed.

## Evidence boundary

This pass proves Migration 70 application, local real-kubelet RBAC, two-replica database-backed leader fencing,
exact leader takeover, Control Plane Pod replacement, and warm-pool continuity on OrbStack. OrbStack is single-node,
so it cannot prove cross-node scheduling, Node partition, or multi-AZ behavior. The paired disposable Kind final4
report remains the local multi-node and 601-second soak evidence. Managed-cloud multi-Region/multi-AZ behavior, real
AWS/GCP/Azure workload identity, external billing exports, and production-duration soak remain external deployment
gates; Stage 4 therefore remains `IN PROGRESS`.
