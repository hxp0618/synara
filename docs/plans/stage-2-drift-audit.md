# Stage 2 Drift Audit

Baseline: `codex/saas-tenancy-user` at `05c3c5da`; status updated after Stage 2 Steps 1-2 implementation.

This audit classifies the productionization plan against executable code rather than plan status text.

| Workflow | Status | Evidence and remaining work |
| --- | --- | --- |
| A. Production authentication | partial | Backend production authentication is implemented: Enterprise Dev Bootstrap fail-fast, trusted Public URL/proxy policy, explicit cookie attributes, absolute/idle expiry, token rotation, audited administrator revocation, and PostgreSQL cross-replica concurrency tests. Application-level Tenant/Organization context remains in Workflow H. |
| B. Session API | partial | Project/Session/Turn and Session command idempotency, active/suspended/archived transitions, ordered replay, and PostgreSQL same-key concurrency are implemented. Runtime Event v1 payload validation and oversized-payload Artifact enforcement remain. |
| C. Execution and Worker lifecycle | partial | Registration, protocol/build versions, capability updates, Drain, registration-token rotation behavior, Claim/Lease/Fencing/Recovery, user Cancel, terminal races, and persisted Approval/User Input resolution are implemented. Bidirectional resolution delivery into Provider Runners remains for Stage 3. |
| D. Stateless replicas | implemented | PostgreSQL is authoritative; periodic jobs use Advisory Lock or `SKIP LOCKED`; pool settings and Migration Lock timeout are configurable; readiness checks write access and migration version/checksum; the repeatable two-replica suite covers concurrent Turn/Claim, cross-replica revocation/SSE and replica loss. |
| E. Artifact completion | partial | Local/MinIO/S3 stores, presigned grants and server-side Stat/Hash validation exist. Temporary-key promotion, distributed cleanup and real S3 compatibility evidence remain. |
| F. SSE | partial | Sequence replay, `Last-Event-ID`, heartbeat and PostgreSQL cross-replica catch-up are covered by automated tests and real two-replica acceptance. Connection limits, catch-up metrics and the explicit slow-client policy remain. |
| G. Reliable Outbox | implemented | Durable Claim/Dispatcher, claim recovery, bounded retry, Dead Letter, audited Replay, lifecycle transaction integration, metrics and alerts are implemented and covered by SQLite/PostgreSQL tests. |
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

The implementation order remains the one in the Stage 2 plan. Steps 0-4 are complete; Artifact and SSE
operational completion is the next implementation step. The full Stage 2 remains in progress.
