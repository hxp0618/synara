# Outbox Delivery v1

Business transactions insert versioned messages into `outbox_messages` before committing. Delivery is
at least once: a publisher may have completed its side effect before the dispatcher records the
acknowledgement, so every consumer must use the Outbox Message ID as an idempotency key.

## Claim and ownership

- PostgreSQL dispatchers claim ordered batches with `FOR UPDATE SKIP LOCKED`.
- Claim transactions only set `claimed_by`, `claimed_at`, and `claim_expires_at`; they never perform
  network I/O.
- Acknowledge and failure updates require the same `claimed_by` value.
- An expired claim is eligible for another replica.
- SQLite uses the same fields and Message contract, but only under the single-replica Personal Profile.

## Retry and dead letter

Publishing failures increment `attempts`, store a whitespace-normalized error summary capped at 512
bytes, and schedule exponential backoff with deterministic jitter. The summary must not contain the
message Payload, Credential, Token, Prompt, or presigned URL. Messages that reach the configured
maximum attempts receive `dead_lettered_at` and stop being claimable.

Tenant Owner/Admin operators may inspect operational metadata without reading Payloads:

```text
GET  /v1/tenants/{tenantId}/outbox-messages?status=pending|retrying|dead-letter|published|all
POST /v1/tenants/{tenantId}/outbox-messages/{messageId}/replay
```

Replay is audited, clears the dead-letter state, resets attempts, and makes the original immutable
Message ID claimable again.

## Ordering

There is no global ordering across Topics. Claim order is `available_at`, `created_at`, then `id`.
Producers use stable resource transition keys, and consumers serialize any stronger per-Session or
per-Execution ordering they require from the authoritative Session Event sequence.

## Built-in driver

The `postgres-outbox` driver acknowledges the durable database dispatch boundary. Workers consume
authoritative Execution state through the idempotent Claim API, so a duplicate wake-up cannot create a
second valid Lease. External queue builds implement the same `Publisher` interface; this repository
does not silently treat an unconfigured external driver as PostgreSQL delivery.
