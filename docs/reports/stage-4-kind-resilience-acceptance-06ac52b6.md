# Stage 4 disposable Kind resilience acceptance — `06ac52b6`

## Result

- Status: **pass**
- Evidence window: `2026-07-25T01:28:08Z` to `2026-07-25T01:42:04Z`
- Total duration: `836` seconds
- Context: `kind-synara-stage4-resilience-20260725-proof`
- Evidence schema: `synara.kubernetes.resilience.acceptance.v1`
- Raw evidence: [stage-4-kind-resilience-acceptance-06ac52b6.json](stage-4-kind-resilience-acceptance-06ac52b6.json)
- Raw evidence SHA-256: `06ac52b6405a1a44a4f9cb11b3098e7f10c59e1db134b1effba28ac05731ffb9`
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`

This is a reproducible local pre-production acceptance result from the current dirty Stage 4 worktree. It is not
a clean-commit release gate, a pushed artifact, or deployment evidence.

## Frozen acceptance inputs

| Asset | SHA-256 |
| --- | --- |
| `deploy/kubernetes/acceptance.sh` | `001a3fc6c981f9898cd862eba45632922f15567968c2138238b85de0b5232191` |
| `deploy/kubernetes/resilience-acceptance.sh` | `9ebdca44c857b7d91964de493496cb68468771d00c493b95bd7d688d5dc60220` |
| `deploy/kubernetes/kind-resilience-acceptance.sh` | `0d2d1b748b00c6909efacc2d5af890567420ab6522c192a46ac0df9278f440b0` |
| `deploy/kubernetes/kind-multinode.yaml` | `4127f63cd7a80bbaa85988fed9670b960d0f9ec8acbc551219fd81a3e982ceda` |

The hashes were captured before the proof and verified unchanged after it completed.

## Baseline and resilience matrix

The Stage 2 two-replica baseline passed in `90` seconds before the additive resilience lane ran.

| Scenario | Status | Readiness evidence |
| --- | --- | --- |
| `rbac` | pass | Required least-privilege grants present; destructive Namespace deletion absent |
| `topology` | pass | 4 nodes, 3 workers, 2 Control Plane Pods on distinct hosts, PDB `minAvailable=1` |
| `control-plane-failover` | pass | Replacement Pod became ready; `0` probe failures |
| `node-drain` | pass | Replacement moved off the cordoned node; `0` probe failures |
| `node-partition` | pass | 20-second Kind network partition recovered; `1` transient probe failure, threshold `2` |

Case totals were `passed=5`, `failed=0`, `skipped=0`; no skip allowlist was configured. PostgreSQL and MinIO
were pinned to the labeled dependency Worker, leaving one safe Control Plane target for drain and partition tests.

## Soak

- Configured duration: `600` seconds
- Actual duration: `602` seconds
- Disruption cycles: `10`
- Passed cycles: `10`
- Failed cycles: `0`
- Required skipped cycles: `0`
- Idle readiness probe failures: `0`

The evidence records actual elapsed time rather than echoing only the configured duration.

## Cleanup

- The owned Kind cluster was deleted; `kind get clusters` returned no clusters after the run.
- The exact test-built image `synara-control-plane:stage2-acceptance` with image ID
  `sha256:10c084216554de7b15f04a07845f4dc49082c2b5a7305f0c9864730648c6fba8` had no container references and was
  removed after evidence capture.
- No broad Docker prune was used.

## Evidence boundary

This pass closes the checked-in disposable Kind RBAC, topology, Pod replacement, node drain, bounded node
partition, cleanup, and ten-minute soak lane. It does not prove a managed Kubernetes provider's Node controller,
storage, load balancer, availability-zone partition, TokenReview outage behavior, or production-duration soak.
Those remain deployment-environment acceptance work.
