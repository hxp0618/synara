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
- Worker/Execution/Execution Target lifecycle status and Target kind;
- Worker Lease expiration state;
- background job kind (`docker`, `kubernetes`, `retention`, `outbox`);
- Artifact operation/result and SSE limit scope.

Tenant, Organization, User, Session, Turn, Execution, Worker, Pod, Artifact,
Credential, Request, and Trace identifiers are forbidden as metric labels. Domain
gauges are read from authoritative metadata at scrape time instead of maintaining
parallel counters that can drift.

## Production metrics

The endpoint includes:

- HTTP request count and latency;
- database pool max/open/in-use/idle connections plus wait count/duration;
- active login sessions using absolute and idle expiry;
- authoritative Execution, Worker, Target and Lease state;
- authoritative active/expired SSE connection leases;
- SSE catch-up latency, delivered backlog Events and connection-limit rejection count;
- Artifact lifecycle operations, processed bytes and authoritative ready bytes;
- Outbox pending/retry/dead-letter count and oldest pending age;
- bounded background-job runs, failures, duration and last success.

SSE connection leases are PostgreSQL rows with a crash-expiring TTL. Connection acquisition locks one
Tenant row in a short transaction before checking Tenant and User limits, so multiple replicas cannot
oversubscribe the configured limits. Slow clients receive a bounded per-write deadline and reconnect
from their last durable Session Event sequence.
