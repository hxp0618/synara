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
selection. Region and cluster components cannot contain `/`; this keeps the version-1 `region/cluster` DR-domain encoding
unambiguous. API responses expose a safe Target view and never return encrypted Target configuration.

`execution_target_health` is the expiring server authority for health and capacity. Observations are monotonic, have a
bounded TTL, and reject stale replacement. Missing, expired, `unknown`, or `unreachable` health is not eligible for new
placement. An observation later than server time is rejected at publication and treated as ineligible if a writer bypasses
the service boundary. Browser presence, Session reads, and Worker heartbeat are not Target-health observations.

For compatibility, `availableCapacityUnits` names the total schedulable capacity ceiling, while
`allocatedCapacityUnits` is current occupancy; remaining free capacity is their difference. A publisher that measures
free units must publish `allocated + free` as `availableCapacityUnits`. `null` means the publisher provides no comparable
numeric ceiling and leaves `capacityStatus` authoritative. A Target is numerically saturated when allocated units reach
the ceiling.

`execution_target_dr_readiness` is a separate destination authority keyed by
`(execution_target_id, source_dr_domain)`. It freezes the destination DR domain, publisher identity, observation/expiry,
monotonic version, `replicatedThroughAt`, and independent Artifact/Checkpoint/Memory readiness bits. A readiness row for
another source domain, a destination domain different from the candidate's current Region/Cluster, an expired or
future-dated observation, a future or stale watermark, or a false bit for a required backing store cannot authorize
placement. Publishing a future observation is rejected before it can consume the monotonic version.

`execution_location_outages` is the tenant-scoped evacuation authority keyed by `(tenant_id, region, cluster_id)`, where
`cluster_id = ''` means a whole-region outage and a non-empty value narrows authority to one cluster. The row freezes a
publisher identity, `draining | unreachable` status, reason, observation/expiry, and monotonic version. A fresh matching
row excludes every candidate in that Region/Cluster from both ordinary routing and automatic failover destination
selection, even when the underlying Target health row is still healthy. This authority is tenant-scoped and does not
mutate platform-shared Target health.

The initial Session selection and every newly created Turn resolve the current group policy inside the same database
transaction that freezes placement. The Session retains the requested Target, group ID/version, and preferred region;
every Execution retains the exact selected Target, region, cluster, group/member versions, and routing reason.

Any caller that commits a mutable cross-Target routing decision must revalidate the selected authority immediately before
its first domain mutation. The transaction locks Tenant range authority, Target, group, member, matching location-outage
rows, health, and DR readiness in that fixed order. While retaining the Target serialization lock, it then recounts the
selected Target's durable `queued | recovering` Executions. It compares the exact selected versions and decision fields,
reruns freshness/capacity/watermark/store checks, and never silently substitutes a different candidate. A queue-count
change returns `target_routing_selection_stale` with `reason = queue-pressure-changed`, the expected count, and the actual
count, so the idempotent caller can retry from a fresh whole-group decision. Location-outage publishers share the Tenant
lock, so an outage row that did not exist during selection cannot cross the commit check as an insert phantom. A changed
or withdrawn authority leaves source, Session, failover attempt, successor, Event, and Outbox state unchanged.

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

`queue-pressure-v1` treats each durable `agent_executions` row in `queued | recovering` as one not-yet-serviced unit. For
a positive numeric capacity ceiling, balanced load rank uses `(allocatedCapacityUnits + queuedExecutionUnits) / (ceiling
* memberWeight)`; when the publisher supplies no numeric ceiling, it uses `queuedExecutionUnits / memberWeight`.
`balanced` evaluates this effective load before priority, while `priority | latency` retain priority before effective
load. Terminal, leased, running, waiting-for-approval, and suspended Executions are not included in this queue count.

This is deliberately a conservative **soft ranking signal**, not hard capacity reservation. A Kubernetes Pod may already
be included in the publisher's allocated occupancy while its Execution is still `queued`, so summing both can double
count. Hard eligibility continues to use only the fresh health publisher's `capacityStatus` and numeric occupancy. Strict
reservation requires a publisher acknowledgement watermark or an equivalent authority that proves which queued units
are already reflected in occupancy; v1 does not invent that proof.

