# Execution Capacity Reservation Authority v1

Status: Stage 4 implementation contract.

## Purpose

Target health publishers report physical occupancy while the Control Plane owns durable `queued | recovering`
Executions. A Kubernetes Pod can already be present in `allocatedCapacityUnits` while its Execution is still queued.
Adding the whole queue to occupancy therefore double-counts some work; using occupancy alone permits concurrent Control
Plane replicas to admit work that the publisher has not observed yet.

Version 1 closes that gap with an exact publisher acknowledgement set and a serialized admission boundary. It does not
infer acknowledgement from browser activity, Worker heartbeat, Pod name, or an eventually consistent queue count.

## Reservation identity and arithmetic

Every durable `agent_executions` row in `queued | recovering` is one active reservation unit. Its identity is the exact
pair `(executionId, generation)`. Generation is mandatory: after a claim increments Generation, a later recovery cannot
reuse an acknowledgement issued for the earlier Worker generation.

An `exact-active-v1` Health observation carries the sorted, unique set of active reservation identities already
represented one-for-one in the same observation's `allocatedCapacityUnits`. The committed Health row stores the mode,
acknowledged count, and canonical acknowledgement-set SHA-256. Current rows live in
`execution_target_reservation_acknowledgements` and are valid only when their `healthVersion` equals the current Health
version.

For one Target:

```text
unacknowledged = active queued/recovering generations without an exact current-Health acknowledgement
strictUsed     = allocatedCapacityUnits + unacknowledged
```

With a numeric ceiling, a new Execution is admitted only when `strictUsed < availableCapacityUnits`; inserting it then
raises the unacknowledged reservation count by one. Acknowledged queued Pods are not added twice. An acknowledgement set
cannot contain more rows than allocated occupancy.

## Health publication boundary

Health publication locks the Execution Target, stages the next-version acknowledgement rows, and advances Health in one
database transaction. PostgreSQL and SQLite reject acknowledgements staged for any version other than the next Health
version. Canonical digest input is:

```text
synara/capacity-reservation-acknowledgements/exact-active-v1\n
execution_target_id
health_version
(execution_id, execution_generation) ordered by execution_id then generation
```

Retention, claim, or terminal transitions may race a publisher observation. Identities that no longer match the current
Execution generation and active status are omitted before commit. This filtering uses an MVCC read, not an Execution row
lock: holding Target and then locking Execution would invert the recovery trigger's Execution-to-Target lock order.
Concurrent recovery cannot commit until the publisher releases Target; a concurrent claim only makes an acknowledgement
irrelevant because the active-reservation join no longer matches.

Once a Target enables `exact-active-v1`, later Health observations cannot omit reservation authority. An unhealthy or
failed managed reconcile publishes the exact mode with an empty set and an ineligible Health status; it does not silently
downgrade to legacy admission. Health TTL, status, and `capacityStatus=saturated` remain independent fail-closed gates.

## Publisher responsibilities

The managed Kubernetes Reconciler acknowledges a queued/recovering generation only when that reconcile has accounted for
one occupied slot through:

- its exact existing per-Execution Pod;
- a successfully applied replacement/new Pod; or
- one distinct ready-idle Warm Pool slot consumed from the reconcile's local capacity map.

Acknowledgement identities are sorted before publication. Deletion-pending, mismatched, or otherwise uncertain Pod slots
may remain unacknowledged; this can reduce utilization but cannot over-admit.

An external signed Platform publisher may include the same nested authority only when its exact Target scope has both
`publishHealth: true` and `publishReservations: true`. Tenant operator health requests intentionally expose no
reservation-authority field. The signed acknowledgement array must be non-null, unique, and sorted.

## Scheduling and commit serialization

`reservation-aware-v1` routing uses `strictUsed` for strategy-compatible load rank and hard eligibility. The existing
`queue-pressure-v1` behavior remains the compatibility path for Health publishers that have not enabled exact authority.
Target Group selection and commit revalidation compare both total active reservation units and unacknowledged units.

All new Execution creation paths—ordinary Turn, review/compact, and disaster-recovery successor—already hold the Target
commit lock before capacity admission. Migration `000084` adds a PostgreSQL trigger that takes the same Target row lock
when an Execution is inserted active or transitions from a non-active state into `queued | recovering`. SQLite uses its
single-writer transaction boundary. Two Control Plane replicas therefore cannot both observe the same free slot: the
second waits, recounts after the first commit, and rejects when capacity is reserved.

A recovery is protected existing work and may re-enter the queue even when physical capacity is currently full. Its new
Generation is immediately unacknowledged, so every later new admission observes the backlog and fails closed. The
authority limits new admissions; it does not discard recovery work to keep the queue numerically below the Pod ceiling.

Fixed Targets preserve legacy explicit-target behavior until an exact authority is enabled. Once enabled, fixed launches
also require fresh eligible Health and strict capacity. Routed launches always require eligible Health.

## Immutable evidence

Every new Execution receives exactly one `execution_capacity_admissions` row in the same transaction as its Scheduling
Decision, Event, and Outbox message. The immutable snapshot records:

- admission mode (`fixed-unbounded-v1 | publisher-health-v1 | exact-active-v1`);
- Target and optional Health version/source/timestamps/capacity snapshot;
- acknowledgement mode/count/digest;
- active and unacknowledged reservation units;
- strict used units and numeric ceiling; and
- a canonical `synara/execution-capacity-admission/v1` SHA-256.

Event and Outbox payloads carry `capacityAdmissionMode` and `capacityAdmissionSnapshotSha256`, alongside the Scheduling
Decision identity. Retention or Tenant purge may cascade the evidence only with its parent Execution; in-place updates
are rejected.

## Database and observability

Migration `000084_execution_capacity_reservation_authority.sql` adds the Health fields, current acknowledgement table,
immutable admission evidence, next-Health staging/digest checks, active-reservation Target locking, and
`reservation-aware-v1` Scheduling Decision allowlist. SQLite installs equivalent shape, staging, immutability, and
parent-lifecycle triggers; Go verifies canonical admission SHA-256 after write.

Prometheus exports only bounded aggregates:

- `synara_execution_capacity_admission_evidence{mode}`;
- `synara_execution_target_reservation_authorities{freshness}`;
- acknowledged and unacknowledged reservation units by freshness; and
- strict capacity used units by freshness.

No Tenant, Target, Execution, Generation, Pod, publisher, or acknowledgement digest is a metric label.

## Explicit boundary

The authority treats each Execution as one Pod-slot unit. Heterogeneous CPU/memory/GPU reservation vectors, preemption,
batch priority classes, and production multi-Tenant load/soak remain separate Stage 4 work. OrbStack PostgreSQL and
Kubernetes evidence is local E3 evidence; target-hardware self-hosted deployment must still establish its production
capacity SLOs. Managed-cloud publisher correctness is outside the supported scope.
