# Control-plane observability v1

## Endpoints

- `GET /health` is a process liveness check. It does not contact dependencies.
- `GET /ready` checks the metadata database and Artifact Store with a bounded timeout
  and returns per-dependency status, configured kind, and latency.
- `GET /metrics` exposes Prometheus text format. A database collection failure returns
  HTTP 503 with `synara_metrics_collection_success 0`.

## Correlation

Every HTTP response includes `X-Request-ID`, `X-Trace-ID`, and a W3C `Traceparent`.
The server accepts a valid 32-hex-character trace ID from either `X-Trace-ID` or
`Traceparent`; otherwise it creates a new trace. Structured request completion and
server-error logs contain both identifiers.

`X-Request-ID` remains the idempotency/audit correlation identifier. `X-Trace-ID` is
diagnostic only and must not be used as a business key.

When `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is configured on a process, that Control Plane
or agentd process exports OTLP/HTTP spans with parent-based ratio sampling (`SYNARA_OTEL_TRACE_SAMPLE_RATIO`, default
`0.1`). Remote/managed agentd processes receive a credentialless collector/relay endpoint and sampling environment;
the Control Plane configuration is not forwarded through a Worker claim. HTTP ingress
extracts W3C context and creates a server span. Migration `000104` freezes that diagnostic parent on each new Execution;
Worker claim returns it so agentd can continue the trace with `worker.execution` and `provider.run`, while Provider Host
receives `SYNARA_TRACEPARENT`. Cross-Target failover inherits the source Execution parent. With no exporter endpoint,
export is disabled but propagation/correlation remains available.

Enterprise Control Plane processes use the fail-closed mTLS exporter policy. When export is enabled, the effective trace
endpoint must be an absolute HTTPS URL without userinfo, query or fragment; the protocol is `http/protobuf`; and both
client-certificate and client-key paths must resolve to absolute regular files. The deployment
cannot use `OTEL_EXPORTER_OTLP[_TRACES]_INSECURE=true` to override that transport boundary. It
also declares a bounded collector Region and trace retention of 1–90 days through `SYNARA_OTEL_COLLECTOR_REGION` and
`SYNARA_OTEL_TRACE_RETENTION_DAYS`. Those values are emitted only as bounded
`synara.telemetry.region|retention_days` resource attributes. They make configuration drift observable but do not prove the
collector stored data in that Region or deleted it on time; the candidate residency/security annex must verify that external
control. Development and single-node processes may use credential-free HTTP collectors, but any OTLP authentication Header
requires HTTPS and incomplete mTLS configuration is always rejected.

Every non-Local agentd uses the separate enterprise Worker policy. It forbids Header credentials, client certificates and
client keys in the Worker process. The endpoint is either credential-free HTTPS, with workload identity enforced outside
the Worker filesystem, or explicit `http://127.0.0.1:<port>` / `http://[::1]:<port>` to a same-host relay. DNS aliases,
remote plaintext endpoints and loopback URLs without an explicit port fail closed. The relay/service mesh owns external
mTLS identity and must not expose it to agentd or the Provider child process.

The Kubernetes production base keeps the endpoint empty by default. Endpoint, protocol, sample ratio, collector Region and
retention are non-secret ConfigMap inputs; the mTLS client certificate and key are mounted from the Control Plane Secret as
read-only files and referenced only by absolute in-container paths. The base does not expose OTLP header authentication or
an insecure transport override. `scripts/stage6-security/validate_observability_deployment.py` protects this source wiring,
but its receipt is not collector connectivity, IAM, storage-location or retention-deletion evidence.

Generated native and warm-pool Kubernetes Workers obtain only explicit non-secret exporter keys from the fixed optional
`synara-agentd-observability-config-<target-uuid>` ConfigMap in their Target Namespace. Full Target UUID suffixes prevent
shared-namespace Targets from sharing exporter policy. The Control Plane does not read or persist the resource. Pod
workload-identity validation rejects resource-name substitution, non-optional references, Header credentials, client
identity and insecure overrides. The sandbox-operator standard template uses the same contract;
Cocoon guest remains responsible for equivalent target-local configuration.

