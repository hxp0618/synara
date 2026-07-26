# Stage 4 disposable Kind resilience acceptance — 2026-07-25 final3

This report supersedes `stage-4-kind-resilience-acceptance-20260725-final.md`. The prior pass used the image from
before the final Resume Snapshot Artifact budget and content-hash authority corrections.

## Result

- Status: **pass**
- Evidence window: `2026-07-25T06:34:14Z` to `2026-07-25T06:47:51Z`
- Total duration: `817` seconds
- Context: `kind-synara-stage4-final3-20260725`
- Evidence schema: `synara.kubernetes.resilience.acceptance.v1`
- Raw evidence: [stage-4-kind-resilience-acceptance-20260725-final3.json](stage-4-kind-resilience-acceptance-20260725-final3.json)
- Raw evidence SHA-256: `6e2acbdd7b1278555292541484a2728fa8473a01535263269c19de819dab9270`
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- Test image: `synara-control-plane:stage4-kind-final3-20260725`
- Test image ID: `sha256:1912648df2a83fb1ab836ba0ffe6529fc68502db2ea20cff1212a7a52ef18f5e`

This is a reproducible local pre-production result from the dirty Stage 4 worktree. It is not a clean-commit release
gate, a pushed artifact, or deployment evidence.

## Frozen acceptance inputs

| Asset                                             | SHA-256                                                            |
| ------------------------------------------------- | ------------------------------------------------------------------ |
| `deploy/kubernetes/acceptance.sh`                 | `596112cbf4401c198cd2c35daf1bb3fc6adb7ddb82d3857fbcaa32d1967f6360` |
| `deploy/kubernetes/resilience-acceptance.sh`      | `9ebdca44c857b7d91964de493496cb68468771d00c493b95bd7d688d5dc60220` |
| `deploy/kubernetes/kind-acceptance.sh`            | `431bca972be151950762edbbb937d23dbbf6a02dbebb3f37dc51957135925d80` |
| `deploy/kubernetes/kind-resilience-acceptance.sh` | `0d2d1b748b00c6909efacc2d5af890567420ab6522c192a46ac0df9278f440b0` |
| `deploy/kubernetes/kind-multinode.yaml`           | `4127f63cd7a80bbaa85988fed9670b960d0f9ec8acbc551219fd81a3e982ceda` |
| `deploy/kubernetes/validate-resilience-assets.py` | `bc1b5e35f1a6e67330d87eb809eb39fa49ab52bf970814f8714c3a5671c0210b` |
| `services/control-plane/Dockerfile`               | `cd8747438e2b1ef24bbcaf23f58a4cd589e5e89776f319052fd8ee3afe561a4c` |

The hashes were captured before the proof and verified unchanged afterward.

## Baseline and resilience matrix

The two-replica baseline passed in `72` seconds with schema migration count `65`. All `28/28` positive and negative
RBAC expectations matched.

| Scenario                 | Status | Readiness evidence                                                                         |
| ------------------------ | ------ | ------------------------------------------------------------------------------------------ |
| `rbac`                   | pass   | Required grants present; destructive Namespace/Secret deletion and Pod update/watch absent |
| `topology`               | pass   | 4 nodes, 3 workers, 2 Control Plane Pods on distinct hosts, PDB `minAvailable=1`           |
| `control-plane-failover` | pass   | Replacement Pod became Ready; `0` probe failures                                           |
| `node-drain`             | pass   | Replacement moved off the cordoned node; `0` probe failures                                |
| `node-partition`         | pass   | 20-second Kind network partition recovered; `1` transient probe failure, threshold `2`     |

Case totals were `passed=5`, `failed=0`, `skipped=0`; the skip allowlist was empty. PostgreSQL and MinIO were pinned
to the dependency Worker, leaving two safe Control Plane targets for bounded disruption.

## Soak

- Configured duration: `600` seconds
- Actual duration: `600` seconds
- Disruption cycles: `10`
- Passed cycles: `10`
- Failed cycles: `0`
- Required skipped cycles: `0`
- Idle readiness probe failures: `0`
- Per-cycle readiness probe failures: `0` for all 10 cycles

## Cleanup

- The owned Kind cluster was deleted and `kind get clusters` returned no clusters.
- The final image had no container references after cluster deletion.
- The exact final3 Kind and OrbStack test image tags and their unreferenced shared image layer were removed; no Docker
  prune was used.

## Evidence boundary

This pass proves the checked-in disposable Kind RBAC, topology, Pod replacement, node drain, bounded node partition,
cleanup, and ten-minute soak lane against the current executable source snapshot. It does not prove a managed
Kubernetes provider's Node controller, storage, load balancer, availability-zone partition, or production-duration soak.
