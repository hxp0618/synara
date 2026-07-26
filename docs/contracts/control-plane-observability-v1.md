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
- physical Worker target kind, Pool mode, capacity class, and lifecycle state;
- Execution Target health/capacity state and immutable failover status/reason;
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
- retained physical Worker/Pod incarnation count, runtime, active/idle seconds, and requested CPU core-seconds,
  memory byte-seconds, and ephemeral-storage byte-seconds. Requested-resource seconds are cost proxies, not currency;
- expiring Target health/capacity inventory, immutable cross-Target failover attempts, and bounded durable Reconciler
  leadership state;
- expiring per-Pool warm-capacity authority as
  `synara_worker_pool_warm_capacity_authorities{capacity_class,freshness,warm_supported}` and fresh desired/claimed/
  ready-idle unit sums as `synara_worker_pool_warm_capacity_units{capacity_class,kind}`;
- tariff-rated `synara_cloud_cost_estimated_micros`, imported `synara_cloud_cost_actual_micros`, and
  `synara_cloud_cost_reconciliation_variance_micros`. Estimates remain explicitly distinct from actual invoice truth;
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

Stage 4 Generation metrics now use immutable `execution_generation_facts`; cold-start and outcome series are bounded to
a trailing 30-day window. Physical resource metrics use immutable `worker_incarnation_facts`, exact terminal/missing-Pod
observations, and authenticated Pod resource requests. Their scrape cost still grows with retained Worker incarnation
history, so rolling pre-aggregation remains required for unlimited retention. Configured `warm_pool_mode` remains demand
inventory; `synara_warm_pool_acquisitions_30d` is the separate observed hit/fallback truth. Requested-resource seconds
must not be described as cloud invoice or currency cost until an authoritative tariff/billing source exists.
Live warm-capacity metrics are Reconciler observations with a bounded TTL, not reservations or execution quota.

SSE connection leases are PostgreSQL rows with a crash-expiring TTL. Connection acquisition locks one
Tenant row in a short transaction before checking Tenant and User limits, so multiple replicas cannot
oversubscribe the configured limits. Slow clients receive a bounded per-write deadline and reconnect
from their last durable Session Event sequence.
