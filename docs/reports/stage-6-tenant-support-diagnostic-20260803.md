# Stage 6 Tenant Support diagnostic snapshot — 2026-08-03

Assessment: `local-tenant-support-diagnostic-byte-bound-validated-not-ga-approved`

The Support diagnostic snapshot advances the internal self-hosted operations boundary. It lets Tenant administrators
and approved read-only Support Access sessions download the same bounded operational and usage evidence needed for
troubleshooting without granting database access or mutating Tenant state. It contains no payment, invoice, settlement,
or external-service workflow.

## Implemented

- `GET /v1/tenants/{tenantID}/support-diagnostic.json` composes the existing Tenant operations aggregate and current
  Token/Network/internal-cost usage service under active-Tenant, `tenant.read`, and `quota.read` authorization.
- The response is aggregate-only and excludes credential payloads, artifact contents, execution prompts, and session
  transcript data; it is audited as `tenant.support_diagnostic_exported`.
- The server emits exact-byte SHA-256, byte-count, schema, `Content-Length`, and `Cache-Control: no-store` headers.
- The control-plane client recomputes SHA-256 over the raw body and rejects integrity mismatches before download.
- Shared Tenant Support UI exposes a “Support diagnostic snapshot” download action for the existing Tenant-visible
  Support Access/Audit destination.

## Local evidence

- Go HTTP tests verify unauthenticated rejection, cross-Tenant isolation, exact-byte headers, audit creation, and the
  absence of secret/payload fields.
- Support Access service tests continue to cover the aggregate operational inventory and the tenant-scoped query.
- Control-plane client and shared Enterprise UI tests cover routing, raw-body preservation, digest validation, and the
  rendered Support diagnostic action.

## Boundary and remaining evidence

This is repository and local-environment evidence only. It does not establish a live deployed 49-operation browser exercise,
production on-call/status-board evidence, third-party penetration testing, residency annex, recovery drill, provider or
legal approval, production rotation, capacity/long-duration acceptance, or final GA approval. Those external gates
remain open in the Stage 6 checklist.
