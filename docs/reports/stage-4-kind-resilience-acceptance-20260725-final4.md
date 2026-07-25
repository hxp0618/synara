# Stage 4 disposable Kind resilience acceptance — 2026-07-25 final4

This report supersedes the final3 Kind proof. Final4 adds an atomic, exact-holder Reconciler leader takeover scenario,
fail-closed scenario reporting, bounded Kubernetes API transport retries, and the current `67`-migration Stage 4 image.

## Result

- Status: **pass**
- Evidence window: `2026-07-25T12:07:09Z` to `2026-07-25T12:21:18Z`
- Total duration: `849` seconds
- Baseline duration: `69` seconds
- Context: `kind-synara-stage4-final4-20260725`
- Evidence schema: `synara.kubernetes.resilience.acceptance.v1`
- Raw evidence: [stage-4-kind-resilience-acceptance-20260725-final4.json](stage-4-kind-resilience-acceptance-20260725-final4.json)
- Raw evidence SHA-256: `d376ec9c5d76b635f534d65e41a7339c567824fd8707e73cf272808070bb6e4c`
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- Test image: `synara-control-plane:stage4-kind-final4-20260725`
- Test image ID: `sha256:064253b900734a262a048ba0ad2fc5dd9116249193648977e97e81394d5b4fd5`

The image ID is identical to the paired OrbStack proof, so both Kubernetes lanes exercised the same executable source
snapshot. This is reproducible local pre-production evidence, not a clean-commit release gate, pushed artifact, or
managed-cloud deployment.

## Frozen acceptance inputs

| Asset | SHA-256 |
| --- | --- |
| `deploy/kubernetes/acceptance.sh` | `476b38842dc09d416b4abbebb30e12916c06270f936bf22402b07faa96c6d3ac` |
| `deploy/kubernetes/resilience-acceptance.sh` | `613cb224360b3697c775257d3fb4c70d2955d80eac25d85e7a247965b527e047` |
| `deploy/kubernetes/kind-acceptance.sh` | `431bca972be151950762edbbb937d23dbbf6a02dbebb3f37dc51957135925d80` |
| `deploy/kubernetes/kind-resilience-acceptance.sh` | `0d2d1b748b00c6909efacc2d5af890567420ab6522c192a46ac0df9278f440b0` |
| `deploy/kubernetes/kind-multinode.yaml` | `4127f63cd7a80bbaa85988fed9670b960d0f9ec8acbc551219fd81a3e982ceda` |
| `deploy/kubernetes/validate-resilience-assets.py` | `01fb834d52380b2577309cec14ce089556944ec3209c6274d2130cc962b16a50` |
| `services/control-plane/Dockerfile` | `cd8747438e2b1ef24bbcaf23f58a4cd589e5e89776f319052fd8ee3afe561a4c` |

The hashes were captured after the proof and before exact image cleanup; the acceptance assets were unchanged during
the run.

## Baseline and six-scenario matrix

The baseline passed with two Control Plane replicas and `67` applied schema migrations. All `28/28` positive and
negative RBAC expectations matched.

| Scenario | Status | Evidence |
| --- | --- | --- |
| `rbac` | pass | Required grants present; destructive deletion and Pod update/watch denied |
| `topology` | pass | 4 nodes, 3 Workers, 2 Control Plane Pods on distinct hosts, PDB `minAvailable=1` |
| `leader-takeover` | pass | Exact holder changed; fencing token `3 -> 4`; `0` readiness failures |
| `control-plane-failover` | pass | Replacement scheduled on another Worker; `0` readiness failures |
| `node-drain` | pass | Pod moved off the cordoned Worker; node uncordoned; `0` readiness failures |
| `node-partition` | pass | 20-second Docker-network partition recovered; node Ready before/after; `0` readiness failures |

PostgreSQL and MinIO were pinned to the dependency Worker. The two initial Control Plane Pods ran on distinct Workers,
leaving two safe disruption targets. The active `synara:kubernetes-execution-reconciler` holder changed from
`synara-control-plane-cdb5dcbd6-bhb2r` to `synara-control-plane-cdb5dcbd6-dq29h`, and its fencing token advanced
exactly once. Case totals were `passed=6`, `failed=0`, `skipped=0`; the skip allowlist was empty and
`unexpectedExitStatus` was `null`.

## Soak

- Configured duration: `600` seconds
- Actual duration: `601` seconds
- Disruption cycles: `10`
- Passed cycles: `10`
- Failed or skipped cycles: `0`
- Idle readiness probe failures: `0`
- Per-cycle readiness probe failures: `0` for all ten cycles

Each cycle deleted a live Control Plane Pod, waited for replacement and steady-state readiness, and then continued only
after the zero-failure threshold was satisfied.

## Cleanup

- The owned Kind cluster `synara-stage4-final4-20260725` was deleted.
- `kind get clusters` returned `No kind clusters found.` after the run.
- No container referenced the final4 test image after cluster deletion.
- No broad Docker prune or unrelated Kubernetes cleanup was used.

## Evidence boundary

This pass proves the checked-in disposable Kind topology, least-privilege RBAC, database-backed leader fencing, exact
leader takeover, Pod replacement, node drain, bounded node partition, cleanup, and ten-minute chaos-soak lane against
the current executable source snapshot. It does not prove a managed Kubernetes provider's Node controller, storage,
load balancer, workload identity, availability-zone partition, cross-region control plane, or production-duration soak.
