# Stage 4 OrbStack Kubernetes acceptance — 2026-07-25 (migrations 65)

This report supersedes the earlier local OrbStack note in
`stage-4-orbstack-kubernetes-acceptance-20260725.md`. That earlier pass was useful evidence for the first
single-node real-kubelet path, but it was taken before the current `migrations=65` worktree and before the final
acceptance harness fixes in `deploy/kubernetes/acceptance.sh`.

## Result

- Status: **pass**
- Completed at: `2026-07-25T05:11:38Z`
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, `linux/arm64`
- Topology: one Ready local control-plane node
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- Final `deploy/kubernetes/acceptance.sh` SHA-256:
  `596112cbf4401c198cd2c35daf1bb3fc6adb7ddb82d3857fbcaa32d1967f6360`
- Test image tag: `synara-control-plane:stage4-orbstack-20260725-rerun1`
- Test image ID:
  `sha256:28df18dac5a8d22f95440c1798b1cc079c6c054cbe3aa6bdc565f09377a71837`

This is still a local real-kubelet acceptance result from the current dirty Stage 4 worktree. It is not a clean
commit release gate, a pushed artifact, or a managed-cloud production deployment.

## Passed behavior

- PostgreSQL, MinIO, bucket initialization, and two Synara Control Plane replicas became Ready.
- Deleting one Control Plane Pod caused zero `/ready` probe failures during replacement.
- A registered Worker token remained valid after Control Plane Pod replacement.
- A complete PostgreSQL outage recovered and both Control Plane replicas returned Ready.
- A complete MinIO outage recovered, the bucket initialization job completed again, and both replicas returned Ready.
- The Control Plane log audit found none of the generated database, object-store, Worker-registration,
  Provider-cursor, credential-master, or Worker-token secrets.
- The least-privilege spot audit passed, including `tokenreviews.create` and the denial of Namespace and Secret
  deletion to the reconciler ServiceAccount.
- The deployed database reported migration count `65`.

## Acceptance harness races found and closed

This final pass closed two more acceptance-harness races beyond the earlier ready-pod log-selection fix:

1. After deleting one Control Plane Pod, the gate was incorrectly using `kubectl rollout status deployment/...`
   to wait for a non-rollout steady-state recovery. That watch can fail with `object has been deleted` even when
   the intended invariant is simply “return to two Ready replicas”. The gate now waits for exact steady-state Ready
   replica recovery instead.
2. The gate cleanup intentionally deletes the fixed `synara-system` namespace asynchronously. An immediate rerun
   could recreate resources while the previous namespace deletion was still in progress, and Kubernetes would then
   garbage-collect the second run mid-flight. The gate now waits for the fixed namespace to be fully absent before
   creating a fresh run and refuses to reuse a live non-terminating namespace.

The static validator `deploy/kubernetes/validate-resilience-assets.py` still passes against the final script.

## Cleanup

- The gate-owned `synara-system` Namespace was deleted and verified absent after the pass.
- The gate-owned `synara-control-plane-reconciler` ClusterRole and ClusterRoleBinding were deleted and verified absent.
- No container references remain for image ID
  `sha256:28df18dac5a8d22f95440c1798b1cc079c6c054cbe3aa6bdc565f09377a71837`.
- The local OrbStack Kubernetes cluster itself was preserved.
- No broad Docker or Kubernetes cleanup was used.

The exact image digest is still present locally because the same image currently also carries the reusable
`synara-control-plane:stage2-acceptance` tag for the pending Kind rerun. That is local-only state, not deployed
runtime state.

## Evidence boundary

This pass proves the shipped manifests and acceptance behavior against OrbStack's real local Kubernetes API,
kubelet, scheduling, storage, ServiceAccount, TokenReview, and RBAC paths for the current `migrations=65` worktree.
Because OrbStack exposes one local node, it still cannot prove multi-node drain, node partition, availability-zone
failure, or managed load-balancer behavior. Those multi-node smoke cases remain complementary with the disposable
[Kind resilience report](stage-4-kind-resilience-acceptance-06ac52b6.md); managed-cloud and long-duration production
soak remain external deployment gates.
