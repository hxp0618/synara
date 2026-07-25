# Stage 4 OrbStack managed-Kubernetes routing publisher acceptance — 2026-07-25

## Result

- Status: **pass**
- Completed at: `2026-07-25T08:15:10Z`
- Kubernetes context and node: `orbstack`, `linux/arm64`, kubelet `v1.34.8+orb1`
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- Test: `TestKubernetesReconcilerAgainstRealAPIServer`

This is local real-API-server and real-kubelet evidence from the dirty Stage 4 worktree. It is not a clean-commit
release gate, a pushed artifact, or a managed-cloud deployment.

## Passed behavior

- A temporary acceptance ServiceAccount reached the real OrbStack Kubernetes API with a short-lived TokenRequest
  credential, the Context's pinned CA certificate, and an explicit allowlist of only the resource kinds needed by the
  reconciler and cleanup.
- One tenant-owned managed Kubernetes Execution Target reconciled its real Namespace, ServiceAccount, registry Secret,
  ResourceQuota, NetworkPolicy, and Worker Pod.
- The same successful per-Target reconcile invoked the routing publisher. No independent loop refreshed health from a
  stale Target status. Production wiring invokes this path from the leader-scoped Kubernetes reconciler; this focused
  real-API test called `ReconcileOnce` directly and does not independently prove Leader Election.
- The persisted `execution_target_health` row was `healthy`, `saturated`, and reported one Pod-slot ceiling and one
  allocated unit: `availableCapacityUnits=1` is the total ceiling, not one remaining free slot. Its source was
  `managed-kubernetes-routing-publisher:real-api-integration`, and its expiry was exactly 90 seconds after the
  reconcile-completion observation.
- No `execution_target_dr_readiness` row was created. Artifact, Checkpoint, and Memory replication readiness therefore
  remains on the external replication/operator authority boundary.

## Finding fixed during the proof

The first live run reached the real API successfully but exposed a stale integration assertion: it expected the old
Worker registration Secret name even though registration now uses projected workload identity and the remaining Secret
is target-scoped registry configuration. The test now checks the current ServiceAccount, registry Secret, ResourceQuota,
and NetworkPolicy names. The second live run passed.

## Cleanup

- The exact bootstrap Namespace, ServiceAccount, ClusterRole, and ClusterRoleBinding were deleted by the test trap.
- The randomly named managed Target Namespace was deleted by the integration-test cleanup.
- No gate-owned Namespace or cluster-scoped RBAC object remained afterward.
- The OrbStack Kubernetes cluster was retained, and node `orbstack` remained `Ready`.

## Evidence boundary

This result proves automatic health and capacity publication from a real successful Kubernetes reconcile. Focused unit
tests cover failure publication and TTL expiry semantics, while separate leadership tests cover the production runner.
The capacity is configured Pod-slot occupancy, not cloud node/zone headroom. The local single-node cluster does not prove
managed-cloud multi-AZ health, external storage replication, production-duration soak, or a production routing/failover
incident.
