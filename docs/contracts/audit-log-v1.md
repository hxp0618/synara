# Audit log v1 contract

Audit logs are immutable, tenant-scoped security records. PostgreSQL rejects updates and deletes at
the database layer; SQLite uses the same append-only service contract for Personal deployments.

## Search

`GET /v1/tenants/{tenantId}/audit-logs` requires `audit.read` and returns newest-first records with an
opaque stable cursor:

```json
{
  "items": [],
  "nextCursor": null
}
```

Supported query parameters are `limit`, `cursor`, `action`, `actorType`, `resourceType`,
`organizationId`, `occurredAfter`, and `occurredBefore`. Times use RFC3339. The cursor contains the
last `(occurredAt, eventId)` pair and must not be modified or reused with a different tenant.

## Export

`GET /v1/tenants/{tenantId}/audit-logs/export?format=jsonl|csv` accepts the same filters except page
limit and cursor. Results are read in bounded keyset batches and written directly to the response, so
the server and browser do not buffer the full export. Responses use `Cache-Control: no-store` and an
attachment filename.

Every export first appends `audit.export_started` with the format and filters. A fully traversed export
then appends `audit.export_completed` with the emitted row count. A client disconnect therefore leaves
an accurate started event without claiming completion. These events are included when they match the
requested filter; export failures never mutate existing audit records.

Tenant roles with `audit.read` are Owner, Admin, Security Admin, and Auditor. Other roles receive a
permission error and cannot infer whether another tenant has matching records.
