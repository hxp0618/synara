# Cloud Cost Accounting v1

This contract defines the first Control Plane billing-accounting boundary for retained worker usage.

It is deliberately split into separate durable domains:

- `worker_claim_facts` is the append-only authoritative request-claim ledger for one worker incarnation.
- `worker_claim_release_facts` is the one-to-one append-only closure ledger for claims that have left active delivery.
- `billing_estimated_usage_charges` is internal cost attribution derived from immutable `worker_incarnation_facts`, `worker_claim_facts`, and versioned provider tariffs.
- `billing_shared_target_ledger_coverages`, `billing_shared_cost_allocation_runs`, and
  `billing_shared_estimated_charge_slices` are the operator-sealed shared-Target estimated-cost allocation graph
  (see "Shared Target estimated-cost allocation" below).
- `billing_shared_actual_allocation_runs`, `billing_shared_actual_allocation_lines`, and
  `billing_shared_actual_charge_slices` are the operator-attested shared-Target actual-invoice allocation graph
  (see "Shared Target actual-invoice allocation" below).
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
to match the platform-configured `SYNARA_BILLING_TARIFF_OPERATOR_TENANT_ID`. This Tenant is the platform billing
operator for both global Tariffs and shared-Target accounting authority; the historical environment-variable name is
retained for compatibility. The mutation APIs fail closed when that authority is unset outside Personal profile;
Personal binds it to the bootstrapped Tenant. A customer Tenant's owner or billing administrator therefore cannot
change rates or seal shared accounting history merely by holding `billing.manage`.

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
- `GET /v1/tenants/{tenantID}/billing/shared-targets/{executionTargetID}/ledger-coverage`
- `POST /v1/tenants/{tenantID}/billing/shared-targets/{executionTargetID}/ledger-coverage`
- `POST /v1/tenants/{tenantID}/billing/shared-targets/{executionTargetID}/allocations:sweep`
- `POST /v1/tenants/{tenantID}/billing/shared-targets/{executionTargetID}/actual-invoices/{invoiceImportID}/allocations`
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

`worker_claim_release_facts` never rewrites the claim row. It records one stable release reason, the business-effective
`released_at`, the later-or-equal `recorded_at`, bounded authority/request evidence, and bounded object metadata. Exact
replay is idempotent; different content for the same claim fails closed. Lease TTL uses the original lease deadline,
Interaction and Session expiry use their persisted deadline, Kubernetes terminal suspension uses the Pod proof
observation, and ordinary Worker/control operations freeze one transaction timestamp.

Migration `000075` is intentionally a first-phase dual-write rollout. A migration-era active lease or cleanup delivery
may lack its older `worker_claim_facts` parent and therefore cannot produce a fabricated release row. Deletion guards are
not enabled until live rows are backfilled and a minimum writer version proves every release path writes the ledger.
Historical already-deleted claims cannot be reconstructed from inference. Billing that requires release completeness or
shared cost allocation must fail closed across that incomplete interval; v1 request charging continues to use claim
timestamps only.

## Shared Target estimated-cost allocation

Migration `000077` adds a separate retained accounting graph for platform-shared Targets:

- `billing_shared_target_ledger_coverages` is the operator-sealed, immutable cutover authority for one shared Target;
- `billing_shared_cost_allocation_runs` freezes one terminal Worker incarnation, period, tariff scope, algorithm,
  complete ledger digest, and tenant/platform-idle second totals;
- `billing_shared_estimated_charge_slices` retains exact `tenant-claim` or `platform-idle` charge slices.

`closed-claim-interval-v1` applies only when the Target and Worker fact are globally shared, the Worker is terminal,
the incarnation registered at or after the sealed `completeFromAt`, every immutable Claim has exactly one immutable
Release, and the Claim/Release counts equal the terminal Worker fact. Claim intervals must not overlap. Any missing
coverage, historical gap, count drift, missing release, invalid timeline, overlap, or future terminal timestamp fails
closed; a positive resource rate with a missing CPU/Memory/Ephemeral request fact also fails closed instead of
silently omitting that charge. Migration `000077` performs no inferred backfill.

