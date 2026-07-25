# Kubernetes enterprise control plane

This Kustomize base runs two stateless control-plane replicas and grants only the
cluster permissions required by the managed Kubernetes Execution Target reconciler.
PostgreSQL, S3, ingress/TLS, and AWS workload identity remain operator-managed.
Set `trusted-proxy-cidrs` to only the ingress or load-balancer network ranges that append
`X-Forwarded-For`; leaving it empty records the direct peer address and ignores forwarded client IPs.

Create the runtime configuration and Secret without committing either generated file:

```bash
cp deploy/kubernetes/config.example.yaml /tmp/synara-config.yaml
cp deploy/kubernetes/secret.example.yaml /tmp/synara-secret.yaml
# Edit both files, then apply them from /tmp or a secret manager.
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -f /tmp/synara-config.yaml -f /tmp/synara-secret.yaml
kubectl apply -k deploy/kubernetes
```

Before applying, replace `synara-control-plane:local` in `deployment.yaml` with an
immutable image digest available to the cluster. Configure AWS IRSA, EKS Pod Identity,
or the equivalent workload identity on the `synara-control-plane` ServiceAccount so
the process can access S3 and the configured KMS key without static AWS credentials.

The ClusterRole is cluster-scoped because a Target may request a dedicated Namespace.
If every Target uses an operator-created Namespace with `manageNamespace=false`, create
equivalent namespaced Roles for those Namespaces and remove Namespace management from
the target configuration before narrowing the supplied ClusterRole.

`deploy/kubernetes/rbac.yaml` is intentionally least-privilege for the current
implementation surface: namespace apply, managed Pod lifecycle, namespaced
foundation objects, and `authentication.k8s.io` `TokenReview` for workload
identity verification.

Managed Worker Pods get a separate ServiceAccount with token automount disabled. The
control-plane ServiceAccount token is used only by the reconciler and is never copied
into Worker Pods or execution events.

`GET /metrics` is available on the existing `http` Service port. Prometheus Operator
users can apply the optional `deploy/kubernetes/monitoring` ServiceMonitor and alert
rules after installing the required CRDs; they are not included in the base so a
plain Kubernetes cluster can still apply the Kustomization.

## Disposable Kind acceptance

Run the Stage 2 two-replica and failure acceptance in a disposable Kind cluster:

```bash
KIND_BIN=/path/to/kind deploy/kubernetes/kind-acceptance.sh
```

The wrapper builds and loads the current Control Plane plus pinned PostgreSQL/MinIO images, creates PVC-backed
test dependencies, applies this Kustomize base, verifies two Ready replicas and migration uniqueness, deletes one
Control Plane Pod, interrupts PostgreSQL and MinIO, verifies Worker-token continuity, checks RBAC and scans Pod
logs for random secret Sentinels. The cluster and test resources are deleted on exit.

## Disposable Kind resilience lane

Run the additive multi-node resilience acceptance in a disposable Kind cluster:

```bash
KIND_BIN=/path/to/kind deploy/kubernetes/kind-resilience-acceptance.sh
```

The resilience lane reuses the existing Stage 2 bootstrap, then records JSON
evidence for:

- least-privilege RBAC, including `TokenReview`, Namespace apply, and Pod operations;
- multi-node Kind topology and control-plane spread;
- PostgreSQL-backed reconciler Leader Election by deleting the exact current
  Kubernetes reconciler holder and requiring a different Ready Pod at the next
  fencing-token epoch;
- control-plane failover with optional pre/post verification hooks;
- bounded node-drain simulation by cordoning a node and replacing only the
  targeted control-plane Pod;
- bounded Kind-only node partition smoke by disconnecting a selected node
  container from its Docker network and reconnecting it by default;
- an explicit non-Kind node-partition adapter point driven by
  `SYNARA_K8S_NODE_PARTITION_START_HOOK`,
  `SYNARA_K8S_NODE_PARTITION_STOP_HOOK` when
  `SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1`;
  `SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS` and
  `SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS` independently bound
  disruption and heal execution, while
  `SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS` remains the shared fallback
  for operators using the original hook contract;
- an optional bounded soak loop driven by `SYNARA_K8S_RESILIENCE_SOAK_SECONDS`.

The multi-node wrapper pins PostgreSQL and MinIO to the dedicated
`synara.io/resilience-dependency=true` Worker before their PVCs bind, leaving a
safe Control Plane node for both node-level scenarios. Required cases fail the
run when they cannot execute; only cases explicitly named in
`SYNARA_K8S_RESILIENCE_ALLOW_SKIPPED_CASES` may be skipped without failing the
overall evidence.

The final evidence report remains the existing JSON document written to
`SYNARA_K8S_RESILIENCE_EVIDENCE_FILE` (or a temporary path for non-soak runs).
The harness also keeps two additive sidecars beside that path:

- `<evidence>.journal.jsonl`: append-only progress events for baseline
  completion, top-level case completion, soak-cycle completion, and final
  completion. It records timestamps, status, case or cycle identity, and only
  bounded detail summaries or digests.
- `<evidence>.partial.json`: the latest running snapshot, updated atomically in
  place and left behind on failures or interrupts for inspection.

When `SYNARA_K8S_RESILIENCE_SOAK_SECONDS` is greater than `0`, you must set
`SYNARA_K8S_RESILIENCE_EVIDENCE_FILE=/path/report.json` explicitly; soak runs
may not fall back to an anonymous temporary file.

Optional failover hook commands are still supported, but recorded hook evidence
redacts command/stdout/stderr content and retains digests plus byte counts
instead, so stored evidence does not leak hook secrets.
Managed node-partition hooks use the same redaction policy. Timeout handling
terminates the hook process group best-effort; operators should keep the stop
hook idempotent and safe to retry.

For non-Kind clusters, node partition remains an external adapter point rather
than a shipped cloud fault injector. The harness can prove that Synara called
the configured start/stop hooks and reacted to their success or failure, but it
does not claim real cloud or AZ-level fault validation from this repository
alone.

Use `deploy/kubernetes/resilience-acceptance.sh --dry-run` to emit the planned
cases without touching a cluster, and `deploy/kubernetes/validate-resilience-assets.sh`
for static validation of the manifests and scripts.

`deploy/kubernetes/acceptance.sh` defaults to Kind contexts and refuses other clusters. Running it against an
explicitly disposable non-Kind cluster requires `SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1`; the script deletes
the `synara-system` Namespace and supplied ClusterRole resources during cleanup, so never use that override on a
shared or production cluster.
