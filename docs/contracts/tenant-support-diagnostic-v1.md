# Tenant Support Diagnostic v1

This contract is for Synara's enterprise-internal self-hosted product. It gives a Tenant owner or an approved
read-only Support Access session a bounded troubleshooting artifact. It contains aggregate Tenant operations,
Token/Network usage, Provider-cost coverage, soft-quota state, and internal platform allocation only. It does not
expose credential payloads, artifact contents, execution prompts, session transcript data, or direct database access.

## Endpoint and authorization

`GET /v1/tenants/{tenantID}/support-diagnostic.json` requires the active Tenant context, `tenant.read`, and
`quota.read`. Support Read-Only receives those permissions only through an unexpired, Tenant-enabled, four-eyes
approved Support Access grant. The request is read-only and is audited as `tenant.support_diagnostic_exported`.

The response is `application/json`, schema `json-v1`, `Cache-Control: no-store`, and an attachment filename. The
Tenant operations section is aggregate-only: members, Organizations, Sessions, Targets, Workers, queue/failure
counts, artifact counts/bytes, credential availability counts, and identity-connection availability counts.

## Integrity boundary

The server appends one newline, hashes the exact UTF-8 bytes sent, and emits
`X-Synara-Support-Diagnostic-Schema`, `X-Synara-Support-Diagnostic-SHA256`, `X-Synara-Support-Diagnostic-Bytes`, and
`Content-Length`. The control-plane client recomputes SHA-256 over the raw response and downloads only a response whose
schema, digest, and byte count match. This is an internal troubleshooting artifact, not an invoice, payment flow, or
external service offering.
