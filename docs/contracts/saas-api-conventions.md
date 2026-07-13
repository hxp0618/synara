# SaaS API conventions v1

## Transport

- Base path: `/v1`
- Media type: `application/json`
- Timestamps: RFC 3339 UTC strings
- IDs: opaque UUID strings
- Request correlation: accept or generate `X-Request-ID`; return it on every response
- Pagination: `limit` (default 50, maximum 200) and opaque `cursor`

## Error envelope

```json
{
  "error": {
    "code": "tenant_forbidden",
    "message": "You do not have permission to access this tenant.",
    "requestId": "01J...",
    "details": {}
  }
}
```

Stable error codes are part of the API contract. Messages are user-readable but are not intended
for programmatic branching.

## Authentication and tenancy

- `login_sessions` represent browser/client login state and are separate from Agent Sessions.
- The login session cookie is HTTP-only and stores an opaque random token; PostgreSQL stores only
  its SHA-256 hash.
- A tenant path parameter and the authenticated membership jointly establish tenant context.
- Resource routes without a tenant path, such as `/v1/projects/{projectId}` and
  `/v1/sessions/{sessionId}`, resolve the tenant from the authenticated login session's active
  tenant and still apply `tenant_id` in every ORM query.
- Client-supplied tenant headers never grant access.
- State-changing requests require JSON content types and same-origin deployment through the
  Synara proxy for v1. External API clients use bearer sessions in a later phase.

## Idempotency

Creation and command endpoints that may be retried accept `Idempotency-Key`. Keys are scoped to the
active Tenant and authenticated actor. The operation, normalized request hash, successful status, and
response are stored in the same transaction as business, Event, Outbox, and Audit writes.

- Same key and request return the stored response with `Idempotency-Replayed: true`.
- Same key with a different operation or normalized request returns `409 idempotency_conflict`.
- A missing key preserves the non-replayable legacy behavior; first-party Web clients should always send
  a unique key for supported commands.
- `X-Request-ID` remains request correlation and Worker receipt identity. It is not a replacement for the
  user/API `Idempotency-Key` contract.

## Phase 2 resource routes

```text
GET    /v1/tenants/{tenantId}/organizations/{organizationId}/projects
POST   /v1/tenants/{tenantId}/organizations/{organizationId}/projects
GET    /v1/projects/{projectId}
PATCH  /v1/projects/{projectId}
DELETE /v1/projects/{projectId}
GET    /v1/projects/{projectId}/sessions
POST   /v1/projects/{projectId}/sessions
GET    /v1/sessions/{sessionId}
GET    /v1/sessions/{sessionId}/events?afterSequence={sequence}&limit={limit}
GET    /v1/sessions/{sessionId}/interactions
POST   /v1/sessions/{sessionId}/turns
POST   /v1/sessions/{sessionId}/suspend
POST   /v1/sessions/{sessionId}/resume
POST   /v1/sessions/{sessionId}/archive
POST   /v1/executions/{executionId}/cancel
GET    /v1/executions/{executionId}/interactions
POST   /v1/executions/{executionId}/approvals/{requestId}/resolve
POST   /v1/executions/{executionId}/user-input/{requestId}/resolve
```

Event pages return `lastSequence` even when no newer events exist, allowing clients to reconnect
from their last durable sequence without inferring state from response length.

`GET /v1/sessions/{sessionId}/interactions` is an approval-authorized, pending-only snapshot for
browser refresh and reconnect. It returns a minimal Interaction DTO plus `snapshotSequence`. The
server reads the Session sequence before loading pending rows; clients reconcile Events newer than
that watermark so a concurrent resolution cannot be missed. Expired rows are not returned.

Session readers without `ExecutionApprove` retain access to the Event cursor but not Approval or
Structured User Input details. Interaction lifecycle Events are returned as the neutral
`session.event.redacted` envelope with the original Event ID and sequence and without payload,
Execution, Worker, Generation, actor, question, answer, command, or file details. REST backlog and
live SSE apply the same projection. Live Interaction delivery rechecks current authorization so an
already-open stream cannot retain obsolete approval visibility after a role downgrade.

## Revised Phase 0 foundation routes

```text
GET  /v1/platform/profile
GET  /v1/tenants/{tenantId}/execution-targets
POST /v1/tenants/{tenantId}/execution-targets
GET  /v1/tenants/{tenantId}/execution-targets/{executionTargetId}
```

The platform profile endpoint is public and contains only safe capability declarations. Execution
target responses never include encrypted configuration or connection secrets.