For each tariff segment, Claim/Release boundaries assign active time to the Claim's exact Tenant and leave every gap
as explicit `platform-idle` cost with no synthetic Tenant. Cumulative whole-second and cumulative-amount differences
conserve the segment despite sub-second Claim boundaries. The complete run additionally requires
`tenantAllocatedSeconds + platformIdleSeconds` to equal the whole terminal usage window. CPU, Memory, Ephemeral
Storage, and Pod amounts therefore conserve the full tariff-segment estimate. Request charges use the exact Claim
timestamp and Tenant; a Claim exactly at terminal `usageEnd` uses the final tariff, while platform idle can never
receive a request charge.

The transaction uses one PostgreSQL advisory lock for the Worker/provider/currency/algorithm scope plus the terminal
Worker-fact row lock. Distinct half-open billing periods may be adjacent but cannot overlap; this prevents full-period
and partial-period Runs from charging the same usage twice. Run and Slice IDs are deterministic. Concurrent identical
first writes serialize to one graph; exact replay returns that graph, and any authoritative evidence mismatch is a
conflict. The single-replica SQLite Service serializes its local check/create/replay sequence with the same outcome.

PostgreSQL and SQLite additionally reject duplicate semantic Slices, a global fallback while an exact regional tariff
is effective, a rate or requested-resource snapshot that differs from the selected Tariff/Worker fact, a nonzero or
mispriced Request amount, and a right-boundary Request unless the Run ends at the exact Worker terminal timestamp.
Whole-second ownership and cumulative time-charge amounts still require the sole Control Plane allocation core; direct
table INSERT privileges must not be granted to tenant/API roles. The original tenant-owned `EstimateUsageCharges` and
its sweeper remain unchanged and still reject a `NULL`-Tenant Worker fact.

The shared management API is intentionally operator-only. Sealing coverage requires the active path Tenant to be the
configured platform billing operator with `billing.manage`, a platform-shared Target, a non-future `completeFromAt`, a
bounded writer version, and a lowercase deployment-attestation SHA-256. The Coverage row and
`billing.shared_target_ledger_coverage_sealed` audit entry commit atomically. PostgreSQL serializes concurrent first
seals by Target; SQLite serializes them inside its single-replica Service. An exact retry returns the retained row and
does not duplicate the audit entry; changing any asserted field returns
`billing_shared_ledger_coverage_conflict`. The API never derives a cutover from historical rows. Operators must first
prove that every production Claim release path is running at least `minimumWriterVersion`, then seal the observed
deployment digest; supplying those assertions is an operational authority action, not an automated inference.

The explicit allocation sweep accepts one shared Target, provider, currency, and already-closed half-open billing
period. It scans every overlapping `NULL`-Tenant Worker fact, including non-terminal or otherwise ineligible rows, and
invokes the same immutable allocator independently for each Worker. Successful Run/Slice graphs commit even when
another Worker fails. The response is `completed` only when every overlapping Worker succeeds; otherwise it is
`retry-required` with counts and at most 20 bounded Worker/error entries plus `failuresOmitted`. Repeating the request
after a crash or failure replays successful deterministic graphs and retries the remainder, so a browser heartbeat is
neither required nor treated as scheduling authority. Every manual request first records
`billing.shared_cost_allocation_sweep_requested`.

Unattended retries use `SYNARA_BILLING_SHARED_ALLOCATION_MAPPINGS_JSON`. Every mapping freezes one shared Target,
provider, currency, settlement delay, and retry interval, then chooses exactly one period mode:

- static mode supplies exact RFC3339 `billingPeriodStartAt` / `billingPeriodEndAt` half-open bounds;
- `calendar: "monthly-utc"` supplies an exact `firstPeriodStartAt` UTC month boundary and an optional
  `lastPeriodEndAt` UTC month boundary. Each generated period is exactly one UTC calendar month.

The scheduler does not infer a local time zone, account billing day, cloud-provider calendar, or missing anchor from
wall-clock state. A local month boundary such as midnight UTC+8 is rejected unless the represented instant is also a
UTC month boundary. A configured calendar spans at most 1200 periods; static periods remain at most 366 days.
Settlement delay is `1m..2160h`, retry interval is at least `1m`, duplicate identities are rejected, static periods for
the same Target/provider/currency cannot overlap, and a calendar mapping cannot be combined with any other mapping in
that same scope. No period runs before `billingPeriodEndAt + settlementDelay`.