Managed Docker and SSH agentd use a deployment-authority root configured only on the Control Plane, never an Execution
Target field. Docker binds only the UUID-named child directory read-only; SSH receives only the derived remote
`<root>/<target-uuid>/observability.env` path. agentd loads that file before exporter construction, accepts six explicit
OTLP keys, confines the optional server-CA path beside the file, and rejects symlinks, writable authority,
duplicate/unknown keys and conflicting ambient values. The file may not carry Header credentials, client identity or
insecure overrides. An empty root keeps export disabled.

Trace context is never an authorization, Tenant-scope, fencing, scheduling or idempotency authority. Spans may contain
bounded resource IDs needed for diagnosis, but must never attach prompt/input text, Credential/KMS/Login/Lease tokens,
Provider payloads or Presigned URLs. Collector access, retention and storage Region are part of the deployment security and
residency annex. PostgreSQL rejects invalid or mutated persisted trace context; SQLite applies the same insertion and
immutability guards.

## Cardinality contract

Metrics may use only bounded labels:

- registered HTTP route pattern, method, and status;
- login method (`dev`, `oidc`, `saml`) and result;
- Worker Lease renewal result and bounded fencing operation;
- Session Event append result;
- Worker/Execution/Execution Target lifecycle status and Target kind;
- Session resource state, configured warm-pool mode, suspend status/reason/completion mode, and Recovery Bundle reason;
- Generation recovery outcome (`started`, `ready`, `terminal-before-start`, `terminal-before-ready`, `pending`),
  durable terminal outcome, requested warm mode/allocation result, bounded resume strategy/reason, and supported
  Provider name for validated runtime fallback;
- bounded Kubernetes Pod failure class (`pod-apply-failed`, `pending-timeout`, `unschedulable`, `image-pull`,
  `container-start`, `evicted`, `oom-killed`, `pod-failed`);
- physical Worker target kind, Pool mode, capacity class, and lifecycle state;
- Execution Target health/capacity state and immutable failover status/reason;
- immutable signed Platform routing publication count and latest receive timestamp, with no publisher/Target label;
- capacity-admission mode and exact reservation-authority freshness; acknowledgement identities and Target IDs are never
  labels;
- Worker Pool warm-capacity class, fresh/expired state, warm-supported state, and bounded counter kind;
- bounded Reconciler controller name and active/expired lease state;
- cloud provider, currency, charge kind, and bounded reconciliation state;
- Worker Lease expiration state;
- Provider Credential access Lease state (`active`, `refresh-window-closed`, `expired`,
  `credential-unavailable`);
- background job kind (`docker`, `kubernetes`, `target-failover`, `resource-lifecycle`, `retention`, `outbox`);
- Artifact operation/result and SSE limit scope.

Tenant, Organization, User, Session, Turn, Execution, Worker, Pod, Artifact,
Credential, Request, and Trace identifiers are forbidden as metric labels. Domain
gauges are read from authoritative metadata at scrape time instead of maintaining
parallel counters that can drift.

The shared label serializer enforces this at runtime for both base and appended
histogram/quantile labels. It rejects identifier-shaped keys, UUID/commit/digest,
URL/email/known credential-shaped values, and values longer than 128 bytes before
rendering. Rejection uses a constant panic message so a failed scrape cannot copy
the rejected identifier or secret into the recovery log.

## Production metrics

The endpoint includes:

- HTTP request count and latency;
- database pool max/open/in-use/idle connections plus wait count/duration;
- completed login attempts as `synara_login_attempts_total{method,result}`;
- active login sessions using absolute and idle expiry;
- authoritative Execution, Worker, Target and Lease state;
- authoritative Session resource-state and configured warm-pool-mode inventory;
- durable suspend-attempt inventory by status/reason and status/completion-proof mode, plus immutable Recovery Bundle inventory;
- Generation claim-to-start and Bundle-to-Provider-`session.started` latency histograms by bounded recovery reason
  and Target kind;
- durable Generation start/Provider-ready outcomes, allowing recovery success ratios to be computed without
  process-local counters;
- trailing-30-day Generation terminal outcomes, Warm Pool hit/fallback results, and end-to-end
  dispatch-request-to-Provider-ready P50/P95/P99 gauges;
