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

The Stage 6 rules also record the four provisional Enterprise SLOs and their 30-day error budgets. Availability requires
an independently operated external blackbox probe whose Prometheus series is exactly
`probe_success{job="synara-public-readiness"}`. The checked-in rules deliberately alert when that series is absent; an
internal `up` series cannot prove public reachability. Configure at least three external probe regions at a 30-second
cadence and retain at least 30 days of data before assessing a production window.

The exact good-event definitions, minimum sample volumes, no-data behavior, and release-evidence boundary are frozen in
[`enterprise-service-level-objectives-v1.md`](../../../docs/contracts/enterprise-service-level-objectives-v1.md).
Alert ownership, paging, public communication, and error-budget response are defined in
[`enterprise-incident-response.md`](../../../docs/runbooks/enterprise-incident-response.md); deployment-specific contacts
and Internal Status Board URLs belong in a private operations annex.
