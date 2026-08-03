# PostgreSQL + versioned S3 billing acceptance

`postgres-minio-acceptance.sh` is the local environment-level acceptance lane for the billing runtime. It creates an
isolated Docker network, disposable PostgreSQL and versioned MinIO instances, compiles the billing package test for a
pinned Linux runtime image, and executes a directly constructed
`RuntimeConfig -> NewAdapterFromRuntime -> S3 -> scheduler` path inside that network. Deployment environment names and
`config.Load()` are covered by focused configuration tests and manifest validation, not by this container lane.

The lane proves:

- all Control Plane migrations apply to real PostgreSQL;
- an immutable S3 `VersionId` is honored even after a poison object becomes the latest version;
- AWS CUR parsing, immutable invoice import, two tariff segments, durable estimate generation, and reconciliation run
  through `RuntimeConfig -> NewAdapterFromRuntime -> RunImportSchedulerOnce`;
- two real PostgreSQL connections serialize the same first-import identity, returning one committed identity for equal
  checksums and a stable conflict for different checksums;
- native CUR 2.0 manifest ingestion uses a pinned manifest VersionId, versioned multi-chunk gzip reads, complete AWS
  `metadata/data/<partition>/<execution-id>` delivery boundaries, split CPU+Memory replacement accounting, source
  provenance, whole-import budgets, adjustment filtering, poison/race rejection, and fail-closed orphan split-child
  detection across chunks;
- a fresh service/adapter replay preserves import, estimate, and audit identities; and
- every run-owned container, internal network, temporary credential, database, and object is removed after success or
  failure.

## Run

Prerequisites are Docker, Go, `jq`, `openssl`, Python 3, and `shasum`. The output path must be explicit and must not
already exist:

```bash
SYNARA_COST_ACCOUNTING_ACCEPTANCE_EVIDENCE_FILE=docs/reports/stage-4-billing-postgres-minio-acceptance-local.json \
  deploy/billing/postgres-minio-acceptance.sh
```

The default PostgreSQL, MinIO, MinIO Client, and Linux test-runtime references can be overridden with the corresponding
`SYNARA_COST_ACCOUNTING_ACCEPTANCE_*_IMAGE` variables. The script resolves each reference before creating a container and
records the exact image ID used in the bounded JSON evidence. Because the repository may be dirty, it also records the
host Go version plus SHA-256 values for the wrapper, billing service, three required integration-test sources,
`blob_parsers.go`, `blob_source.go`, `cloud_sources.go`, and the compiled Linux test binary. The binary and the three
CUR 2.0 source files are hashed again after execution and any mutation fails the run. Required tests are:

- `TestBillingRuntimePostgresVersionedS3ImportEstimateReconcileReplay`
- `TestBillingCUR2ManifestVersionedS3Acceptance`
- `TestPostgresConcurrentInitialInvoiceImportsSerializeByIdentity`

All three are captured through `go tool test2json`; the wrapper requires one root `run`/`pass` event for each, both
named concurrency subtests, and these mandatory CUR 2.0 negative-path subtests:

- `TestBillingCUR2ManifestVersionedS3Acceptance/child_only_rejected`
- `TestBillingCUR2ManifestVersionedS3Acceptance/net_parent_gross_child_rejected`
- `TestBillingCUR2ManifestVersionedS3Acceptance/cross_chunk_missing_parent_rejected`

Each mandatory subtest must have exactly one `run` and one `pass`; any nested `skip` or `fail` event prevents success.

The script does not publish host ports. PostgreSQL and MinIO data live on container-lifetime `tmpfs`, generated
credentials are passed through mode-`0600` environment files rather than command arguments, and evidence is rejected if
it contains any generated credential or database URL. Cleanup is restricted to exact container/network IDs whose names
and run labels still match the current run.

## Boundary

This lane is repository/runtime evidence for the billing core, not a supported cloud-provider integration. Synara's
current product boundary is self-hosted Kubernetes with operator-managed versioned tariffs and durable
requested-resource accounting. Native AWS/GCP/Azure billing exports, provider Workload Identity, cross-account billing,
and managed-cloud acceptance are deferred and do not block Stage 4.

The provider-shaped parsers and versioned Blob adapters remain internal compatibility surfaces and test fixtures; they
must not be advertised as a supported AWS, GCP, or Azure billing connector. The combined lane uses time-based resource
charges only. Request-charge claim-ledger behavior is covered by the separate migrated PostgreSQL concurrency tests.