- trailing-30-day `synara_execution_pod_queue_duration_seconds_30d` dispatch-to-first-apply and
  `synara_execution_pod_provisioning_duration_seconds_30d` first-apply-to-first-Running P50/P95/P99 gauges, plus
  `synara_execution_pod_failure_generations_30d{failure_class,target_kind}`;
- authoritative durable queued/recovering Execution inventory as
  `synara_execution_queue_depth{target_kind,capacity_class}` and
  `synara_execution_queue_oldest_age_seconds{target_kind,capacity_class}`; Tenant, Target, and Execution
  identifiers are never labels;
- retained physical Worker/Pod incarnation count, runtime, active/idle seconds, and requested CPU core-seconds,
  memory byte-seconds, and ephemeral-storage byte-seconds. Terminal history comes from exact daily rollups while
  pending-rollup terminal facts and nonterminal facts remain authoritative raw input. Requested-resource seconds are
  cost proxies, not currency;
- `synara_metric_rollup_pending_facts{kind="worker-incarnation"}` and
  `synara_metric_rollup_buckets{kind="worker-incarnation"}` for bounded rollup backlog/inventory health;
- `synara_execution_generation_metric_rollup_pending_facts{kind}` and
  `synara_execution_generation_metric_rollup_buckets{kind}` for bounded Generation and Pod-failure rollup health;
- expiring Target health/capacity inventory, immutable cross-Target failover attempts, and bounded durable Reconciler
  leadership state;
- immutable signed routing-ingestion progress as `synara_platform_routing_publications_total` and
  `synara_platform_routing_publication_latest_timestamp_seconds`; publisher identity, key ID, nonce, and Target ID are
  intentionally absent from metric labels and remain queryable only in the receipt table;
- retained immutable admission evidence as `synara_execution_capacity_admission_evidence{mode}` and exact Target
  reservation authority/acknowledged/unacknowledged/strict-used unit aggregates by bounded `freshness`; no Tenant,
  Target, Execution, Generation, Pod, publisher, or digest identity is exposed;
- expiring per-Pool warm-capacity authority as
  `synara_worker_pool_warm_capacity_authorities{capacity_class,freshness,warm_supported}` and fresh desired/claimed/
  ready-idle unit sums as `synara_worker_pool_warm_capacity_units{capacity_class,kind}`;
- tariff-rated `synara_cloud_cost_estimated_micros`, imported `synara_cloud_cost_actual_micros`, and
  `synara_cloud_cost_reconciliation_variance_micros`. Estimates remain explicitly distinct from actual invoice truth;
- durable shared-allocation scheduler inventory as
  `synara_billing_shared_allocation_schedule_periods{schedule_kind,outcome,due_state}`; schedule kind is bounded to
  `static | monthly-utc` and no schedule digest, Target ID, or period timestamp becomes a label;
- sealed account-invoice allocation inventory and conservation gaps as
  `synara_billing_shared_actual_allocation_runs{provider,currency,state}`,
  `synara_billing_shared_actual_allocation_lines{provider,currency,state,kind}` and
  `synara_billing_shared_actual_allocation_amount_micros{provider,currency,state,kind}`; `kind` is bounded to
  `selected | unallocated`, and no Tenant, Target, import, resource, digest, or period identity becomes a label;
- bounded `execution.leased` resume decisions and strictly validated native-cursor runtime fallback reasons;
- online/draining Workers whose last Heartbeat is older than the configured Worker timeout as
  `synara_stale_workers{status,target_kind}`; offline/terminated Workers are not double-counted as stale;
- Worker Lease renewal outcomes as `synara_worker_lease_renewals_total{result}`;
- authoritative short-lived Provider Credential access inventory as
  `synara_provider_credential_access_leases{state}`. Its state is derived from the persisted access window,
  immutable Generation Grant, Session absolute expiry, and current Credential missing/revoked/expired/version
  truth; it never uses browser presence or Worker heartbeat as semantic activity;
- Lease, Generation and Worker-incarnation rejections as
  `synara_worker_fencing_rejections_total{operation}`;
- Worker Runtime Event append latency as
  `synara_session_event_append_duration_seconds{result}`;
