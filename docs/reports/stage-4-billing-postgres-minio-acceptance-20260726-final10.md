# Stage 4 PostgreSQL + versioned MinIO billing runtime acceptance (final10)

Superseded by [final11](stage-4-billing-postgres-minio-acceptance-20260726-final11.md), which adds real PostgreSQL
concurrent-first-import serialization and exact dirty-source/test-binary provenance.

## Result

On 2026-07-26 local time, the billing runtime passed an isolated environment-level acceptance against real PostgreSQL
and a versioned S3-compatible MinIO API. The lane directly constructed `RuntimeConfig` with acceptance-only variables,
then exercised `NewAdapterFromRuntime`, scheduled import, built-in estimate sweep, reconciliation, audit persistence,
and a fresh service/adapter replay. It did not execute the deployment `SYNARA_BILLING_* -> config.Load()` path.

This report describes a local dirty worktree. It has not been committed, pushed, deployed to managed cloud, or released:

- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`
- Worktree dirty: `true`
- Machine-readable evidence:
  [stage-4-billing-postgres-minio-acceptance-20260726-final10.json](stage-4-billing-postgres-minio-acceptance-20260726-final10.json)
- Status: `passed`, exit code `0`
- Run ID: `20260725184216-70901-5788`
- UTC window: `2026-07-25T18:42:16Z` to `2026-07-25T18:42:23Z`

## Command

```bash
SYNARA_BILLING_ACCEPTANCE_EVIDENCE_FILE=docs/reports/stage-4-billing-postgres-minio-acceptance-20260726-final10.json \
  deploy/billing/postgres-minio-acceptance.sh
```

No host port was published. The test binary ran inside the run-owned internal Docker network.

## Runtime

| Role | Exact runtime |
| --- | --- |
| Metadata | disposable PostgreSQL `17.10`, image ID `sha256:8ee7c900f4de054e8f0a3b42b8f5da38b8fba57c3ccd7c1a02be28cb0b06494f` |
| Object store | disposable versioned MinIO, image ID `sha256:6c83c74c8028019d82d3f20d65cc6645fd5694b82a25dc38733df5227ffa4e8d` |
| Object client | MinIO Client image ID `sha256:0029bb25aef96434e35c2c7d8aa39b32913ec38c4c044f0c7551489f8d0d0e71` |
| Test runtime | pinned Go 1.26 bookworm digest, resolved image ID `sha256:1b67dd879851e02ab4035680c180ed8d607ef4aaaf19e17ea1328a00f2450d86`, `arm64` |

The wrapper first uploaded the bounded AWS CUR fixture and recorded its exact `VersionId`, then overwrote the same key
with a poison payload. The runtime mapping pinned the old version. Parsing the expected three-line fixture rather than
the poison latest version proves that the configured immutable object version was honored.

## Passed assertions

All seven allowlisted assertions were `true`:

| Assertion | Evidence |
| --- | --- |
| `postgresMigrationsApplied` | real PostgreSQL bootstrap succeeded and billing migrations 64 and 68 were present |
| `exactObjectVersionRead` | the pinned old S3-compatible `VersionId` returned the expected AWS CUR fixture after replacement |
| `invoiceImported` | one immutable import and three normalized invoice lines were persisted with a SHA-256 source checksum |
| `estimateTariffSegmentsPersisted` | one terminated Worker fact crossed two tariff versions and produced eight immutable time-based estimates |
| `reconciliationPersisted` | tagged CPU and memory invoice lines matched the exact resource-key estimates and persisted matched totals/counts |
| `restartReplayIdempotent` | a fresh adapter/service replay preserved import ID/checksum, line count, and all estimate IDs |
| `scheduledAuditIdempotent` | import and reconciliation retained exactly two audit rows with one non-empty shared scheduler correlation ID; replay added none |

The bounded test detail was 735 bytes with SHA-256
`8bbabd1354735896a22d94c89f16133ce35a1ea0d22f74cda5887e19ca52f3a5`. The outer final10 JSON SHA-256 is
`6a87f48c48ca072d4fed98e4ab09ca73f18d3b63feb52ed5843fa706178071eb`.

## PostgreSQL-only defect found and closed

The first real PostgreSQL scheduler attempt exposed that scheduled import/reconciliation audit entries had an empty
`request_id`. SQLite fixtures did not enforce the production `audit_logs` length constraint, so the mutation rolled
back only in PostgreSQL. The scheduler now creates one bounded `billing-import-scheduler:<UUID>` correlation ID for a
due job and shares it across its import and reconciliation audit rows. Unit coverage and final10 verify the value and
replay behavior.

## Cleanup and safety

- PostgreSQL and MinIO data used container-lifetime `tmpfs`; no anonymous or named Docker volume backed either data path.
- Generated credentials were written only to mode-`0600` temporary environment files, never command arguments or final
  evidence.
- Cleanup matched exact IDs, names, and the run label before removal and used `docker container rm -f -v`.
- Post-run inspection found no container, network, or volume carrying final10's run ID.
- Evidence reports `disposableRuntimeRemoved=true`, `credentialsPersistedAfterCleanup=false`, and
  `objectContentsPersistedAfterCleanup=false`.

Earlier final1-final4 attempts revealed that the original cleanup left image-declared anonymous volumes. Those exact
volumes were subsequently identified and removed, their evidence was corrected in place with the remediation time and
exact volume IDs, and the wrapper moved both state directories to `tmpfs`. final5-final8 then failed closed on real test
fixture/constraint defects while cleanup passed. final9 was the first passing lane; final10 supersedes it after adding
the shared audit-correlation assertion. The failed evidence remains retained as an audit trail.

## Evidence boundary

- This is local MinIO S3-compatible evidence. `cloudWorkloadIdentityVerified=false`; it does not prove AWS IRSA/EKS Pod
  Identity, GCP Workload Identity, Azure Workload Identity, or native cloud billing-export delivery.
- The combined lane verifies the AWS CUR parser. GCP and Azure parsers remain covered by focused tests but still require
  real export objects and cloud identity in deployment acceptance.
- The Worker fact intentionally has no request claims, so the eight estimates cover CPU, memory, ephemeral storage, and
  Pod time across two tariff segments. Migration 68 claim-ledger concurrency and request charges are covered by separate
  migrated PostgreSQL tests, not re-proved here.
- Shared/foreign Target allocation, historical incomplete claim ledgers, native export partition discovery,
  multi-account cost allocation, and long-term rolling preaggregation remain fail-closed or future work.
- The local run does not prove production retention, multi-region database/object-store recovery, or long-duration soak.

## Verification

- `go test ./internal/billing -count=1`: passed.
- final10 PostgreSQL + versioned MinIO runtime lane: passed.
- No Bun workspace checks were run; this work does not change the web application and repository policy requires an
  explicit user request before those heavyweight checks.
