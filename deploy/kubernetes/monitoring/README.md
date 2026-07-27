# Optional Prometheus Operator integration

These resources are intentionally not part of the base Kustomization because they
require the Prometheus Operator CRDs. Install the CRDs first, then apply:

```bash
kubectl apply -f deploy/kubernetes/monitoring/service-monitor.yaml
kubectl apply -f deploy/kubernetes/monitoring/prometheus-rules.yaml
```

The metrics endpoint contains only bounded route patterns, HTTP status values, login methods/results,
Worker operation/results, Execution Target kinds, and lifecycle states. It never labels metrics with
Tenant, Organization, User, Session, Execution, Worker, Pod, or Credential identifiers.

The Stage 4 recording rules pre-aggregate fresh CPU/Memory/Ephemeral/GPU/Pod capacity vectors, resource-constrained
schedulable units, Queue depth/oldest age, and Worker Pool autoscaling decisions using only bounded labels. Historical
capacity is deliberately a Prometheus time-series concern rather than a PostgreSQL transaction-authority table;
operators must configure Prometheus retention to the period required for their capacity planning and SLO review.
PostgreSQL retains only the latest versioned capacity authority used by the scheduler.
