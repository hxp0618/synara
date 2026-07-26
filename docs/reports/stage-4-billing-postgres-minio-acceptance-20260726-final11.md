# Stage 4 PostgreSQL + versioned MinIO billing runtime acceptance (final11)

Superseded by [final12](stage-4-billing-postgres-minio-acceptance-20260726-final12.md), which requires structured
`test2json` pass events and rejects skipped required tests.

## Result

On 2026-07-26 local time, the billing runtime passed two isolated environment-level tests against real PostgreSQL and a
versioned S3-compatible MinIO API:

1. a directly constructed `RuntimeConfig -> NewAdapterFromRuntime -> S3 -> scheduler -> estimate -> reconcile -> restart`
   path; and
2. two-connection PostgreSQL serialization of a concurrent first import for the same
   `(tenant, provider, externalImportID)` identity.

This is local evidence from a dirty worktree. It has not been committed, pushed, deployed to managed cloud, or released:

- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`
- Worktree dirty: `true`
- Machine-readable evidence:
  [stage-4-billing-postgres-minio-acceptance-20260726-final11.json](stage-4-billing-postgres-minio-acceptance-20260726-final11.json)
- Status: `passed`, exit code `0`
- Run ID: `20260725185315-85226-30139`
- UTC window: `2026-07-25T18:53:15Z` to `2026-07-25T18:53:24Z`

## Command

```bash
SYNARA_BILLING_ACCEPTANCE_EVIDENCE_FILE=docs/reports/stage-4-billing-postgres-minio-acceptance-20260726-final11.json \
  deploy/billing/postgres-minio-acceptance.sh