Provider affinity is not Provider support authority. Routing eligibility first uses a read-only placement preview and a
Provider capability check scoped to the selected Pool semantics. After final placement acquires the Target/policy locks,
the same capability gate always runs again against that final Pool—even when the Pool ID/version did not change—so a
concurrent Worker registration or heartbeat cannot cross the capability decision-to-commit boundary. An ineligible
candidate is added to the request's excluded Target set and selection continues; the Session's Target authority is
advanced only after the final candidate passes both gates. Warm Pool observations are
matched by exact Pool ID/version, while resident and per-execution placement uses only general/unassigned Worker
observations. This filtering is a correctness boundary. Queue-pressure-v1 supplies conservative cross-Target ranking,
and Migration `000076` freezes the final selected Target, post-lock health/queue authority, placement, and optional DR
readiness in an immutable `selected-only` Decision. Rejected routing/preview/capability candidates are not retained yet,
so a truthful full candidate-set trace remains outside this contract revision. Heterogeneous CPU/memory/GPU units and
preemption also remain outside v1; Tenant equal-share is defined separately in the Worker Pool placement contract.

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

`suspended` deliberately does not consume tenant concurrent-Execution capacity. Failover from that state must reacquire
the same Tenant-serialized quota admission used by ordinary resume before any failover mutation. A queued or recovering
source already consumes a slot, so replacing it with one successor is capacity-neutral rather than a second admission.

Pending/delivered Control Commands and delivered, recovery-bound, failed, or outcome-unknown Interaction deliveries make
the boundary unsafe. Only `not-ready` and `resume-recorded` Interaction rows may move to the successor. The source is
atomically interrupted and fenced; its placement remains historical truth.

When a source Recovery Bundle exists, the destination starts with `nextRecoveryReason = disaster-recovery`. Its first
Generation Bundle names the exact predecessor Execution and Bundle, while freezing the destination Target-local Worker
Pool, release, runtime binding, Workspace materialization, checkpoint, Memory revisions, pending Interaction state, and
routing snapshot. Workspace rematerialization continues to require a portable ready checkpoint; dirty local state cannot
be silently discarded.

Before failover mutates the source, the Control Plane must validate the persisted source Recovery Bundle through the same
canonical encoder used at creation. A stored SHA-256 mismatch or an envelope mismatch in tenant, Session, Turn, Execution,
Generation, schema version, reason, or predecessor identity fails closed without creating a successor, Event, or Outbox
row. The successor's first Claim follows predecessor ancestry to the original Turn boundary, so current-turn Context and
Result Artifact authority from the source are not mistaken for an empty initial history.

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

The committed Event and Outbox decision snapshot includes the exact destination readiness version, source/destination DR
domains, replication watermark, publisher/timestamps, and all three Artifact/Checkpoint/Memory readiness bits. The
mutable readiness row is not sufficient historical evidence on its own.

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

The checked-in OrbStack-plus-disposable-Kind lane is a repeatable local cross-cluster control-path exercise. It proves two
distinct Kubernetes API servers, source and successor Worker runtime readiness, fail-closed DR-readiness selection,
immutable source placement, successor lineage, obsolete source Pod removal, and exact owned-resource cleanup. Both
clusters additionally run the production Kubernetes identity verifier against their real TokenReview and Pod GET APIs;
the Target-audience projected token succeeds and an unbound Control Plane Kubernetes credential is rejected.

This local lane does not prove independent Region or availability-zone failure domains, production metadata-store
failover, the production Worker registration handler and persistence boundary, cloud Workload Identity, backing-store
replication, successor consumption of the predecessor Recovery Bundle, or measured production RPO. `replicatedThroughAt`
is an authorization assertion from the configured readiness publisher; without external replication evidence it must not
be reported as measured RPO. Production cross-Region acceptance remains incomplete until the declared Artifact,
Checkpoint, and Memory required set is replicated by an authenticated publisher, consumed by the successor in the
destination failure domain, and validated with authoritative RTO/RPO evidence.