- authoritative active/expired SSE connection leases;
- SSE catch-up latency, delivered backlog Events and connection-limit rejection count;
- Artifact lifecycle operations, processed bytes and authoritative ready bytes;
- Outbox pending/retry/dead-letter count and oldest pending age;
- bounded background-job runs, failures, duration and last success.

The checked-in Prometheus rules alert on an unavailable Control Plane, database saturation,
expired Worker Leases, Worker offline surges, Execution recovery surges, Outbox delay/dead letters,
Artifact failures, and SSE catch-up delay. Worker and Execution surge rules use authoritative status
gauges and require a sustained absolute threshold so a transient single-instance replacement does not page.

Stage 6 adds 30-day recording rules and budget alerts for Availability, API Latency, Execution Start Delay, and Event
Delay. Availability is intentionally sourced from an external `probe_success{job="synara-public-readiness"}` series;
the in-process endpoint cannot measure time when it is unreachable. Definitions and claim boundaries are in
[`enterprise-service-level-objectives-v1.md`](enterprise-service-level-objectives-v1.md). Missing or insufficient samples
are not passing evidence.

Stage 4 Generation metrics now use immutable `execution_generation_facts`; cold-start, queue, Pod provisioning, and
outcome series are bounded to a trailing 30-day window. `execution_generation_pod_failure_facts` retains one immutable
first proof per Generation/failure class and only advances its last-observed time, so a later healthy replacement does
not erase earlier failure evidence and repeated reconcile polls do not increase a counter. Kubernetes status messages
and Pod/Execution identifiers are never labels. Migration `000081` seals terminal metric-source fields behind one
append-only Generation membership cursor and gives every immutable Pod-failure first proof its own cursor. The shared
leader cycle increments categorical counts and integer duration-histogram buckets before completing those cursors in
the same transaction. Duration buckets grow by at most two percent (plus one microsecond at the smallest values), so
P50/P95/P99 are derived from merged sample ranks rather than by summing precomputed percentiles.

The trailing window remains exact at both time boundaries: metrics merge rollups only for complete UTC days strictly
inside the 30-day interval. The first and current partial UTC days, nonterminal Generations, and pending-rollup facts
remain bounded raw reads in the same PostgreSQL `REPEATABLE READ` snapshot. A concurrent rollup commit therefore
appears entirely as raw membership or entirely as a bucket, never both and never neither.
Local PostgreSQL migration, two-connection coordination, replay, projection, and negative-gate evidence is recorded in
[`stage-4-generation-metric-rollup-orbstack-pg-20260726-final1.md`](../reports/stage-4-generation-metric-rollup-orbstack-pg-20260726-final1.md).

Physical resource metrics use immutable `worker_incarnation_facts`, exact terminal/missing-Pod observations, and
authenticated Pod resource requests. Migration `000079` creates exact UTC-day terminal rollups keyed only by bounded
Target kind, Pool mode, and capacity class. Every terminal fact has one append-only membership entry. A leader-scoped
`synara:metric-rollup` cycle takes a transaction advisory lock, increments all selected buckets, and changes those
entries from pending to completed in the same transaction. A failed transaction changes neither side. Metrics read a
PostgreSQL `REPEATABLE READ` snapshot and combine rollup totals, pending terminal facts, and nonterminal facts, so a
commit concurrent with scrape can neither create a gap nor double-count. Delayed scheduling therefore affects backlog
and scrape work, not metric truth. Daily buckets are aggregated by SQL before projection, so scrape transfer no longer
grows with terminal incarnation count.

Configured `warm_pool_mode` remains demand inventory; `synara_warm_pool_acquisitions_30d` is the separate observed
hit/fallback truth. Requested-resource seconds must not be described as cloud invoice or currency cost until an
authoritative tariff/billing source exists. Live warm-capacity metrics are Reconciler observations with a bounded TTL,
not reservations or execution quota.

SSE connection leases are PostgreSQL rows with a crash-expiring TTL. Connection acquisition locks one
Tenant row in a short transaction before checking Tenant and User limits, so multiple replicas cannot
oversubscribe the configured limits. Slow clients receive a bounded per-write deadline and reconnect
from their last durable Session Event sequence.
