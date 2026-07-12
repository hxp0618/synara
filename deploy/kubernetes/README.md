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

`deploy/kubernetes/acceptance.sh` defaults to Kind contexts and refuses other clusters. Running it against an
explicitly disposable non-Kind cluster requires `SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1`; the script deletes
the `synara-system` Namespace and supplied ClusterRole resources during cleanup, so never use that override on a
shared or production cluster.
