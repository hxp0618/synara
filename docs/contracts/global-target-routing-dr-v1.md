# Global Target Routing and Disaster Recovery v1

This contract defines how one tenant can route a Session across Execution Targets, regions, and clusters without
rewriting an already-started Execution's placement or replaying an uncertain side effect.

## Routing authority

`execution_target_groups` is the tenant-scoped routing policy. A group freezes:

- strategy: `priority | balanced | latency`;
- ordered preferred regions and whether cross-region placement is allowed;
- maximum failover attempts;
- the maximum acceptable health-observation age;
- a monotonic policy version.

`execution_target_group_members` binds a Target to an explicit region, cluster, priority, weight, status, and member
version. A shared Target may be added only through the same tenant/organization authorization rules as direct Target
selection. API responses expose a safe Target view and never return encrypted Target configuration.

`execution_target_health` is the expiring server authority for health and capacity. Observations are monotonic, have a
bounded TTL, and reject stale replacement. Missing, expired, `unknown`, or `unreachable` health is not eligible for new
placement. Browser presence, Session reads, and Worker heartbeat are not Target-health observations.

For compatibility, `availableCapacityUnits` names the total schedulable capacity ceiling, while
`allocatedCapacityUnits` is current occupancy; remaining free capacity is their difference. A publisher that measures
free units must publish `allocated + free` as `availableCapacityUnits`. `null` means the publisher provides no comparable
numeric ceiling and leaves `capacityStatus` authoritative. A Target is numerically saturated when allocated units reach
the ceiling.

`execution_target_dr_readiness` is a separate destination authority keyed by
`(execution_target_id, source_dr_domain)`. It freezes the destination DR domain, publisher identity, observation/expiry,
monotonic version, `replicatedThroughAt`, and independent Artifact/Checkpoint/Memory readiness bits. A readiness row for
another source domain, an expired observation, a future or stale watermark, or a false bit for a required backing store
cannot authorize placement.

`execution_location_outages` is the tenant-scoped evacuation authority keyed by `(tenant_id, region, cluster_id)`, where
`cluster_id = ''` means a whole-region outage and a non-empty value narrows authority to one cluster. The row freezes a
publisher identity, `draining | unreachable` status, reason, observation/expiry, and monotonic version. A fresh matching
row excludes every candidate in that Region/Cluster from both ordinary routing and automatic failover destination
selection, even when the underlying Target health row is still healthy. This authority is tenant-scoped and does not
mutate platform-shared Target health.

The initial Session selection and every newly created Turn resolve the current group policy inside the same database
transaction that freezes placement. The Session retains the requested Target, group ID/version, and preferred region;
every Execution retains the exact selected Target, region, cluster, group/member versions, and routing reason.

## Selection

Selection is fail closed and applies these filters before ranking:

1. tenant and optional organization scope;
2. active group, member, and Target;
3. required Target kind, when specified;
4. fresh `healthy | degraded` health with available capacity;
5. fresh absence of a matching Region/Cluster evacuation authority;
6. cross-region policy and explicit exclusions.

Preferred Target, health, and preferred-region matches rank ahead of fallback candidates. An optional Target-local
`providerPolicy.routingPreferences` map then applies a soft `prefer | avoid` Provider affinity before the existing
strategy-specific load, priority, and weight tie-breaks. Missing affinity is neutral; `avoid` never makes a Target
ineligible, and affinity cannot outrank an explicit Target/Region choice or healthier capacity. A saturated Target is
never treated as available merely because it has a higher policy priority or Provider preference.

Provider affinity is not Provider support authority. Routing eligibility is finalized only after a read-only placement preview and a Provider capability check scoped to the
selected Pool semantics. An ineligible candidate is added to the request's excluded Target set and selection continues;
the Session's Target authority is advanced only after the final candidate passes both gates. Warm Pool observations are
matched by exact Pool ID/version, while resident and per-execution placement uses only general/unassigned Worker
observations. This filtering is a correctness boundary, not yet a live-capacity or queue-fairness ranking policy.

Once a Session has a previous Execution, the preferred Target bypasses DR readiness only when both its Target ID and its
current member Region/Cluster equal the previous Execution's frozen source snapshot. Reusing the same Target ID under a
new Region or Cluster is a cross-domain move and must satisfy the same exact readiness authority as every other candidate.

## Disaster-recovery boundary

Failover never edits the Target fields of the source Execution. It creates a new `attempt = N + 1` Execution and records
an immutable `execution_failover_attempts` row linking source, destination, reason, routing snapshot, Recovery Bundle,
and leader fencing token.

A source is eligible only when it is lease-free and one of:

- an unclaimed Generation-0 `queued` Execution;
- `suspended` at an exact `suspend-resume` boundary;
- `recovering` with a portable authoritative-history or exact suspend-resume boundary.

Pending/delivered Control Commands and delivered, recovery-bound, failed, or outcome-unknown Interaction deliveries make
the boundary unsafe. Only `not-ready` and `resume-recorded` Interaction rows may move to the successor. The source is
atomically interrupted and fenced; its placement remains historical truth.

When a source Recovery Bundle exists, the destination starts with `nextRecoveryReason = disaster-recovery`. Its first
Generation Bundle names the exact predecessor Execution and Bundle, while freezing the destination Target-local Worker
Pool, release, runtime binding, Workspace materialization, checkpoint, Memory revisions, pending Interaction state, and
routing snapshot. Workspace rematerialization continues to require a portable ready checkpoint; dirty local state cannot
be silently discarded.

Before either ordinary new-Turn routing or explicit failover crosses the frozen source Region/Cluster, the Control Plane
derives the required backing-store subset and its latest ready watermark. The candidate must present the exact source-domain
readiness row at or beyond that watermark. A legacy Execution missing its frozen Region/Cluster cannot be repaired from a
mutable current group member and fails closed whenever portable state is required.

Result Artifact authority is derived twice and merged: current logical history is read through the locked Session's
`lastEventSequence`, while every frozen Recovery Bundle reference is resolved at that Bundle's exact
`authoritativeHistorySequence`. Resolution is rollback- and fork-lineage-aware and preserves the logical origin Session and
Execution. A missing/zero Artifact ID, absent sequence, sequence beyond the Bundle watermark, or conflicting duplicate
Session/Execution authority invalidates failover rather than weakening the check. This also captures Result Artifacts emitted
after the source Bundle was created. Resume construction also rejects missing, unready, deleted, hashless, or origin-mismatched
Artifact rows. Resume Snapshot byte/token budgeting does not delete Result Artifact references
themselves; it first sheds narrative/tool/interaction detail and optional Artifact metadata, then fails closed if the exact
reference set still cannot fit. Cross-domain DR therefore gates the same Result Artifact set that a successor Recovery Bundle
or authoritative-history Resume Snapshot will carry.

The global failover sweep runs under the durable `synara:global-target-failover-sweep` lease. The exact holder and fencing
token are locked and revalidated in the same PostgreSQL transaction that commits the successor, Session Target move,
Event, and Outbox row. A stale leader therefore cannot commit a failover after takeover.

When the source Execution is already lease-free and its frozen `selected_region` / `selected_cluster_id` matches a fresh
`execution_location_outages` row, the sweep commits the successor with reason `region-failover`. The source Execution's
stored placement remains immutable historical truth.

## Operator API

- `GET /v1/tenants/{tenantID}/execution-target-groups`
- `POST /v1/tenants/{tenantID}/execution-target-groups`
- `POST /v1/tenants/{tenantID}/execution-target-groups/{targetGroupID}/members`
- `PUT /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/health-observation`
- `GET /v1/tenants/{tenantID}/location-outages`
- `PUT /v1/tenants/{tenantID}/location-outages`

The health-observation request may carry both Target health/capacity and a DR-readiness observation. Tenant operators
cannot mutate a platform-shared Target's authority; a production platform publisher for shared Targets remains an
operator integration boundary and must not reuse a tenant user's authority.

The location-outage API is a narrow tenant operator authority. `PUT` upserts one `(region, optional clusterId)` row with a
fresh observation timestamp and TTL; it does not mutate any `execution_targets` row, including platform-shared Targets.

Read requires tenant Worker read authority. Mutation and operator health observation require tenant Worker management
authority and the path tenant must be the caller's active tenant.

## Operational boundary

The v1 database and controller contract supports cross-Target, cross-region, and cross-cluster placement. Production
deployment must still provide an authenticated health/capacity publisher for every Target and replicate Recovery Bundle,
Workspace checkpoint, Memory Artifact, and credential-reference backing stores into the declared disaster-recovery
failure domain. Tenant-owned managed Kubernetes Targets now satisfy the health/capacity publisher requirement through
the fresh per-target Kubernetes Reconciler conclusion that writes `execution_target_health` with an expiring TTL.
Its initial capacity value is the configured `maxActivePods` Pod-slot ceiling and the reconcile's observed, created, and
deletion-pending Pod occupancy; it is not a forecast of cloud node or availability-zone headroom.
Platform-shared Targets, external Targets, and all cross-domain `execution_target_dr_readiness` publication remain
operator/integration-owned authority. A successful routing decision does not by itself prove those external data planes
are replicated.
