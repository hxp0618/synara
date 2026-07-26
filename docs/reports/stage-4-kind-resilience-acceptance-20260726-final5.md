# Stage 4 disposable Kind resilience acceptance — 2026-07-26 final5

Final5 supersedes the 2026-07-25 final4 Kind proof for the current dirty Stage 4 worktree. It exercises the same image
ID as the paired OrbStack final14 run, with Lease Guard v2, strict managed-hook evidence validation, PID/EOF
fail-closed cleanup, and the current schema-73 Control Plane.

## Result

- Status: **pass**
- Evidence window: `2026-07-26T01:07:21Z` to `2026-07-26T01:21:02Z`
- Total duration: `821` seconds
- Baseline duration: `71` seconds
- Context: `kind-synara-stage4-resilience-final5`
- Evidence schema: `synara.kubernetes.resilience.acceptance.v1`
- Cases: `passed=6`, `failed=0`, `skipped=0`
- Raw evidence:
  [stage-4-kind-resilience-acceptance-20260726-final5.json](stage-4-kind-resilience-acceptance-20260726-final5.json)
- Progress journal:
  [stage-4-kind-resilience-acceptance-20260726-final5.json.journal.jsonl](stage-4-kind-resilience-acceptance-20260726-final5.json.journal.jsonl)
- Atomic progress snapshot:
  [stage-4-kind-resilience-acceptance-20260726-final5.json.partial.json](stage-4-kind-resilience-acceptance-20260726-final5.json.partial.json)
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`
- Source worktree dirty: `true`
- Test image: `synara-control-plane:stage4-kind-final5-20260726`
- Test image ID: `sha256:e0c9b0079432052203e044c4becad86eb251746cd220f0951ac1b4557905de39`

The image ID is identical to OrbStack final14. This is reproducible local pre-production evidence, not a clean-commit
release gate, pushed artifact, managed-cloud deployment, or production release.

## Frozen acceptance inputs

| Asset | SHA-256 |
| --- | --- |
| `deploy/kubernetes/acceptance.sh` | `476b38842dc09d416b4abbebb30e12916c06270f936bf22402b07faa96c6d3ac` |
| `deploy/kubernetes/resilience-acceptance.sh` | `832d2c4476218d72b831b46317377a03dc359e24390a7da647c9e1edef7abd34` |
| `deploy/kubernetes/kind-acceptance.sh` | `431bca972be151950762edbbb937d23dbbf6a02dbebb3f37dc51957135925d80` |
| `deploy/kubernetes/kind-resilience-acceptance.sh` | `0d2d1b748b00c6909efacc2d5af890567420ab6522c192a46ac0df9278f440b0` |
| `deploy/kubernetes/kind-multinode.yaml` | `4127f63cd7a80bbaa85988fed9670b960d0f9ec8acbc551219fd81a3e982ceda` |
| `deploy/kubernetes/managed-hook-controller.py` | `c9086ba399ee84163d78444ecd6a6bf08712861b4bc1c0a79e08d6e60e7adfb3` |
| `deploy/kubernetes/validate-resilience-assets.py` | `4df6bd26c4beb60562d23dade59ca39c16f57ba6dbd3dc72b7d7fb42b2ed1a24` |
| `services/control-plane/Dockerfile` | `cd8747438e2b1ef24bbcaf23f58a4cd589e5e89776f319052fd8ee3afe561a4c` |
| Final5 JSON evidence | `ee5538c1c44b226bb7dfb3d1dc2d4579297864803bb66eb217b924511cb7a56b` |
| JSONL progress journal | `083aa68bd620e980a9b7b55e9b0946c4e7ec55f8defced496925fc2c82111821` |
| Atomic progress snapshot | `c208cede0203b3e23cb731cba46557c954a33c3fc7b4f90d7b7a865e75c7342e` |

## Baseline and six-scenario matrix

The baseline passed with two Control Plane replicas and `73` applied schema migrations. All `28/28` positive and
negative RBAC expectations matched.

| Scenario | Status | Evidence |
| --- | --- | --- |
| `rbac` | pass | Required grants present; destructive deletion and Pod update/watch denied |
| `topology` | pass | 4 nodes, 3 Workers, 2 Control Plane Pods on distinct hosts, PDB `minAvailable=1` |
| `leader-takeover` | pass | Guard v2; UID-preconditioned delete; token `3 -> 4`; `0` readiness failures |
| `control-plane-failover` | pass | Replacement scheduled and converged; `0` readiness failures |
| `node-drain` | pass | Pod moved off the cordoned Worker; node uncordoned; `0` readiness failures |
| `node-partition` | pass | 20-second Docker-network partition recovered; node Ready before/after; `0` readiness failures |

The skip allowlist was empty and `unexpectedExitStatus` was `null`.

## Ten-minute soak

- Configured duration: `600` seconds
- Actual duration: `601` seconds
- `control-plane-failover` cycles: `10`
- Passed cycles: `10`
- Failed or skipped cycles: `0`
- Idle readiness probe failures: `0`
- Per-cycle readiness probe failures: `0` for all ten cycles

Each cycle deleted a live Control Plane Pod, waited for a replacement and steady-state `2/2` readiness, then completed
the bounded idle probe before starting the next disruption.

## Cleanup

- The owned Kind cluster `synara-stage4-resilience-final5` was deleted.
- `kind get clusters` returned `No kind clusters found.` after the run.
- No running container referenced the final5 cluster name.
- No broad Docker prune or unrelated Kubernetes cleanup was used.
- The retained OrbStack deployment remained `2/2`; the Warm Pod kept its original UID and zero restarts.

## Evidence boundary

This pass proves the checked-in disposable multi-node Kind topology, least-privilege RBAC, PostgreSQL-backed leader
fencing, UID-safe leader takeover, Pod replacement, node drain, bounded node partition, cleanup, and ten-minute
chaos-soak lane against the current executable source snapshot. Kind node partition uses the owned Docker network and
does not prove the non-Kind `systemd-user` managed-hook backend. It also does not prove a managed Kubernetes provider's
Node controller, storage, load balancer, Workload Identity, availability-zone partition, cross-Region control plane,
or production-duration soak.
