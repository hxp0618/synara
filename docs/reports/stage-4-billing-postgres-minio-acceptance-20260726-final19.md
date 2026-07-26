# Stage 4 PostgreSQL + versioned MinIO billing runtime acceptance (final19)

## Result

final19 is the current local Billing environment proof. It passed against disposable PostgreSQL 17.10 and a
versioned MinIO S3-compatible object store from `2026-07-25T21:38:07Z` to `21:38:18Z`:

- status `passed`, exit code `0`;
- run ID `20260725213807-40220-2873`;
- dirty branch `codex/saas-tenancy-user` at base HEAD
  `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`;
- machine evidence:
  [stage-4-billing-postgres-minio-acceptance-20260726-final19.json](stage-4-billing-postgres-minio-acceptance-20260726-final19.json),
  SHA-256 `0591222a6d8be889efb453a68cecc6d260d3039bd6d06b854a850078ef693f38`.

This remains uncommitted, unpushed, local-only evidence. It is not AWS Data Exports, managed-cloud identity, or
production acceptance.

## Required tests

The wrapper executed these exact root tests separately through `go tool test2json`:

1. `TestPostgresConcurrentInitialInvoiceImportsSerializeByIdentity`;
2. `TestBillingRuntimePostgresVersionedS3ImportEstimateReconcileReplay`;
3. `TestBillingCUR2ManifestVersionedS3Acceptance`.

It required exactly one root `run` and `pass` event for every invocation and rejected any root or nested `skip` or
`fail`. The CUR2 root additionally required exactly one `run` and `pass` for each fail-closed subtest:

- `child_only_rejected`;
- `net_parent_gross_child_rejected`;
- `cross_chunk_missing_parent_rejected`.

This proves that orphan split children cannot be imported merely because their parent row or parent chunk is
missing. CPU and memory children are aggregated under one instance/hour/operation/product/cost-family replacement
boundary; every boundary must have exactly one parent and the child sum must match it. Duplicate parents, mixed
net/gross families, malformed positive adjustments, and incomplete boundaries fail the whole import.

## Runtime assertions

The environment lane also proved:

- current PostgreSQL migrations and immutable invoice import;
- exact old MinIO `VersionId` reads after the latest object version was replaced by poison data;
- an AWS CUR2-shaped native manifest with multiple version-pinned CSV/GZIP chunks using one complete
  `root/{metadata|data}/partition/execution/file` delivery boundary;
- bounded compressed, decompressed, chunk, row, and aggregate budgets;
- two-segment tariff estimates, reconciliation, leader-scoped scheduler audit, and restart replay;
- serialized concurrent first import: equal checksums return the same durable identity, while different checksums
  return the stable conflict.

The bounded detail was 735 bytes with SHA-256
`924bf5152979d33a7e70c1ef125851ec81cda94b36dedb6f740e0dfcedd32de6`.

## Exact dirty provenance

| Asset | SHA-256 |
| --- | --- |
| Acceptance wrapper | `fe2ae900d89304ab3d28b4513684fb21517058e2525c277f817b8fb4e4304e79` |
| Billing runtime PostgreSQL test | `29097245652556079087aa73967dfe844de137c62a2d37534136390cdfb2ba69` |
| Concurrent import PostgreSQL test | `fc547c4026403f608be59cdb26e0141dbff1bc5d2ed2442941be39fc257e5d06` |
| Native CUR2 manifest test | `7c81ab5264b2bf35914a26425b2f062b2950f8b7a43cd7e4b93e586967616102` |
| Billing service | `9e75b8d1d5f28c69ff1faa9b6e5808fc814bf022f9771f0a83f28bca9dc6e5af` |
| Blob parsers | `90b1343c19bfa583569742b76a020a0ac51a1fa33e10dba01e4f6ba5f7cc9b3d` |
| Blob source | `ab830965db6fc2224ac01d0b052407ab81a2a0dbc291da7d4e851e29378045ff` |
| Cloud sources | `fef9ac7350b79151c28bcc364ecbcb3f42e54d66a9471b91a8a5949a1fa344fc` |
| Executed Linux `billing.test` | `e7c2891d55f2f02bafb927bb188b954ce096543da3db2e2d2bbb9c8828967f35` |

The wrapper verified the same test-binary and source hashes before and after execution. The disposable binary was
removed with the environment, so the JSON records its bounded run provenance rather than claiming a retained
artifact.

## Cleanup and boundary

- PostgreSQL and MinIO used disposable, run-owned storage and generated credentials; exact cleanup passed and no
  run-owned container, network, volume, Credential, or object content remained.
- `cloudWorkloadIdentityVerified=false`: real AWS S3 Data Exports, EKS IRSA/Pod Identity, cross-account IAM/KMS and
  bucket-policy denial paths remain deployment gates.
- The native fixture is AWS-shaped but synthetic. A sanitized real CUR2 delivery must still validate native headers,
  resource-tag serialization, ETag/LastModified behavior, RI/Savings Plans combinations, and multi-chunk delivery.
- Parquet/Snappy Parquet remains explicitly unsupported and fails closed.
- GCP and Azure production exports and identities retain their separate cloud acceptance gates.

final19 supersedes final18 and every earlier local Billing snapshot for the current worktree. final13-final18 remain
as an honest diagnostic and review trail; they are not current release evidence.
