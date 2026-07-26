# Stage 4 OrbStack Kubernetes resilience acceptance — 2026-07-26 final9

Historical evidence: this run was superseded by
[final10](stage-4-orbstack-resilience-acceptance-20260726-final10.md) after adding the narrow HTTP retry mapping for
Migration 70 logical-identity lock contention. Its raw evidence remains immutable.

This run validates the current dirty Stage 4 worktree after Migration `000070`. It reuses the retained local
OrbStack PostgreSQL, MinIO, and two-replica Control Plane deployment, rolls both Control Plane replicas to an
immutable final9 image, and records focused resilience evidence without rebuilding the baseline or disturbing the
separate warm-pool namespace.

## Result

- Status: **pass**
- Evidence window: `2026-07-25T16:53:06Z` to `2026-07-25T16:54:12Z`
- Total duration: `66` seconds
- Baseline: skipped; the retained final9 deployment was inspected before the run
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, local single-node OrbStack cluster
- Planned cases: `rbac`, `leader-takeover`, `control-plane-failover`
- Case totals: `passed=3`, `failed=0`, `skipped=0`
- Final evidence:
  [stage-4-orbstack-resilience-acceptance-20260726-final9.json](stage-4-orbstack-resilience-acceptance-20260726-final9.json)
- Progress journal:
  [stage-4-orbstack-resilience-acceptance-20260726-final9.json.journal.jsonl](stage-4-orbstack-resilience-acceptance-20260726-final9.json.journal.jsonl)
