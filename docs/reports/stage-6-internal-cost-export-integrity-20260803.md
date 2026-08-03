# Stage 6 internal cost export integrity — 2026-08-03

## Scope and assessment

The internal self-hosted cost-accounting CSV now exposes a schema marker, SHA-256 and byte-count response headers. The
HTTP test recomputes the digest from the exact response bytes and checks the declared byte count. This is an internal
usage/cost artifact check, not an invoice, payment flow, external finance import or GA approval.

Assessment: `local-internal-cost-export-integrity-validated-not-finance-import`.

## Contract

`GET /v1/tenants/{tenantID}/cost-accounting/export.csv` returns:

- `X-Synara-Cost-Export-Schema: csv-v1`
- `X-Synara-Cost-Export-SHA256: <lowercase hex digest>`
- `X-Synara-Cost-Export-Bytes: <byte count>`

The export remains deterministic by Project/currency, records `internal_cost.allocation_exported` Audit, and contains
Token, Network, Provider-cost and internal platform allocation data only.

## Verification

- `go test ./internal/httpapi -count=1` passed, including response-byte digest and byte-count assertions.
- `go test ./... -count=1` passed.
- The latest isolated Compose self-hosted usage/cost and dependency-failure acceptance passed; its containers, volumes and
  network were removed after the run.

## Deliberate limits

The headers authenticate only the bytes received from the current Control Plane response. They do not authenticate the
underlying cost source, prove reporting-period completeness, prove external finance-system import, or introduce any
payment/settlement capability.
