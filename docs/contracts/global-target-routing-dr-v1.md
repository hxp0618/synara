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

`queue-pressure-v1` remains the compatibility path for publishers without reservation authority: it treats each durable
`agent_executions` row in `queued | recovering` as one conservative soft-pressure unit and does not add that count to Pod
occupancy for hard admission.

Migration `000084` adds the stronger `reservation-aware-v1` path. An `exact-active-v1` Health publisher atomically names
the sorted `(executionId, generation)` set already represented in `allocatedCapacityUnits`. Hard and ranking usage become
`allocatedCapacityUnits + unacknowledged active reservation units`; an acknowledged queued Pod is therefore not counted
twice. A recovered Generation cannot reuse its predecessor's acknowledgement. New admission, active-state recovery, and
Health publication serialize on the Target row. Full rules, immutable admission evidence, downgrade behavior, and
publisher responsibilities are frozen in
[`Execution Capacity Reservation Authority v1`](execution-capacity-reservation-authority-v1.md).

`balanced` evaluates the strategy-compatible effective load before priority, while `priority | latency` retain priority
before effective load. Terminal, leased, running, waiting-for-approval, and suspended Executions are not active
reservations.

Provider affinity is not Provider support authority. Routing eligibility first uses a read-only placement preview and a
Provider capability check scoped to the selected Pool semantics. After final placement acquires the Target/policy locks,
the same capability gate always runs again against that final Pool—even when the Pool ID/version did not change—so a
concurrent Worker registration or heartbeat cannot cross the capability decision-to-commit boundary. An ineligible
candidate is added to the request's excluded Target set and selection continues; the Session's Target authority is
advanced only after the final candidate passes both gates. Warm Pool observations are
matched by exact Pool ID/version, while resident and per-execution placement uses only general/unassigned Worker
observations. This filtering is a correctness boundary. Queue-pressure-v1 supplies conservative cross-Target ranking,
and Migration `000076` now freezes every active Group Member observed by the successful routed launch in an immutable
`complete` Decision. Eligible losers retain rank inputs; routing, preview, policy, and capability rejections retain bounded
codes and any observed Pool snapshot; the winner is refreshed from post-lock authority and final placement. Fixed Target
and historical evidence remain explicitly `selected-only` and `legacy-selected-only`. Heterogeneous CPU/memory/GPU units
and preemption remain outside v1; Tenant equal-share is defined separately in the Worker Pool placement contract.

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
- `PATCH /v1/tenants/{tenantID}/execution-target-groups/{targetGroupID}/members/{targetGroupMemberID}`
- `POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/kubernetes/disable`
- `PUT /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/health-observation`
- `GET /v1/tenants/{tenantID}/location-outages`
- `PUT /v1/tenants/{tenantID}/location-outages`
- `PUT /v1/platform/routing-authority/execution-targets/{executionTargetID}/observations`

The health-observation request may carry both Target health/capacity and a DR-readiness observation. Tenant operators
cannot mutate a platform-shared Target's authority and cannot overwrite a health or source-DR-domain authority assigned
to a configured Platform publisher. The Platform route does not accept a Login Session, Service Account bearer token, or
browser heartbeat as authority. It accepts only an Ed25519-signed publication from
`SYNARA_PLATFORM_ROUTING_PUBLISHERS_JSON`.

The configuration contains public keys only. Each publisher identity has one to eight rotation keys and exact Target
scopes. Every Target scope freezes `platform-shared` versus an exact `tenant-owned` Tenant ID, independently authorizes
health publication, optionally authorizes exact reservation publication with `publishReservations`, and lists exact
`(sourceDrDomain, drDomain)` pairs. Reservation authority requires health authority. Overlapping health or source-domain
authorities across publisher identities are rejected at startup. Tenant-owned Kubernetes health remains owned by the managed
Kubernetes Reconciler; the signed integration route rejects an overlapping health write, although a separately scoped DR
replication publisher may publish readiness for that Target.

Signed schema v1 prepends `synara.platform-routing-authority.v1\n` to canonical JSON with this fixed field order:
`schemaVersion`, `publisherIdentity`, `keyId`, `nonce`, `issuedAt`, `expiresAt`, `executionTargetId`, `observedAt`, optional
`health`, and sorted `drReadiness`. UUIDs are canonical lowercase. Timestamps are canonical UTC RFC3339Nano. Signatures
use canonical padded base64. A first publication has a maximum five-minute request validity window with 30 seconds of
clock-skew tolerance; health and readiness TTL remain independently bounded to 10 seconds through one hour. DR entries
must be unique and sorted by `sourceDrDomain`.

The nested health field order is `status`, `capacityStatus`, optional `availableCapacityUnits`,
`allocatedCapacityUnits`, optional `reservationAuthority`, optional `reason`, `ttlSeconds`. Reservation authority uses
`mode: exact-active-v1` and a non-null acknowledgement array sorted by `executionId` then `generation`. Each readiness
item uses `sourceDrDomain`, `drDomain`, `replicatedThroughAt`, `artifactsReady`, `checkpointsReady`, `memoryReady`, optional
`reason`, `ttlSeconds`. Integrations
should use `go run ./cmd/routing-authority-sign --private-key-file /run/secrets/publisher-key.pem` rather than reproduce
the encoder. The tool reads one unsigned JSON value on stdin, rejects unknown fields, existing signatures, symlinked or
group/other-writable key files, and emits the signed request on stdout. `--public-key-only` emits the padded-base64 public
key for Control Plane configuration. The private key file must contain exactly one PKCS#8 Ed25519 `PRIVATE KEY` PEM block.
Kubernetes projected Secret keys are symlinks by construction; a publisher may explicitly add `--allow-key-symlink` only
when that path is on its read-only kubelet-managed Secret volume. The resolved target must still be a regular file and
must not be group/other writable.

