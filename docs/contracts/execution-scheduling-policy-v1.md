# Execution Scheduling Policy v1

Status: Stage 4 implementation contract.

## Purpose

Execution Scheduling Policy is the server-authoritative hard admission layer for choosing an Execution Target and its
target-local placement. It applies to both explicitly selected Targets and Target Group routing. Frontend controls edit
this authority and display its effective state; the browser is never an authority and cannot bypass the policy by sending
a preferred Target.

This contract covers Tenant and Organization restrictions for:

- Execution Target ID;
- Region;
- Cluster ID;
- Provider;
- Capacity Class (`standard | interactive`).

Structured CPU, memory, ephemeral-storage, GPU, and accelerator profiles require a separately versioned Resource Profile
authority. Arbitrary Kubernetes scheduling-template JSON is not an authorization boundary and is outside v1.

### Effective location

Target Group membership and target-local Worker Pools both carry location fields, so v1 defines one effective location
instead of allowing either source to override the other:

- for a routed launch, the Target Group Member is the global Region/Cluster authority; an empty Pool location inherits
  that Member field, while a non-empty Pool field must match it exactly;
- for a fixed-Target launch, the selected Pool's Region/Cluster is the effective location;
- a routed Pool/Member mismatch fails closed before commit and cannot be retried after locks are held;
- an empty fixed-Target location is allowed only by an `any` rule. A restricted allow-list cannot match an unknown empty
  location.

The effective placement Region/Cluster is frozen separately on the Execution. Existing global-routing
`selectedRegion/selectedClusterId` fields retain their Target Group Member meaning and remain null for fixed-Target
Executions.

## Authority and versioning

Tenant and Organization policies are append-only revisions selected by a mutable head:

- a missing head is version `0`, the well-known unrestricted policy;
- every update supplies `expectedVersion` and creates exactly one immutable revision;
- the head advances with compare-and-swap from `expectedVersion` to `expectedVersion + 1`;
- a revision stores a canonical SHA-256 digest of its normalized rule document;
- revision and rule rows cannot be updated or deleted;
- an Organization policy belongs to exactly one Tenant and cannot survive its Organization.

All policy updates and launch commits use this fixed order: Tenant row, Organization row when present, Tenant policy
head/revision, Organization policy head/revision, then routing and placement authorities. This shared Tenant authority
also serializes creation of a previously absent head with an in-flight routing commit.

## Rule semantics

Every dimension is an explicit rule set:

```json
{
  "mode": "any | allow",
  "values": []
}
```

- `any` means unrestricted at Tenant scope and inherit at Organization scope; its values must be empty.
- `allow` means only the normalized listed values are allowed.
- `allow` with an empty list deliberately denies the entire dimension. It is not equivalent to an omitted field or
  `any`.
- `denyAll: true` denies every launch regardless of the dimension rules.

Target IDs use canonical UUID text. Providers use the canonical Provider catalog name. Capacity Classes are lowercase
catalog values. Region and Cluster IDs are trimmed, bounded, case-sensitive location identifiers consistent with Target
Group membership.

The effective policy is the intersection of Tenant and Organization policies. An Organization revision may inherit or
narrow Tenant authority, but it cannot add a value excluded by a restricted Tenant rule. An Organization update that
attempts to widen a restricted Tenant dimension fails closed.

v1 applies only the exact Organization attached to the Project/Session; parent Organization policies are not inherited.
Tenant policy is the cross-Organization boundary. If a Tenant policy is tightened after an Organization revision was
created, that historical Organization revision remains valid and the newly effective policy is simply the stricter live
intersection. The no-widening check runs when a new Organization revision is written.

## Selection and commit

The launch coordinator resolves one effective policy before candidate ranking:

1. hard policy filters Target ID, Region, Cluster ID, and Provider;
2. existing health, capacity, outage, DR readiness, preferred Region, Provider affinity, load, priority, and weight logic
   ranks the remaining Target Group candidates;
3. placement preview selects a Worker Pool and Capacity Class;
4. hard policy filters the Capacity Class;
5. immediately before final placement, the transaction locks Tenant, Organization when present, Tenant policy head and
   exact revision, Organization policy head and exact revision, Target, Group, Member, matching location-outage rows,
   Health, DR readiness, and placement authority in that order;
6. any policy identity, version, digest, normalized rule, or eligibility change returns
   `target_routing_selection_stale` for routed launches, or `execution_scheduling_policy_stale` for fixed-Target launches.

Hard policy denial is never a soft ranking penalty. Preferred Target, Provider affinity, capacity, priority, weight, warm
capacity, or a caller-supplied Target ID cannot override it. A routed candidate denied only after placement preview may be
excluded during the read-only preview phase; once commit locks are acquired, the transaction does not lock and retry a
second Target.

Stable rejection codes are:

- `execution_scheduling_policy_denied` for a fixed Target or placement rejected by the effective policy;
- `target_group_no_policy_eligible_destination` when every otherwise scoped routed candidate is hard-policy denied;
- `execution_scheduling_policy_stale` when a fixed-Target policy snapshot changes before commit;
- `target_routing_selection_stale` when a routed policy or routing authority changes before commit.

## Frozen execution evidence

Every new Execution freezes:

- Tenant policy version and digest;
- Organization policy version and digest;
- the selected Target, global routed Region/Cluster when applicable, effective placement Region/Cluster, and Capacity
  Class already frozen by routing and placement.

Version `0` uses the well-known unrestricted digest, so the absence of a policy remains explicit and replayable. A later
policy edit affects future launch decisions but does not rewrite an existing Execution snapshot. Resume/recovery of an
already admitted Execution remains bound to its frozen Target and placement unless a separate revocation or evacuation
authority requires a new launch decision.

## Fail-closed requirements

- A head that references a missing revision, a digest mismatch, malformed rule, duplicate conflicting rule, foreign
  Organization, or invalid Provider/Capacity Class blocks launch and policy reads.
- A new Execution insert must match the currently locked Tenant and exact-Organization head versions and revision digests.
  Unrestricted column defaults exist only for migration compatibility; they are rejected whenever a non-zero policy head
  is current, so an application path cannot silently omit the snapshot and masquerade as version `0`.
- Policy CAS conflict leaves the old head and revisions unchanged.
- Policy denial or stale validation leaves Session routing authority, Turn, Execution, Event, Audit, and Outbox unchanged.
- PostgreSQL provides the production locking proof. SQLite must enforce the same ownership, append-only, digest, and
  snapshot invariants, but is not evidence of multi-writer row-lock behavior.

## API surface

- `GET /v1/tenants/{tenantID}/execution-scheduling-policy`
- `PUT /v1/tenants/{tenantID}/execution-scheduling-policy`
- `GET /v1/tenants/{tenantID}/organizations/{organizationID}/execution-scheduling-policy`
- `PUT /v1/tenants/{tenantID}/organizations/{organizationID}/execution-scheduling-policy`

Reads require `scheduling_policy.read`; writes require `scheduling_policy.manage`. Worker management permission does not
implicitly grant policy management. Responses include the scope revision, effective policy, version, digest, and parent
Tenant version/digest for Organization scope.

## Explicit non-goals

This hard-policy contract does not define queue priority, preemption, fair-share, autoscaling targets, actual cloud-cost
allocation, or browser-configured activity. The separate global-routing contract defines `queue-pressure-v1` as a soft
ranking signal only. Migration `000076` freezes the committed winner in an immutable selected-only scheduling-decision
record; a truthful full candidate trace remains future work. Neither form can weaken this hard admission boundary.
