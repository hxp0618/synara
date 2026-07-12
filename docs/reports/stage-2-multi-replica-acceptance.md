# Stage 2 Multi-replica Acceptance Report

- Date: 2026-07-12
- Branch: `codex/saas-tenancy-user`
- Scope: Stage 2 Steps 2-3 PostgreSQL, stateless multi-replica correctness, and authentication regression
- Result: PASS

## Runtime topology

The isolated acceptance suite started:

- PostgreSQL 17 as authoritative metadata and event storage.
- MinIO as the shared object store.
- Two Control Plane containers built from the current worktree.
- One disposable Alpine container used as an in-network HTTP/SSE test runner. The acceptance suite does not
  rebuild the unrelated TypeScript Provider Runtime.

The latest successful run used Compose project `synara-stage2-step4-acceptance-12757`. The script removed all
containers, networks and volumes after completion.

## Verified behavior

1. Both replicas became healthy against one database and object store.
2. `/ready` reported database connectivity, database write capability and required/applied Schema Version.
3. Login Session revocation written through replica B was immediately rejected by replica A.
4. An SSE connection attached to replica A received a Turn Event written through replica B by polling the
   authoritative PostgreSQL Session Event sequence.
5. Two replicas concurrently attempted to Claim the same explicit Execution; exactly one received it.
6. Replica A and replica B concurrently created separate Turns for one Session; row locking produced distinct
   Turns and contiguous Event sequences.
7. Replica A was stopped. Replica B remained Ready and recovered missed Events from `Last-Event-ID` without
   replaying the acknowledged Event.
8. Startup migrations completed under two concurrent replicas with one Advisory Lock boundary.
9. The production-authentication changes retained immediate cross-replica logout rejection while using
   the configured cookie and Session lifetime policy.
10. Project and Session replay returned the original resources through the other replica.
11. Two replicas concurrently submitted the same Turn `Idempotency-Key`; one request executed and the
    other returned the stored response without duplicating the Event sequence.

## Automated evidence

The following focused coverage was added:

- Cross-service-instance SSE catch-up with separate process-local Brokers.
- PostgreSQL Advisory Lock single-owner and post-release reacquisition for Docker Reconciler, Kubernetes
  Reconciler and Retention Sweeper keys.
- Schema readiness for missing migration/checksum state and missing SQLite tables.
- Database write readiness rejection for SQLite query-only and PostgreSQL read-only transactions.
- Configured PostgreSQL pool limit verification.
- Bounded startup Migration Lock wait.
- PostgreSQL Login Session idle expiry with independent replica connection pools.
- Cross-replica administrator revocation and concurrent Authenticate/Revoke without Session resurrection.

Full Go tests passed against a fresh PostgreSQL 17 container:

```bash
SYNARA_TEST_DATABASE_URL='postgres://...' go test -count=1 ./...
```

The repeatable runtime acceptance command is:

```bash
deploy/saas/multi-replica-acceptance.sh
```

## Remaining scope

This report closes the runtime regression evidence for Stage 2 Steps 2-3. It does not claim the final
Enterprise K8s acceptance from Step 7, API idempotency, Artifact/SSE operational limits, Tenant Context,
or the Web main-flow cutover.