Migration `000080` materializes durable `billing_shared_allocation_schedule_periods` state for every static or generated
period. The schedule-config SHA-256 and exact period form the identity; Target/provider/currency, mode, delay, and
interval are immutable. A due attempt locks the row, monotonically increments `attempt_count`, sets `last_outcome` to
`running`, and advances `next_attempt_at` before audit/allocation work begins. Completion records `completed` or
`failed` without deleting history. State rows, identities, and parent Targets cannot be deleted, attempt/timeline fields
cannot regress, and PostgreSQL/SQLite enforce the same transition shape.

Only the holder of the separate `synara:billing-shared-allocation-scheduler` database lease runs configured mappings.
The lease carries a monotonic fencing token and every scheduler mutation receives the transaction write fence. A
standby therefore cannot write while the leader is active. The durable period row is the retry throttle across process
restart and leader handoff. A paused old leader cannot claim or finish a period after its transaction write fence is
lost. If a process crashes after claim, the next leader retries after the configured `next_attempt_at`; if allocation
committed but outcome persistence did not, deterministic Run/Slice identity makes that retry safe. Repeated
settled-period scans intentionally catch late Worker facts. Operators cap or remove a mapping only after their external
settlement/ingestion authority says no later facts can arrive.

Every due scheduled attempt records `billing.shared_cost_allocation_sweep_scheduled` as a `system` actor under the
configured platform billing operator Tenant before scanning. A partial Worker result fails the leader cycle with
`billing_shared_allocation_sweep_partial_failure`, preserves successful graphs, and is retried after the configured
interval. Local operator/API, PostgreSQL concurrency, and two-holder leadership-handoff evidence is recorded in
[`stage-4-shared-cost-scheduler-orbstack-pg-20260726-final3.md`](../reports/stage-4-shared-cost-scheduler-orbstack-pg-20260726-final3.md).
Migration `000080` monthly generation, durable restart throttle, two-replica period claim, database fences, and
PostgreSQL metric projection are recorded in
[`stage-4-billing-calendar-scheduler-orbstack-pg-20260726-final1.md`](../reports/stage-4-billing-calendar-scheduler-orbstack-pg-20260726-final1.md).

This graph is an estimated shared-cost allocation from the versioned tariff catalog. Local OrbStack PostgreSQL evidence
is recorded in
[`stage-4-shared-cost-allocation-orbstack-pg-20260726-final4.md`](../reports/stage-4-shared-cost-allocation-orbstack-pg-20260726-final4.md).

## Shared Target actual-invoice allocation

Migration `000082` connects operator-owned account invoice truth to the existing shared-Target estimate graph without
rewriting either source. It creates three retained tables:

- `billing_shared_actual_allocation_runs` freezes one operator Tenant, immutable invoice import, platform-shared Target,
  ledger coverage, exact provider/currency/period, source checksum, source-scope attestation, selected-line digest,
  source/unallocated totals, algorithm, and creator;
- `billing_shared_actual_allocation_lines` assigns one exact actual invoice line at most once and freezes its source
  amount, complete estimated-slice set digest, estimated weight, and conserved allocated amount;
- `billing_shared_actual_charge_slices` references one immutable shared estimated slice at most once and preserves its
  exact Tenant or `platform-idle` ownership while carrying the signed actual micros.

The API is operator-only: the active/path Tenant must be the configured platform billing operator and the caller must
have `billing.manage`. The request supplies a lowercase `sourceScopeAttestationSHA256`. That digest is an explicit
operator assertion that the provider account/export scope, object lineage, and settlement boundary have been checked;
the Control Plane never invents it from an account name or treats a local digest as managed-cloud proof.

Selection is fail closed. An invoice line participates only when `provider`, `currency_code`, charge kind, resource
correlation key, and both exact billing-period boundaries match a retained shared estimated slice for the selected
Target. A line matching more than one shared Target rejects the whole attempt. Lines with no match for this Target stay
explicit in `unallocatedLineCount` and signed `unallocatedAmountMicros`; a Target-scoped run with nonzero unallocated
totals is not a claim that the complete account invoice has been attributed.

`proportional-shared-estimate-v1` distributes each signed actual line over the complete matching estimate-slice set.
It uses arbitrary-precision cumulative integer division over nonnegative estimate micros, then reapplies the actual
line's sign. The difference between adjacent cumulative amounts becomes each slice, so the final slice receives the
exact remainder and every line conserves micros without floating-point arithmetic. A nonzero actual line with a zero
estimate basis fails closed; zero, positive, and negative actual lines remain supported.

