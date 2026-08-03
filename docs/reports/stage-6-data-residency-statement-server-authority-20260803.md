# Stage 6 server-authoritative data-residency statement — 2026-08-03

Assessment: `local-execution-residency-statement-server-authority-validated-not-residency-approved`

This is an internal self-hosted product capability. It describes the Tenant execution Region boundary and does not add
payments, Checkout, subscriptions, external invoicing, or a customer-facing hosted service.

## Implemented contract

`GET /v1/tenants/{tenantID}/data-residency-statement` now:

- requires the active Tenant context and `SchedulingPolicyRead` permission;
- reads the Tenant name/home Region and the versioned execution scheduling policy on the server;
- emits `synara-data-residency-statement-v1` with policy version, policy digest, enforcement state and allowed Regions;
- marks Metadata, Artifacts and KMS as `deployment-annex-required` rather than implying full residency;
- records `tenant.data_residency_statement_exported` with policy and byte-integrity metadata;
- returns `X-Synara-Data-Residency-SHA256`, `X-Synara-Data-Residency-Bytes`, `Content-Length`, `no-store` and an
  attachment filename.

The shared Enterprise UI fetches the raw response bytes, validates the byte-count header, and downloads those exact bytes only
when the server statement's policy version/digest matches the displayed policy; an unavailable, stale, or integrity-invalid
statement is not downloadable. The previous pure statement builder remains available for isolated fixture/evidence helpers,
but it is no longer the user-facing download authority.

## Local evidence

- Go HTTP coverage verifies authentication, active-Tenant isolation, permission denial, server policy projection, exact
  response SHA-256/byte headers, and the immutable audit row.
- Control-plane client coverage verifies the encoded route, typed response, exact-byte preservation and stale-byte rejection.
- Existing execution scheduling policy CAS and audit tests continue to pass.

## Deliberate boundary

This is repository-local evidence only. It does not verify a signed deployment annex, PostgreSQL/object-storage/KMS/log/
backup/support processing Regions, external identity/signatures, or live failover/evacuation exercises. The permanent
assessment therefore remains `validated-not-residency-approved`; those production/Legal/Privacy gates are still open.