```

The wrapper published no host port. Both test cases ran from one compiled Linux binary inside the run-owned internal
Docker network, and the wrapper first listed the binary's tests and required both exact names before execution.

## Runtime and provenance

| Role          | Exact runtime                                                                                                                        |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| Metadata      | disposable PostgreSQL `17.10`, image ID `sha256:8ee7c900f4de054e8f0a3b42b8f5da38b8fba57c3ccd7c1a02be28cb0b06494f`                    |
| Object store  | disposable versioned MinIO, image ID `sha256:6c83c74c8028019d82d3f20d65cc6645fd5694b82a25dc38733df5227ffa4e8d`                       |
| Object client | MinIO Client image ID `sha256:0029bb25aef96434e35c2c7d8aa39b32913ec38c4c044f0c7551489f8d0d0e71`                                      |
| Test runtime  | pinned Go 1.26 bookworm digest, resolved image ID `sha256:1b67dd879851e02ab4035680c180ed8d607ef4aaaf19e17ea1328a00f2450d86`, `arm64` |
| Host compiler | `go version go1.26.5 darwin/arm64`                                                                                                   |

Because HEAD alone cannot identify uncommitted source, final11 binds the pass to these exact dirty inputs/artifact:

| Asset                                                          | SHA-256                                                            |
| -------------------------------------------------------------- | ------------------------------------------------------------------ |
| `deploy/billing/postgres-minio-acceptance.sh`                  | `74e9fc6282e75611610129f0e0f72e578c7412718ce3c61853c445bc73359ab7` |
| `internal/billing/runtime_postgres_integration_test.go`        | `29097245652556079087aa73967dfe844de137c62a2d37534136390cdfb2ba69` |
| `internal/billing/invoice_import_postgres_integration_test.go` | `fc547c4026403f608be59cdb26e0141dbff1bc5d2ed2442941be39fc257e5d06` |
| `internal/billing/service.go`                                  | `02e5a53a81aaf21215051c43ff27f6a0f798a62c0e96c6bb96b58b0612b6f646` |
| Compiled Linux `billing.test`                                  | `ed9d48c96bd1290c0edbc31cee2554b2cb68b6b459727516a30310aa4c439690` |

The binary hash was recomputed after both tests and remained unchanged. This does not make the entire dirty repository
reconstructible, but it identifies the exact executed artifact and its highest-risk source inputs more precisely than
the earlier HEAD-only evidence.

## Versioned object and runtime assertions

The wrapper uploaded the bounded AWS CUR fixture and recorded its exact `VersionId`, then overwrote the same key with a
poison payload. The runtime mapping pinned the old version. Parsing the expected three-line fixture rather than the
poison latest version proves that the immutable object version was honored.

All seven allowlisted runtime assertions were `true`:

| Assertion                         | Evidence                                                                                                                        |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `postgresMigrationsApplied`       | real PostgreSQL bootstrap succeeded and billing migrations 64 and 68 were present                                               |
| `exactObjectVersionRead`          | the pinned old S3-compatible `VersionId` returned the expected AWS CUR fixture after replacement                                |
| `invoiceImported`                 | one immutable import and three normalized invoice lines were persisted with a SHA-256 source checksum                           |
| `estimateTariffSegmentsPersisted` | one terminated Worker fact crossed two tariff versions and produced eight immutable time-based estimates                        |
| `reconciliationPersisted`         | tagged CPU and memory invoice lines matched the exact resource-key estimates and persisted matched totals/counts                |
| `restartReplayIdempotent`         | a fresh adapter/service replay preserved import ID/checksum, line count, and all estimate IDs                                   |
| `scheduledAuditIdempotent`        | import and reconciliation retained exactly two audit rows with one shared non-empty scheduler correlation ID; replay added none |

The bounded detail was 735 bytes with SHA-256
`39b7c6b838577970ef2d7dc30ea018e43f7b49ee4a810af35cb69be684087695`. The outer final11 JSON SHA-256 is
`7d2ba0069fd311d361d05309b51bc5b1e347667d05064ef590a8bce1bd4a3e07`.

## Concurrent first-import guarantee

The production import transaction now takes a blocking PostgreSQL transaction advisory lock derived from the exact
tenant/provider/normalized external ID before checking or inserting the immutable import. The two-connection test held
the first transaction open and used `pg_blocking_pids` to prove the second backend actually waited on that identity.

- Equal checksums: both calls succeeded, the second returned the first committed import/line IDs, and PostgreSQL retained
  exactly one import plus one line.
- Different checksums: the second call returned stable `billing_invoice_import_conflict`, and PostgreSQL still retained
  exactly one import plus one line.
- The shared transaction path is used by manual and scheduled imports. SQLite remains a single-replica profile and keeps
  its existing no-op advisory-lock behavior.

The lock uses PostgreSQL's 64-bit `hashtextextended`; a theoretical hash collision can serialize unrelated invoice
identities but cannot merge or corrupt them because the database uniqueness and checksum checks still use full values.

## PostgreSQL-only audit defect closed

An earlier real PostgreSQL attempt exposed empty scheduled `audit_logs.request_id` values, which SQLite fixtures did not
reject. The scheduler now creates one bounded `billing-import-scheduler:<UUID>` request ID per due job and shares it
across import and reconciliation audit entries. final11 verifies the PostgreSQL constraint, shared correlation, and
mutation-only replay behavior.

## Cleanup and safety

- PostgreSQL and MinIO data used container-lifetime `tmpfs`; no anonymous or named Docker volume backed either data path.
- Generated credentials were written only to mode-`0600` temporary environment files, never command arguments or final
  evidence.
- Cleanup matched exact IDs, names, and the run label before removal and used `docker container rm -f -v`.
- Post-run inspection found no container, network, or volume carrying final11's run ID.
- Evidence reports `disposableRuntimeRemoved=true`, `credentialsPersistedAfterCleanup=false`,
  `objectContentsPersistedAfterCleanup=false`, and `testBinaryUnchangedDuringRun=true`.

Earlier final1-final4 attempts revealed that the original cleanup left image-declared anonymous volumes. Those exact
volumes were subsequently identified and removed, their evidence was corrected with the remediation time and exact
volume IDs, and the wrapper moved both state directories to `tmpfs`. final5-final8 failed closed on real fixture/schema
defects while cleanup passed. final9/final10 were sequential-replay passes; final11 supersedes them with concurrent
identity serialization and exact binary/source provenance. Failed evidence remains retained as an audit trail.

## Evidence boundary

- The test directly constructs `RuntimeConfig` with acceptance-only variables. It does not execute the deployment
  `SYNARA_BILLING_* -> config.Load()` path; focused config tests and manifest validation cover that wiring separately.
- This is local MinIO evidence. `cloudWorkloadIdentityVerified=false`; it does not prove AWS/GCP/Azure workload identity
  or native cloud billing-export delivery.
- The environment lane executes the AWS CUR parser. GCP and Azure parsers remain focused-test coverage until real export
  objects and cloud identities are available.
- The combined runtime Worker fact has no request claims, so its eight estimates cover CPU, memory, ephemeral storage,
  and Pod time across two tariff segments. Migration 68 request-claim accounting remains covered by separate migrated
  PostgreSQL concurrency tests.
- Shared/foreign Target allocation, historical incomplete claim ledgers, native partition discovery, multi-account cost
  allocation, rolling preaggregation, multi-region recovery, and production-duration soak remain external/future gates.

## Verification

- `go test ./internal/billing -count=1`: passed.
- final11 PostgreSQL + versioned MinIO runtime and concurrent-import lane: passed.
- The final11 JSON covers only the two named Billing integration tests and wrapper safety checks. Separate repository,
  Kubernetes-manifest, and Compose validation is not part of this evidence file.
- No Bun workspace checks were run; this work does not change the web application and repository policy requires an
  explicit user request before those heavyweight checks.
