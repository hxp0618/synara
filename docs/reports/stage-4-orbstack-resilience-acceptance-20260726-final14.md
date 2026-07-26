# Stage 4 OrbStack Kubernetes resilience acceptance — 2026-07-26 final14

Final14 supersedes final11 for the current dirty Stage 4 worktree. It runs the current schema-73 Control Plane image
after the resilience harness moved leader deletion to a UID-preconditioned Lease Guard v2 protocol and moved managed
node-partition hooks behind strict, sanitized controller evidence. Failed final12/final13 evidence remains retained as
the audit trail for final12's transport EOF and final13's leader-deletion failure before this pass.

## Result

- Status: **pass**
- Evidence window: `2026-07-26T01:04:49Z` to `2026-07-26T01:05:32Z`
- Total duration: `43` seconds
- Baseline: skipped; the retained deployment and schema were inspected before and after the run
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, local single-node OrbStack cluster
- Planned cases: `rbac`, `leader-takeover`, `control-plane-failover`
- Case totals: `passed=3`, `failed=0`, `skipped=0`
- Final evidence:
  [stage-4-orbstack-resilience-acceptance-20260726-final14.json](stage-4-orbstack-resilience-acceptance-20260726-final14.json)
- Progress journal:
  [stage-4-orbstack-resilience-acceptance-20260726-final14.json.journal.jsonl](stage-4-orbstack-resilience-acceptance-20260726-final14.json.journal.jsonl)
- Atomic progress snapshot:
  [stage-4-orbstack-resilience-acceptance-20260726-final14.json.partial.json](stage-4-orbstack-resilience-acceptance-20260726-final14.json.partial.json)
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`
- Source worktree dirty: `true`
- Test image: `synara-control-plane:stage4-orbstack-final12-20260726`
- Test image ID: `sha256:e0c9b0079432052203e044c4becad86eb251746cd220f0951ac1b4557905de39`

This is local real-kubelet evidence from an uncommitted worktree. It is not a pushed artifact, managed-cloud
deployment, or production release gate.

## Frozen acceptance inputs

| Asset                                                                          | SHA-256                                                            |
| ------------------------------------------------------------------------------ | ------------------------------------------------------------------ |
| `deploy/kubernetes/resilience-acceptance.sh`                                   | `832d2c4476218d72b831b46317377a03dc359e24390a7da647c9e1edef7abd34` |
| `deploy/kubernetes/managed-hook-controller.py`                                 | `c9086ba399ee84163d78444ecd6a6bf08712861b4bc1c0a79e08d6e60e7adfb3` |
| `deploy/kubernetes/test_managed_hook_controller.py`                            | `9fe1819dfb625bc0b77e3e45e0762a3338910a4e8b22a871a79c84cc282550c3` |
| `deploy/kubernetes/validate-resilience-assets.py`                              | `4df6bd26c4beb60562d23dade59ca39c16f57ba6dbd3dc72b7d7fb42b2ed1a24` |
| `deploy/kubernetes/validate-resilience-assets.sh`                              | `59003c25b7001c26ae391034e9d883c0e5bb9c266add651d0f8129ad054cfbdd` |
| `services/control-plane/Dockerfile`                                            | `cd8747438e2b1ef24bbcaf23f58a4cd589e5e89776f319052fd8ee3afe561a4c` |
| `services/control-plane/migrations/000071_ssh_target_operation_fencing.sql`    | `db979123f806bcab43ce3fd2997330932052ff83b3fd7e93c9505bc45cfabb39` |
| `services/control-plane/migrations/000072_ssh_bootstrap_authority.sql`         | `975171705f3f8af8f6fe214589f9bd6a9166dbd0403ef9276672640c97fb64ce` |
| `services/control-plane/migrations/000073_ssh_worker_bootstrap_generation.sql` | `7aa8fe173a3faba337093eae88166d2efe4d4e1306c2d73045f4fc0882dbb2da` |
| Final14 JSON evidence                                                          | `a9583456dc73d20b75c2c1d403df7f4be34e1875086a4aa48154f2d362235ca4` |
| JSONL progress journal                                                         | `74a2dacb13b53cab9cf7be0ac527b42f6d931d4c524f896b72c872eab4ac4a6b` |
| Atomic progress snapshot                                                       | `45f40a888acc0e011015c183d6e25eb9bcdaf01f311367b3d075df2daea59aa9` |

The hashes bind this report to the exact dirty files used for the local run; they do not make the worktree
reconstructible.

## Rollout and schema state

- The retained Deployment had `2/2` Ready replicas on the exact image above, with zero restarts.
- A validated PostgreSQL custom-format backup was captured before rollout at
  `.tmp/orbstack-rollout-20260726-final12/pre-rollout.dump`; its SHA-256 is
  `2735ca28739889d4fb13259ef7a3ced780401a3f7118360aa8af13cd8060c465`.
- Readiness and the migration ledger both reported `73/73`; migration rows `71`–`73` matched the frozen SQL hashes.

## Resilience evidence

- All `28/28` least-privilege RBAC expectations matched.
- Lease Guard v2 opened one exact PostgreSQL Pod stream, bound its Pod UID, held the advisory lock, and submitted one
  raw Kubernetes DELETE carrying the original Control Plane Pod UID precondition.
- The deleted leader UID was `752dfc8e-78f6-43f5-a84c-f992d2db55cf`. The holder changed from
  `synara-control-plane-66dcdc9bf-krcrw` to `synara-control-plane-66dcdc9bf-9lrd9`, the fencing token advanced exactly
  once from `20` to `21`, and readiness probe failures remained `0`.
- The independent failover case deleted `synara-control-plane-66dcdc9bf-vkhpv`, observed
  `synara-control-plane-66dcdc9bf-2mrp4` as its replacement, and also recorded `0` readiness failures.
- Final inspection found two Ready final12-image replicas, both at zero restarts, an unexpired reconciler lease at
  token `21`, and schema `73/73`.

## Warm-pool continuity

The retained warm namespace was outside the harness mutation scope. Pod
`synara-warm-3a5601e801-v1-unmanaged-s0` retained UID
`9eb6984a-b500-4676-80f5-ca66e80d95dd`, remained Running, and stayed at zero restarts through both disruptions.

## Evidence boundary

This pass proves local real-kubelet RBAC, two-replica PostgreSQL-backed leader fencing, UID-safe leader takeover,
Control Plane Pod replacement, schema-73 readiness, and warm-pool continuity. OrbStack is single-node: the Kubernetes
control plane, both application replicas, PostgreSQL, MinIO, networking, storage, and warm Pod share one Mac failure
domain. The selected cases did not invoke the real `systemd-user` managed node-partition controller. This result cannot
prove managed-cloud Node behavior, multi-AZ partition, cloud Workload Identity, external database/object-store
failover, actual cross-Region data replication, or production-duration soak. Stage 4 remains **IN PROGRESS**.