Creation is one transaction. The run starts as `building`; after all deterministic Line/Slice rows exist, the database
validates import totals, complete selected-line coverage, per-line slice count/weight/amount sums, run totals, exact
Target scope, and cross-Target uniqueness before allowing the sole `building -> sealed` transition. PostgreSQL and
SQLite reject any other Run mutation, all Line/Slice updates or deletes, a late line appended to a sealed invoice
import, and a late shared estimate slice matching an already allocated actual line. Actual lines and estimate slices
therefore cannot be double used by a corrected or competing invoice.

PostgreSQL serializes all Target allocations for one import and freezes shared estimate publication for the exact
provider/currency/period with transaction advisory locks. The existing shared estimate allocator takes the same period
snapshot lock. Concurrent identical first calls return one sealed deterministic graph and one mutation audit; an
attestation or authoritative evidence change is a conflict. SQLite keeps its single-replica Service serialization and
the same seal/fence triggers. One run is bounded to 100,000 selected actual lines and 1,000,000 estimate slices, inserted
in bounded batches.

The low-cardinality metrics are
`synara_billing_shared_actual_allocation_runs{provider,currency,state}`,
`synara_billing_shared_actual_allocation_lines{provider,currency,state,kind}` and
`synara_billing_shared_actual_allocation_amount_micros{provider,currency,state,kind}`, where `kind` is `selected` or
`unallocated`. No Tenant, Target, import, resource, digest, or period identity becomes a label.

Local SQLite and OrbStack PostgreSQL 17.10 evidence is recorded in
[`stage-4-shared-actual-allocation-orbstack-pg-20260726-final1.md`](../reports/stage-4-shared-actual-allocation-orbstack-pg-20260726-final1.md).
It proves database/runtime correctness, not AWS/GCP/Azure workload identity, provider account ownership, export
settlement, or managed-cloud delivery; those remain provider-specific E4 gates.

## Actual invoice imports

`billing_actual_invoice_imports` is immutable and idempotent by `(tenant_id, provider, external_import_id)`.
PostgreSQL serializes that exact identity with a transaction-scoped advisory lock before lookup/insert: concurrent
checksum-identical first imports return the same committed import/line identities, while different contents return
`billing_invoice_import_conflict`. The single-replica SQLite profile retains its in-process transaction behavior.

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
Managed-cloud acceptance evidence is governed separately by
[`managed-cloud-billing-acceptance-v1.md`](managed-cloud-billing-acceptance-v1.md); local Kubernetes, MinIO, and
provider-compatible emulators cannot be promoted to a managed-cloud identity or native-export pass.

Delimited provider/object mappings are configured by `(tenant_id, provider, external_import_id)` and resolve to one export object plus one parser format:

- `aws-cur-csv`
- `aws-cur-2-manifest`
- `gcp-cloud-billing-json`
- `azure-cost-normalized-csv`
- `azure-cost-normalized-json`

The provider label and parser format are one fail-closed pair: both AWS formats require `provider=aws`, the GCP
format requires `provider=gcp`, and both Azure formats require `provider=azure`. A mismatched mapping is rejected
during configuration normalization, before any object-store read or invoice import can occur.

Security and failure-mode requirements:

