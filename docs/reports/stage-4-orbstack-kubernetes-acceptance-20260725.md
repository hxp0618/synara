# Stage 4 OrbStack Kubernetes acceptance — 2026-07-25

## Result

- Status: **pass**
- Completed at: `2026-07-25T01:53:12Z`
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, `linux/arm64`
- Topology: one Ready local control-plane node
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- Final `deploy/kubernetes/acceptance.sh` SHA-256:
  `73f6359713852db1a7c2db19fc788823478b09de5f935607896839082f87edc0`

This is a local real-kubelet acceptance result from the current dirty Stage 4 worktree. It is not a clean-commit
release gate, a pushed artifact, or a managed-cloud production deployment.

## Passed behavior

- PostgreSQL, MinIO, bucket initialization, and two Synara Control Plane replicas became Ready.
- Deleting one Control Plane Pod caused zero `/ready` probe failures during replacement.
- A registered Worker token remained valid after Control Plane Pod replacement.
- A complete PostgreSQL outage recovered and both Control Plane replicas returned Ready.
- A complete MinIO outage recovered, the bucket initialization job completed again, and both replicas returned
  Ready.
- The Control Plane log audit found none of the generated database, object-store, Worker-registration,
  Provider-cursor, credential-master, or Worker-token secrets.
- The least-privilege spot audit passed, including `tokenreviews.create` and the denial of Namespace and Secret
  deletion to the reconciler ServiceAccount.
- The deployed database reported migration count `64`.

## Runtime race found and closed

The first OrbStack run reached a successful two-replica rollout but the acceptance log audit selected a
`ContainerCreating` Pod and called `kubectl logs` immediately. The gate failed even though the Deployment was
Ready. The script now uses one shared selector that excludes terminating and non-Ready Pods, and both before/after
log audits use `--pod-running-timeout=60s`. The static Kubernetes asset validator freezes these requirements. The
second run passed the full acceptance flow.

## Cleanup

- The gate-owned `synara-system` Namespace was deleted.
- The gate-owned `synara-control-plane-reconciler` ClusterRole and ClusterRoleBinding were deleted.
- The exact test image `synara-control-plane:stage4-orbstack-20260725` with image ID
  `sha256:10c084216554de7b15f04a07845f4dc49082c2b5a7305f0c9864730648c6fba8` had no container references and was
  removed.
- The OrbStack Kubernetes cluster itself was preserved.
- No broad Docker or Kubernetes cleanup was used.

## Evidence boundary

This pass proves the shipped manifests and acceptance behavior against OrbStack's real local Kubernetes API,
kubelet, scheduling, storage, ServiceAccount, TokenReview, and RBAC paths. Because OrbStack exposes one local node,
it cannot prove multi-node drain, node partition, availability-zone failure, or managed load-balancer behavior.
Those multi-node smoke cases are covered separately by the
[disposable Kind resilience report](stage-4-kind-resilience-acceptance-06ac52b6.md); managed-cloud and long-duration
production soak remain external deployment gates.
