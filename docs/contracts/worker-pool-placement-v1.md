# Worker Pool and Placement Contract v1

Status: Stage 4 implementation contract

This contract adds a schedulable layer inside an existing Execution Target. It does not replace the Target as the
tenant, credential, provisioner, Workspace, or revocation boundary.

```text
Tenant / Organization
        |
        v
Execution Target  (security + provisioner boundary)
        |
        +---- Worker Pool  (capacity + placement boundary)
                    |
                    +---- Worker / Pod
                              |
                              +---- one fenced Execution Generation
```

## Boundaries

1. A Worker Pool belongs to exactly one Execution Target. A placement policy cannot reference a Pool owned by a
   different Target or Tenant.
2. A Session keeps its immutable `executionTargetId`. Placement only chooses capacity inside that Target.
3. Release selection and placement selection are independent, durable decisions. A Worker must satisfy both before
   it can claim an Execution.
4. Tenant execution quota counts running work. Idle warm capacity is reported separately and never consumes or
   bypasses the execution quota.
5. Workspace materialization, checkpoint, Provider credential, Memory, and Recovery Bundle authority remain scoped
   to the Execution/Session. An idle warm Worker has none of those bindings.

## Worker Pools

Each Pool has a stable UUID, Target, name, mode, capacity class, target-local location, capacity bounds, scheduling
template, status, and optimistic version.

Pool modes:

- `resident`: long-lived general Workers, such as the existing Docker pool.
- `per-execution`: capacity is created for one assigned Execution Generation.
- `warm`: an idle Worker/Pod may wait without Session state, claim one compatible Generation, and then terminate.

Capacity classes begin with `standard` and `interactive`. They are scheduling codes, not billing SKUs. CPU, memory,
storage, accelerators, and price data stay in the Pool's versioned scheduling template until a shared class registry is
introduced.

`clusterId`, `region`, and `namespace` are target-local placement attributes in v1. For the first Kubernetes
implementation, `clusterId` is the canonical local value `kubernetes` and Pool namespace must equal the Target's
managed namespace. Cross-Target or global multi-cluster selection is deliberately outside this contract.

Pool status is `active`, `draining`, or `disabled`. New placements can select only `active` Pools. Existing leases on
a draining Pool remain fenced and are allowed to reach their normal drain boundary.

## Placement Policy

Each Target has one versioned placement policy:

- `defaultPoolId` handles `warmPoolMode=disabled` and all legacy/default traffic.
- `balancedPoolId`, when present, is preferred for `warmPoolMode=balanced`.
- `lowLatencyPoolId`, when present, is preferred for `warmPoolMode=low-latency`.
- Missing optional mappings fall back in this exact order:
  - `balanced`: balanced, then default.
  - `low-latency`: low-latency, then balanced, then default.

Policy updates use compare-and-swap on `version`. The selected Pool ID, capacity class, and policy version are frozen
on the queued Execution before an Outbox dispatch becomes visible. Claim-time recomputation is forbidden.

The current schema stores one mutable Pool row rather than a revision-history table. Therefore a Pool version update is
rejected while any queued, recovering, leased, running, waiting, or suspended Execution still references that version.
This conservative fence prevents a frozen scheduling template from being overwritten underneath recovery.

Legacy Executions may have no placement snapshot. They use the Target's version-1 default Pool only; they must not be
silently routed to a newly introduced warm Pool.

## Worker Mode

Worker mode is explicit and persisted:

- `execution-pinned`: can request only its assigned Execution and exits after that attempt.
- `warm-pool`: can request compatible warm-pool work, cannot claim Workspace cleanup, and exits after one claimed
  Execution attempt.
- `general-pool`: may keep polling for work and may claim Workspace cleanup when otherwise authorized.

For Kubernetes, Worker mode and Pool identity come from the TokenReview-authenticated, Pod-UID-matched Pod labels.
Worker JSON cannot elevate or change them. A legacy managed Pod that has an Execution label but no mode label is
treated as `execution-pinned`; an unlabelled unassigned Kubernetes Pod is not treated as warm capacity.

## Warm Pod Safety

The v1 Kubernetes warm primitive is **prewarm once, execute once, terminate**. It is not a reusable user container.

An idle warm Pod:

