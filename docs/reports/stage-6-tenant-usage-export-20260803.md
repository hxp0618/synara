# Stage 6 Tenant Usage export — 2026-08-03

Assessment: `local-tenant-usage-export-byte-bound-validated-not-ga-approved`

This capability remains inside Synara's enterprise-internal self-hosted boundary. It exposes Token, Network,
Provider-cost coverage, soft-quota and internal platform-allocation information for internal accounting and operations;
it does not create an invoice, collect payment data, settle funds, or activate a payment-provider integration.

## Implemented

- `GET /v1/tenants/{tenantID}/usage/export.json` is protected by the active Tenant context and `quota.read`.
- The export contains the current reporting-period usage snapshot, including Token and Network totals, Provider-cost
  reported/missing coverage, soft-quota state, and separate Provider/platform/known cost totals.
- The server emits `json-v1`, exact-byte SHA-256 and byte-count headers, `Content-Length`, `Cache-Control: no-store`,
  and the `tenant.usage_exported` audit event.
- The control-plane client recomputes SHA-256 over the raw UTF-8 response, validates the byte-bound response, and the
  shared Tenant Usage settings UI downloads only the verified response body as JSON.

## Local evidence

- Go HTTP coverage verifies unauthenticated rejection, cross-Tenant isolation, response digest/length, and the audit row.
- Control-plane client coverage verifies encoded Tenant routing, credentials, response headers, raw-body preservation and
  SHA-256 mismatch rejection.
- Shared UI coverage verifies the Usage export action; package typechecks pass.

## Boundary and remaining evidence

This is repository and local-environment evidence only. It does not establish live production completeness, an external
finance-system import, a residency deployment annex, or final GA approval. Those external operational, security, legal,
and release-authority gates remain tracked separately in the Stage 6 checklist.
