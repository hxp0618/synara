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

## Internal incident communication intent

Broad-impact employee-safe Incident updates are also written transactionally to the `incident.internal-update` Topic.
The payload contains only the Incident identity, severity, sanitized summary, affected component/Region sets and the
credential-free internal Status Board references. It contains no operator email, Credential, Prompt or Tenant-private
payload. The Outbox row proves only that a durable notification intent was committed; it does not prove Status Board,
paging or employee-channel delivery. `incident.internal-update` is never acknowledged by the built-in database
publisher. When `SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL` and its dedicated 32-byte HMAC key are configured, the Control
Plane routes only this Topic to the failure-independent HTTPS receiver, sends the Outbox Message ID as
`Idempotency-Key`, identifies the active signing-key version through `X-Synara-Key-Id`, and signs the exact versioned JSON
body as `X-Synara-Signature: v1=<hex-hmac-sha256>`. Key bytes may never change without a new Key ID.
Redirects are not followed. A non-2xx response is retried through the normal Outbox policy and can dead-letter; without
the adapter, the intent fails closed instead of being reported as published. A 2xx response proves receiver acceptance,
not employee receipt, paging or Status Board publication, so the external system must retain its own delivery evidence
under the incident exercise contract.
