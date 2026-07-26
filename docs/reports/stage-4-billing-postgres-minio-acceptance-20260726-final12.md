# Stage 4 PostgreSQL + versioned MinIO billing runtime acceptance (final12)

## Result

final12 is the current local Billing environment proof. It passed against disposable PostgreSQL 17.10 and versioned
MinIO from `2026-07-25T19:07:23Z` to `19:07:34Z`:

- status `passed`, exit code `0`;
- run ID `20260725190723-91271-32328`;
- dirty branch `codex/saas-tenancy-user` at base HEAD
  `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`;
- machine evidence:
  [stage-4-billing-postgres-minio-acceptance-20260726-final12.json](stage-4-billing-postgres-minio-acceptance-20260726-final12.json),
  SHA-256 `76a71a72b50fc8979390e533aa000f3c88c529b633429435218f3773dd661d5a`.

This remains uncommitted, unpushed, local-only evidence—not managed-cloud or production acceptance.

## Required tests

The wrapper executed the two tests separately through `go tool test2json`:

1. `TestPostgresConcurrentInitialInvoiceImportsSerializeByIdentity`;
2. `TestBillingRuntimePostgresVersionedS3ImportEstimateReconcileReplay`.

It required exactly one root `run` and `pass` event for each test, pass events for both named concurrency subtests, and
zero `skip` or `fail` events anywhere under either test. The evidence records
`requiredTestCasesPassedWithoutSkip=true`; a skipped opt-in PostgreSQL test can no longer be mislabeled as passed.

The concurrent test used two real PostgreSQL backends and `pg_blocking_pids` to prove identity-lock waiting. Equal
checksums returned the same committed import/line IDs; different checksums returned stable
`billing_invoice_import_conflict`; each case left one import and one line.

## Runtime assertions

All seven bounded assertions were `true`:

- current PostgreSQL migrations completed, with billing migrations 64 and 68 explicitly checked;
- an old pinned MinIO `VersionId` returned the expected three-line AWS CUR fixture after the latest object was replaced
  by a poison payload;
- immutable invoice import persisted;
- eight CPU/memory/ephemeral/Pod estimates crossed two tariff segments;
- tagged CPU and memory actual lines persisted matched reconciliation totals/counts;
- fresh service/adapter replay preserved import, line, estimate, and audit identities;
- scheduled import/reconciliation shared one non-empty correlation ID and replay added no audit rows.

The bounded detail was 735 bytes, SHA-256
`6ddf9a920e3e733f34c0d98bf8326c447a63cd2f2656c9ebf72b78e4475ce0f5`.

## Exact dirty provenance

| Asset                             | SHA-256                                                            |
| --------------------------------- | ------------------------------------------------------------------ |
| Acceptance wrapper                | `b7ddaf3220e044a35e740d46f32a953d9752d302da55613937781abe88e5ff85` |
| Billing runtime PostgreSQL test   | `29097245652556079087aa73967dfe844de137c62a2d37534136390cdfb2ba69` |
| Concurrent import PostgreSQL test | `fc547c4026403f608be59cdb26e0141dbff1bc5d2ed2442941be39fc257e5d06` |
| Billing service                   | `02e5a53a81aaf21215051c43ff27f6a0f798a62c0e96c6bb96b58b0612b6f646` |
| Executed Linux `billing.test`     | `b7c88c2121185e6052d5bf67d3e1cb9d1a16471c429b89f265606f8132be5d8f` |

The host compiler was `go version go1.26.5 darwin/arm64`; the test ran in the pinned Go 1.26 bookworm image resolved to
`sha256:1b67dd879851e02ab4035680c180ed8d607ef4aaaf19e17ea1328a00f2450d86`. The binary hash was unchanged after
execution.

## Cleanup and boundary

- PostgreSQL and MinIO data used container-lifetime `tmpfs`; generated credentials used mode-`0600` temporary env files.
- Cleanup matched exact IDs, names, and run labels. A post-run query found no final12 container, network, or volume.
- Evidence reports runtime removed, no persisted credentials/object contents, and no credential in bounded detail.
- `cloudWorkloadIdentityVerified=false`: real AWS/GCP/Azure workload identity and native export objects remain deployment
  gates.
- The test directly constructs `RuntimeConfig`; it does not execute the deployment
  `SYNARA_BILLING_* -> config.Load()` path.
- AWS CUR is environment-tested here. GCP/Azure parsers, shared allocation, historical incomplete claim ledgers,
  multi-account allocation, rolling preaggregation, and production soak retain their documented boundaries.

final12 supersedes final11. final1-final8 failure evidence and final9-final11 intermediate passes remain retained as an
honest fail-closed/provenance audit trail.