- has no Execution, Session, Workspace restore, Provider cursor, Recovery Bundle, or credential grant;
- is counted against Target Pod/ResourceQuota capacity;
- is release-aware and may claim only an Execution with the same active release assignment;
- must pass the existing Provider manifest, capability, absolute lifetime, quota, and placement fences.

After Claim, the Worker ID, incarnation, Pod name, and Pod UID are frozen into the Generation lease. Suspend completion
continues to require the exact kubelet-authored terminal proof. A successful or failed attempt does not return the Pod
to the idle pool; the reconciler backfills a new spare.

Scale-down and release replacement may delete only an unleased warm Pod. A Claim and a drain decision serialize on the
exact Worker incarnation row and recheck Execution/Cleanup Leases so exactly one wins. A successful Kubernetes DELETE
records only `draining`; kubelet `Succeeded`/`Failed`, or a later successful target Pod list that confirms the exact UID
is missing, records physical termination. Deletion-pending Pods continue to count against capacity during that reconcile
cycle.

## Live warm-capacity authority

Target-wide Pod admission and warm reuse are different authorities. `execution_target_health` remains the hard,
expiring Target-wide ceiling/occupancy observation and includes execution Pods, idle warm Pods, claimed warm Pods, and
deletion-pending Pods. `worker_pool_warm_capacity` reports only the release-aware warm capacity that the Kubernetes
Reconciler has proved reusable now.

Each warm-capacity row is scoped to an exact tenant-owned Kubernetes Target, Pool ID/version, capacity class, and current
release revision/channel. It freezes the Pool's desired/max configuration, observed claimed footprint, computed desired
warm footprint, and ready-idle units. Observations have a bounded TTL, a monotonic observation time, and a CAS version;
missing, stale, unsupported, or release-mismatched observations never advertise a warm hit.

`readyIdleUnits` is published after the Reconciler validates exact Pod/Worker identity and subtracts ready Workers already
needed by visible queued/recovering Executions. It is therefore a performance preference, not a reservation ledger or an
execution quota. Launches between two reconcile cycles may contend for the same last warm unit; the Worker Lease/Claim
transaction remains the correctness boundary, and excess work may use the existing cold execution-Pod fallback or wait
behind the Target-wide Pod ceiling. `maxActiveUnits` must never be reinterpreted as a count of active Sessions or
Executions.

A resource-suspended Execution consumes neither a Worker Lease nor tenant concurrent-execution quota. Every
`suspended -> recovering` transition must reacquire tenant quota under the same durable tenant admission lock used by new
Turn/review/compact creation. The frozen Pool snapshot is preserved, but warm availability cannot block Resume because
the Reconciler may legitimately restore that Generation through the cold fallback path.

## Durable Observability

Metrics must be derived from bounded durable facts rather than unbounded Session Event joins:

- one Generation fact keyed by `(tenantId, executionId, generation)` records dispatch, lease, execution start,
  Provider ready, terminal outcome, recovery reason, requested warm mode, and warm allocation result;
- one physical Pod/Worker-incarnation fact records registration, active/idle transitions, termination, and resource
  class.

Required bounded labels include Target kind, recovery reason, Pool mode, requested warm mode, allocation result, and
terminal outcome. Tenant, Session, Execution, Worker, and Pod IDs are never Prometheus labels.

Pod runtime and requested-resource seconds can be reported once their lifecycle facts are complete. Currency cost
must not be exposed until an authoritative tariff or cloud-billing source is bound to the resource class.

The first implementation exports durable trailing-30-day Generation outcomes, warm allocation results, and
dispatch-to-Provider-ready P50/P95/P99 gauges. Worker-incarnation gauges expose retained runtime, active/idle seconds,
and requested CPU/memory/ephemeral-storage seconds. They deliberately contain no Tenant, Execution, Worker, or Pod ID
labels.

## Rollout Order

1. Add Worker mode and the Worker Pool/placement schema with a default Pool for every existing Target.
2. Freeze placement snapshots for new Executions while legacy nullable snapshots continue to use the default Pool.
3. Enforce Pool compatibility in Claim and expose the target-scoped management API/UI.
4. Enable release-aware Kubernetes warm Pods behind an explicit non-zero desired-idle capacity.
5. Publish durable cold-start, hit/miss/fallback, resource-duration, and recovery-outcome metrics.
6. Only after those gates pass, add cross-Target Cluster/Region placement and global capacity policy.
