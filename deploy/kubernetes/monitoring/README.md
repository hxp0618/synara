# Optional Prometheus Operator integration

These resources are intentionally not part of the base Kustomization because they
require the Prometheus Operator CRDs. Install the CRDs first, then apply:

```bash
kubectl apply -f deploy/kubernetes/monitoring/service-monitor.yaml
kubectl apply -f deploy/kubernetes/monitoring/prometheus-rules.yaml
```

The metrics endpoint contains only bounded route patterns, HTTP status values,
Execution Target kinds, and lifecycle states. It never labels metrics with Tenant,
Organization, User, Session, Execution, Worker, Pod, or Credential identifiers.
