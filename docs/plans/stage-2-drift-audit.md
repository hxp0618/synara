# Stage 2 Drift Audit

Baseline: `codex/saas-tenancy-user` at `b507b0c3`; status updated on 2026-07-15 after Stage 2 Steps 0-7,
Control Plane stream-recovery browser verification and the repository-controlled deployment suites were rerun
from a clean detached worktree.

Evidence boundary: the latest fixed evidence record is
`docs/reports/stage-2-production-acceptance-b507b0c3.md` at Migration `000031`, with Result `PASS`. Single-node,
Multi-replica, Failure and disposable Stage 2 Kind passed at this baseline together with Go/Race/PostgreSQL 17 and
the isolated local SQLite UI path. Real AWS S3 remains a separate external evidence gate.

This audit classifies the productionization plan against executable code rather than plan status text.

| Workflow                          | Status                                                     | Evidence and remaining work                                                                                                                                                                                                                                                                                                                                                                   |
| --------------------------------- | ---------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A. Production authentication      | implemented                                                | Enterprise Dev Bootstrap fail-fast, trusted Public URL/proxy policy, explicit cookie attributes, absolute/idle expiry, token rotation, audited administrator revocation, PostgreSQL cross-replica tests, and application-level Tenant/Organization context are implemented.                                                                                                                   |
| B. Session API                    | implemented                                                | Project/Session/Turn and Session command idempotency, active/suspended/archived transitions, ordered replay, PostgreSQL same-key concurrency, canonical Runtime Event v2 validation and 65,536-byte Artifact fallback are implemented. Runtime Event v1 remains only for the explicit legacy compatibility path and existing-row replay.                                                         |
| C. Execution and Worker lifecycle | implemented                                                | Managed Worker Protocol v2 registration, instance fencing, protocol/build versions, capability updates, Drain, registration-token rotation behavior, Claim/Lease/Fencing/Recovery, user Cancel, terminal races, persisted Approval/User Input resolution, Worker-offline recovery and Generation replacement are implemented. The v1 Worker contract is legacy-only.                       |
| D. Stateless replicas             | implemented                                                | PostgreSQL is authoritative; periodic jobs use Advisory Lock or `SKIP LOCKED`; pool settings and Migration Lock timeout are configurable; readiness checks write access and migration version/checksum; the repeatable two-replica suite covers concurrent Turn/Claim, cross-replica revocation/SSE and replica loss.                                                                         |
| E. Artifact completion            | implemented, external AWS evidence pending                 | Temporary-key isolation/promotion, server-side Stat/read/SHA-256 verification, distributed expiry cleanup, forged-hash rejection, live MinIO and Enterprise `s3` adapter behavior against an S3-compatible endpoint are implemented. The shared live-store suite is ready, but a writable real AWS S3 test bucket is still required before claiming AWS service/IAM evidence.                 |
| F. SSE                            | implemented                                                | Sequence replay, `Last-Event-ID`, heartbeat, PostgreSQL cross-replica catch-up, globally exact Tenant/User connection leases, write-deadline slow-client handling and SSE metrics are covered by unit, PostgreSQL and two-replica acceptance tests. Web projection/reconnect ownership remains in Workflow H rather than this transport workflow.                                             |
| G. Reliable Outbox                | implemented                                                | Durable Claim/Dispatcher, claim recovery, bounded retry, Dead Letter, audited Replay, lifecycle transaction integration, metrics and alerts are implemented and covered by SQLite/PostgreSQL tests.                                                                                                                                                                                           |
| H. Web main flow                  | implemented                                                | Application-level Control Plane context owns authentication, Tenant/Organization capabilities and SaaS projection. Main Project/Session/Turn/SSE flow is Control Plane authoritative; delayed local snapshots are rejected, refresh restores PostgreSQL state, and the reconnect banner stays visible through Control Plane loss without projecting a false completion.                     |
| I. Operations                     | implemented and deployment-verified at `b507b0c3`/`000031` | HTTP, DB pool, Login Session, Worker, Execution, Artifact, SSE, Outbox and background metrics, Prometheus alerts, production Runbook, release checklist and dynamic sensitive-log audits are implemented. The fixed evidence includes Go/Race/PostgreSQL 17, TypeScript tests/builds, local SQLite UI, browser SSE restart recovery, four deployment suites, frontend/backend Proxy and exact cleanup. |

## Process-local state classification

| Component                     | Classification                                                            |
| ----------------------------- | ------------------------------------------------------------------------- |
| Session Event Broker          | `process-local-wakeup-only`; PostgreSQL Session Events are authoritative. |
| Docker/Kubernetes reconcilers | `best-effort-local-cache` plus database/advisory-lock coordination.       |
| Retention sweeper             | `best-effort-local-cache` plus database advisory lock.                    |
| Worker receipts               | `authoritative-postgres`.                                                 |
| Login sessions                | `authoritative-postgres`.                                                 |
| Execution Claim and Lease     | `authoritative-postgres`.                                                 |
| Provider resume cursor        | `authoritative-postgres`, encrypted.                                      |
| Artifact metadata             | `authoritative-postgres`; payload is `authoritative-object-store`.        |

Steps 0-7 and all repository-controlled completion gates are implemented and accepted. The
`b507b0c3`/`000031` report fixes the current DDL digests, Go/Race/PostgreSQL 17 result, 32-file/398-test TypeScript
result, production builds, isolated local SQLite UI, browser SSE stop/restart recovery, SaaS Web Proxy path,
Single-node and Multi-replica Compose, Worker/MinIO/PostgreSQL fault drills, disposable two-replica Kind rollout
and exact cleanup.

The only remaining external provider-specific evidence is the real AWS S3 Live Store run. It requires an
explicitly authorized writable Bucket and must remain recorded as not executed until that authority is supplied;
MinIO or a custom S3-compatible endpoint is not presented as AWS evidence.