- sources are read-only
- local sources use root-relative file access and reject a symlink base, intermediate symlink, final symlink, inode replacement, and non-regular files
- S3, GCS, and Azure Blob sources use their default SDK credential/workload-identity chain; configured imports pin an immutable S3/Azure version or GCS generation
- `aws-cur-2-manifest` is S3-only. The configured manifest is read at its exact VersionId. Each listed chunk is first resolved by HEAD to an immutable VersionId, ETag, LastModified, and size, then read with a versioned GET; an unversioned GET is never allowed.
- CUR 2.0 manifests must expose the native `dataFiles` list with path strings or `filePath` objects. Legacy `reportKeys` alone and unknown file-list shapes fail closed rather than being guessed.
- CUR 2.0 ingestion accepts gzip CSV chunks. Parquet and Snappy Parquet are rejected before chunk resolution until a bounded native Parquet reader is implemented.
- Native manifests must use the AWS execution-specific layout `<root>/metadata/<partition>/<execution-id>/<manifest>` and list chunks only from `<root>/data/<same-partition>/<same-execution-id>/`. Root, partition, execution ID, and the complete common chunk directory must match; basename-only matching is forbidden. A chunk whose captured LastModified is later than the pinned manifest is rejected as an overwritten/stale-manifest mismatch; equal timestamps are allowed because S3-compatible implementations may expose coarse timestamp precision.
- CUR 2.0 source provenance includes the pinned manifest and every sorted chunk key, VersionId, ETag, LastModified, size, and content SHA-256. A deterministic bundle checksum is included in the durable import SourceChecksum and the detailed provenance is returned by the import adapter/result and recorded in scheduled-import audit metadata.
- a custom S3 endpoint requires an explicit enable flag, accepts only an HTTP(S) origin without userinfo/query/fragment/path, and requires HTTPS unless a second local-emulator flag enables HTTP
- an Azure container URL accepts only HTTPS, or HTTP under its explicit local-emulator flag, rejects userinfo/query/fragment, and must name a container path
- object keys and prefixes are relative and cannot escape their configured root
- oversized blobs are rejected before parsing completes. CUR 2.0 additionally enforces one whole-import budget before accumulation: at most 128 chunks, 64 MiB total source/compressed bytes, 256 MiB total manifest-plus-decompressed bytes, and 1,000,000 CSV rows. Per-import test overrides may only lower these ceilings.
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
- each due scheduler job creates one bounded system correlation request ID shared by its import and reconciliation audit
  entries; the ID is never empty, and checksum-identical replay does not create replacement audit rows
- scheduled import audit is written only for a newly created import; checksum-identical replay does not create a
  duplicate import audit, while estimate/reconciliation follow-up may rerun idempotently to close post-import crash gaps
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
- CUR 2.0 exact `kubernetes:` keys require either all four trusted `user:synara:*` identity tags or a structurally valid EKS Pod ARN. Instance/Pod UID must parse as a UUID; ARN region/account must agree with the row; trusted tag and ARN identity must agree. Generic `cluster`, `namespace`, `pod`, or UID aliases never produce an exact key.
- CUR 2.0 rows that cannot prove exact workload identity use an explicit `aws-allocation-<quality>:` key. Current qualities include partial/malformed Kubernetes identity, split rows missing Pod UID, provider-resource fallback, and a parent compute row referenced by split children but not safely replaced.
- A ResourceId-less `Credit`, `Refund`, Savings Plan negation, or discount is an account-level adjustment rather than Pod usage. The v1 five-kind line schema cannot persist a provider-wide adjustment honestly, so only recognized, account-identified adjustments whose original exported amount is already non-positive are explicitly filtered. The importer does not change their sign based on line-item type. Their count and signed micros total remain in source provenance, the durable checksum, and audit metadata. Unknown, positive, or account-less ResourceId-less rows fail closed.

Provider-specific expectations:

- AWS CUR CSV requires line-item IDs, billing-period start/end, currency, decimal cost, and either complete Synara Kubernetes tags or a resource ID.
- AWS CUR 2.0 split children use `NetSplitCost + NetUnusedCost` when the net pair is present, otherwise `SplitCost + UnusedCost`. Net and non-net layers are never mixed. Parent EC2 compute suppression requires an exact replacement boundary over parent resource, usage interval, operation, product code, and net/non-net cost family. CPU and Memory children in that boundary are combined before comparison with the single parent cost. Every child boundary must have exactly one parent across the complete multi-chunk import; a child-only boundary, missing parent chunk, net/non-net family mismatch, incomplete boundary, duplicate parent, or amount mismatch fails the entire import. A complete different boundary on the same instance is retained as unrelated usage.
- The versioned S3 acceptance root must execute and individually gate named child-only, net-parent/gross-child, and cross-chunk-missing-parent negative subtests. Root-only success is insufficient evidence for these fail-closed paths.
- GCP Cloud Billing JSON requires line-item IDs, currency, decimal cost, `invoice.month` or explicit billing-period start/end, and either complete Kubernetes labels or a resource global name.
- Azure normalized CSV/JSON requires explicit external line IDs, billing-period start/end, currency, decimal cost, charge kind, and either complete Kubernetes identity fields or a resource ID.
