# Stage 2 Drift Audit

Baseline: `codex/saas-tenancy-user` at `05c3c5da`; status updated after Stage 2 Steps 1-2 implementation.

This audit classifies the productionization plan against executable code rather than plan status text.

| Workflow | Status | Evidence and remaining work |
| --- | --- | --- |
| A. Production authentication | partial | Backend production authentication is implemented: Enterprise Dev Bootstrap fail-fast, trusted Public URL/proxy policy, explicit cookie attributes, absolute/idle expiry, token rotation, audited administrator revocation, and PostgreSQL cross-replica concurrency tests. Application-level Tenant/Organization context remains in Workflow H. |
| B. Session API | partial | Project/Session/Turn and Session command idempotency, active/suspended/archived transitions, ordered replay, and PostgreSQL same-key concurrency are implemented. Runtime Event v1 payload validation and oversized-payload Artifact enforcement remain. |
| C. Execution and Worker lifecycle | partial | Registration, protocol/build versions, capability updates, Drain, registration-token rotation behavior, Claim/Lease/Fencing/Recovery, user Cancel, terminal races, and persisted Approval/User Input resolution are implemented. Bidirectional resolution delivery into Provider Runners remains for Stage 3. |
| D. Stateless replicas | implemented | PostgreSQL is authoritative; periodic jobs use Advisory Lock or `SKIP LOCKED`; pool settings and Migration Lock timeout are configurable; readiness checks write access and migration version/checksum; the repeatable two-replica suite covers concurrent Turn/Claim, cross-replica revocation/SSE and replica loss. |
| E. Artifact completion | partial | Temporary-key isolation/promotion, server-side Stat/read/SHA-256 verification, distributed expiry cleanup, forged-hash rejection and live MinIO compatibility are implemented. The shared live-store suite is ready, but a writable real AWS S3 test bucket is still required for final external evidence. |
| F. SSE | implemented | Sequence replay, `Last-Event-ID`, heartbeat, PostgreSQL cross-replica catch-up, globally exact Tenant/User connection leases, write-deadline slow-client handling and SSE metrics are covered by unit, PostgreSQL and two-replica acceptance tests. Web projection/reconnect ownership remains in Workflow H rather than this transport workflow. |
| G. Reliable Outbox | implemented | Durable Claim/Dispatcher, claim recovery, bounded retry, Dead Letter, audited Replay, lifecycle transaction integration, metrics and alerts are implemented and covered by SQLite/PostgreSQL tests. |
| H. Web main flow | partial | Settings can operate Control Plane resources. The primary chat still uses the TypeScript orchestration authority and lacks an application-level Control Plane context/projection. |
| I. Operations | partial | HTTP, DB pool, Login Session, Worker, Execution, Artifact, SSE, Outbox and background metrics plus production alert rules exist. Production runbooks and final release checklist remain incomplete. |

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

The implementation order remains the one in the Stage 2 plan. Steps 0-4 are complete; Step 5 implementation
and local/PostgreSQL/MinIO/multi-replica evidence are complete, with real AWS S3 evidence deferred until a
writable test bucket is explicitly supplied. The full Stage 2 remains in progress while Web and deployment
acceptance continue.
