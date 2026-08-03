# Internal cost allocation v1

## Product boundary

This contract is for an enterprise-internal self-hosted installation. It reports Token, Network, Provider-reported cost
and allocated platform cost. It does not create invoices, collect payment data, settle funds or expose a payment-provider
adapter.

## Project dimensions

`project_cost_allocations` assigns one Project to one `costCenterCode` and one `departmentCode`. Codes are 1-64 characters,
start with a letter or number, and contain only letters, numbers, `.`, `_`, `/` or `-`. Missing mappings are reported as
`unallocated`; deleting history is not supported. Writes use exact version compare-and-swap, preserve Tenant/Project
identity, advance the version exactly once and record `internal_cost.project_allocation_assigned` in Tenant Audit.

Readers need `quota.read`. Assignment requires `cost.manage`, held by Tenant Owner and `cost_admin`. The current surface is:

- `GET /v1/tenants/{tenantID}/cost-accounting/report`
- `GET /v1/tenants/{tenantID}/cost-accounting/export.csv`
- `PUT /v1/tenants/{tenantID}/cost-accounting/projects/{projectID}`

## Current-period report

The report uses the active Plan-assignment period and groups immutable Execution usage through Session → Project. Each row
contains Token, execution time, Network, Provider cost coverage, Provider cost by currency and platform allocation by
currency. A sealed actual cloud-cost slice replaces its corresponding estimate; both are never added. Known cost is
Provider plus authoritative platform allocation within the same currency and is never converted across currencies.

Projects with current-period usage and no mapping remain explicit as `unallocated`. Provider-unreported Executions remain
in Token/Network totals and the missing-coverage count but contribute no invented monetary value.

## CSV export

The CSV is deterministic by Project and currency and uses two row types:

- `usage`: one row per Project with Token, Network, duration and Provider coverage, with no monetary currency;
- `cost`: one row per Project/currency with Provider, platform and known micros, with usage columns empty.

This prevents consumers from multiplying usage totals when a Project has multiple currencies. Every export records
`internal_cost.allocation_exported` with the period, Project count, unallocated count and format. The CSV is an internal
accounting artifact, not an invoice or proof that an external finance system imported it.

The response also carries `X-Synara-Cost-Export-Schema: csv-v1`, `X-Synara-Cost-Export-SHA256` and
`X-Synara-Cost-Export-Bytes`. Consumers should hash the received bytes and compare both integrity headers before
passing the internal accounting artifact to another system; these headers do not authenticate the source or prove
external import.

## Storage enforcement

Migration `000159_project_internal_cost_allocations.sql` owns PostgreSQL identity, format, monotonic-version and no-delete
constraints. The SQLite startup safety layer enforces the same write shape. Service, SQLite and PostgreSQL tests cover
unallocated projection, first assignment, stale-version rejection, actual-over-estimate cost, multi-currency-safe totals,
CSV shape and Audit actions.

## Candidate evidence

Use `stage6:cost:prepare` to turn a path-only internal-cost draft and the exact usage, Provider-cost, platform-allocation
and reconciliation exports into an immutable manifest/receipt pair. The preparer computes evidence SHA-256 values from
stable bounded files and refuses symlinks, duplicate JSON fields, obvious credential material, invalid coverage,
cross-currency arithmetic and payment semantics. Both outputs and sidecars are new `0600` files; partial publication is
rolled back.

The resulting `evidence-validated-not-internal-cost-approved` receipt proves structural consistency only. It does not
authenticate the data source, prove the reporting period is complete, verify signatures, or replace distinct Operations
and cost-owner review through Platform Admin.
