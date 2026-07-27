# Stage 4 self-hosted dual-cluster pressure and chaos acceptance — 2026-07-27 final7

Status: **PASS**
Evidence level: local real-Kubernetes/PostgreSQL mechanics evidence
Machine-readable evidence: [`final7.json`](stage-4-dual-cluster-pressure-chaos-20260727-final7.json)
Evidence SHA-256: `1af60582c6934308e034d26c5978e92991f9a138f1eddc8e67d66d6611bf6c5b`

## Scope and topology

- Primary Worker cluster: OrbStack Kubernetes `v1.34.8+orb1`, one real node.
- Secondary Worker cluster: run-owned disposable Kind `v1.33.1`, one real node and isolated kubeconfig.
- Metadata authority: run-owned PostgreSQL 17 container with all migrations through `000092` applied.
- Control Plane concurrency: two independent `sessions.Service` and Kubernetes Reconciler instances sharing the
  same PostgreSQL authority.
- Worker image: current dirty-worktree `worker-acceptance` image
  `sha256:b9d4bab2caf8dcdfdee23bd71679ae42c916b6070472cee6e8ed1b006b7b54a1`.
- Runtime identity: real Pods and kubelets on both APIs; the bounded Worker API stub invokes the production
  Pod-bound TokenReview/Pod-GET verifier before accepting register/heartbeat/claim.

The source HEAD was `b0182cf6436683a32a25b3905d57c1936d20564e`; the worktree was intentionally dirty. This is not a
clean-SHA or production release claim.

## Workload and disruption

1. A source Execution reached runtime-ready on OrbStack.
2. Missing destination readiness rejected failover without mutation.
3. Exact destination Artifact watermark readiness admitted failover.
4. Two concurrent Control Plane sweeps observed the same PostgreSQL authority and produced exactly one successor;
   any losing candidate converged through an allowlisted stale/conflict result. A repeat sweep found no candidate.
5. The successor became runtime-ready on Kind and the obsolete source Pod UID disappeared.
6. The pressure phase queued eight additional interactive Executions per cluster. Each Target enforced
   `maxActivePods=4`, so two waves were required and peak live capacity remained four Pods per cluster.
7. Three chaos cycles deleted one exact Pod UID in each cluster with an API-server UID precondition. Each of the six
   deleted physical identities was absent before Reconcile, and each logical Execution returned through a different
   Running/Ready Pod UID with zero container restarts.
8. All sixteen pressure Executions reached the test terminal boundary, all Pods were removed, and both cluster
   namespaces were empty before cleanup.

The Go acceptance test passed in `189.95s`. The evidence records all fifteen allowlisted assertions as `true`,
including shared PostgreSQL authority, single successor, Reconciler handoff, pressure, bounded chaos, Pod-bound
identity, source placement immutability, and RecoveryBundle integrity.

## Cleanup and safety

- Final runner status and cleanup status are both `passed`; exit code is `0`.
- The user's current Kubernetes context remained unchanged (it was unset before and after the run).
- The Kind cluster, run-owned PostgreSQL container, Worker image tag, PriorityClasses, Namespaces, ServiceAccounts,
  Roles and bindings were removed only after run-label and immutable identity checks.
- `synara-system` was never touched.
- TokenRequest credentials, Kubernetes API endpoints, CAs and PostgreSQL credentials are absent from final evidence.
- The evidence detail was valid JSON, under 64 KiB, secret-free, and reduced to allowlisted bounded assertions plus a
  SHA-256 digest.

## Evidence boundary

This closes the Stage 4 self-hosted multiple-cluster concurrent-pressure and bounded-chaos gap when combined with the
existing real two-Pod Control Plane/DB/MinIO disruption soak in
[`schema87-final1`](stage-4-orbstack-isolated-schema87-20260727-final1.md). It does not claim geographic RPO/RTO,
production-duration endurance, a multi-node failure domain, full production Worker-row registration in the bounded
stub, cross-site object replication, or managed-cloud availability.

The immutable `final1` through `final6` JSON files retain fail-closed intermediate runs that exposed and then closed
runner RBAC, PostgreSQL fixture-state, concurrent-sweep expectation, terminal-transition, and ResourceQuota mismatches.
They are not passing evidence and are superseded only by this `final7` result.