Migration `000083` stores one immutable `(publisher_identity, nonce)` receipt with request/public-key/response SHA-256
digests. Target ownership is locked and revalidated before mutation. Health, all readiness rows, and the receipt commit in
one transaction; one stale or invalid row rolls the entire bundle back. Repeating the exact signed request returns the
sealed response even after the request window closes, while using the nonce for different signed content returns a
conflict. `synara_platform_routing_publications_total` and
`synara_platform_routing_publication_latest_timestamp_seconds` expose bounded publication progress; HTTP status metrics
cover authenticated replay and rejection without publisher or Target labels.

Migration `000084` binds exact reservation acknowledgements to the same Health version, enforces count/digest and
one-acknowledgement-per-allocated-slot bounds, and writes immutable per-Execution capacity-admission evidence. The signed
receipt response includes the committed reservation authority summary.

The location-outage API is a narrow tenant operator authority. `PUT` upserts one `(region, optional clusterId)` row with a
fresh observation timestamp and TTL; it does not mutate any `execution_targets` row, including platform-shared Targets.

The member `PATCH` body contains only `expectedVersion` and `status`. It implements the persisted
`active -> draining -> disabled` lifecycle, permits `draining -> active` as an explicit abort, and also permits
`active -> disabled` when the member is already empty. `disabled` is terminal. `draining` and `disabled` members are
immediately ineligible for every new placement, but the transition does not interrupt an existing Execution; a planned
evacuation uses the separate location-outage authority when lease-free failover is required. Disabling fails with
`target_group_member_execution_active` while any group-scoped Execution on that exact Target is `queued`, `leased`,
`running`, `waiting-for-approval`, `recovering`, or `suspended`.

The mutation locks the Group and Member, uses exact version CAS, advances the immutable Member version once, and writes
one bounded Audit row in the same transaction. An exact same-status request at the expected version, or a retry that
finds exactly `expectedVersion + 1` with the requested status, is a no-write replay and returns
`Idempotency-Replayed: true`; any other stale version conflicts. The Member lock is the same commit authority used by
routed launch, so a concurrent Execution either commits first and blocks disable, or observes the non-active Member and
cannot commit its stale selection. Direct row updates are not an operator interface.

The managed Kubernetes Target `disable` operation has no request body and is terminal. It applies only to a tenant-owned
Kubernetes Target and requires Worker management authority. A successful first call changes `active -> disabled`, writes
one `execution_target.kubernetes_disabled` Audit row in the same transaction, and returns the safe Target view. Repeating
the operation after that commit is a no-write replay with `Idempotency-Replayed: true`; there is no reactivation path.

Target disable is deliberately serialized with the complete managed Kubernetes reconciliation cycle through the same
`synara:kubernetes-execution-reconciler` advisory lock. This fences a Reconciler that selected the Target before the
HTTP transaction and is already approaching a Kubernetes API write; a Target row lock alone is insufficient for that
race. A busy cycle returns `kubernetes_reconciler_busy` and the caller may retry with bounded backoff. Target Group member
creation also locks the Target before insert, so it cannot add a new active Member behind a concurrent terminal disable.

While holding the cycle lock and Target row lock, disable fails closed unless all of these conditions hold:

- every Target Group Member for the Target is already `disabled`;
- no unarchived `active | suspended` fixed-Target Session remains;
- no `queued | recovering | leased | running | waiting-for-approval | suspended` Execution remains;
- every Worker Pool is `disabled`;
- every physical Workspace materialization is `cleaned`, and no cleanup command is `pending | leased | running`;
- every Worker incarnation is authoritatively `terminated` with `terminatedAt`, and no Worker Lease remains;
- the current health row is unexpired `healthy/available`, was emitted by the managed Kubernetes publisher with
  `exact-active-v1`, and proves both allocated and acknowledged capacity are zero.

The operation disables Control Plane placement, Worker registration, and all future Reconciler maintenance. It does not
delete the historical Target row, encrypted configuration, Namespace, or another Kubernetes object. An operator may
remove an exclusively Target-owned Namespace only after the terminal response, using the previously captured Namespace
UID as a Kubernetes delete precondition and proving the exact UID absent. Shared Namespaces must be cleaned per exact
owned object instead. Database-row deletion and direct status updates are never lifecycle APIs.

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
operator/integration-owned authority, but now have a signed, exact-scope, atomic Control Plane ingestion path. The
external publisher is still responsible for measuring the Target and for reporting readiness only after the backing
stores have actually crossed the declared failure domain. A successful publication or routing decision does not by
itself prove those external data planes are replicated.

The checked-in OrbStack-plus-disposable-Kind lane is a repeatable local cross-cluster control-path exercise. It proves two
distinct Kubernetes API servers, source and successor Worker runtime readiness, fail-closed DR-readiness selection,
immutable source placement, successor lineage, obsolete source Pod removal, and exact owned-resource cleanup. Both
clusters additionally run the production Kubernetes identity verifier against their real TokenReview and Pod GET APIs;
the Target-audience projected token succeeds and an unbound Control Plane Kubernetes credential is rejected.

This local lane does not prove independent physical failure domains, production metadata-store failover, the production
Worker registration handler and persistence boundary, backing-store replication, successor consumption of the
predecessor Recovery Bundle, or measured production RPO. `replicatedThroughAt` is an authorization assertion from the
configured readiness publisher; without external replication evidence it must not be reported as measured RPO. A
self-hosted operator may claim geographic DR only after the declared Artifact, Checkpoint, and Memory required set is
replicated by an authenticated publisher, consumed by the successor in the destination failure domain, and validated
with authoritative RTO/RPO evidence. Geographic DR is optional and does not block the supported single-site/logical
multi-cluster product boundary.
