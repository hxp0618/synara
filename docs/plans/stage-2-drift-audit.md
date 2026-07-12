# Stage 2 Drift Audit

Baseline: `codex/saas-tenancy-user` at `425554e6`; status updated after Stage 2 Steps 0-7 implementation,
repository-controlled acceptance, and the post-`702cb0d0` browser route verification on 2026-07-13.

This audit classifies the productionization plan against executable code rather than plan status text.

| Workflow | Status | Evidence and remaining work |
| --- | --- | --- |
| A. Production authentication | implemented | Enterprise Dev Bootstrap fail-fast, trusted Public URL/proxy policy, explicit cookie attributes, absolute/idle expiry, token rotation, audited administrator revocation, PostgreSQL cross-replica tests, and application-level Tenant/Organization context are implemented. |
| B. Session API | implemented | Project/Session/Turn and Session command idempotency, active/suspended/archived transitions, ordered replay, PostgreSQL same-key concurrency, Runtime Event v1 object/version validation and 65,536-byte Artifact fallback are implemented. |
| C. Execution and Worker lifecycle | implemented | Registration, protocol/build versions, capability updates, Drain, registration-token rotation behavior, Claim/Lease/Fencing/Recovery, user Cancel, terminal races, persisted Approval/User Input resolution, Worker-offline recovery and Generation replacement are implemented. Provider Runner bidirectional resolution delivery is intentionally Stage 3 scope. |
| D. Stateless replicas | implemented | PostgreSQL is authoritative; periodic jobs use Advisory Lock or `SKIP LOCKED`; pool settings and Migration Lock timeout are configurable; readiness checks write access and migration version/checksum; the repeatable two-replica suite covers concurrent Turn/Claim, cross-replica revocation/SSE and replica loss. |
| E. Artifact completion | implemented, external AWS evidence pending | Temporary-key isolation/promotion, server-side Stat/read/SHA-256 verification, distributed expiry cleanup, forged-hash rejection, live MinIO and Enterprise `s3` adapter behavior against an S3-compatible endpoint are implemented. The shared live-store suite is ready, but a writable real AWS S3 test bucket is still required before claiming AWS service/IAM evidence. |
| F. SSE | implemented | Sequence replay, `Last-Event-ID`, heartbeat, PostgreSQL cross-replica catch-up, globally exact Tenant/User connection leases, write-deadline slow-client handling and SSE metrics are covered by unit, PostgreSQL and two-replica acceptance tests. Web projection/reconnect ownership remains in Workflow H rather than this transport workflow. |
| G. Reliable Outbox | implemented | Durable Claim/Dispatcher, claim recovery, bounded retry, Dead Letter, audited Replay, lifecycle transaction integration, metrics and alerts are implemented and covered by SQLite/PostgreSQL tests. |
| H. Web main flow | implemented | Application-level Control Plane context owns authentication, Tenant/Organization capabilities and SaaS projection. Main Project/Session/Turn/SSE flow is Control Plane authoritative; delayed local snapshots are rejected, refresh restores PostgreSQL state, and unconfigured instances retain local SQLite behavior. |
| I. Operations | implemented | HTTP, DB pool, Login Session, Worker, Execution, Artifact, SSE, Outbox and background metrics, Prometheus alerts, Compose/Kind failure acceptance, production Runbook, release checklist and dynamic sensitive-log audits are complete. |

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

Steps 0-7 and every repository-controlled completion gate are implemented and accepted. Evidence includes the
SaaS browser main flow, local-mode refresh recovery, single-node and multi-replica Compose, Worker/MinIO/database
fault drills, and a disposable two-replica Kind rollout with Pod deletion and PVC-backed dependency recovery.

The only remaining external provider-specific evidence is the real AWS S3 Live Store run. It requires an
explicitly authorized writable Bucket and must remain recorded as not executed until that authority is supplied;
MinIO or a custom S3-compatible endpoint is not presented as AWS evidence.