- Atomic progress snapshot:
  [stage-4-orbstack-resilience-acceptance-20260726-final9.json.partial.json](stage-4-orbstack-resilience-acceptance-20260726-final9.json.partial.json)
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`
- Source worktree dirty: `true`
- Test image: `synara-control-plane:stage4-orbstack-final9-20260726`
- Test image ID: `sha256:dbe29b14341ba23eb7a947118cb998076222383bd0bc187e41a3a7fef0e147ce`

This is local real-kubelet evidence from an uncommitted worktree. It is not a pushed artifact, managed-cloud
deployment, or production release gate.

## Frozen acceptance inputs

| Asset                                                                         | SHA-256                                                            |
| ----------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| `deploy/kubernetes/acceptance.sh`                                             | `476b38842dc09d416b4abbebb30e12916c06270f936bf22402b07faa96c6d3ac` |
| `deploy/kubernetes/resilience-acceptance.sh`                                  | `c2171e0f0fcef6ae1c6d826e29d4df326db6a4478a7882b012b0fab086fded63` |
| `deploy/kubernetes/validate-resilience-assets.py`                             | `4cb1b1d48f913ce63cc094b65880d277d5c2045dedd51bc9a59a7afd282f1d3d` |
| `services/control-plane/Dockerfile`                                           | `cd8747438e2b1ef24bbcaf23f58a4cd589e5e89776f319052fd8ee3afe561a4c` |
| `services/control-plane/migrations/000069_worker_pool_warm_capacity.sql`      | `3c3563ff70e589afa3d99424aba70898528c5dd757f97b5b71aabf8676ba9812` |
| `services/control-plane/migrations/000070_kubernetes_pod_deletion_fences.sql` | `e9a936a14a481180fabaacb6fe2814caddd39dd510f8f65fc44c49dc2482dfcd` |
| Final JSON evidence                                                           | `71fc63e1f17c35b8bb8ad27962f4479f4636952c9381be94b4c61977c1849132` |
| JSONL progress journal                                                        | `fb18672f52f94ace2fccfe109bff583a61cd1b1891c4a9118b24e07653c6638a` |
| Atomic progress snapshot                                                      | `7666990e3d1b61b52e3c68c16624100f8be654b8a9586c008e7ccbcf72b8d577` |

The hashes were captured after the proof. The exact final9 image and retained Kubernetes resources remain available
for follow-up inspection.

## Resilience evidence

- The final9 deployment rolled out successfully and returned to `2/2` Ready replicas. Both Pods used the exact
  final9 image ID and had zero restarts before the disruption run.
- All `28/28` harness RBAC expectations matched: `21` required permissions were allowed and `7` prohibited
  permissions were denied.
- The active `synara:kubernetes-execution-reconciler` holder changed from
  `synara-control-plane-9f8f7bd8f-w5htp` to `synara-control-plane-9f8f7bd8f-2fz89` after the exact holder Pod was
  deleted.
- The reconciler fencing token advanced exactly once, from `9` to `10`; leader takeover recorded `0` readiness
  probe failures.
- The independent Control Plane failover case replaced `synara-control-plane-9f8f7bd8f-2fz89` with
  `synara-control-plane-9f8f7bd8f-bl9dz` and recorded `0` readiness probe failures.
- After both disruptions, the deployment was again `2/2` Ready, `/ready` returned HTTP 200, and all six
  database-authoritative reconciler leadership gauges reported one active holder.
- `unexpectedExitStatus` remained `null`; the final report recorded `status=passed`.

## Migration 70 and deletion-fence inspection

The live PostgreSQL deployment and final9 `/ready` response were inspected after rollout and again after the
resilience cases:

- `/ready` reported `expectedVersion=70` and `appliedVersion=70`;
- migration row
  `70|kubernetes_pod_deletion_fences|e9a936a14a481180fabaacb6fe2814caddd39dd510f8f65fc44c49dc2482dfcd`
  matched the checked-in migration bytes;
- table `kubernetes_pod_deletion_fences`, its primary key, and
  `idx_kubernetes_pod_deletion_fences_requested` existed;
- fence triggers `trg_kubernetes_pod_deletion_fences_insert`,
  `trg_kubernetes_pod_deletion_fences_immutable`, and
  `trg_kubernetes_pod_deletion_fences_delete` existed;
- Worker guards `trg_worker_instances_pod_deletion_fencing` and
  `trg_worker_instances_fenced_reactivation` existed;
- functions `try_lock_worker_logical_identity`, `enforce_kubernetes_pod_deletion_fencing`, and
  `prevent_fenced_kubernetes_worker_reactivation` existed.

The durable lifecycle contract is keyed by exact
`(execution_target_id, namespace, pod_name, pod_uid)`. The Reconciler serializes deletion intent with Worker
registration on the same logical-identity lock, checks any Execution Lease or active Workspace cleanup delivery,
persists an immutable fence, drains the exact Worker, commits, and only then calls the Kubernetes DELETE API with a
UID precondition. The exact fenced UID cannot register, authenticate, heartbeat, claim, or be reactivated, while a
same-name replacement with a new UID remains valid.

Focused PostgreSQL tests proved fence scope, immutability, idempotency, target cascade, direct-write guards,
same-name/new-UID replacement, and two-transaction logical-identity serialization. The equivalent SQLite
AutoMigrate and trigger guards passed. The full Control Plane Go suite passed after the final source changes. Two
isolated-schema PostgreSQL concurrency tests also proved same-request Execution and Workspace-cleanup claims replay
one authoritative ledger/receipt result.

The resilience harness itself does not create the Register/Claim/Delete race. Migration 70 race correctness is
therefore supported by the focused PostgreSQL/SQLite tests, while this run proves that the migration is applied and
the resulting image remains healthy under local Kubernetes leader and Pod failover.

## Warm-pool continuity

The retained warm-pool proof used a separate namespace, `synara-warm-final6b`, which the resilience harness did not
modify:

- Pod `synara-warm-3a5601e801-v1-unmanaged-s0` retained exact UID
  `9eb6984a-b500-4676-80f5-ca66e80d95dd`;
- it remained `Running`, Ready, and at zero restarts through the final9 rollout and both Control Plane disruptions;
- its current Worker remained `online/active`, Protocol v2, `compatible`, lease/fencing-capable, and
  `kubernetes-pod-bound-v1`;
- eight consecutive Reconciler samples advanced warm-capacity version from `3891` to `3925` while holding
  `desiredTotal=1`, `claimed=0`, `readyIdle=1`, with the authority TTL continuously refreshed;
- Prometheus reported `desired_total=1`, `claimed=0`, and `ready_idle=1` for capacity class `standard`.

Additional direct RBAC probes confirmed that the Control Plane ServiceAccount can create and delete Pods in the
warm namespace, while the agentd ServiceAccount cannot get Pods or list Secrets.

## Retained resources

- `synara-system` remains Active with PostgreSQL, MinIO, and two final9 Control Plane replicas.
- `synara-warm-final6b` and its Ready warm Pod remain Active for follow-up inspection.
- The final9 Docker image remains present in OrbStack.
- No broad Kubernetes, Docker, or database cleanup was performed.

## Evidence boundary

This pass proves Migration 70 application, local real-kubelet RBAC, two-replica database-backed leader fencing,
exact leader takeover, Control Plane Pod replacement, and warm-pool continuity on OrbStack. OrbStack is single-node,
so it cannot prove cross-node scheduling, Node partition, or multi-AZ behavior. The paired disposable Kind final4
report remains the local multi-node and 601-second soak evidence. Managed-cloud multi-Region/multi-AZ behavior, real
AWS/GCP/Azure workload identity, external billing exports, and production-duration soak remain external deployment
gates; Stage 4 therefore remains `IN PROGRESS`.
