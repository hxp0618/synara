# Control Plane API conventions v1

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
  tenant, or from a developer API Key's immutable tenant scope, and still apply `tenant_id` in every
  ORM query.
- Client-supplied tenant headers never grant access.
- Routes classified `public-beta` in `docs/api/openapi.yaml` also accept
  `Authorization: Bearer syna_sa_...`. The token authenticates the existing Service Account entity;
  it must carry `api.access` and a fixed tenant or organization RBAC role. Tenant path substitution
  is rejected, organization-scoped Keys cannot cross their Organization, and revoke/expiry is
  rechecked on every request.
- The `api.access` scope does not authorize internal Worker, Platform governance, dev-login, SCIM,
  or Artifact content-channel routes. SCIM continues to use its dedicated scopes and middleware on
  the same Service Account entity; Artifact content uses short-lived grant tokens.
- Machine actions scope idempotency, SSE connection admission, Session Events, and Audit records to
  the Service Account ID. The creating User remains the database responsibility owner where legacy
  resource tables require a User foreign key, but their membership and user-scoped Provider
  Credentials are never inherited by the machine Principal. Machine Principals cannot create or
  read `private` Projects or Sessions; machine-created Sessions default to `project` visibility.
- State-changing requests require JSON content types. Cookie authentication remains same-origin
  through the Synara proxy; the server-to-server developer API does not enable CORS.
- Each Service Account has a persistent `rateLimitPerMinute` (default 600; allowed range 1–60000).
  Public-beta resource requests share one UTC-minute admission window per key across all control-plane
  replicas. Successful admission returns `RateLimit-Limit`, `RateLimit-Remaining`, and `RateLimit-Reset`;
  exhaustion returns `429 developer_api_rate_limited` plus `Retry-After`. The existing SSE connection
  leases remain an additional concurrent-connection guard rather than a replacement for request admission.
- Usage windows attribute admitted requests, 429 responses, success, 4xx, 5xx, and aggregate duration to
  Tenant, optional Organization, Service Account, and classified route pattern. The browser console can
  query a bounded (maximum 31-day) per-route summary through
  `GET /v1/tenants/{tenantId}/service-accounts/{serviceAccountId}/usage`; this management route remains
  internal and requires the existing `service_accounts.read` permission.
- Historical payment-provider routes are not registered by the `internal-self-hosted` product runtime.

## Internal entitlement vocabulary

The public API models internal capacity and feature assignment, not commercial plans or subscriptions:

- Tenant and session responses use `entitlementProfileCode` with `standard | enterprise` and
  `evaluationExpiresAt`; lifecycle status uses `evaluation`, never the retained database value `trialing`.
- `GET /v1/platform/tenants/{tenantId}/entitlements` returns `profile` and `profileAssignment` with
  `evaluationEndsAt`, `reportingPeriodStart`, `reportingPeriodEnd`, and a version for optimistic concurrency.
- `PUT /v1/platform/tenants/{tenantId}/entitlement-profile` accepts the same vocabulary and is the only
  Platform Admin write route for profile assignment.
- Usage and internal-cost reports expose `entitlementProfileVersion` so an export can be reconciled to the
  assignment that selected its reporting period.

The database retains historical `plan_code`, `trialing`, and `tenant_subscriptions` identifiers for migration
compatibility. They are not public JSON fields, routes, audit vocabulary, or product positioning.
Tenant data exports carrying the public entitlement vocabulary use schema version `synara-tenant-export-v2`;
consumers must not parse a v2 bundle with the v1 field map.

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
from their last durable sequence without inferring state from response length. `afterSequence`
remains the explicit domain cursor; public callers use a default `limit` of 50 and maximum 200.
The official SDK uses this route as a bounded fallback only when the SSE user/Tenant connection pool
is saturated, while preserving replay suppression and strict sequence-gap detection.

`GET /v1/projects/{projectId}/sessions` uses the common opaque `cursor` contract with a default
`limit` of 50 and maximum 200. The response contains `items` and nullable `nextCursor`; cursors are
bound to the authenticated Tenant and Project and preserve the descending `updatedAt`/ID order.

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

The platform profile endpoint is public and contains only safe capability declarations. Its
`internalStatusBoard` object is always present: `configured` is `false` and `url` is omitted until an
operator sets `SYNARA_INTERNAL_STATUS_BOARD_URL` together with the signed failure-independent incident publisher; a
configured value is a credential-free HTTPS URL without query or fragment on a failure-independent origin. The endpoint
never exposes paging providers,
contacts, employee-recipient data, or incident-drafting authority. It exposes no external Status Page,
payment-provider, or commercial-billing capability object.
Execution target responses never include encrypted configuration or connection secrets.

Broad-impact incident APIs use `internalImpactSummary`, `broadInternalImpact`,
`internalStatusBoardIncidentReference`, `internalUpdates`, and the read-only
`internalNotifications` Outbox status projection. The notification projection exposes only Message ID, update kind,
pending/retrying/published/dead-letter state, attempts and timestamps; it never exposes Outbox payload or credentials.
Current write routes are
`/v1/platform/incidents/{incidentId}/status-board` and
`/v1/platform/incidents/{incidentId}/internal-updates`; customer-facing Status Page routes are not
registered by the internal-self-hosted runtime.

## Public API lifecycle

Public `/v1` operations are additive-only: new optional response fields, new enum/event values where
documented as forward compatible, and new operations may be added without changing existing authorization,
idempotency, or resource semantics. Breaking changes use `/v2`. `public-beta` identifies a source contract
that can still change before GA; it must not be represented as `public-ga` by deployment or documentation.

A deprecated `public-ga` operation is announced in the changelog and developer documentation and returns
standards-based `Deprecation` and `Sunset` headers. Sunset is at least twelve months after first public notice,
except when continued operation creates an urgent security risk. Removal never silently redirects callers to
an operation with different authorization or side-effect semantics.

Official SDK release metadata declares `minimumControlPlaneVersion`. The TypeScript and Python declarations
must match during release staging; a newer SDK must not be advertised as compatible with an older Control
Plane than its package metadata permits.
