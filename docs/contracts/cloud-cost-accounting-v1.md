# Cloud Cost Accounting v1

This contract defines the first Control Plane billing-accounting boundary for retained worker usage.

It is deliberately split into three durable domains:

- `worker_claim_facts` is the append-only authoritative request-claim ledger for one worker incarnation.
- `billing_estimated_usage_charges` is internal cost attribution derived from immutable `worker_incarnation_facts`, `worker_claim_facts`, and versioned provider tariffs.
- `billing_actual_invoice_imports` and `billing_actual_invoice_lines` are imported external billing truth keyed by tenant-scoped provider external IDs.

Estimates and actuals must never share a table or a mutable "final cost" column.

## Provider tariffs

`billing_provider_tariffs` stores append-only tariff versions by:

- `provider`
- `region` where `''` means a provider-wide fallback
- `currency_code`
- `version`
- effective interval `[effective_start_at, effective_end_at)`

Tariff rows are platform-global catalog entries today and therefore do not carry `tenant_id`.
Listing is tenant-authorized: callers must operate through an active tenant with `billing.manage`, but that
authorization does not re-scope row ownership. Appending a global row additionally requires the path/active Tenant
to match the platform-configured `SYNARA_BILLING_TARIFF_OPERATOR_TENANT_ID`. The mutation API fails closed when that
authority is unset outside Personal profile; Personal binds it to the bootstrapped Tenant. A customer Tenant's owner
or billing administrator therefore cannot change rates used by other Tenants merely by holding `billing.manage`.

Rates are stored in currency micros for these units:

- `cpu_core_hour_rate_micros`
- `memory_gib_hour_rate_micros`
- `ephemeral_gib_hour_rate_micros`
- `request_rate_micros`
- `pod_hour_rate_micros`

For one `(provider, region, currency_code)` tuple, tariff intervals must not overlap.
Tariffs are append-only. After insert, `UPDATE` and `DELETE` must be rejected at the database layer.
PostgreSQL serializes each tuple with a transaction-scoped advisory lock inside the overlap trigger, closing the
concurrent READ COMMITTED check/insert race. SQLite rejects overlapping direct inserts with an equivalent trigger.

Current management/runtime surface:

- `GET /v1/tenants/{tenantID}/billing/tariffs`
- `POST /v1/tenants/{tenantID}/billing/tariffs` (configured platform tariff-operator Tenant only)
- `POST /v1/tenants/{tenantID}/billing/imports/{provider}/{externalImportID}`
- `POST /v1/tenants/{tenantID}/billing/imports/{importID}/reconcile`

## Estimated usage charges

`billing_estimated_usage_charges` stores immutable rows keyed to one worker incarnation and one tariff slice.

Every estimate row is tenant-scoped. Reconciliation indexes and deterministic IDs include `tenant_id`, and the row's execution target link must remain inside the same tenant.

Each row carries:

- the immutable worker link: `worker_id`, `worker_incarnation`
- the tariff link: `tariff_id`
- `charge_kind`: `cpu | memory | ephemeral-storage | request | pod`
- the billing period the estimate was requested for
- the exact usage sub-interval inside that billing period
- the persisted resource snapshot copied from `worker_incarnation_facts`
- `rate_micros` and computed `amount_micros`
- a deterministic `resource_correlation_key`

Time-based charge kinds (`cpu`, `memory`, `ephemeral-storage`, `pod`) split exactly across tariff interval boundaries.

`request` charges are derived from `worker_claim_facts` and remain intentionally fail-closed when that ledger is incomplete:

- `worker_claim_facts` is append-only and records one authoritative `claimed_at` timestamp for each `claim_count` increment, scoped to `execution` and `workspace-cleanup` claim kinds.
- The ledger also owns the business identity of the claim: one row per `(execution_id, execution_generation)` or `(cleanup_command_id, cleanup_dispatch_generation)`. Referenced operational parents are delete-restricted while the ledger row exists; normal Tenant deletion is a soft-delete workflow, and a future accounting-retention purge must be explicit rather than cascading history away.
- `request_id` is retained as transport evidence but is not the durable business key. The existing Worker request receipt controls its bounded replay window, so expiration of an ephemeral receipt does not make an otherwise valid later claim collide with permanent accounting history.
- `request_rate_micros` currently meters both claim kinds because `worker_incarnation_facts.claim_count` already counts both of them.
- No historical backfill is synthesized. An incarnation is treated as request-ledger-complete only when `count(worker_claim_facts) == worker_incarnation_facts.claim_count`.
- When the ledger is complete, request-rate estimates are emitted by `claimed_at`, split per billing period and per tariff segment, so billing boundaries and request-tariff changes are handled authoritatively.
- Billing windows are half-open. The sole tie-breaker is a claim whose timestamp exactly equals its Worker's terminal timestamp: it belongs to that Worker's final tariff segment, preventing timestamp-precision truncation from dropping the last accepted claim; it cannot appear in a later period because the Worker is already terminal.
- When the ledger is incomplete, the old safe fallback remains: only a fully enclosed incarnation lifetime under one effective request rate may emit request charges; crossing a billing boundary, using a non-terminal worker, or spanning a request-tariff change still fails closed with `billing_request_charge_delta_unavailable`.
- A shared Target whose Worker fact has no authoritative tenant attribution still fails closed with `billing_worker_fact_tenant_unattributed` instead of creating a global or `NULL`-tenant estimate.

## Actual invoice imports

`billing_actual_invoice_imports` is immutable and idempotent by `(tenant_id, provider, external_import_id)`.

