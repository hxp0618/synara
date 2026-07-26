# Stage 4 OrbStack + Kind dual-cluster DR acceptance — 2026-07-26 final4

## Result

- Status: **pass**
- Evidence window: `2026-07-25T22:34:46Z` to `2026-07-25T22:37:21Z`
- Run ID: `20260725223446-18106-3826`
- Primary: `orbstack`, Kubernetes `v1.34.8+orb1`, one local node
- Secondary: disposable `kind-synara-dr-20260725223446-18106-3826`, Kubernetes `v1.33.1`, one local node
- Source: `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`, dirty worktree on `codex/saas-tenancy-user`
- Machine-readable evidence:
  [stage-4-dual-cluster-dr-acceptance-20260726-final4.json](stage-4-dual-cluster-dr-acceptance-20260726-final4.json)
- Evidence SHA-256: `7d2041cb318feeeb4b7d2a1e617a774f811a868f2d2aa63a09fb016b15e562fd`

The current Worker image was built from the dirty worktree as
`synara-worker:dual-cluster-20260725223446-18106-3826`. Its Docker config ID and BuildKit manifest digest both equal
`sha256:e0a45e4cc90cd1478d2db72230b1d8b543ac9c6d68617d14eec94962d1d8b8b7`.

## Verified behavior

The production Kubernetes registration verifier called both real API Servers for TokenReview and Pod GET before the
bounded acceptance Worker API issued a token. All ten allowlisted assertions passed:

- Pod-bound workload identity was verified on both clusters, and an unbound Kubernetes API credential was rejected.
- Missing destination readiness failed closed; exact readiness then admitted failover.
- The source placement snapshot remained immutable and the obsolete primary Pod UID disappeared.
- Exactly one successor was committed, repeated recovery was idempotent, and predecessor/bundle/routing/fence lineage
  persisted.
- Source and successor agentd Pods became Running and Ready with registration, heartbeat, and claim activity.
- The persisted RecoveryBundle envelope and SHA-256 were checked before failover.

The bounded detail was 2,244 bytes with SHA-256
`1200e4796052bd9cf9ab7212a2384e6cffd21c5196ddf1fa3ba553d6e8a9c442` and contained no connection material.

## Cleanup and boundary

Cleanup passed. The disposable Kind cluster, run-owned OrbStack Namespace/RBAC, and temporary Worker image were
removed. The user's unset current context stayed unchanged, and the lane did not touch `synara-system`; its two
Control Plane replicas, PostgreSQL, MinIO, and retained Warm Pod remained Ready.

This is local real-API evidence, not cloud DR proof. Both clusters share one Mac and one failure domain. The Worker API
is still a bounded stub around the production identity verifier; production Worker-row persistence is not covered.
Artifact, Checkpoint, Memory, and database replication across independent regions are not verified.

final4 supersedes final3 for the current worktree. It remains local, uncommitted, unpushed, and not released.
