# Stage 4 OrbStack Kubernetes acceptance — 2026-07-25 final3

This report supersedes `stage-4-orbstack-kubernetes-acceptance-20260725-final.md`. The prior pass used the image
from before the final Resume Snapshot Artifact budget and content-hash authority corrections.

## Result

- Status: **pass**
- Evidence window: `2026-07-25T06:30:25Z` to `2026-07-25T06:32:22Z`
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, `linux/arm64`
- Topology: one Ready local control-plane node
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- `deploy/kubernetes/acceptance.sh` SHA-256: `596112cbf4401c198cd2c35daf1bb3fc6adb7ddb82d3857fbcaa32d1967f6360`
- Test image: `synara-control-plane:stage4-orbstack-final3-20260725`
- Test image ID: `sha256:1912648df2a83fb1ab836ba0ffe6529fc68502db2ea20cff1212a7a52ef18f5e`

The image ID is identical to the final3 Kind proof, so both Kubernetes lanes exercised the same executable source
snapshot. This is local real-kubelet evidence from a dirty worktree, not a pushed artifact or managed-cloud deployment.

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
- The image had no container references; its exact test tag was removed after the paired Kind proof completed.
- The OrbStack Kubernetes cluster was preserved and its node remained Ready.
- No broad Docker or Kubernetes cleanup was used.

## Evidence boundary

This pass proves the current manifests against OrbStack's local Kubernetes API, kubelet, scheduling, storage,
ServiceAccount, TokenReview, and RBAC paths. OrbStack is single-node, so the complementary final3 Kind report provides
the multi-node drain, partition, and ten-minute soak evidence. Managed-cloud multi-AZ and production-duration soak
remain external deployment gates.
