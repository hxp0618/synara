# Polaris Developer Docs Kubernetes base

This Kustomize base deploys the immutable Developer Docs image as two non-root, read-only replicas with readiness,
liveness and disruption protection. It creates only a `ClusterIP` Service in `synara-system`; it does **not** create
an Ingress, DNS record, certificate, public port or cloud load balancer.

Build and push the root Dockerfile's `developer-docs-runtime` target with an immutable revision and the approved
Console origin, then replace `polaris-developer-docs:local` with the resulting Registry digest in a deployment
overlay. Render the base without mutating a cluster:

```sh
kubectl kustomize deploy/kubernetes/developer-docs
```

Copy `ingress.example.yaml` into the deployment authority's private overlay only after the operator provides:

- the approved Developer Docs hostname;
- its ingress class and public load-balancer path;
- a certificate Secret issued for that exact hostname;
- the immutable image digest and source revision; and
- the approved Console HTTPS origin used at image build time.

The example uses the reserved `.invalid` domain and is intentionally excluded from `kustomization.yaml`. Applying the
base alone cannot publish the site. A release candidate must verify `/`, `/getting-started/`, `/api-reference/` and
`/ready` through the external HTTPS origin, record the certificate and artifact/image digests, and monitor that origin
from the independently operated regions required by the release checklist.
