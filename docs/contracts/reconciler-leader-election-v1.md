# Reconciler Leader Election v1

This contract coordinates singleton background controllers across multiple Control Plane replicas. It complements,
rather than replaces, idempotency keys, row-level compare-and-set updates, and exact Kubernetes UID preconditions.

## Durable lease

`reconciler_leases` stores one row per bounded controller name:

- process-incarnation `holder_id`;
- monotonic `fencing_token`;
- database-authored acquire, renew, and expiry times.

Acquire, renew, assert, release, and takeover use PostgreSQL row locking and database time. A takeover increments the
token; an old holder/token pair can never renew or release the new epoch. Holder IDs must not be reused across process
restarts.

The runtime renews and asserts in separate monitor ticks and cancels the work context immediately on loss. Defaults are a
30-second lease, renewal near one third of the TTL, and a shorter assertion interval; all intervals are validated at
startup.

## Authoritative write fence

The leader work context carries the exact lease name, holder, and token. GORM create, update, and delete operations made
with that context install an otherwise opt-in transaction wrapper and lock/revalidate the lease row in the same database
transaction as the mutation. This is required because process pause can delay the monitor goroutine beyond lease expiry;
context cancellation alone is not a write fence.

Controllers that also perform external side effects retain their existing PostgreSQL session advisory lock for the whole
reconcile cycle. An expired durable lease can be taken over only after a transaction advisory lock with that same key
succeeds. PostgreSQL places session and transaction advisory locks in one namespace, so a new epoch cannot begin while an
old cycle is still issuing Kubernetes, Docker, or retention side effects. Once the old cycle releases the lock, its next
database mutation or leader assertion observes the transferred/expired epoch and fails closed.

Cross-Target disaster recovery additionally calls `AssertFenceInTransaction` explicitly before source/destination
Execution mutation, Event append, and Outbox enqueue. This keeps its fencing requirement visible at the highest-risk
transaction boundary.

## Controllers

Durable leadership currently covers:

- Docker Worker Pool reconciliation;
- Kubernetes Execution reconciliation;
- global Target failover;
- Session resource lifecycle enforcement;
- Worker Release automatic rollback; and
- Tenant retention sweeping.

The Outbox dispatcher remains multi-active because its database claim/lease protocol is independently safe across
replicas.

## Operational requirements

- All replicas must use the same PostgreSQL database; SQLite profiles remain single-replica only.
- `SYNARA_RECONCILER_LEASE_HOLDER_ID`, when supplied, must uniquely identify one process incarnation.
- The controller advisory-lock key must exactly match its durable lease name.
- Authoritative mutations must preserve the leader work context. Creating a fresh background context inside a reconcile
  path drops the transaction fence and is prohibited.
- Raw SQL writes and external APIs do not inherit GORM callbacks. They require an explicit lease assertion/CAS or the
  matching full-cycle advisory lock and must be reviewed as such.
