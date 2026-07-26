# Stage 4 OrbStack Kubernetes resilience acceptance — 2026-07-25 final4

This report supersedes the earlier OrbStack final4 attempts. The final run used the fail-closed scenario harness: an
individual command failure is recorded as a failed scenario, unexpected harness termination writes a failed report,
and Kubernetes API transport failures receive only a short bounded retry. A service response that is not Ready still
fails immediately.

## Result

- Status: **pass**
- Evidence window: `2026-07-25T12:00:37Z` to `2026-07-25T12:05:29Z`
- Total duration: `292` seconds
- Baseline duration: `188` seconds
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, local single-node OrbStack cluster
- Raw evidence: [stage-4-orbstack-resilience-acceptance-20260725-final4.json](stage-4-orbstack-resilience-acceptance-20260725-final4.json)
- Raw evidence SHA-256: `e77b00335bc1e896f670ffdf867044c78f360e94354389b4fbf7f111ea5a7b72`
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- Test image: `synara-control-plane:stage4-orbstack-final4-20260725`
- Test image ID: `sha256:064253b900734a262a048ba0ad2fc5dd9116249193648977e97e81394d5b4fd5`

This is local real-kubelet evidence from the dirty Stage 4 worktree. It is not a pushed artifact, managed-cloud
deployment, or production release gate.

## Frozen acceptance inputs

| Asset                                             | SHA-256                                                            |
| ------------------------------------------------- | ------------------------------------------------------------------ |
| `deploy/kubernetes/acceptance.sh`                 | `476b38842dc09d416b4abbebb30e12916c06270f936bf22402b07faa96c6d3ac` |
| `deploy/kubernetes/resilience-acceptance.sh`      | `613cb224360b3697c775257d3fb4c70d2955d80eac25d85e7a247965b527e047` |
| `deploy/kubernetes/validate-resilience-assets.py` | `01fb834d52380b2577309cec14ce089556944ec3209c6274d2130cc962b16a50` |
| `services/control-plane/Dockerfile`               | `cd8747438e2b1ef24bbcaf23f58a4cd589e5e89776f319052fd8ee3afe561a4c` |

The hashes were captured after the proof and before exact image cleanup; the acceptance assets were unchanged during
the run.

## Baseline and resilience evidence

- PostgreSQL, MinIO, bucket initialization, and two Control Plane replicas became Ready.
- The deployed database reported `67` applied schema migrations.
- A normal Control Plane Pod replacement caused `0` readiness probe failures, and the registered Worker token remained
  valid.
- Complete PostgreSQL and MinIO outages recovered.
- Sensitive generated credentials were absent from the Control Plane log audit.
- All `28/28` positive and negative RBAC expectations matched. Required create/patch permissions were present while
  Namespace, Secret, ServiceAccount, ResourceQuota, and NetworkPolicy deletion plus Pod update/watch were denied.
- The active `synara:kubernetes-execution-reconciler` holder changed from
  `synara-control-plane-78d6bd9d85-st7dc` to `synara-control-plane-78d6bd9d85-jcc9q` after the exact holder Pod was
  deleted under the advisory-lock guard.
- The reconciler fencing token advanced exactly once, from `3` to `4`; leader takeover recorded `0` readiness probe
  failures.
- The independent Control Plane failover case replaced `synara-control-plane-78d6bd9d85-f67nz` with
  `synara-control-plane-78d6bd9d85-dt547` and recorded `0` readiness probe failures.
- Case totals were `passed=3`, `failed=0`, `skipped=0`; `unexpectedExitStatus` was `null`.

## Cleanup

- The gate-owned `synara-system` Namespace was verified absent after asynchronous deletion completed.
- The gate-owned reconciler ClusterRole and ClusterRoleBinding were verified absent.
- The OrbStack Kubernetes node remained Ready.
- The reusable OrbStack Kubernetes cluster was preserved; no broad Docker or Kubernetes cleanup was used.

## Evidence boundary

This pass proves the current two-replica manifests, RBAC contract, database-backed leader lease fencing, exact leader
takeover, Pod replacement, dependency outage recovery, and cleanup paths against OrbStack's real local Kubernetes API
and kubelet. OrbStack is single-node, so it cannot prove cross-node placement, drain, partition, or a meaningful long
soak. Those are covered by the paired disposable Kind final4 report. Managed-cloud multi-AZ behavior and
production-duration soak remain external deployment gates.
