# Stage 4 OrbStack Kubernetes acceptance — 2026-07-25 final

This report supersedes `stage-4-orbstack-kubernetes-acceptance-20260725-m65.md`. The earlier migrations-65 pass was
taken before the final Recovery Bundle Artifact authority and fork-lineage fixes.

## Result

- Status: **pass**
- Evidence window: `2026-07-25T06:01:18Z` to `2026-07-25T06:03:17Z`
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, `linux/arm64`
- Topology: one Ready local control-plane node
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- `deploy/kubernetes/acceptance.sh` SHA-256: `596112cbf4401c198cd2c35daf1bb3fc6adb7ddb82d3857fbcaa32d1967f6360`
- Test image: `synara-control-plane:stage4-orbstack-final2-20260725`
- Test image ID: `sha256:b90efaa91bcdd81c6926618df0209f7aa2499c3c55febc3831c1355cd81a07c6`

The image ID is identical to the final Kind proof, so both Kubernetes lanes exercised the same built source snapshot.
This remains local real-kubelet evidence from a dirty worktree, not a pushed artifact or managed-cloud deployment.

## Passed behavior

- PostgreSQL, MinIO, bucket initialization, and two Synara Control Plane replicas became Ready.
- Deleting one Control Plane Pod caused zero `/ready` probe failures during replacement.
- A registered Worker token remained valid after Control Plane Pod replacement.
- Complete PostgreSQL and MinIO outages both recovered.
- Generated database, object-store, Worker-registration, Provider-cursor, credential-master, and Worker-token secrets
  were absent from the Control Plane log audit.
- The least-privilege RBAC spot audit passed, including `tokenreviews.create` and denial of Namespace/Secret deletion.
- The deployed database reported migration count `65`.

## Cleanup

- The gate-owned `synara-system` Namespace was verified absent after asynchronous deletion completed.
- The gate-owned reconciler ClusterRole and ClusterRoleBinding were verified absent.
- The image had no container references and its exact test tags were removed.
- The OrbStack Kubernetes cluster was preserved and its node remained Ready.
- No broad Docker or Kubernetes cleanup was used.

## Evidence boundary

This pass proves the final manifests against OrbStack's local Kubernetes API, kubelet, scheduling, storage,
ServiceAccount, TokenReview, and RBAC paths. OrbStack is single-node, so the complementary final Kind report provides
the multi-node drain, partition, and ten-minute soak evidence. Managed-cloud multi-AZ and production-duration soak
remain external deployment gates.
