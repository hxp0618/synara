# Execution Scheduling Decision v1

Status: Stage 4 complete routed-candidate implementation contract.

## Purpose

Every newly created Execution must retain the immutable scheduling evidence that authorized its selected Target and
Worker Pool. Mutable Target health, queue depth, routing membership, DR readiness, placement policy, and scheduling
policy heads are not historical evidence by themselves.

Every successful Target Group launch records all active Group Members observed by the first routing pass, including
eligible losers and candidates rejected by request exclusion, scope, Target kind/status, Scheduling Policy, Health,
capacity, Region, location outage, DR readiness, placement preview, post-preview Scheduling Policy, or Provider
capability admission. Repeated excluded-Target passes merge into the same stable Member-ID-ordered trace; a coordinator
rejection is not overwritten by the later synthetic `request-excluded` filter. These rows use
`evidenceCompleteness = complete`.

An explicit fixed-Target launch still has exactly one honest `selected-only` candidate because no Target Group candidate
set was evaluated. Migration backfill remains `legacy-selected-only`; it never fabricates evidence that was not retained.
The bounded producer rejects a Target Group with more than 4096 active Members before launch.

## Atomic graph

Migration `000076_execution_scheduling_decisions.sql` adds:

- nullable `agent_executions.scheduling_decision_id` for rolling-writer compatibility;
- one immutable `execution_scheduling_decisions` header per covered Execution;
- ordered `execution_scheduling_candidates` rows with exactly one selected candidate;
- a canonical SHA-256 per candidate and an ordered candidate-set SHA-256 on the decision.

The Execution points to the Decision and the Decision points to the same tenant-scoped Execution. New Turn,
review/compact, and disaster-recovery successor creation use one shared transaction boundary that:

1. applies the final post-lock routing, policy, and placement snapshot;
2. freezes routing filters, ranking inputs, coordinator rejection codes, and any Worker Pool preview retained for every
   candidate;
3. freezes the selected Worker release;
4. creates the Execution with its Decision identity;
5. creates the Decision and every ordered Candidate;
6. verifies every stored candidate hash, exact count, contiguous ordinal, single selection, and aggregate hash;
7. appends the Event and Outbox dispatch in the caller's same transaction.

Migration `000084_execution_capacity_reservation_authority.sql` extends the same transaction with one immutable
`execution_capacity_admissions` snapshot. It is a separate one-to-one graph because fixed Targets and routed Targets share
capacity admission, while only routed selections have a Candidate/Member graph.

Any later Event, Outbox, audit, or command failure rolls back the entire graph. Idempotent request replay returns the
original graph and never creates a second Decision.

## Evidence allowlist

Each Candidate contains only bounded scheduling authority available at the stage it reached:

- Target ID and kind;
- Target Group, Member, and policy versions for routed decisions;
- Member Region/Cluster for routed decisions, or effective placement Region/Cluster for fixed-Target decisions;
- health version/status/capacity, observation/expiry, numeric capacity, and allocated units;
- `queue-pressure-v1` queued units, or `reservation-aware-v1` exact-authority effective load rank, plus Member priority
  and weight;
- DR readiness version, source/destination domains, replication watermark, readiness bits, and observation/expiry;
- Worker Pool ID/version, Capacity Class, and placement-policy version;
- eligibility, selection, stable Member-ID ordinal, route rank, preferred-Region rank, health-status rank, and a bounded
  stable rejection code when rejected.

It never copies encrypted Target configuration, Target capability maps, Pool scheduling templates, policy documents,
Provider error details, publisher identity, or free-form health/readiness reasons. Events and Outbox messages carry the
Decision ID, algorithm version, evidence completeness, candidate-set digest, capacity-admission mode, and
capacity-admission digest—not either evidence body.

The capacity-admission snapshot separately freezes Health version/source/timestamps, capacity ceiling and allocation,
exact acknowledgement count/digest, active/unacknowledged reservation units, strict used units, and its canonical
SHA-256. See [`Execution Capacity Reservation Authority v1`](execution-capacity-reservation-authority-v1.md).

## Candidate ordering, ranking, and rejection codes

Candidate `ordinal` is deterministic active Member-ID order, not winner order. For candidates that reached routing rank,
`priorityRank` stores the zero-based total route order for that pass, `regionRank` stores the normalized preferred-Region
position, and `capacityRank` stores the Health status rank. Health/capacity, queue pressure, Member priority/weight, and
Target ID remain frozen alongside those derived ranks. After a coordinator rejection, the next routing pass may have a
new rank 0; the rejected candidate retains its earlier route rank plus the stage-specific rejection code.

Routing rejection codes are allowlisted operational tokens:

- `request-excluded`, `target-inactive`, `tenant-scope-mismatch`, `organization-scope-mismatch`, and
  `target-kind-mismatch`;
- `scheduling-policy-denied` and `region-policy-denied`;
- `health-missing`, `health-status-ineligible`, `health-observed-in-future`, `health-expired`, `health-stale`,
  `capacity-saturated`, and `capacity-exhausted`;
- `location-outage-active`; and
- `dr-readiness-context-missing`, `dr-readiness-missing`, `dr-readiness-expired`, `dr-readiness-unready`,
  `dr-readiness-source-mismatch`, `dr-readiness-destination-mismatch`, `dr-readiness-observed-in-future`, and
  `dr-readiness-watermark-stale`.

Coordinator retries retain the existing bounded API problem code, currently covering unsupported Provider capability,
missing/incompatible Provider, Worker Manifest admission, unavailable placement Pool/policy/location, and post-preview
Scheduling Policy denial. Free-form Provider details are never copied. A corrupt Member reference to a missing Target or
an unrecognized error aborts the transaction instead of inventing a trace row.

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

`complete` describes a successfully committed Target Group decision, not every failed launch attempt. If no candidate
survives, final placement/capability validation fails, or commit revalidation detects stale authority, the caller's
transaction remains mutation-free and no Decision is written. Persisting those failed attempts would require a separate
authorized audit transaction and is not part of v1.

Only the selected Target and its final Pool are commit-locked. Losing rows intentionally retain the observations that the
algorithm actually evaluated; they do not claim that mutable loser Health or Pool authority was linearized at commit.
Fixed-Target and historical rows remain explicitly selected-only as described above.