`billing_actual_invoice_imports` and `billing_actual_invoice_lines` are tenant-owned rows:

- both tables persist `tenant_id`
- invoice lines must reference the parent import through `(tenant_id, invoice_import_id)`
- deterministic import/line IDs are tenant-scoped so the same provider-owned IDs may safely repeat in another tenant

`billing_actual_invoice_lines` stores one normalized line per:

- external line ID
- charge kind
- resource correlation key
- exact billing period
- currency

Line identity and amount are immutable after import. Only reconciliation fields may change:

- `reconciliation_state`
- `matched_estimate_count`
- `matched_estimate_amount_micros`
- `reconciled_at`

Current reconciliation states are:

- `pending`
- `matched`
- `variance`
- `unmatched-actual`

## Reconciliation

Reconciliation matches actual lines against estimated charges on:

- `tenant_id`
- `provider`
- `currency_code`
- `charge_kind`
- `resource_correlation_key`
- exact billing period

One actual line may match multiple estimated rows when a worker's usage crosses tariff boundaries inside the same billing period.

The current v1 report also returns unmatched estimated charge IDs for the imported billing period and tenant. Those unmatched estimates are intentionally not rewritten into the estimate table, preserving the estimate/actual separation.

## Provider Adapter Appendix

The `services/control-plane/internal/billing` package now includes blob-backed actual-invoice adapters for strict offline imports.

Delimited provider/object mappings are configured by `(tenant_id, provider, external_import_id)` and resolve to one export object plus one parser format:

- `aws-cur-csv`
- `gcp-cloud-billing-json`
- `azure-cost-normalized-csv`
- `azure-cost-normalized-json`

Security and failure-mode requirements:

- sources are read-only
- local sources use root-relative file access and reject a symlink base, intermediate symlink, final symlink, inode replacement, and non-regular files
- S3, GCS, and Azure Blob sources use their default SDK credential/workload-identity chain; configured imports pin an immutable S3/Azure version or GCS generation
- a custom S3 endpoint requires an explicit enable flag, accepts only an HTTP(S) origin without userinfo/query/fragment/path, and requires HTTPS unless a second local-emulator flag enables HTTP
- an Azure container URL accepts only HTTPS, or HTTP under its explicit local-emulator flag, rejects userinfo/query/fragment, and must name a container path
- object keys and prefixes are relative and cannot escape their configured root
- oversized blobs are rejected before parsing completes
- missing required fields fail closed
- mixed billing periods inside one object fail closed
- mixed currencies inside one object fail closed
- duplicate raw external line IDs inside one object fail closed
- decimal amounts are parsed as decimal text and converted to micros without binary-float math
- bare JSON numeric literals must preserve their original decimal text; adapters must not round-trip them through `float64`

Runtime scheduling rules:

- import mappings are parsed with unknown-field and trailing-JSON rejection
- only the current `synara:billing-import-scheduler` leader performs scheduled reads
- a provider external ID is immutable: the same checksum is an idempotent replay and a different checksum is a conflict
- scheduled audit and optional follow-up work run only for a newly created import; unchanged replay does not create duplicate audit entries
- reconciliation audit is written only when line reconciliation state actually changes
- `estimateAfterImport` requires an explicit non-empty `executionTargetIds` list. Every listed Target must be tenant-owned;
  shared, missing, or foreign Targets fail closed, and one Target cannot be assigned to different estimate providers in
  the same runtime configuration.
- the built-in estimate sweeper pages through overlapping Worker incarnation facts for only those Targets and invokes
  the same immutable/idempotent estimate engine used by the direct service method
- the durable invoice import is also the retry cursor: checksum-identical scheduler replays and repeated manual imports
  rerun the sweep without duplicating estimate rows, so a crash after import cannot permanently skip estimation
- a manual trigger whose invoice commit succeeds but whose follow-up sweep fails returns the durable import identity with
  `EstimateSweep.Status = retry-required`, counts, and a stable error code. It must not report the committed import as rolled back
- one Worker estimate failure does not prevent other Worker facts from being processed, but the job remains failed and
  reports the failed Worker count. Reconciliation is skipped until the full sweep succeeds; the immutable invoice import
  remains available for a later retry. Shared attribution and unavailable per-period request deltas retain their existing
  fail-closed behavior; the sweeper never invents allocation or request history.

Normalization rules:

- normalized actual lines still use the contract charge kinds: `cpu | memory | ephemeral-storage | request | pod`
- provider rows may aggregate into one normalized line per `(resource_correlation_key, charge_kind, billing period, currency)`
- when Kubernetes identity labels/tags are complete, adapters emit the same six-part correlation key shape as estimates:
  `target_kind:cluster_id:region:namespace:pod_name:instance_uid`
- otherwise adapters fall back to provider resource keys:
  `aws:<account>:<region>:<resource-id>`
  `gcp:<project>:<region>:<resource-name>`
  `azure:<subscription>:<region>:<resource-id>`

Provider-specific expectations:

- AWS CUR CSV requires line-item IDs, billing-period start/end, currency, decimal cost, and either complete Synara Kubernetes tags or a resource ID.
- GCP Cloud Billing JSON requires line-item IDs, currency, decimal cost, `invoice.month` or explicit billing-period start/end, and either complete Kubernetes labels or a resource global name.
- Azure normalized CSV/JSON requires explicit external line IDs, billing-period start/end, currency, decimal cost, charge kind, and either complete Kubernetes identity fields or a resource ID.
