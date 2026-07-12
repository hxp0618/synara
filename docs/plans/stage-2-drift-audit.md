# Stage 2 Drift Audit

Baseline: `codex/saas-tenancy-user` at `05c3c5da` plus the current uncommitted roadmap documents.

This audit classifies the productionization plan against executable code rather than plan status text.

| Workflow | Status | Evidence and remaining work |
| --- | --- | --- |
| A. Production authentication | partial | OIDC, SAML, persisted login sessions and secure-cookie support exist. Enterprise dev-bootstrap rejection, trusted-proxy policy, idle expiry and rotation/revocation concurrency still need completion. |
| B. Session API | partial | Tenant-scoped Project/Session/Turn/Event APIs and ordered replay exist. Uniform idempotency keys, the full state machine and payload validation limits remain. |
| C. Execution and Worker lifecycle | partial | Registration, heartbeat, Claim, Lease, Generation fencing and recovery exist. Protocol version negotiation, Drain, token rotation, Cancel races and persisted Approval/Input remain. |
| D. Stateless replicas | partial | PostgreSQL is authoritative and reconcilers use database coordination in several paths. Pool configuration, schema-aware readiness and a repeatable two-replica suite remain. |
| E. Artifact completion | partial | Local/MinIO/S3 stores, presigned grants and server-side Stat/Hash validation exist. Temporary-key promotion, distributed cleanup and real S3 compatibility evidence remain. |
| F. SSE | partial | Sequence replay, `Last-Event-ID`, heartbeat and PostgreSQL catch-up exist. Connection limits, catch-up metrics, slow-client policy and cross-replica acceptance remain. |
| G. Reliable Outbox | missing | Rows are inserted transactionally, but there is no durable Claim/Dispatcher, retry, claim recovery, dead letter, replay or complete metric set. |
| H. Web main flow | partial | Settings can operate Control Plane resources. The primary chat still uses the TypeScript orchestration authority and lacks an application-level Control Plane context/projection. |
| I. Operations | partial | HTTP, Worker, Execution and dependency metrics exist. Outbox/SSE/DB-pool coverage, alerts and production runbooks remain incomplete. |

## Process-local state classification

| Component | Classification |
| --- | --- |
| Session Event Broker | `process-local-wakeup-only`; PostgreSQL Session Events are authoritative. |
| Docker/Kubernetes reconcilers | `best-effort-local-cache` plus database/advisory-lock coordination. |
| Retention sweeper | `best-effort-local-cache` plus database advisory lock. |
| Worker receipts | `authoritative-postgres`. |
| Login sessions | `authoritative-postgres`. |
| Execution Claim and Lease | `authoritative-postgres`. |
| Provider resume cursor | `authoritative-postgres`, encrypted. |
| Artifact metadata | `authoritative-postgres`; payload is `authoritative-object-store`. |

The implementation order remains the one in the Stage 2 plan. Reliable Outbox is the first missing
production primitive and must be completed before broader multi-replica acceptance.
