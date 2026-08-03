# Stage 6 internal incident notification intent and status projection — 2026-08-03

## Scope and assessment

This report records the local engineering verification of employee-safe Incident updates, their transactional
PostgreSQL Outbox intent, and the read-only notification status projection exposed to Platform Admin. It is source and
local test evidence from a dirty development worktree; it is not Status Board delivery evidence, an employee-channel
exercise, a production release receipt or GA approval.

Assessment: `local-incident-notification-intent-and-status-projection-validated-not-delivery`.

## Implemented boundary

- `incident.internal-update` is appended in the same database transaction as the immutable Incident update.
- The payload contains only Incident identity, severity, employee-safe summary, update kind, affected components/Regions
  and credential-free internal Status Board references.
- Summaries containing credential assignments or Bearer material are rejected before persistence.
- Platform Admin receives only Outbox Message ID, update kind, attempts, timestamps and one of `pending`, `retrying`,
  `published` or `dead-letter`; Outbox payload and operator identity data are not projected.
- A `published` Outbox row is explicitly presented as publisher acknowledgement state, never as proof of Status Board
  or employee-channel delivery.

## Verification

The following checks passed:

```bash
go test ./internal/incidentgovernance ./internal/outbox -count=1
go test ./... -count=1
bun run --cwd apps/admin test:browser -- src/PlatformAdmin.browser.tsx --reporter=verbose
```

Incident tests cover atomic Outbox creation, payload redaction, sensitive-summary rejection and deterministic status
mapping. Admin browser tests cover the read-only status card and preserve the separate external-delivery boundary.
The Stage 6 Incident and internal-self-hosted boundary Python validators also passed.

## Deliberate limits

- No Status Board, paging provider, email, Slack, Teams or other employee channel was called.
- No notification row was treated as delivered evidence; the real Publisher, delivery receipt and employee exercise
  remain deployment-owned external evidence.
- The worktree and local artifacts are dirty, unsigned and unbound to a release candidate; no production GA claim is made.
