# Tenant Usage Export v1

This contract is for the enterprise-internal self-hosted product. It exports the current Tenant reporting-period
Token, Network, Provider-cost coverage, soft-quota and internal platform-allocation snapshot. It does not create an
invoice, collect payment data, settle funds, or expose a payment-provider adapter.

## Endpoint

`GET /v1/tenants/{tenantID}/usage/export.json` requires the active Tenant context and `quota.read`. The response is
`application/json` with schema `json-v1`, `Cache-Control: no-store`, and an attachment filename. It is audited as
`tenant.usage_exported` with the period, entitlement-profile version, Provider-cost coverage and exact byte digest.

The response carries `X-Synara-Usage-Export-Schema`, `X-Synara-Usage-Export-SHA256`, `X-Synara-Usage-Export-Bytes` and
`Content-Length`. The client recomputes SHA-256 over the exact UTF-8 response body, rejects a missing/stale schema,
digest or byte header, and downloads only the verified response bytes.

## Accounting boundary

Provider cost remains separate from estimated/actual platform allocation. Missing Provider cost is reported as
unavailable and excluded from known monetary totals; Token and Network usage remain visible. Currency totals are not
converted or combined across currencies. This is an internal usage and cost explanation artifact, not an invoice or
proof that an external finance system imported it.
