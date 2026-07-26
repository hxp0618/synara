# Execution Scheduling Decision v1

Status: Stage 4 selected-only implementation contract.

## Purpose

Every newly created Execution must retain the immutable scheduling evidence that authorized its selected Target and
Worker Pool. Mutable Target health, queue depth, routing membership, DR readiness, placement policy, and scheduling
policy heads are not historical evidence by themselves.

Version 1 deliberately records only the final selected candidate. The current routing coordinator discards candidates
rejected during routing, placement preview, hard-policy admission, and Provider capability admission. Calling the final
winner a complete candidate trace would therefore be false. New rows use `evidenceCompleteness = selected-only`; only
migration backfill uses `legacy-selected-only`. `complete` is reserved for a future bounded full-trace producer.

## Atomic graph

Migration `000076_execution_scheduling_decisions.sql` adds:

- nullable `agent_executions.scheduling_decision_id` for rolling-writer compatibility;
- one immutable `execution_scheduling_decisions` header per covered Execution;
- ordered `execution_scheduling_candidates` rows with exactly one selected candidate;
- a canonical SHA-256 per candidate and an ordered candidate-set SHA-256 on the decision.

The Execution points to the Decision and the Decision points to the same tenant-scoped Execution. New Turn,
review/compact, and disaster-recovery successor creation use one shared transaction boundary that:

1. applies the final post-lock routing, policy, and placement snapshot;
2. freezes the selected Worker release;
3. creates the Execution with its Decision identity;
4. creates the Decision and selected Candidate;
5. verifies the stored candidate hash, count, ordinal, selection, and aggregate hash;
6. appends the Event and Outbox dispatch in the caller's same transaction.

Migration `000084_execution_capacity_reservation_authority.sql` extends the same transaction with one immutable
`execution_capacity_admissions` snapshot. It is a separate one-to-one graph because fixed Targets and routed Targets share
capacity admission, while only routed selections have a Candidate/Member graph.

Any later Event, Outbox, audit, or command failure rolls back the entire graph. Idempotent request replay returns the
original graph and never creates a second Decision.

## Evidence allowlist

The selected Candidate contains only bounded scheduling authority:

- Target ID and kind;
- Target Group, Member, and policy versions for routed decisions;
- Member Region/Cluster for routed decisions, or effective placement Region/Cluster for fixed-Target decisions;
- health version/status/capacity, observation/expiry, numeric capacity, and allocated units;
- `queue-pressure-v1` queued units, or `reservation-aware-v1` exact-authority effective load rank, plus Member priority
  and weight;
- DR readiness version, source/destination domains, replication watermark, readiness bits, and observation/expiry;
- Worker Pool ID/version, Capacity Class, and placement-policy version;
- eligibility, selected ordinal, and a bounded stable rejection code when complete evidence is implemented.

It never copies encrypted Target configuration, Target capability maps, Pool scheduling templates, policy documents,
Provider error details, publisher identity, or free-form health/readiness reasons. Events and Outbox messages carry the
Decision ID, algorithm version, evidence completeness, candidate-set digest, capacity-admission mode, and
capacity-admission digest—not either evidence body.

The capacity-admission snapshot separately freezes Health version/source/timestamps, capacity ceiling and allocation,
exact acknowledgement count/digest, active/unacknowledged reservation units, strict used units, and its canonical
SHA-256. See [`Execution Capacity Reservation Authority v1`](execution-capacity-reservation-authority-v1.md).

## Location authority

`selectedRegion` and `selectedClusterId` on a routed Decision retain Target Group Member semantics. Effective
`placementRegion` and `placementClusterId` remain frozen on the Execution and Recovery Bundle and must match the Member
when the launch is routed. A fixed-Target Decision has no Member authority and uses the effective placement location.

## Recovery and replay

Recovery Bundle payloads freeze `schedulingDecisionId` alongside Target, placement, policy, runtime binding, Workspace,
checkpoint, Memory, Context, and pending Interaction state. A mismatch fails Claim validation. Immutable Bundles created
before migration `000076` legitimately omit this additive field; after their Execution receives a
`legacy-selected-only` backfill Decision, that exact omission remains accepted because the old Bundle cannot be
rewritten.

Historical Executions are backfilled deterministically from their already-frozen selected Target/placement fields. Their
Decision ID derives from Tenant and Execution identity, their decision time is the original `queuedAt`, and their
evidence is explicitly labeled `legacy-selected-only`. Backfill never fabricates historical health, queue, Member ID, or
DR readiness observations that were not retained on the Execution.

## Database invariants and lifecycle

- Decision and Candidate updates are rejected.
- Capacity Admission updates are rejected.
- Direct Candidate deletion fails while its Decision survives.
- Direct Decision deletion fails while its Execution survives.
- Deleting an Execution through the existing retention or tenant-purge lifecycle removes its Decision, Candidates, and
  Capacity Admission evidence; audit evidence does not make the parent Execution undeletable.
- PostgreSQL uses deferred tenant-scoped graph constraints so Execution and Decision can be inserted atomically in either
  direction inside one transaction.
- SQLite retains the same ownership, shape, selection, and immutability fences. The Go writer remains its authority for
  exact SHA-256 aggregate verification because SQLite has no built-in SHA-256/deferred commit trigger equivalent.
- A rolling old writer may temporarily create an Execution with a null Decision ID. Enforcing non-null for every new
  Execution requires a later minimum-writer-version gate.

## Explicit limitations

Selected-only evidence explains and replays the committed winner, but not why every losing candidate was rejected. A
truthful complete trace requires structured evidence across routing filters, repeated excluded-Target iterations,
placement preview, hard scheduling-policy admission, Provider capability admission, and commit revalidation. Rejected
transactions remain mutation-free; persisting failed-attempt evidence would require a separately authorized audit
transaction and is not part of v1.

Until that producer exists, the Stage 4 completion item “调度决策可解释、可审计、可重放” remains open rather than treating
selected-only evidence as full completion.
